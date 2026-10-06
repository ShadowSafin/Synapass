package providers

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// anthropicVersion is the API version header Anthropic requires. It is pinned
// rather than configurable: a silently changing version header would make
// behaviour depend on deployment timing, and an operator who genuinely needs a
// different version can set it through the provider's custom headers, which are
// applied before credentials and therefore win.
const anthropicVersion = "2023-06-01"

// defaultAnthropicMaxTokens is the allowance used when neither the request nor
// the catalogue states a ceiling. It is generous on purpose: Anthropic's field
// is mandatory, so this value is what a client that said nothing is actually
// limited to, and a small value would silently truncate long answers. Operators
// who want a tighter bound set it as a routing-policy limit.
const defaultAnthropicMaxTokens = 32768

// anthropicAdapter translates Synapass's normalized chat request into
// Anthropic's Messages API and back.
//
// The translation is real work, not a payload rename: Anthropic takes the system
// prompt as a top-level field rather than a message, requires an explicit
// max_tokens, expresses tool calls as content blocks, and reports usage with a
// different vocabulary. Keeping that mapping in one adapter is the point of the
// adapter interface — nothing above this file knows Anthropic exists.
type anthropicAdapter struct {
	*baseAdapter
}

// newAnthropicAdapter constructs an Anthropic adapter.
func newAnthropicAdapter(p domain.Provider, opts Options) (*anthropicAdapter, error) {
	base, err := newBaseAdapter(p, opts)
	if err != nil {
		return nil, err
	}
	// Anthropic authenticates with x-api-key rather than a bearer token.
	if base.provider.AuthStyle == "" {
		base.provider.AuthStyle = domain.AuthHeader
		if base.provider.HeaderName == "" {
			base.provider.HeaderName = "x-api-key"
		}
	}
	if base.provider.Headers == nil {
		base.provider.Headers = map[string]string{}
	}
	if _, set := base.provider.Headers["anthropic-version"]; !set {
		base.provider.Headers["anthropic-version"] = anthropicVersion
	}
	return &anthropicAdapter{baseAdapter: base}, nil
}

// chatPath is the Messages endpoint.
func (a *anthropicAdapter) chatPath() string { return "/messages" }

