package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// probeStub answers completions by inspecting the request shape, so tests can
// script per-capability behaviour the way a real provider would.
type probeStub struct {
	// seen records every request for shape assertions.
	seen []*providers.Request
	// succeed lists capabilities whose probe gets a 200.
	succeed map[domain.Capability]bool
	// refuse lists capabilities whose probe gets a 400 naming the feature.
	refuse map[domain.Capability]string
	// failAll makes every probe die with this error (auth, rate limit, 5xx).
	failAll error
}

func (s *probeStub) which(req *providers.Request) domain.Capability {
	if req == nil || req.Params == nil {
		return ""
	}
	if len(req.Params.Tools) > 0 {
		return domain.CapTools
	}
	if req.Params.ResponseFormat != nil {
		if req.Params.ResponseFormat.Type == "json_schema" {
			return domain.CapJSONSchema
		}
		return domain.CapJSONMode
	}
	return ""
}

func (s *probeStub) Name() string { return "probe-stub" }

func (s *probeStub) HealthCheck(context.Context) domain.ProviderHealth {
	return domain.ProviderHealth{}
}

func (s *probeStub) ChatCompletion(_ context.Context, req *providers.Request) (*providers.Response, error) {
	s.seen = append(s.seen, req)
	if s.failAll != nil {
		return nil, s.failAll
	}
	cap := s.which(req)
	if msg, ok := s.refuse[cap]; ok {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, msg).WithStatus(400)
	}
	if s.succeed[cap] {
		return &providers.Response{Choices: []domain.Choice{{Message: &domain.ChatMessage{
			Role:    domain.RoleAssistant,
			Content: domain.NewTextContent("ok"),
		}}}}, nil
	}
	// Anything unscripted is indeterminate rather than a guess.
	return nil, domain.NewError(domain.ErrCodeUnavailable, "upstream exploded").WithStatus(502)
}

func testRef(t *testing.T) domain.ModelRef {
	t.Helper()
	return domain.ModelRef{ProviderID: "p1", ProviderName: "probe-stub", Model: "m1"}
}

// A 200 proves the capability. This is the entire product: the gateway has
// asked the model, and the model answered.
func TestProbeProvesOnSuccess(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	out := ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapTools},
	})
	// Chat rides along: the successful tools call was itself a chat completion.
	if len(out.Proven) != 2 || out.Proven[0] != domain.CapChat || out.Proven[1] != domain.CapTools {
		t.Fatalf("proven = %v, want [chat tools]", out.Proven)
	}
	if out.Indeterminate {
		t.Error("a success must not be indeterminate")
	}
}

// A 400 that names the feature decides the question: the provider understood
// everything except the one thing under test. Absence is deliberately not
// recorded — the registry reads an empty list as "unknown", and writing "no
// tools" here would misroute later traffic on a guess.
func TestProbeRefusalDecidesWithoutRecording(t *testing.T) {
	stub := &probeStub{refuse: map[domain.Capability]string{
		domain.CapTools: `unexpected field "tools": this model does not support tool_choice`,
	}}
	out := ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapTools},
	})
	if len(out.Proven) != 0 {
		t.Fatalf("proven = %v, want nothing", out.Proven)
	}
	if out.Indeterminate {
		t.Error("a feature refusal decides the question; it is not indeterminate")
	}
}

// Everything that is not a feature refusal decides nothing: credentials, rate
// limits, provider outages, timeouts. Each of these misread as "no tools" would
// silently break routing for a model that is perfectly capable.
func TestProbeFailuresAreIndeterminate(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"auth", domain.NewError(domain.ErrCodeAuthentication, "bad key").WithStatus(401)},
		{"rate limit", domain.NewError(domain.ErrCodeRateLimited, "slow down").WithStatus(429)},
		{"provider 5xx", domain.NewError(domain.ErrCodeUnavailable, "upstream exploded").WithStatus(502)},
		{"transport", errors.New("connection reset by peer")},
		{"400 about something else", domain.NewError(domain.ErrCodeInvalidRequest, "max_tokens must be positive").WithStatus(400)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &probeStub{failAll: tc.err}
			out := ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
				Probes: []domain.Capability{domain.CapTools},
			})
			if len(out.Proven) != 0 {
				t.Fatalf("proven = %v, want nothing", out.Proven)
			}
			if !out.Indeterminate {
				t.Errorf("%v must leave the question open", tc.err)
			}
		})
	}
}

