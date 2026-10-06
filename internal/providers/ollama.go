package providers

import (
	"context"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ollamaAdapter speaks Ollama's native API.
//
// Ollama earns a dedicated adapter rather than being treated as an
// OpenAI-compatible server, even though recent versions expose an OpenAI
// compatibility layer, for three reasons:
//
//   - its streaming format is newline-delimited JSON, not SSE, so a shared
//     implementation would need a second parser anyway;
//   - its sampling controls live in an `options` object with different names
//     (num_predict, num_ctx, repeat_penalty), and mapping them is what makes a
//     local runtime behave predictably rather than falling back to defaults;
//   - its usage fields are named prompt_eval_count and eval_count, and its model
//     lifecycle endpoints (/api/tags, /api/version) only exist on the native API.
//
// The native API is therefore the one Synapass targets, and the OpenAI shim is
// reachable through the openai_compatible kind for operators who prefer it.
type ollamaAdapter struct {
	*baseAdapter
}

// newOllamaAdapter constructs an Ollama adapter.
func newOllamaAdapter(p domain.Provider, opts Options) (*ollamaAdapter, error) {
	base, err := newBaseAdapter(p, opts)
	if err != nil {
		return nil, err
	}
	// Ollama has no authentication by default. Sending a bogus bearer token to a
	// local daemon is a common source of confusing 401s when a reverse proxy is
	// present, so the default is "no auth" unless the operator configured one.
	if base.provider.AuthStyle == "" {
		base.provider.AuthStyle = domain.AuthNone
	}
	return &ollamaAdapter{baseAdapter: base}, nil
}

// chatPath is the native chat endpoint.
func (a *ollamaAdapter) chatPath() string { return "/api/chat" }

// healthPath uses the version endpoint, which is cheaper and more stable than
// listing every installed model.
func (a *ollamaAdapter) healthPath() string { return "/api/version" }

// ollamaRequest is the native chat request body.
type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []domain.Tool   `json:"tools,omitempty"`
	Options  *ollamaOptions  `json:"options,omitempty"`
	// KeepAlive controls how long the model stays resident. A short value saves
	// memory; a long one keeps latency predictable. Left unset so the daemon's
	// own configuration governs, which is what a self-hoster expects.
	KeepAlive string `json:"keep_alive,omitempty"`
}

// ollamaMessage is a native chat message. Content is always a string, and images
// travel in a separate base64 array.
type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// ollamaToolCall is a native tool invocation.
type ollamaToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

