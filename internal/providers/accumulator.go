package providers

import (
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// streamAccumulator rebuilds a complete Response from incremental chunks.
//
// Streaming responses must still produce a full record: usage accounting, trace
// attempts and (when the client asked for it) a final non-streamed payload all
// need the assembled content. Rebuilding it here means each adapter's streaming
// loop stays a shallow translation layer rather than embedding accumulation
// logic of its own.
//
// Content is accumulated with a strings.Builder per choice. Tool-call arguments
// arrive as fragments that must be concatenated, and they are keyed by index
// because the model may interleave calls.
type streamAccumulator struct {
	id          string
	model       string
	created     int64
	fingerprint string

	// order preserves choice indices in first-seen order so the final payload is
	// stable regardless of arrival order.
	order   []int
	choices map[int]*accumulatedChoice

	usage    domain.TokenUsage
	hasUsage bool

	// upstreamModel is the model the provider reported, which may differ from
	// the one requested when the provider routes to a dated snapshot.
	upstreamModel string

	// chunkCount is used to decide whether the stream produced anything at all,
	// which distinguishes an empty successful stream from a truncated one.
	chunkCount int
}

// accumulatedChoice is the per-index state of a streaming choice.
type accumulatedChoice struct {
	index        int
	role         domain.MessageRole
	content      strings.Builder
	reasoning    strings.Builder
	refusal      string
	toolCalls    []*accumulatedToolCall
	toolCallIdx  map[int]int // upstream index -> position in toolCalls
	finishReason *domain.FinishReason
	logprobs     []byte
}

// accumulatedToolCall is a partially received tool call.
type accumulatedToolCall struct {
	index  int
	id     string
	typ    string
	name   string
	args   strings.Builder
	hasID  bool
	stream bool
}

// newStreamAccumulator creates an accumulator.
func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{choices: map[int]*accumulatedChoice{}}
}

// choice returns (creating if needed) the state for a choice index.
func (a *streamAccumulator) choice(index int) *accumulatedChoice {
	if existing, ok := a.choices[index]; ok {
		return existing
	}
	c := &accumulatedChoice{index: index, toolCallIdx: map[int]int{}}
	a.choices[index] = c
	a.order = append(a.order, index)
	return c
}

// ObserveChunk folds one normalized chunk into the accumulated state.
func (a *streamAccumulator) ObserveChunk(chunk Chunk) {
	a.chunkCount++

	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Model != "" {
		a.upstreamModel = chunk.Model
		a.model = chunk.Model
	}
	if chunk.Created != 0 {
		a.created = chunk.Created
	}
	if chunk.SystemFingerprint != "" {
		a.fingerprint = chunk.SystemFingerprint
	}
	if chunk.Usage != nil {
		u := chunk.Usage.Normalize()
		a.usage = u
		a.hasUsage = true
	}

	c := a.choice(chunk.Index)
	if chunk.Delta.Role != "" {
		c.role = chunk.Delta.Role
	}
	if chunk.Delta.Refusal != "" {
		c.refusal = chunk.Delta.Refusal
	}
	if text := chunk.Delta.Content.PlainText(); text != "" {
		c.content.WriteString(text)
	}
	// Thinking fragments accumulate exactly like content fragments, in
	// arrival order. Providers emit thinking before the answer, so the
	// assembled trace reads in the order the model thought.
	if chunk.Delta.Reasoning != "" {
		c.reasoning.WriteString(chunk.Delta.Reasoning)
	}
	if chunk.Delta.Content.IsParts && len(chunk.Delta.Content.Parts) > 0 {
		// Structured deltas are uncommon but supported by providers that stream
		// multimodal output; their serialized form is appended so nothing is lost.
		if text := chunk.Delta.Content.PlainText(); text == "" {
			c.content.WriteString(serializeParts(chunk.Delta.Content.Parts))
		}
	}
	for _, tc := range chunk.Delta.ToolCalls {
		a.observeToolCall(c, tc)
	}
	if chunk.FinishReason != nil {
		fr := *chunk.FinishReason
		c.finishReason = &fr
	}
}