// The probe must be cheap by construction: a two-word prompt and a one-token
// allowance, with tool_choice auto rather than a forced call so the provider
// may answer in text.
func TestProbeRequestShapeIsMinimal(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapTools},
	})
	if len(stub.seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(stub.seen))
	}
	req := stub.seen[0]
	if req.Model != "m1" {
		t.Errorf("probed model = %q, want m1", req.Model)
	}
	if req.MaxTokens != 1 {
		t.Errorf("allowance = %d, want the 1-token minimum", req.MaxTokens)
	}
	if len(req.Params.Tools) != 1 || req.Params.Tools[0].Function.Name == "" {
		t.Fatalf("probe carries no usable tool: %+v", req.Params.Tools)
	}
	if got := strings.TrimSpace(string(req.Params.ToolChoice)); got != `"auto"` {
		t.Errorf("tool_choice = %s, want auto: a forced call bills real tokens", got)
	}
}

// One model, three questions, all answered: the runner's unit of work.
func TestDetectProvesAndPersists(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{
		domain.CapTools: true, domain.CapJSONMode: true,
	}}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "m1", Status: domain.ModelActive},
	}}
	report, err := DetectMissingCapabilities(context.Background(), stub, store,
		domain.Provider{ID: "p1", Name: "probe-stub"}, DetectionOptions{
			Probes: []domain.Capability{domain.CapTools, domain.CapJSONMode, domain.CapJSONSchema},
		})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(report.Attempted) != 1 || len(report.Proven["m1"]) != 3 {
		t.Fatalf("report = %+v", report)
	}
	// Chat is proven by construction: every probe is itself a chat completion.
	// Without it the written list would unroute the model from plain requests,
	// because the registry reads a non-empty list exhaustively.
	for _, want := range []domain.Capability{domain.CapChat, domain.CapTools, domain.CapJSONMode} {
		found := false
		for _, got := range report.Proven["m1"] {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("proven list %v is missing %q", report.Proven["m1"], want)
		}
	}
	row := store.rows[len(store.rows)-1]
	if !row.CapabilitySet().Contains(domain.CapTools) {
		t.Fatalf("proven capabilities were not persisted: %v", row.Capabilities)
	}
	if row.Metadata[CapabilitiesSourceKey] != CapabilitiesSourceProbed {
		t.Errorf("source = %q, want %q", row.Metadata[CapabilitiesSourceKey], CapabilitiesSourceProbed)
	}
	if len(report.Indeterminate) != 0 {
		t.Errorf("indeterminate = %v", report.Indeterminate)
	}
}

// The load-bearing rule: a model that already declares a list is never probed
// and never rewritten, even when the probe would have said something else.
func TestDetectNeverTouchesDeclaredCapabilities(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	declared := []domain.Capability{domain.CapChat, domain.CapVision}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "m1", Status: domain.ModelActive, Capabilities: declared},
	}}
	report, err := DetectMissingCapabilities(context.Background(), stub, store,
		domain.Provider{ID: "p1", Name: "probe-stub"}, DetectionOptions{})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(stub.seen) != 0 {
		t.Fatalf("probed a declared model: %d requests", len(stub.seen))
	}
	if report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", report.Skipped)
	}
	if len(store.rows) != 1 || store.rows[0].CapabilitySet().Contains(domain.CapTools) {
		t.Fatalf("declared list was overwritten: %+v", store.rows)
	}
}