// ollamaOptions maps the sampling controls onto Ollama's vocabulary.
type ollamaOptions struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	MinP             *float64 `json:"min_p,omitempty"`
	RepeatPenalty    *float64 `json:"repeat_penalty,omitempty"`
	NumPredict       int      `json:"num_predict,omitempty"`
	NumCtx           int      `json:"num_ctx,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
}

// buildBody translates the normalized request into Ollama's native format.
func (a *ollamaAdapter) buildBody(req *Request, stream bool) *ollamaRequest {
	out := &ollamaRequest{
		Model:  req.Model,
		Stream: stream,
	}

	opts := &ollamaOptions{}
	hasOption := false

	if p := req.Params; p != nil {
		out.Messages = make([]ollamaMessage, 0, len(p.Messages))
		for _, m := range p.Messages {
			msg := ollamaMessage{
				Role: string(m.Role.Normalize()),
			}
			// Ollama's assistant role is "assistant"; tool results use "tool".
			if msg.Role == string(domain.RoleDeveloper) {
				msg.Role = string(domain.RoleSystem)
			}
			msg.Content = m.Content.PlainText()

			// Images are passed as bare base64 payloads. A remote URL is skipped
			// because the daemon cannot fetch it, and forwarding the URL as
			// content would be actively misleading.
			if m.Content.IsParts {
				for _, part := range m.Content.Parts {
					if part.Type != domain.PartImageURL || part.ImageURL == nil {
						continue
					}
					if data, ok := extractBase64Image(part.ImageURL.URL); ok {
						msg.Images = append(msg.Images, data)
					}
				}
			}

			for _, tc := range m.ToolCalls {
				var call ollamaToolCall
				call.Function.Name = tc.Function.Name
				call.Function.Arguments = parseArgumentsObject(tc.Function.Arguments)
				msg.ToolCalls = append(msg.ToolCalls, call)
			}

			out.Messages = append(out.Messages, msg)
		}

		out.Tools = p.Tools

		if p.Temperature != nil {
			opts.Temperature = p.Temperature
			hasOption = true
		}
		if p.TopP != nil {
			opts.TopP = p.TopP
			hasOption = true
		}
		if p.TopK != nil {
			opts.TopK = p.TopK
			hasOption = true
		}
		if p.MinP != nil {
			opts.MinP = p.MinP
			hasOption = true
		}
		if p.RepetitionPenalty != nil {
			opts.RepeatPenalty = p.RepetitionPenalty
			hasOption = true
		}
		if p.Seed != nil {
			opts.Seed = p.Seed
			hasOption = true
		}
		if stops := parseStopSequences(p.Stop); len(stops) > 0 {
			opts.Stop = stops
			hasOption = true
		}
		if p.PresencePenalty != nil {
			opts.PresencePenalty = p.PresencePenalty
			hasOption = true
		}
		if p.FrequencyPenalty != nil {
			opts.FrequencyPenalty = p.FrequencyPenalty
			hasOption = true
		}
	}

	// num_predict is Ollama's max_tokens equivalent. Passing it is important:
	// without it the daemon generates until the context fills, which makes cost
	// and latency unbounded.
	if req.MaxTokens > 0 {
		opts.NumPredict = req.MaxTokens
		hasOption = true
	}

	if hasOption {
		out.Options = opts
	}
	return out
}

// extractBase64Image pulls the base64 payload out of a data URL, or returns the
// raw string when it already looks like base64.
func extractBase64Image(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	if strings.HasPrefix(raw, "data:") {
		_, data, ok := strings.Cut(strings.TrimPrefix(raw, "data:"), ";base64,")
		if !ok {
			return "", false
		}
		return data, true
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return "", false
	}
	return raw, true
}

// parseArgumentsObject converts a JSON argument string into a map, which is the
// shape Ollama expects.
func parseArgumentsObject(raw string) map[string]any {
	out := map[string]any{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}
	if err := decodeJSON([]byte(trimmed), &out); err != nil {
		// Argument fragments are sometimes incomplete; preserving the raw text
		// keeps the information available to the model rather than dropping it.
		out["_raw"] = trimmed
	}
	return out
}

// ollamaResponse is the native chat response, used for both streaming lines and
// the single non-streaming object.
type ollamaResponse struct {
	Model      string         `json:"model"`
	CreatedAt  time.Time      `json:"created_at"`
	Message    *ollamaMessage `json:"message"`
	Done       bool           `json:"done"`
	DoneReason string         `json:"done_reason"`
	// PromptEvalCount and EvalCount are Ollama's token counters.
	PromptEvalCount int   `json:"prompt_eval_count"`
	EvalCount       int   `json:"eval_count"`
	TotalDuration   int64 `json:"total_duration"`
	// Error is returned with a 200 status for some failure modes.
	Error string `json:"error"`
}

// ChatCompletion performs one non-streaming attempt.
func (a *ollamaAdapter) ChatCompletion(ctx context.Context, req *Request) (*Response, error) {
	ctx, cancel := contextWithAttempt(ctx, a.Timeout().PerAttempt)
	defer cancel()

	body := a.buildBody(req, false)
	_, respBody, err := a.postJSON(ctx, a.chatPath(), req.Model, 1, body)
	if err != nil {
		return nil, err
	}

	var parsed ollamaResponse
	if err := decodeJSON(respBody, &parsed); err != nil {
		return nil, NormalizeDecodeError(a.Name(), req.Model, 1, err, respBody)
	}
	if parsed.Error != "" {
		return nil, domain.NewError(domain.ErrCodeUpstream, parsed.Error).
			WithProvider(a.Name(), req.Model, 1)
	}

	msg := &domain.ChatMessage{Role: domain.RoleAssistant}
	if parsed.Message != nil {
		msg.Content = domain.NewTextContent(parsed.Message.Content)
		for i, tc := range parsed.Message.ToolCalls {
			args, _ := encodeJSON(tc.Function.Arguments)
			msg.ToolCalls = append(msg.ToolCalls, domain.ToolCall{
				Index: intPtr(i),
				Type:  "function",
				Function: domain.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: string(args),
				},
			})
		}
	}

	finish := convertOllamaDoneReason(parsed.DoneReason, len(msg.ToolCalls) > 0)

	out := &Response{
		ID:      "",
		Model:   firstNonEmpty(parsed.Model, req.Model),
		Created: parsed.CreatedAt.Unix(),
		Choices: []domain.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finish,
		}},
		Usage: domain.TokenUsage{
			PromptTokens:     parsed.PromptEvalCount,
			CompletionTokens: parsed.EvalCount,
			TotalTokens:      parsed.PromptEvalCount + parsed.EvalCount,
		},
		Raw: rawJSON(respBody),
	}
	if err := rejectVacuousToolResponse(a.Name(), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ChatCompletionStream performs one streaming attempt.
func (a *ollamaAdapter) ChatCompletionStream(ctx context.Context, req *Request, handler StreamHandler) (*Response, error) {
	ctx, cancel := contextWithAttempt(ctx, a.Timeout().PerAttempt)
	defer cancel()

	body := a.buildBody(req, true)
	resp, err := a.postStream(ctx, a.chatPath(), req.Model, 1, body)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)

	acc := newStreamAccumulator()
	reader := newNDJSONReader(resp.Body)

	toolIndex := 0
	var finalUsage domain.TokenUsage
	for {
		line, ok, err := reader.Next()
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

		var parsed ollamaResponse
		if err := decodeJSON(line, &parsed); err != nil {
			a.logger.Debug("skipping undecodable ollama line",
				"provider", a.Name(), "error", err)
			continue
		}
		if parsed.Error != "" {
			return nil, domain.NewError(domain.ErrCodeUpstream, parsed.Error).
				WithProvider(a.Name(), req.Model, 1)
		}

		if parsed.Model != "" {
			acc.model = parsed.Model
		}
		if parsed.CreatedAt.Unix() > 0 {
			acc.created = parsed.CreatedAt.Unix()
		}

		if parsed.Message != nil {
			chunk := Chunk{
				Model: parsed.Model,
				Delta: domain.ChatMessage{
					Role:    domain.RoleAssistant,
					Content: domain.NewTextContent(parsed.Message.Content),
				},
			}
			for _, tc := range parsed.Message.ToolCalls {
				args, _ := encodeJSON(tc.Function.Arguments)
				chunk.Delta.ToolCalls = append(chunk.Delta.ToolCalls, domain.ToolCall{
					Index: intPtr(toolIndex),
					Type:  "function",
					Function: domain.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: string(args),
					},
				})
				toolIndex++
			}
			if !chunk.Delta.Content.IsEmpty() || len(chunk.Delta.ToolCalls) > 0 {
				acc.ObserveChunk(chunk)
				if handler != nil {
					if err := handler(chunk); err != nil {
						return nil, newStreamError(err)
					}
				}
			}
		}

		if parsed.Done {
			finalUsage = domain.TokenUsage{
				PromptTokens:     parsed.PromptEvalCount,
				CompletionTokens: parsed.EvalCount,
				TotalTokens:      parsed.PromptEvalCount + parsed.EvalCount,
			}
			finish := convertOllamaDoneReason(parsed.DoneReason, toolIndex > 0)
			chunk := Chunk{
				Model:        parsed.Model,
				FinishReason: &finish,
				Usage:        &finalUsage,
			}
			acc.ObserveChunk(chunk)
			if handler != nil {
				if err := handler(chunk); err != nil {
					return nil, newStreamError(err)
				}
			}
		}
	}

	if finalUsage.TotalTokens > 0 {
		acc.SetUsage(finalUsage)
	} else {
		// Ollama always reports counts, so reaching here means the stream ended
		// before the done line — treat the totals as estimated rather than zero.
		acc.SetUsage(domain.TokenUsage{
			PromptTokens: req.PromptTokens,
			Estimated:    true,
		})
	}
	final := acc.Response()
	if err := rejectVacuousToolResponse(a.Name(), req, final); err != nil {
		return nil, err
	}
	return final, nil
}

// convertOllamaDoneReason maps Ollama's done_reason onto the domain enum.
func convertOllamaDoneReason(reason string, hasToolCalls bool) domain.FinishReason {
	if hasToolCalls {
		return domain.FinishToolCalls
	}
	switch reason {
	case "stop", "":
		return domain.FinishStop
	case "length":
		return domain.FinishLength
	case "load":
		// "load" means the model had to be paged in and generation stopped
		// short. Reporting it as a length stop is the closest honest mapping.
		return domain.FinishLength
	default:
		return domain.FinishReason(reason)
	}
}

// HealthCheck probes the daemon version endpoint.
//
// Ollama reports the version rather than a model list, which makes the probe
// cheap even on a host with hundreds of models pulled. The version is included in
// the health message so the dashboard can show drift across a fleet.
func (a *ollamaAdapter) HealthCheck(ctx context.Context) domain.ProviderHealth {
	start := time.Now()

	ctx, cancel := contextWithAttempt(ctx, a.opts.Timeouts.Connect)
	defer cancel()

	var parsed struct {
		Version string `json:"version"`
	}
	status, err := a.getJSON(ctx, a.healthPath(), &parsed)
	latency := time.Since(start)

	health := domain.ProviderHealth{
		ProviderID:     a.provider.ID,
		ProviderName:   a.provider.Name,
		CheckedAt:      domain.Now(),
		LatencyMS:      latency.Milliseconds(),
		Source:         "active_probe",
		ProbeLatencyMS: latency.Milliseconds(),
	}
	if err != nil {
		health.State = domain.HealthUnhealthy
		health.ErrorRate = 1
		health.ConsecutiveFailures = 1
		health.Message = err.Error()
		return health
	}
	health.State = domain.HealthHealthy
	health.SuccessRate = 1
	if parsed.Version != "" {
		health.Message = "ollama " + parsed.Version
	}
	_ = status
	return health
}

// ListModels returns the models the daemon has pulled.
func (a *ollamaAdapter) ListModels(ctx context.Context) ([]string, error) {
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if _, err := a.getJSON(ctx, "/api/tags", &parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out, nil
}
