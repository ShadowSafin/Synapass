package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// openAIAdapter serves the OpenAI wire format.
//
// A single implementation covers three of the five provider kinds:
//
//   - openai               api.openai.com
//   - vllm                 a self-hosted vLLM server
//   - openai_compatible    anything else speaking the same protocol
//
// They differ only in base URL, credentials and which optional request fields
// are accepted, so keeping them in one adapter means a fix to the wire format
// benefits every compatible provider at once. The behavioural differences are
// expressed as small conditionals rather than as divergent code paths.
type openAIAdapter struct {
	*baseAdapter
	// kind is duplicated out of the embedded config so the hot path avoids a
	// pointer hop through the provider struct.
	kind domain.ProviderKind
}

// newOpenAIAdapter constructs an adapter for the given provider.
func newOpenAIAdapter(p domain.Provider, opts Options) (*openAIAdapter, error) {
	base, err := newBaseAdapter(p, opts)
	if err != nil {
		return nil, err
	}
	return &openAIAdapter{baseAdapter: base, kind: p.Kind}, nil
}

// chatPath is the completions endpoint path.
func (a *openAIAdapter) chatPath() string { return "/chat/completions" }

// healthPath returns the probe endpoint. OpenAI's /models doubles as a cheap
// credential check; vLLM exposes no path under /v1 that is cheaper.
func (a *openAIAdapter) healthPath() string { return "/models" }

// openAIRequestBody is the serialized request. Field presence is controlled with
// pointers and omitempty so the gateway forwards exactly what the client sent
// plus the resolved model and token allowance.
type openAIRequestBody struct {
	Model       string               `json:"model"`
	Messages    []domain.ChatMessage `json:"messages"`
	Temperature *float64             `json:"temperature,omitempty"`
	TopP        *float64             `json:"top_p,omitempty"`
	// TopK, MinP and RepetitionPenalty are outside OpenAI's own API and are
	// only sent to kinds known to accept them (vLLM, OpenAI-compatible
	// servers). Sending them to OpenAI proper would 400 every request that
	// sets them, which is worse than dropping a control the provider cannot
	// honour.
	TopK              *int                   `json:"top_k,omitempty"`
	MinP              *float64               `json:"min_p,omitempty"`
	RepetitionPenalty *float64               `json:"repetition_penalty,omitempty"`
	N                 *int                   `json:"n,omitempty"`
	Stream            bool                   `json:"stream,omitempty"`
	StreamOptions     *domain.StreamOptions  `json:"stream_options,omitempty"`
	Stop              jsonRaw                `json:"stop,omitempty"`
	MaxTokens         int                    `json:"max_tokens,omitempty"`
	PresencePenalty   *float64               `json:"presence_penalty,omitempty"`
	FrequencyPenalty  *float64               `json:"frequency_penalty,omitempty"`
	LogitBias         map[string]int         `json:"logit_bias,omitempty"`
	Logprobs          *bool                  `json:"logprobs,omitempty"`
	TopLogprobs       *int                   `json:"top_logprobs,omitempty"`
	User              string                 `json:"user,omitempty"`
	Seed              *int                   `json:"seed,omitempty"`
	ResponseFormat    *domain.ResponseFormat `json:"response_format,omitempty"`
	Tools             []domain.Tool          `json:"tools,omitempty"`
	ToolChoice        jsonRaw                `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                  `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string                 `json:"reasoning_effort,omitempty"`
}

// jsonRaw is json.RawMessage under a shorter name. The omitempty tag does
// suppress a zero-length RawMessage, because the encoder's emptiness check for a
// slice is a length check, so a field the client omitted stays omitted.
type jsonRaw = json.RawMessage