// observeToolCall merges a tool-call fragment into the choice's list.
func (a *streamAccumulator) observeToolCall(c *accumulatedChoice, tc domain.ToolCall) {
	idx := -1
	if tc.Index != nil {
		idx = *tc.Index
	}

	pos, ok := c.toolCallIdx[idx]
	if !ok {
		// A tool call with no index and no id is a fragment of the most recent
		// call (some OpenAI-compatible servers omit the index).
		if idx < 0 && len(c.toolCalls) > 0 {
			pos = len(c.toolCalls) - 1
		} else {
			c.toolCalls = append(c.toolCalls, &accumulatedToolCall{index: idx, stream: true})
			pos = len(c.toolCalls) - 1
			c.toolCallIdx[idx] = pos
		}
	}

	acc := c.toolCalls[pos]
	if tc.ID != "" {
		acc.id = tc.ID
		acc.hasID = true
	}
	if tc.Type != "" {
		acc.typ = tc.Type
	}
	if tc.Function.Name != "" {
		// The name arrives whole on the first fragment; a provider that streams
		// it in pieces would already be violating the schema.
		acc.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		acc.args.WriteString(tc.Function.Arguments)
	}
}

// Response renders the accumulated state as a Response.
func (a *streamAccumulator) Response() *Response {
	resp := &Response{
		ID:                a.id,
		Model:             a.model,
		Created:           a.created,
		SystemFingerprint: a.fingerprint,
	}
	if a.upstreamModel != "" {
		resp.Model = a.upstreamModel
	}

	resp.Choices = make([]domain.Choice, 0, len(a.order))
	for _, idx := range a.order {
		c := a.choices[idx]

		role := c.role
		if role == "" {
			role = domain.RoleAssistant
		}

		msg := &domain.ChatMessage{
			Role:      role,
			Content:   domain.NewTextContent(c.content.String()),
			Reasoning: c.reasoning.String(),
			Refusal:   c.refusal,
		}
		if len(c.toolCalls) > 0 {
			msg.ToolCalls = make([]domain.ToolCall, 0, len(c.toolCalls))
			for _, tc := range c.toolCalls {
				index := tc.index
				var indexPtr *int
				if index >= 0 {
					indexPtr = &index
				}
				typ := tc.typ
				if typ == "" {
					typ = "function"
				}
				msg.ToolCalls = append(msg.ToolCalls, domain.ToolCall{
					Index: indexPtr,
					ID:    tc.id,
					Type:  typ,
					Function: domain.FunctionCall{
						Name:      tc.name,
						Arguments: tc.args.String(),
					},
				})
			}
			// A model that emitted tool calls but no text should report
			// tool_calls as the finish reason even if the provider omitted it.
			if c.finishReason == nil {
				fr := domain.FinishToolCalls
				c.finishReason = &fr
			}
		}

		resp.Choices = append(resp.Choices, domain.Choice{
			Index:        c.index,
			Message:      msg,
			FinishReason: c.finishReason,
		})
	}

	if a.hasUsage {
		resp.Usage = a.usage
	} else {
		// Providers that do not report streaming usage still need to be billed
		// and charted, so the assembled text is measured as a fallback. The
		// Estimated flag keeps estimated numbers out of exact-cost reporting.
		resp.Usage = domain.TokenUsage{Estimated: true}
	}
	return resp
}

// ChunkCount returns how many chunks were observed.
func (a *streamAccumulator) ChunkCount() int { return a.chunkCount }

// HasUsage reports whether the provider reported usage.
func (a *streamAccumulator) HasUsage() bool { return a.hasUsage }

// SetUsage records usage discovered outside the chunk stream, which is how
// providers that only report usage on a final event are handled.
func (a *streamAccumulator) SetUsage(u domain.TokenUsage) {
	a.usage = u
	a.hasUsage = true
}

// serializeParts renders non-text content parts for accumulation. It is a
// last-resort representation used only for logging and trace fidelity; the
// client always receives the provider's own chunks verbatim.
func serializeParts(parts []domain.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if len(p.Raw) > 0 {
			b.Write(p.Raw)
			continue
		}
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
