package domain

import (
	"encoding/json"
	"strings"
)

// MessageRole is the speaker of a chat message.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleDeveloper MessageRole = "developer"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
	RoleFunction  MessageRole = "function"
)

// Normalize maps legacy role spellings onto current ones so downstream code
// compares against a single canonical value.
func (r MessageRole) Normalize() MessageRole {
	switch r {
	case RoleFunction:
		return RoleTool
	default:
		return r
	}
}

// ContentPartType discriminates multimodal content parts.
type ContentPartType string

const (
	PartText     ContentPartType = "text"
	PartImageURL ContentPartType = "image_url"
	PartAudio    ContentPartType = "input_audio"
	PartFile     ContentPartType = "file"
)

// ContentPart is one element of a multimodal message.
type ContentPart struct {
	Type ContentPartType `json:"type"`
	// Text is set for "text" parts.
	Text string `json:"text,omitempty"`
	// ImageURL is set for "image_url" parts.
	ImageURL *ImageURL `json:"image_url,omitempty"`
	// CacheControl carries an Anthropic-style prompt-caching hint
	// (e.g. {"type":"ephemeral"}) when a client attaches one to a part.
	// Synapass accepts and ignores it: it is an upstream optimization
	// hint, not a routing input.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Raw preserves parts Synapass does not model, so unknown multimodal
	// payloads pass through to the provider untouched.
	Raw json.RawMessage `json:"-"`
}

// ImageURL carries an image reference for vision models.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// MessageContent models the OpenAI content field, which is either a plain
// string or an array of typed parts depending on the model. The custom
// (un)marshalling preserves the client's original representation so a
// round-tripped request is byte-comparable in the common case.
type MessageContent struct {
	// Text is populated when the content was a JSON string.
	Text string
	// Parts is populated when the content was a JSON array.
	Parts []ContentPart
	// IsParts records which of the two representations was used. A request with
	// content: [] must not be rewritten to content: "" or vice versa.
	IsParts bool
}

// NewTextContent builds string content.
func NewTextContent(s string) MessageContent { return MessageContent{Text: s} }

// NewPartsContent builds array content.
func NewPartsContent(parts ...ContentPart) MessageContent {
	return MessageContent{Parts: parts, IsParts: true}
}

// MarshalJSON emits the original representation.
func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.IsParts {
		if c.Parts == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

// UnmarshalJSON accepts either representation and never fails on an unexpected
// shape: a part Synapass does not understand is retained as Raw rather than
// rejected, because a gateway must not be the component that rejects a valid
// upstream feature it has not learned about yet.
func (c *MessageContent) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		c.Text, c.Parts, c.IsParts = "", nil, false
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		c.Text, c.Parts, c.IsParts = s, nil, false
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var rawParts []json.RawMessage
		if err := json.Unmarshal(data, &rawParts); err != nil {
			return err
		}
		parts := make([]ContentPart, 0, len(rawParts))
		for _, raw := range rawParts {
			var part ContentPart
			if err := json.Unmarshal(raw, &part); err != nil {
				// Unknown shape: keep it verbatim.
				parts = append(parts, ContentPart{Raw: raw})
				continue
			}
			if part.Text == "" && part.ImageURL == nil {
				part.Raw = raw
			}
			parts = append(parts, part)
		}
		c.Parts, c.Text, c.IsParts = parts, "", true
		return nil
	}
	// Any other scalar (number, bool) is preserved as raw text content.
	return json.Unmarshal(data, &c.Text)
}