// buildBody translates a normalized request into the OpenAI wire format.
func (a *openAIAdapter) buildBody(req *Request, stream bool) *openAIRequestBody {
	p := req.Params

	body := &openAIRequestBody{
		Model:    req.Model,
		Messages: p.Messages,
		Stream:   stream,
	}

	// OpenAI moved to max_completion_tokens for newer reasoning models, but the
	// broad ecosystem (and vLLM) still expects max_tokens. Sending the field the
	// client originally used keeps behaviour identical to calling the provider
	// directly, which is the compatibility promise.
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}

	// temperature and top_p are accepted by vLLM and OpenAI. Some OpenAI
	// reasoning models reject them, in which case the provider returns a 400
	// that the fallback chain can act on; silently dropping them would break
	// deterministic clients.
	body.Temperature = p.Temperature
	body.TopP = p.TopP
	body.N = p.N
	body.Stop = jsonRaw(p.Stop)
	body.PresencePenalty = p.PresencePenalty
	body.FrequencyPenalty = p.FrequencyPenalty
	body.LogitBias = p.LogitBias
	body.Logprobs = p.Logprobs
	body.TopLogprobs = p.TopLogprobs
	body.User = p.User
	body.ResponseFormat = p.ResponseFormat
	body.Tools = p.Tools
	body.ToolChoice = jsonRaw(p.ToolChoice)
	body.ParallelToolCalls = p.ParallelToolCall
	body.ReasoningEffort = p.ReasoningEffort

	// The seed field is an OpenAI extension; vLLM supports it, Ollama does not,
	// so only OpenAI-family providers receive it.
	if a.kind == domain.ProviderOpenAI || a.kind == domain.ProviderVLLM {
		body.Seed = p.Seed
	}

	// Sampling controls outside OpenAI's own API surface go only to the kinds
	// that implement them.
	if a.kind == domain.ProviderVLLM || a.kind == domain.ProviderOpenAICompatible {
		body.TopK = p.TopK
		body.MinP = p.MinP
		body.RepetitionPenalty = p.RepetitionPenalty
	}

	if stream {
		// Requesting usage on a stream is what makes streaming requests billable
		// with exact numbers. Not every provider supports the field, so it is
		// only set for the kinds known to accept it.
		if a.kind != domain.ProviderOpenAICompatible {
			include := true
			if p.StreamOptions != nil {
				include = p.StreamOptions.IncludeUsage
			}
			body.StreamOptions = &domain.StreamOptions{IncludeUsage: include}
		}
	}

	return body
}

// openAIResponse is the non-streaming response envelope.
type openAIResponse struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	Choices           []openAIChoice    `json:"choices"`
	Usage             *openAIUsage      `json:"usage"`
	SystemFingerprint string            `json:"system_fingerprint"`
	Error             *openAIErrorField `json:"error"`
}

// openAIChoice is one completion candidate.
type openAIChoice struct {
	Index        int            `json:"index"`
	Message      *openAIMessage `json:"message"`
	Delta        *openAIMessage `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
	Logprobs     jsonRaw        `json:"logprobs"`
}

// openAIMessage is a chat message in the wire format.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    jsonRaw          `json:"content"`
	Name       string           `json:"name"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
	Refusal    string           `json:"refusal"`
	// ReasoningContent carries the thinking trace on providers that emit
	// it (DeepSeek, OpenRouter and compatible proxies). Usually a string,
	// kept raw because some servers send null or a structured variant.
	ReasoningContent jsonRaw `json:"reasoning_content,omitempty"`
}

// openAIToolCall is a tool invocation fragment.
type openAIToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIUsage mirrors the provider's usage object.
type openAIUsage struct {
	PromptTokens        int                       `json:"prompt_tokens"`
	CompletionTokens    int                       `json:"completion_tokens"`
	TotalTokens         int                       `json:"total_tokens"`
	PromptTokensDetails *openAIPromptTokenDetails `json:"prompt_tokens_details"`
}