// Indeterminate outcomes change nothing. The row stays empty and stays
// retryable, which is exactly what "unknown" means.
func TestDetectLeavesIndeterminateRowsAlone(t *testing.T) {
	stub := &probeStub{failAll: domain.NewError(domain.ErrCodeUnavailable, "boom").WithStatus(502)}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "m1", Status: domain.ModelActive},
	}}
	report, err := DetectMissingCapabilities(context.Background(), stub, store,
		domain.Provider{ID: "p1", Name: "probe-stub"}, DetectionOptions{})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(report.Indeterminate) != 1 {
		t.Fatalf("indeterminate = %v", report.Indeterminate)
	}
	if len(store.rows) != 1 || len(store.rows[0].Capabilities) != 0 {
		t.Fatalf("indeterminate run wrote capabilities: %+v", store.rows)
	}
	if _, ok := store.rows[0].Metadata[CapabilitiesSourceKey]; ok {
		t.Error("no source should be recorded when nothing was learned")
	}
}

// Bounds are real: MaxModels caps the bill and Only scopes the run.
func TestDetectRespectsBounds(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	store := &memModelStore{rows: []domain.Model{
		{ID: "a", ProviderID: "p1", Name: "m1", Status: domain.ModelActive},
		{ID: "b", ProviderID: "p1", Name: "m2", Status: domain.ModelActive},
		{ID: "c", ProviderID: "p1", Name: "m3", Status: domain.ModelActive},
	}}
	report, err := DetectMissingCapabilities(context.Background(), stub, store,
		domain.Provider{ID: "p1", Name: "probe-stub"},
		DetectionOptions{MaxModels: 2, Concurrency: 2})
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(report.Attempted) != 2 {
		t.Fatalf("attempted = %v, want exactly the cap", report.Attempted)
	}
}

// Duplicate probes in one run are collapsed: one model is never billed twice
// for the same question.
func TestProbeDeduplicates(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapTools, domain.CapTools},
	})
	if len(stub.seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(stub.seen))
	}
}

// streamStub serves both the plain and the streaming completion paths.
type streamStub struct {
	probeStub
	streamErr error
	streamed  int
}

func (s *streamStub) ChatCompletionStream(_ context.Context, req *providers.Request, handler providers.StreamHandler) (*providers.Response, error) {
	s.streamed++
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	// One chunk is the whole proof: the event stream exists.
	_ = handler(providers.Chunk{})
	return &providers.Response{}, nil
}

// A working event stream proves streaming, and the stream itself proves chat.
func TestProbeStreamingProven(t *testing.T) {
	stub := &streamStub{}
	out := ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapStreaming},
	})
	if stub.streamed != 1 {
		t.Fatalf("stream attempts = %d, want 1", stub.streamed)
	}
	has := func(c domain.Capability) bool {
		for _, p := range out.Proven {
			if p == c {
				return true
			}
		}
		return false
	}
	if !has(domain.CapStreaming) {
		t.Errorf("proven = %v, want streaming", out.Proven)
	}
	if !has(domain.CapChat) {
		t.Errorf("proven = %v, want chat by construction", out.Proven)
	}
}

// A 400 that names streaming decides the question without recording anything.
func TestProbeStreamingRefusal(t *testing.T) {
	stub := &streamStub{streamErr: domain.NewError(domain.ErrCodeInvalidRequest, "streaming is not supported on this deployment").WithStatus(400)}
	out := ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapStreaming},
	})
	if len(out.Proven) != 0 {
		t.Fatalf("proven = %v, want nothing", out.Proven)
	}
	if out.Indeterminate {
		t.Error("a streaming refusal decides the question")
	}
}

// An adapter without a streaming path skips the probe entirely: no call, no
// claim, no indeterminacy. probeStub deliberately lacks ChatCompletionStream.
func TestProbeStreamingSkippedWithoutSupport(t *testing.T) {
	stub := &probeStub{succeed: map[domain.Capability]bool{domain.CapTools: true}}
	out := ProbeModelCapabilities(context.Background(), stub, testRef(t), "m1", ProbeOptions{
		Probes: []domain.Capability{domain.CapTools, domain.CapStreaming},
	})
	for _, p := range out.Proven {
		if p == domain.CapStreaming {
			t.Fatalf("streaming recorded without a stream ever opening: %v", out.Proven)
		}
	}
}