// PlainText returns the concatenated text of the content, ignoring non-text
// parts. It is used for prompt-size estimation and content policy matching.
func (c MessageContent) PlainText() string {
	if !c.IsParts {
		return c.Text
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Type == PartText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// HasImages reports whether the content includes an image part, which is how
// the vision capability requirement is detected.
func (c MessageContent) HasImages() bool {
	for _, p := range c.Parts {
		if p.Type == PartImageURL {
			return true
		}
	}
	return false
}

// IsEmpty reports whether the content carries nothing routable.
func (c MessageContent) IsEmpty() bool {
	if c.IsParts {
		return len(c.Parts) == 0
	}
	return c.Text == ""
}

// FunctionCall is an OpenAI-style function invocation.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	// Index is present only in streaming deltas, where tool call fragments are
	// accumulated by index.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role       MessageRole    `json:"role"`
	Content    MessageContent `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	// Refusal is returned by providers that expose safety refusals.
	Refusal string `json:"refusal,omitempty"`
	// CacheControl carries a message-level prompt-caching hint sent by
	// clients such as VS Code Copilot Chat. Accepted and ignored.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Reasoning carries the model's thinking trace (OpenAI-compatible
	// `reasoning_content`, Anthropic thinking blocks). It is populated on
	// responses the gateway serves, never required on requests: a client
	// that echoes it back gets continuity on providers that honour it,
	// and providers that do not honour it ignore it. Omitted when empty
	// so plain responses carry no new keys.
	Reasoning string `json:"reasoning_content,omitempty"`
}

// Text is the message's textual content.
func (m ChatMessage) Text() string { return m.Content.PlainText() }

// FunctionDefinition declares a callable tool.
type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Tool declares a tool the model may call.
type Tool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// ResponseFormat requests structured output.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// StreamOptions tunes streaming behaviour.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatCompletionRequest is the normalized body of POST /v1/chat/completions.
//
// Pointer fields distinguish "absent" from "explicitly zero", which matters
// because Synapass forwards only what the client sent rather than inventing
// values the provider might treat differently.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	// Prompt is a shorthand for a single user message, for clients that do
	// not speak the chat format (CLIs, webhooks, simple agents). It is
	// expanded into Messages by Normalize and rejected when both are set.
	Prompt string `json:"prompt,omitempty"`
	// System is a shorthand for a leading system message. When Messages are
	// also supplied it is prepended, so one field covers both styles.
	System      string   `json:"system,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	// TopK, MinP and RepetitionPenalty are accepted because Anthropic, Ollama,
	// vLLM and most OpenAI-compatible servers honour them; each adapter
	// forwards only the subset its upstream understands.
	TopK              *int            `json:"top_k,omitempty"`
	MinP              *float64        `json:"min_p,omitempty"`
	RepetitionPenalty *float64        `json:"repetition_penalty,omitempty"`
	N                 *int            `json:"n,omitempty"`
	Stream            *bool           `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Stop              json.RawMessage `json:"stop,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	MaxCompletion     *int            `json:"max_completion_tokens,omitempty"`
	PresencePenalty   *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty  *float64        `json:"frequency_penalty,omitempty"`
	LogitBias         map[string]int  `json:"logit_bias,omitempty"`
	Logprobs          *bool           `json:"logprobs,omitempty"`
	TopLogprobs       *int            `json:"top_logprobs,omitempty"`
	User              string          `json:"user,omitempty"`
	Seed              *int            `json:"seed,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCall  *bool           `json:"parallel_tool_calls,omitempty"`
	// ToolExecution is Synapass's namespaced tool control (Phase 4). It is
	// separate from tool_choice, which is OpenAI's: tool_choice says whether
	// the model must call a tool, this says who runs it.
	ToolExecution   *ToolRunConfig    `json:"tool_execution,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	// CacheControl carries a top-level prompt-caching hint. Accepted and
	// ignored, like the message/part-level variants.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Store, ServiceTier, SafetyIdentifier and PromptCacheKey are
	// OpenAI-compatible passthrough fields that common clients send.
	// Accepted and ignored: they do not change routing.
	Store            *bool  `json:"store,omitempty"`
	ServiceTier      string `json:"service_tier,omitempty"`
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	PromptCacheKey   string `json:"prompt_cache_key,omitempty"`
}

// Streaming reports whether the client asked for a stream.
func (r *ChatCompletionRequest) Streaming() bool {
	return r.Stream != nil && *r.Stream
}

// RequestedMaxTokens returns the completion allowance the client asked for,
// preferring the modern max_completion_tokens field.
func (r *ChatCompletionRequest) RequestedMaxTokens() int {
	if r.MaxCompletion != nil {
		return *r.MaxCompletion
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 0
}

// PromptText concatenates all message text for estimation and inspection.
func (r *ChatCompletionRequest) PromptText() string {
	var b strings.Builder
	for _, m := range r.Messages {
		b.WriteString(m.Text())
		b.WriteByte('\n')
	}
	for _, t := range r.Tools {
		b.WriteString(t.Function.Name)
		b.WriteByte('\n')
		b.WriteString(t.Function.Description)
		b.WriteByte('\n')
		b.Write(t.Function.Parameters)
		b.WriteByte('\n')
	}
	return b.String()
}

// RequiredCapabilities derives the capability set a request demands. Deriving
// rather than accepting this from the client keeps capability matching honest:
// a client cannot claim to need vision while sending an image the selected
// model cannot read.
func (r *ChatCompletionRequest) RequiredCapabilities() []Capability {
	caps := NewCapabilitySet(CapChat)
	if r.Streaming() {
		caps[CapStreaming] = struct{}{}
	}
	if len(r.Tools) > 0 {
		caps[CapTools] = struct{}{}
		if r.ParallelToolCall != nil && *r.ParallelToolCall {
			caps[CapParallelTool] = struct{}{}
		}
	}
	for _, m := range r.Messages {
		if m.Content.HasImages() {
			caps[CapVision] = struct{}{}
		}
	}
	if r.ResponseFormat != nil {
		switch r.ResponseFormat.Type {
		case "json_object":
			caps[CapJSONMode] = struct{}{}
		case "json_schema":
			caps[CapJSONSchema] = struct{}{}
		}
	}
	if r.Seed != nil {
		caps[CapSeed] = struct{}{}
	}
	return caps.Slice()
}

// Normalize expands the prompt/system shorthands into Messages so the rest of
// the pipeline only ever sees the chat format.
//
// The expansion runs before Validate, which means a prompt-only request is
// subject to exactly the same structural checks as a chat request rather than
// to a parallel set of rules that could drift.
func (r *ChatCompletionRequest) Normalize() error {
	if strings.TrimSpace(r.Prompt) != "" {
		if len(r.Messages) > 0 {
			return NewError(ErrCodeInvalidRequest,
				"'prompt' and 'messages' are mutually exclusive; send one or the other").withParam("prompt")
		}
		r.Messages = []ChatMessage{{
			Role:    RoleUser,
			Content: NewTextContent(r.Prompt),
		}}
		r.Prompt = ""
	}
	if strings.TrimSpace(r.System) != "" {
		system := ChatMessage{Role: RoleSystem, Content: NewTextContent(r.System)}
		if len(r.Messages) > 0 && r.Messages[0].Role == RoleSystem {
			r.Messages[0] = system
		} else {
			r.Messages = append([]ChatMessage{system}, r.Messages...)
		}
		r.System = ""
	}
	return nil
}

// Validate performs the structural checks the gateway can make without a
// provider. Returning a normalized error here keeps invalid traffic off the
// upstream network entirely.
func (r *ChatCompletionRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return NewError(ErrCodeInvalidRequest, "you must provide a model parameter").withParam("model")
	}
	if len(r.Messages) == 0 {
		return NewError(ErrCodeInvalidRequest, "'messages' must contain at least one message").withParam("messages")
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem, RoleDeveloper, RoleUser, RoleAssistant, RoleTool, RoleFunction:
		default:
			return Errorf(ErrCodeInvalidRequest,
				"'messages[%d].role' must be one of system, developer, user, assistant or tool, got %q", i, m.Role).
				withParam("messages")
		}
		if m.Content.IsEmpty() && len(m.ToolCalls) == 0 {
			return Errorf(ErrCodeInvalidRequest, "'messages[%d].content' must not be empty", i).withParam("messages")
		}
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return NewError(ErrCodeInvalidRequest, "'temperature' must be between 0 and 2").withParam("temperature")
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		return NewError(ErrCodeInvalidRequest, "'top_p' must be between 0 and 1").withParam("top_p")
	}
	if r.TopK != nil && *r.TopK < 1 {
		return NewError(ErrCodeInvalidRequest, "'top_k' must be greater than or equal to 1").withParam("top_k")
	}
	if r.MinP != nil && (*r.MinP < 0 || *r.MinP > 1) {
		return NewError(ErrCodeInvalidRequest, "'min_p' must be between 0 and 1").withParam("min_p")
	}
	if r.RepetitionPenalty != nil && *r.RepetitionPenalty < 0 {
		return NewError(ErrCodeInvalidRequest, "'repetition_penalty' must not be negative").withParam("repetition_penalty")
	}
	if r.N != nil && *r.N < 1 {
		return NewError(ErrCodeInvalidRequest, "'n' must be greater than or equal to 1").withParam("n")
	}
	if r.RequestedMaxTokens() < 0 {
		return NewError(ErrCodeInvalidRequest, "'max_tokens' must be greater than or equal to 0").withParam("max_tokens")
	}
	return nil
}

