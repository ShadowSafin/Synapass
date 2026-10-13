package admin

import (
	"context"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// CapabilitiesSourceProbed records that a model's capability list was learned
// by asking the model, not read from a catalogue or written by hand. It sits
// alongside CapabilitiesSourceProvider and CapabilitiesSourceDeclared in model
// metadata so an operator can see why a model claims what it claims.
const CapabilitiesSourceProbed = "probed"

// Default probes: the capabilities that are cheap to test and expensive to
// get wrong. Vision is deliberately excluded — probing it needs an image
// payload and real inference tokens, which is a different cost class from a
// max_tokens: 1 text probe.
//
// Chat is not listed because it needs no test: every probe below IS a chat
// completion, so a single success proves the model serves chat, and that is
// recorded automatically. A probed list therefore always contains chat when it
// contains anything at all, which matters because the registry reads a
// non-empty list exhaustively — a list without chat would unroute the model
// from every request.
var defaultProbeCapabilities = []domain.Capability{
	domain.CapTools,
	domain.CapStreaming,
	domain.CapJSONMode,
	domain.CapJSONSchema,
}

// ProbeOptions bounds one capability probe.
type ProbeOptions struct {
	// Timeout bounds a single probe request. Zero means 30 seconds.
	Timeout time.Duration
	// MaxTokens is the completion allowance per probe. Zero means 1. The
	// allowance is not about quality: a probe only needs the provider to
	// accept the request shape, not to produce a useful answer.
	MaxTokens int
	// Probes selects which capabilities to test. Empty means the default set.
	Probes []domain.Capability
}

// ProbeOutcome is what asking one model proved.
//
// Only affirmative evidence is recorded. A refusal that names the feature means
// the model cannot do it, and that is deliberately NOT recorded as a negative
// claim: the registry's contract is that an empty list means "unknown, use the
// kind default", and writing "no tools" into a row would silently downgrade a
// model that might simply have been misconfigured at that moment.
//
// Proven always contains chat when it contains anything: every probe is itself
// a chat completion, so a success proves the model serves chat by construction.
type ProbeOutcome struct {
	// Model is the wire name that was probed.
	Model string
	// Proven holds only capabilities a successful call demonstrated.
	Proven []domain.Capability
	// Indeterminate means nothing could be decided — transport failure, auth,
	// rate limiting, provider 5xx, timeout. Callers should leave the row alone
	// and may retry later.
	Indeterminate bool
}

// ProbeModelCapabilities asks a model what it can do.
//
// Each probe is a minimal completion that exercises exactly one feature. The
// prompt is two words and the allowance is one token, because the call exists
// to test the request shape, not to buy an answer.
func ProbeModelCapabilities(ctx context.Context, adapter TestAdapter, ref domain.ModelRef, model string, opts ProbeOptions) ProbeOutcome {
	out := ProbeOutcome{Model: model}
	probes := opts.Probes
	if len(probes) == 0 {
		probes = defaultProbeCapabilities
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1
	}

	// Deduplicate the probe list so one model is never billed twice for the
	// same question in one run.
	anySuccess := false
	seen := map[domain.Capability]bool{}
	for _, cap := range probes {
		if seen[cap] {
			continue
		}
		seen[cap] = true
		step, cancel := context.WithTimeout(ctx, timeout)
		proven, decided := probeCapability(step, adapter, ref, model, cap, maxTokens)
		cancel()
		if proven {
			out.Proven = append(out.Proven, cap)
			anySuccess = true
		} else if !decided {
			out.Indeterminate = true
		}
	}
	// Every probe is a chat completion, so a success proves chat by
	// construction. This is load-bearing, not cosmetic: the registry reads a
	// non-empty capability list exhaustively, so a proven list without chat
	// would unroute the model from plain requests.
	if anySuccess {
		out.Proven = append([]domain.Capability{domain.CapChat}, out.Proven...)
	}
	return out
}

// probeCapability tests one capability. It reports whether the feature was
// demonstrated, and separately whether anything was decided at all — a
// transport failure decides nothing, and must not be read as an absence.
func probeCapability(ctx context.Context, adapter TestAdapter, ref domain.ModelRef, model string, cap domain.Capability, maxTokens int) (proven, decided bool) {
	if cap == domain.CapStreaming {
		return probeStreaming(ctx, adapter, ref, model, maxTokens)
	}
	req, ok := probeRequest(ref, model, cap, maxTokens)
	if !ok {
		// Not a probed capability. This is a caller bug rather than a model
		// fact, so it decides nothing.
		return false, false
	}
	_, err := adapter.ChatCompletion(ctx, req)
	if err == nil {
		return true, true
	}
	// A 400 that names the feature is the provider refusing the shape: the
	// model understood everything except the one thing being tested.
	if refused, feature := featureRefused(err, cap); refused && feature {
		return false, true
	}
	// Auth, rate limits, provider 5xx, timeouts and context overflow say
	// nothing about the feature. Record nothing.
	return false, false
}

// streamProber is the streaming half of the adapter contract. TestAdapter does
// not include it, so adapters that cannot stream simply do not implement it
// and the probe is skipped rather than failed.
type streamProber interface {
	ChatCompletionStream(ctx context.Context, req *providers.Request, handler providers.StreamHandler) (*providers.Response, error)
}

// probeStreaming tests SSE by opening a real stream and reading it. The
// allowance stays at one token: establishing the event stream is the proof,
// not the content it carries.
func probeStreaming(ctx context.Context, adapter TestAdapter, ref domain.ModelRef, model string, maxTokens int) (proven, decided bool) {
	streamer, ok := adapter.(streamProber)
	if !ok {
		return false, false
	}
	req, ok := probeRequest(ref, model, domain.CapStreaming, maxTokens)
	if !ok {
		return false, false
	}
	// The handler only needs to observe; returning nil keeps the stream open
	// for its single token, and the tight allowance bounds the cost.
	var chunks int
	_, err := streamer.ChatCompletionStream(ctx, req, func(providers.Chunk) error {
		chunks++
		return nil
	})
	if err == nil {
		return true, true
	}
	if refused, feature := featureRefused(err, domain.CapStreaming); refused && feature {
		return false, true
	}
	return false, false
}

// probeRequest builds the minimal completion that exercises one capability.
func probeRequest(ref domain.ModelRef, model string, cap domain.Capability, maxTokens int) (*providers.Request, bool) {
	params := &domain.ChatCompletionRequest{
		Model:     model,
		Messages:  []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("Reply with ok.")}},
		MaxTokens: &maxTokens,
	}
	stream := false
	switch cap {
	case domain.CapStreaming:
		// No special shape: the streaming itself is the test, selected by the
		// call path rather than the body.
		stream = true
	case domain.CapTools:
		params.Tools = []domain.Tool{{
			Type: "function",
			Function: domain.FunctionDefinition{
				Name:        "probe_ping",
				Description: "A probe that accepts any input and returns ok.",
				Parameters:  []byte(`{"type":"object","properties":{}}`),
			},
		}}
		// tool_choice auto is the weakest assertion that still exercises the
		// tools path: the provider may answer in text, but it must accept the
		// shape. Requiring an actual tool call would bill real tokens and make
		// the probe depend on model behaviour rather than capability.
		params.ToolChoice = []byte(`"auto"`)
	case domain.CapJSONMode:
		params.ResponseFormat = &domain.ResponseFormat{Type: "json_object"}
	case domain.CapJSONSchema:
		params.ResponseFormat = &domain.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: []byte(`{"name":"probe","schema":{"type":"object"}}`),
		}
	default:
		return nil, false
	}
	return &providers.Request{
		Model:     model,
		Params:    params,
		Ref:       ref,
		MaxTokens: maxTokens,
		Stream:    stream,
	}, true
}