// openAIPromptTokenDetails reports cached prompt tokens on providers that
// implement prompt caching.
type openAIPromptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// openAIErrorField is an error returned with a 2xx status, which some
// compatible servers do.
type openAIErrorField struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// ChatCompletion performs one non-streaming attempt.
func (a *openAIAdapter) ChatCompletion(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := contextWithAttempt(ctx, a.Timeout().PerAttempt)
	defer cancel()

	body := a.buildBody(req, false)
	_, respBody, err := a.postJSON(ctx, a.chatPath(), req.Model, 1, body)
	if err != nil {
		return nil, err
	}

	var parsed openAIResponse
	if err := decodeJSON(respBody, &parsed); err != nil {
		return nil, NormalizeDecodeError(a.Name(), req.Model, 1, err, respBody)
	}

	// A 200 carrying an error object is a provider bug; surface it as upstream
	// rather than returning an empty completion that looks like success.
	if parsed.Error != nil && len(parsed.Choices) == 0 {
		return nil, domain.NewError(domain.ErrCodeUpstream,
			"provider returned an error in a successful response: "+parsed.Error.Message).
			WithStatus(http.StatusBadGateway).
			WithProvider(a.Name(), req.Model, 1)
	}
	if len(parsed.Choices) == 0 {
		return nil, domain.NewError(domain.ErrCodeUpstream,
			"provider returned no completion choices").
			WithStatus(http.StatusBadGateway).
			WithProvider(a.Name(), req.Model, 1)
	}

	out := &Response{
		ID:                parsed.ID,
		Model:             firstNonEmpty(parsed.Model, req.Model),
		Created:           parsed.Created,
		SystemFingerprint: parsed.SystemFingerprint,
		Raw:               rawJSON(respBody),
	}
	if out.Created == 0 {
		out.Created = time.Now().Unix()
	}

	out.Choices = make([]domain.Choice, 0, len(parsed.Choices))
	for _, c := range parsed.Choices {
		choice := domain.Choice{Index: c.Index, Logprobs: c.Logprobs}
		if c.Message != nil {
			choice.Message = a.convertMessage(c.Message)
		}
		choice.FinishReason = convertFinishReason(c.FinishReason)
		out.Choices = append(out.Choices, choice)
	}

	if parsed.Usage != nil {
		out.Usage = domain.TokenUsage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
			TotalTokens:      parsed.Usage.TotalTokens,
		}.Normalize()
		if d := parsed.Usage.PromptTokensDetails; d != nil {
			out.Usage.CachedPromptTokens = d.CachedTokens
		}
	} else {
		out.Usage = domain.TokenUsage{Estimated: true}
	}

	// A tool-carrying request answered with nothing fails over to a backend
	// that can serve tools instead of stalling the agent with an empty 200.
	if err := rejectVacuousToolResponse(a.Name(), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ChatCompletionStream performs one streaming attempt.
func (a *openAIAdapter) ChatCompletionStream(ctx context.Context, req *Request, handler StreamHandler) (*Response, error) {
	ctx, cancel := contextWithAttempt(ctx, a.Timeout().PerAttempt)
	defer cancel()

	body := a.buildBody(req, true)
	resp, err := a.postStream(ctx, a.chatPath(), req.Model, 1, body)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)

	acc := newStreamAccumulator()
	reader := newSSEReader(resp.Body)
	defer reader.Close()

	idle := a.Timeout().StreamIdle
	var lastActivity = time.Now()

	for {
		event, ok, err := reader.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil, NormalizeTransportError(a.Name(), req.Model, 1, ctx.Err())
			}
			// A read failure mid-stream is an upstream fault: the response was
			// already committed as 200 to the client, so there is no status to
			// report, only a truncated stream.
			return nil, domain.NewError(domain.ErrCodeUpstream,
				"stream from provider was interrupted").
				WithProvider(a.Name(), req.Model, 1).Wrap(err)
		}
		if !ok {
			break
		}
		if event.empty() {
			// Keepalive comments must not extend the idle window forever.
			if idle > 0 && time.Since(lastActivity) > idle {
				return nil, domain.NewError(domain.ErrCodeTimeout,
					"stream stalled waiting for provider data").
					WithProvider(a.Name(), req.Model, 1)
			}
			continue
		}
		if event.done() {
			break
		}
		lastActivity = time.Now()

		var chunk openAIResponse
		if err := decodeJSON([]byte(event.Data), &chunk); err != nil {
			// A chunk that does not decode is usually a provider-specific event
			// the OpenAI schema does not describe. Skipping is safer than
			// aborting a stream that is otherwise healthy, but it is logged so a
			// systematic mismatch is discoverable.
			a.logger.Debug("skipping undecodable stream chunk",
				"provider", a.Name(), "error", err, "data", truncateMessage(event.Data, 200))
			continue
		}

		if chunk.ID != "" {
			acc.id = chunk.ID
		}
		if chunk.Usage != nil {
			acc.SetUsage(domain.TokenUsage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}.Normalize())
		}

		for _, c := range chunk.Choices {
			normalized := Chunk{
				ID:                chunk.ID,
				Model:             chunk.Model,
				Created:           chunk.Created,
				Index:             c.Index,
				FinishReason:      convertFinishReason(c.FinishReason),
				SystemFingerprint: chunk.SystemFingerprint,
			}
			if c.Delta != nil {
				normalized.Delta = *a.convertMessage(c.Delta)
			} else if c.Message != nil {
				// Some compatible servers send a full message instead of a delta.
				normalized.Delta = *a.convertMessage(c.Message)
			}
			acc.ObserveChunk(normalized)
			if handler != nil {
				if err := handler(normalized); err != nil {
					return nil, newStreamError(err)
				}
			}
		}
	}

	final := acc.Response()
	if !acc.HasUsage() && final.Usage.TotalTokens == 0 {
		// Without provider usage the assembled text is all we have. Marking it
		// estimated keeps it out of exact billing paths.
		final.Usage = domain.TokenUsage{Estimated: true}
	}
	// The stream bytes are already committed, so this cannot fail over; it
	// surfaces as an in-stream error the client displays instead of hanging
	// on an empty stop, and the health tracker still learns.
	if err := rejectVacuousToolResponse(a.Name(), req, final); err != nil {
		return nil, err
	}
	return final, nil
}