// withParam annotates a normalized error with the offending field.
func (e *Error) withParam(param string) *Error {
	e.Param = param
	return e
}

// FinishReason describes why a completion stopped.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
)

// Choice is one completion candidate.
type Choice struct {
	Index        int             `json:"index"`
	Message      *ChatMessage    `json:"message,omitempty"`
	Delta        *ChatMessage    `json:"delta,omitempty"`
	FinishReason *FinishReason   `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// ChatCompletionResponse is the non-streaming response body.
type ChatCompletionResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []Choice    `json:"choices"`
	Usage   *TokenUsage `json:"usage,omitempty"`
	// SystemFingerprint is passed through from providers that emit it.
	SystemFingerprint string `json:"system_fingerprint,omitempty"`
	// Synapass is a namespaced extension block. Extra top-level keys are
	// tolerated by OpenAI clients, but a namespaced object is unambiguous and
	// cannot collide with a future upstream field.
	Synapass *ResponseMetadata `json:"synapass,omitempty"`
}

// ObjectChatCompletion is the response object discriminator.
const ObjectChatCompletion = "chat.completion"

// ChatCompletionChunk is one SSE event of a streaming response.
type ChatCompletionChunk struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	Choices           []Choice          `json:"choices"`
	Usage             *TokenUsage       `json:"usage,omitempty"`
	SystemFingerprint string            `json:"system_fingerprint,omitempty"`
	Synapass        *ResponseMetadata `json:"synapass,omitempty"`
}

// ObjectChatCompletionChunk is the chunk object discriminator.
const ObjectChatCompletionChunk = "chat.completion.chunk"

// ResponseMetadata explains how a response was produced. It is the client-facing
// view of the routing decision and is present on every response Synapass
// generates, including errors.
type ResponseMetadata struct {
	RequestID         string  `json:"request_id"`
	TraceID           string  `json:"trace_id,omitempty"`
	PolicyID          string  `json:"policy_id,omitempty"`
	PolicyName        string  `json:"policy_name,omitempty"`
	Provider          string  `json:"provider,omitempty"`
	RequestedModel    string  `json:"requested_model,omitempty"`
	RoutedModel       string  `json:"routed_model,omitempty"`
	Strategy          string  `json:"strategy,omitempty"`
	Attempts          int     `json:"attempts,omitempty"`
	FallbackUsed      bool    `json:"fallback_used"`
	CacheHit          bool    `json:"cache_hit"`
	CacheKind         string  `json:"cache_kind,omitempty"`
	CacheSimilarity   float64 `json:"cache_similarity,omitempty"`
	CacheReuseCount   int64   `json:"cache_reuse_count,omitempty"`
	Task              string  `json:"task,omitempty"`
	Shaping           string  `json:"shaping,omitempty"`
	LatencyMS         int64   `json:"latency_ms"`
	ProviderLatencyMS int64   `json:"provider_latency_ms,omitempty"`
	EstimatedCostUSD  float64 `json:"estimated_cost_usd,omitempty"`
	RouteReason       string  `json:"route_reason,omitempty"`
	Degraded          bool    `json:"degraded,omitempty"`
	// ToolRun describes tool use for this request. It is omitted entirely when
	// no tool was involved, so a plain chat response carries no new keys.
	ToolRun *ToolRunMetadata `json:"tool_run,omitempty"`
	// Structured reports how the answer was checked against a requested
	// response_format. Like ToolRun it is omitted when nothing was requested,
	// so a request that did not ask for JSON carries no new keys.
	Structured *StructuredOutputMeta `json:"structured,omitempty"`
	// Completion describes how the answer was bounded and whether it finished
	// on its own terms. It is omitted when nothing was capped, so an ordinary
	// short response carries no new keys.
	Completion *CompletionMeta `json:"completion,omitempty"`
}

// CompletionMeta explains output bounding and completeness.
//
// A response that stops mid-sentence is a failure mode, so the reason travels
// with the response: `truncated` says whether the answer ended because a limit
// was reached, and `reason` says which one. Without it a caller can only
// infer truncation from the absence of a sentence ending.
type CompletionMeta struct {
	// FinishReason is the provider's own reason: stop, length, tool_calls, ...
	FinishReason string `json:"finish_reason,omitempty"`
	// Truncated is true when the answer was cut short by a limit rather than
	// the model finishing: max tokens reached, or a timeout mid-answer.
	Truncated bool `json:"truncated,omitempty"`
	// Reason names the limit: "max_tokens" or "timeout".
	Reason string `json:"reason,omitempty"`
	// RequestedTokens is what the client asked for (0 when it said nothing).
	RequestedTokens int `json:"requested_tokens,omitempty"`
	// AppliedTokens is the ceiling actually sent upstream, after the policy and
	// model caps were applied. It is what the provider stopped at.
	AppliedTokens int `json:"applied_tokens,omitempty"`
	// PromptTokens and CompletionTokens are the observed sizes.
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	// TimedOut is true when a deadline cancelled generation mid-answer.
	TimedOut bool `json:"timed_out,omitempty"`
	// BudgetMS is the attempt budget that applied, so a shortened answer can be
	// compared against the ceiling it had.
	BudgetMS int64 `json:"budget_ms,omitempty"`
}

// ErrorResponse is the OpenAI-compatible error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// NewErrorResponse renders a normalized error as the wire envelope.
func NewErrorResponse(err error) ErrorResponse {
	e := AsError(err)
	if e == nil {
		e = NewError(ErrCodeInternal, "unknown error")
	}
	body := ErrorBody{
		Message: e.Message,
		Type:    e.ErrorType(),
		Param:   e.ErrorParam(),
		Code:    string(e.Code),
	}
	return ErrorResponse{Error: body}
}

// ModelObject is one entry in the GET /v1/models response.
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Synapass extensions describing upstream placement and pricing. They are
	// additive fields, which OpenAI-compatible clients ignore.
	Provider        string   `json:"synapass_provider,omitempty"`
	UpstreamModel   string   `json:"synapass_upstream_model,omitempty"`
	ContextWindow   int      `json:"synapass_context_window,omitempty"`
	MaxOutputTokens int      `json:"synapass_max_output_tokens,omitempty"`
	InputCost       float64  `json:"synapass_input_cost_per_million,omitempty"`
	OutputCost      float64  `json:"synapass_output_cost_per_million,omitempty"`
	Capabilities    []string `json:"synapass_capabilities,omitempty"`
	Status          string   `json:"synapass_status,omitempty"`
}

// ModelListResponse is the GET /v1/models response body.
type ModelListResponse struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}