// featureRefused reports whether err is the provider declining the tested
// feature specifically, as opposed to anything else going wrong.
//
// Only 400-class responses count. A 401 is credentials, a 429 is a rate limit,
// a 5xx is somebody else's problem, and any of those misread as "no tools"
// would silently misroute later traffic.
func featureRefused(err error, cap domain.Capability) (refused, namesFeature bool) {
	var derr *domain.Error
	if !asDomainError(err, &derr) {
		// An unwrapped transport error or context cancellation decides nothing.
		return false, false
	}
	if derr.Status != 400 && derr.Status != 422 {
		return false, false
	}
	return true, refusalNamesCapability(derr, cap)
}

// refusalNamesCapability checks the provider's own words for the feature. The
// refusal text is the only honest signal here: status alone says "bad request"
// without saying which part was bad, and the tools probe is the only unusual
// field the request carries.
func refusalNamesCapability(derr *domain.Error, cap domain.Capability) bool {
	text := strings.ToLower(derr.Message + " " + derr.Param)
	matches := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(text, strings.ToLower(w)) {
				return true
			}
		}
		return false
	}
	switch cap {
	case domain.CapTools:
		return matches("tool_choice", `"tools"`, "tools", "function_call", "function call", "parallel_tool")
	case domain.CapStreaming:
		// Deliberately no bare "stream": it matches "upstream" and
		// "downstream" in generic error text, which would misread an
		// unrelated 400 as "no streaming".
		return matches("streaming", "sse", "event-stream", "event stream", "stream not supported", "does not support stream")
	case domain.CapJSONMode:
		return matches("response_format", "json_object", "json mode")
	case domain.CapJSONSchema:
		return matches("response_format", "json_schema", "json schema", "response schema")
	default:
		return false
	}
}

// asDomainError unwraps err to a *domain.Error.
func asDomainError(err error, target **domain.Error) bool {
	for err != nil {
		if e, ok := err.(*domain.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