// anthropicRequest is the Messages API request body.
type anthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	System        string               `json:"system,omitempty"`
	Messages      []anthropicMessage   `json:"messages"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	// Metadata attributes the request for Anthropic's abuse monitoring.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// anthropicMessage is one conversational turn.
type anthropicMessage struct {
	Role string `json:"role"`
	// Content is always a block array. Anthropic accepts a bare string too, but
	// blocks are used uniformly so tool results and images need no special case.
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one content block. A single struct covers every block type
// because the API's blocks are a tagged union and only one variant's fields are
// ever populated at a time.
type anthropicBlock struct {
	Type string `json:"type"`
	// Text is set for text blocks.
	Text string `json:"text,omitempty"`
	// Source is set for image blocks.
	Source *anthropicImageSource `json:"source,omitempty"`
	// ID, Name and Input are set for tool_use blocks.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// Thinking is set for thinking blocks: the model's visible trace.
	Thinking string `json:"thinking,omitempty"`
	// ToolUseID and Content are set for tool_result blocks.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// anthropicImageSource references an image.
type anthropicImageSource struct {
	Type      string `json:"type"`
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

// anthropicTool declares a callable tool.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// anthropicToolChoice mirrors the API's tool_choice object.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// buildBody translates the normalized request.
func (a *anthropicAdapter) buildBody(req *Request, stream bool) *anthropicRequest {
	p := req.Params

	// Anthropic requires max_tokens: omitting the field is a 400, so there is no
	// way to express "no ceiling". An unset allowance therefore falls back to the
	// model ceiling recorded in the catalogue, and only then to a conservative
	// floor — never to a small constant that would cut a long answer short.
	//
	// A 4096 default here was the second half of the truncation bug: the router
	// stopped sending a limit of its own, and the adapter reinstated one silently.
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}

	out := &anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		Stream:    stream,
	}
	if p != nil {
		out.Temperature = p.Temperature
		out.TopP = p.TopP
		out.TopK = p.TopK
		if len(p.Stop) > 0 {
			out.StopSequences = parseStopSequences(p.Stop)
		}
		if p.User != "" {
			// Anthropic's metadata takes a user_id, which maps naturally onto
			// the OpenAI user field.
			out.Metadata = map[string]string{"user_id": p.User}
		}
		out.Tools = convertTools(p.Tools)
		out.ToolChoice = convertToolChoice(p.ToolChoice)
	}

	// System prompts become a top-level string and are excluded from the message
	// list, which Anthropic requires.
	var systemParts []string
	messages := make([]anthropicMessage, 0, len(p.Messages))

	for _, m := range p.Messages {
		role := m.Role.Normalize()
		switch role {
		case domain.RoleSystem, domain.RoleDeveloper:
			if text := m.Content.PlainText(); text != "" {
				systemParts = append(systemParts, text)
			}
		case domain.RoleTool:
			// A tool result is a user-role message containing a tool_result block.
			messages = append(messages, anthropicMessage{
				Role: "user",
				Content: []anthropicBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   jsonString(m.Content.PlainText()),
				}},
			})
		case domain.RoleAssistant:
			messages = append(messages, anthropicMessage{
				Role:    "assistant",
				Content: a.convertAssistantContent(m),
			})
		default:
			messages = append(messages, anthropicMessage{
				Role:    "user",
				Content: convertUserContent(m.Content),
			})
		}
	}

	out.System = strings.Join(systemParts, "\n\n")
	out.Messages = mergeAdjacentMessages(messages)
	return out
}

// mergeAdjacentMessages collapses consecutive same-role messages.
//
// Anthropic requires strictly alternating roles and returns a 400 otherwise.
// Clients that use OpenAI's convention of several system or tool turns, or that
// append tool results back to back, would otherwise fail for a reason that has
// nothing to do with their intent. Merging preserves ordering and content.
func mergeAdjacentMessages(msgs []anthropicMessage) []anthropicMessage {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(out[n-1].Content, m.Content...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// convertUserContent maps a user message's content onto Anthropic blocks.
func convertUserContent(content domain.MessageContent) []anthropicBlock {
	if !content.IsParts {
		if content.Text == "" {
			return []anthropicBlock{{Type: "text", Text: ""}}
		}
		return []anthropicBlock{{Type: "text", Text: content.Text}}
	}
	blocks := make([]anthropicBlock, 0, len(content.Parts))
	for _, part := range content.Parts {
		switch part.Type {
		case domain.PartText:
			blocks = append(blocks, anthropicBlock{Type: "text", Text: part.Text})
		case domain.PartImageURL:
			if part.ImageURL == nil {
				continue
			}
			if source, ok := convertImageURL(part.ImageURL.URL); ok {
				blocks = append(blocks, anthropicBlock{Type: "image", Source: source})
			} else {
				// An image reference Anthropic cannot consume would otherwise be
				// dropped silently, so it is surfaced to the model as text.
				blocks = append(blocks, anthropicBlock{
					Type: "text",
					Text: "[image omitted: " + part.ImageURL.URL + "]",
				})
			}
		default:
			if len(part.Raw) > 0 {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: string(part.Raw)})
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: ""})
	}
	return blocks
}

// convertImageURL turns an OpenAI-style image reference into an Anthropic source.
//
// Anthropic accepts either a remote URL or inline base64. A data URL already
// carries base64 and a media type, so it is split apart; a plain HTTPS URL is
// passed through as a URL source.
func convertImageURL(raw string) (*anthropicImageSource, bool) {
	if raw == "" {
		return nil, false
	}
	if strings.HasPrefix(raw, "data:") {
		rest := strings.TrimPrefix(raw, "data:")
		mediaType, data, ok := strings.Cut(rest, ";base64,")
		if !ok {
			return nil, false
		}
		return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}, true
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return &anthropicImageSource{Type: "url", URL: raw}, true
	}
	return nil, false
}

// convertAssistantContent maps an assistant turn onto blocks, including any tool
// calls it made.
func (a *anthropicAdapter) convertAssistantContent(m domain.ChatMessage) []anthropicBlock {
	blocks := make([]anthropicBlock, 0, 1+len(m.ToolCalls))
	if text := m.Content.PlainText(); text != "" {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: text})
	}
	for _, tc := range m.ToolCalls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		// Anthropic takes tool input as a JSON object, not a string, so an
		// unparsable argument string is wrapped as a single field rather than
		// rejecting the whole request.
		if !json.Valid([]byte(args)) {
			encoded, _ := json.Marshal(map[string]string{"_raw": args})
			args = string(encoded)
		}
		blocks = append(blocks, anthropicBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: ""})
	}
	return blocks
}

// convertTools maps OpenAI tool declarations onto Anthropic's input_schema form.
func convertTools(tools []domain.Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out
}

// convertToolChoice maps the OpenAI tool_choice value onto Anthropic's object.
func convertToolChoice(raw json.RawMessage) *anthropicToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}
		case "none":
			// Anthropic has no "none"; omitting the field is equivalent unless
			// tools were supplied, in which case auto is the closest honest
			// approximation and is documented as such.
			return &anthropicToolChoice{Type: "auto"}
		case "required", "any":
			return &anthropicToolChoice{Type: "any"}
		}
		return nil
	}
	var asObject struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil && asObject.Function.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: asObject.Function.Name}
	}
	return nil
}

// parseStopSequences accepts either a single string or an array, mirroring the
// OpenAI schema.
func parseStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		out := make([]string, 0, len(many))
		for _, s := range many {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// jsonString marshals a string to JSON, falling back to an empty object.
func jsonString(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

// anthropicUsage is the API's usage object.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// toDomain converts usage to the normalized form. Cached prompt tokens are
// reported separately by Anthropic and are mapped onto the cached field so cost
// accounting applies the discounted rate.
func (u anthropicUsage) toDomain() domain.TokenUsage {
	return domain.TokenUsage{
		PromptTokens:       u.InputTokens,
		CompletionTokens:   u.OutputTokens,
		TotalTokens:        u.InputTokens + u.OutputTokens,
		CachedPromptTokens: u.CacheReadInputTokens,
	}
}

// anthropicResponse is the non-streaming Messages response.
type anthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []anthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

// ChatCompletion performs one non-streaming attempt.
func (a *anthropicAdapter) ChatCompletion(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := contextWithAttempt(ctx, a.Timeout().PerAttempt)
	defer cancel()

	body := a.buildBody(req, false)
	_, respBody, err := a.postJSON(ctx, a.chatPath(), req.Model, 1, body)
	if err != nil {
		return nil, err
	}

	var parsed anthropicResponse
	if err := decodeJSON(respBody, &parsed); err != nil {
		return nil, NormalizeDecodeError(a.Name(), req.Model, 1, err, respBody)
	}

	msg := &domain.ChatMessage{Role: domain.RoleAssistant}
	var text, thinking strings.Builder
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, domain.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: domain.FunctionCall{
					Name:      block.Name,
					Arguments: string(block.Input),
				},
			})
		case "thinking":
			// Extended thinking is exposed as reasoning, never as answer
			// text: clients render reasoning_content separately so the
			// trace does not read as part of the answer.
			thinking.WriteString(block.Thinking)
		case "redacted_thinking":
			// Encrypted and undisplayable by design. Dropped rather than
			// leaked as opaque bytes a client would surface as text.
		}
	}
	msg.Content = domain.NewTextContent(text.String())
	msg.Reasoning = thinking.String()

	finish := convertAnthropicStopReason(parsed.StopReason)

	out := &Response{
		ID:      parsed.ID,
		Model:   firstNonEmpty(parsed.Model, req.Model),
		Created: time.Now().Unix(),
		Choices: []domain.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finish,
		}},
		Usage: parsed.Usage.toDomain(),
		Raw:   rawJSON(respBody),
	}
	if err := rejectVacuousToolResponse(a.Name(), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

// anthropicStreamEvent is the union of every streaming event the API emits.
type anthropicStreamEvent struct {
	Type string `json:"type"`
	// Message is set on message_start.
	Message *anthropicResponse `json:"message"`
	// Index is the content block index for block events.
	Index int `json:"index"`
	// ContentBlock is set on content_block_start.
	ContentBlock *anthropicBlock `json:"content_block"`
	// Delta carries either a text delta, a partial JSON delta or a stop reason.
	Delta *anthropicDelta `json:"delta"`
	// Usage is set on message_delta, carrying the final output token count.
	Usage *anthropicUsage `json:"usage"`
	// Error is set on an error event.
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicDelta is the union of delta payloads.
type anthropicDelta struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	// PartialJSON is set on input_json_delta events.
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// ChatCompletionStream performs one streaming attempt.
func (a *anthropicAdapter) ChatCompletionStream(ctx context.Context, req *Request, handler StreamHandler) (*Response, error) {
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

	// Anthropic reports the prompt token count once, at message_start, and the
	// output count at message_delta, so both are collected and combined.
	usage := domain.TokenUsage{}
	var stopReason string
	// toolIndexes maps an Anthropic content block index onto the tool call index
	// in the emitted chunk stream, which is dense rather than sparse.
	toolIndexes := map[int]int{}
	nextToolIndex := 0

	for {
		event, ok, err := reader.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil, NormalizeTransportError(a.Name(), req.Model, 1, ctx.Err())
			}
			return nil, domain.NewError(domain.ErrCodeUpstream,
				"stream from provider was interrupted").
				WithProvider(a.Name(), req.Model, 1).Wrap(err)
		}
		if !ok {
			break
		}
		if event.empty() || event.done() {
			continue
		}

		var ev anthropicStreamEvent
		if err := decodeJSON([]byte(event.Data), &ev); err != nil {
			a.logger.Debug("skipping undecodable anthropic event",
				"provider", a.Name(), "error", err)
			continue
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				acc.id = ev.Message.ID
				acc.model = ev.Message.Model
				u := ev.Message.Usage.toDomain()
				usage.PromptTokens = u.PromptTokens
				usage.CachedPromptTokens = u.CachedPromptTokens
				acc.SetUsage(usage.Normalize())
			}

		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			switch ev.ContentBlock.Type {
			case "text":
				if ev.ContentBlock.Text != "" {
					chunk := Chunk{
						ID:    acc.id,
						Model: acc.model,
						Index: 0,
						Delta: domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(ev.ContentBlock.Text)},
					}
					acc.ObserveChunk(chunk)
					if handler != nil {
						if err := handler(chunk); err != nil {
							return nil, newStreamError(err)
						}
					}
				}
			case "tool_use":
				idx := nextToolIndex
				toolIndexes[ev.Index] = idx
				nextToolIndex++
				chunk := Chunk{
					ID:    acc.id,
					Model: acc.model,
					Delta: domain.ChatMessage{
						Role: domain.RoleAssistant,
						ToolCalls: []domain.ToolCall{{
							Index: intPtr(idx),
							ID:    ev.ContentBlock.ID,
							Type:  "function",
							Function: domain.FunctionCall{
								Name: ev.ContentBlock.Name,
							},
						}},
					},
				}
				acc.ObserveChunk(chunk)
				if handler != nil {
					if err := handler(chunk); err != nil {
						return nil, newStreamError(err)
					}
				}
			}

		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text == "" {
					continue
				}
				chunk := Chunk{
					ID:    acc.id,
					Model: acc.model,
					Delta: domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(ev.Delta.Text)},
				}
				acc.ObserveChunk(chunk)
				if handler != nil {
					if err := handler(chunk); err != nil {
						return nil, newStreamError(err)
					}
				}
			case "input_json_delta":
				idx, known := toolIndexes[ev.Index]
				if !known {
					// A delta for an unknown block means the start event was
					// missed; allocating a fresh index keeps the payload
					// consistent rather than dropping the arguments.
					idx = nextToolIndex
					toolIndexes[ev.Index] = idx
					nextToolIndex++
				}
				chunk := Chunk{
					ID:    acc.id,
					Model: acc.model,
					Delta: domain.ChatMessage{
						Role: domain.RoleAssistant,
						ToolCalls: []domain.ToolCall{{
							Index:    intPtr(idx),
							Type:     "function",
							Function: domain.FunctionCall{Arguments: ev.Delta.PartialJSON},
						}},
					},
				}
				acc.ObserveChunk(chunk)
				if handler != nil {
					if err := handler(chunk); err != nil {
						return nil, newStreamError(err)
					}
				}
			case "thinking_delta":
				// Thinking streams as reasoning fragments, in its own
				// channel from the answer text. Clients that understand
				// reasoning_content render it as a collapsible trace;
				// clients that do not ignore the unknown field.
				if ev.Delta.Thinking == "" {
					continue
				}
				chunk := Chunk{
					ID:    acc.id,
					Model: acc.model,
					Delta: domain.ChatMessage{
						Role:      domain.RoleAssistant,
						Reasoning: ev.Delta.Thinking,
					},
				}
				acc.ObserveChunk(chunk)
				if handler != nil {
					if err := handler(chunk); err != nil {
						return nil, newStreamError(err)
					}
				}
			}

		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.CompletionTokens = ev.Usage.OutputTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				acc.SetUsage(usage.Normalize())
			}

		case "message_stop":
			// Terminal event; the loop ends naturally on EOF.

		case "error":
			if ev.Error != nil {
				code := domain.ErrCodeUpstream
				switch ev.Error.Type {
				case "overloaded_error":
					code = domain.ErrCodeRateLimited
				case "invalid_request_error":
					code = domain.ErrCodeInvalidRequest
				case "authentication_error":
					code = domain.ErrCodeAuthentication
				}
				return nil, domain.NewError(code, ev.Error.Message).
					WithProvider(a.Name(), req.Model, 1)
			}

		case "ping":
			// Keepalive.
		}
	}

	// Emit the terminal finish reason so the client's stream assembler sees it.
	if stopReason != "" {
		finish := convertAnthropicStopReason(stopReason)
		chunk := Chunk{
			ID:           acc.id,
			Model:        acc.model,
			FinishReason: &finish,
		}
		acc.ObserveChunk(chunk)
		if handler != nil {
			if err := handler(chunk); err != nil {
				return nil, newStreamError(err)
			}
		}
	}

	final := acc.Response()
	if err := rejectVacuousToolResponse(a.Name(), req, final); err != nil {
		return nil, err
	}
	return final, nil
}

// convertAnthropicStopReason maps the API's stop reasons onto the domain enum.
func convertAnthropicStopReason(reason string) domain.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence":
		return domain.FinishStop
	case "max_tokens":
		return domain.FinishLength
	case "tool_use":
		return domain.FinishToolCalls
	case "refusal":
		return domain.FinishContentFilter
	case "":
		return domain.FinishStop
	default:
		return domain.FinishReason(reason)
	}
}

// intPtr returns a pointer to v, for populating optional index fields.
func intPtr(v int) *int { return &v }

// HealthCheck probes the Messages API's model list.
//
// Anthropic's /models endpoint is used rather than a synthetic completion because
// a probe that costs money on every interval is not acceptable, and /models still
// exercises DNS, TLS and the credential.
func (a *anthropicAdapter) HealthCheck(ctx context.Context) domain.ProviderHealth {
	start := time.Now()
	res := a.probe(ctx, "/models")

	health := domain.ProviderHealth{
		ProviderID:     a.provider.ID,
		ProviderName:   a.provider.Name,
		State:          res.State,
		CheckedAt:      domain.Now(),
		LatencyMS:      time.Since(start).Milliseconds(),
		ProbeLatencyMS: res.Latency.Milliseconds(),
		Message:        res.Message,
		Source:         "active_probe",
	}
	if res.State == domain.HealthHealthy {
		health.SuccessRate = 1
	} else {
		health.ErrorRate = 1
		health.ConsecutiveFailures = 1
	}
	if res.AuthFailed {
		health.Message = "credential rejected: " + res.Message
	}
	return health
}

// ListModels returns the model identifiers Anthropic reports.
func (a *anthropicAdapter) ListModels(ctx context.Context) ([]string, error) {
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