// convertMessage translates a wire message into a domain message.
func (a *openAIAdapter) convertMessage(m *openAIMessage) *domain.ChatMessage {
	out := &domain.ChatMessage{
		Role:       domain.MessageRole(m.Role).Normalize(),
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
		Refusal:    m.Refusal,
	}
	if out.Role == "" {
		out.Role = domain.RoleAssistant
	}

	// The content field's shape is preserved verbatim: a string stays a string
	// and an array stays an array. Re-encoding through MessageContent's
	// unmarshaller is what makes that possible.
	if len(m.Content) > 0 {
		_ = out.Content.UnmarshalJSON(m.Content)
	}
	out.Reasoning = decodeReasoning(m.ReasoningContent)

	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, domain.ToolCall{
			Index: tc.Index,
			ID:    tc.ID,
			Type:  tc.Type,
			Function: domain.FunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return out
}

// decodeReasoning extracts thinking text from a raw reasoning_content
// field. Providers send a JSON string or null; anything else (a structured
// variant) is preserved as-is rather than dropped, because a gateway must
// not be the component that loses a valid upstream feature.
func decodeReasoning(raw jsonRaw) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return trimmed
}

// ListModels returns the model identifiers the provider reports.
//
// The /v1/models response carries an id and nothing else, so this cannot say
// what a model can do. See ListModelsWithCapabilities for that.
func (a *openAIAdapter) ListModels(ctx context.Context) ([]string, error) {
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if _, err := a.getJSON(ctx, "/models", &parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// ListModelsWithCapabilities returns the catalogue with whatever capability
// metadata the provider publishes.
//
// Routers built on LiteLM and its derivatives expose a per-model capability view
// at /model/info. When it is there this is free and exact, which is worth a
// great deal more than a probe: no tokens, no per-model requests, and the
// answer comes from the provider rather than from inference.
//
// The endpoint is optional and commonly absent, so a miss is an ordinary result
// rather than an error — callers fall back to ListModels and the kind default.
func (a *openAIAdapter) ListModelsWithCapabilities(ctx context.Context) ([]RemoteModel, error) {
	// Both spellings are tried because the base URL almost always ends in /v1
	// while LiteLM serves this view at the server root, so the naive
	// base-relative path 404s on exactly the platforms most likely to have it.
	for _, path := range modelInfoPaths(a.baseAdapter.baseURL) {
		body, err := a.getRawJSON(ctx, path)
		if err != nil {
			continue
		}
		if models, perr := parseModelInfo(body); perr == nil && len(models) > 0 {
			return models, nil
		}
	}
	return nil, errModelInfoUnsupported
}

// modelInfoPaths returns the locations to try for capability metadata, most
// specific first: relative to the configured base, then at the server root with
// a trailing API version stripped.
func modelInfoPaths(baseURL string) []string {
	paths := []string{"/model/info"}
	trimmed := baseURL
	for _, suffix := range []string{"/v1", "/v1beta", "/openai/v1", "/api/v1"} {
		if strings.HasSuffix(strings.TrimRight(trimmed, "/"), suffix) {
			trimmed = strings.TrimRight(trimmed, "/")
			trimmed = trimmed[:len(trimmed)-len(suffix)]
			break
		}
	}
	if trimmed != "" && trimmed != baseURL {
		paths = append(paths, trimmed+"/model/info")
	}
	return paths
}
// convertFinishReason maps the provider's finish reason onto the domain enum.
func convertFinishReason(raw *string) *domain.FinishReason {
	if raw == nil {
		return nil
	}
	var mapped domain.FinishReason
	switch strings.ToLower(strings.TrimSpace(*raw)) {
	case "stop", "end_turn", "eos":
		mapped = domain.FinishStop
	case "length", "max_tokens":
		mapped = domain.FinishLength
	case "tool_calls", "function_call", "tool_use":
		mapped = domain.FinishToolCalls
	case "content_filter":
		mapped = domain.FinishContentFilter
	default:
		// An unrecognized reason is passed through as-is rather than being
		// invented, so a client sees what the provider actually said.
		mapped = domain.FinishReason(strings.ToLower(*raw))
	}
	return &mapped
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
