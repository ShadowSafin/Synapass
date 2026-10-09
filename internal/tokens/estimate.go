// Package tokens estimates prompt size before a request reaches a provider.
//
// Estimation happens on the hot path and must never be a bottleneck or a source
// of failure, so it uses a heuristic rather than a real tokenizer:
//
//   - Character-based estimation is within roughly 15% for English prose and
//     source code, which is the target workload.
//   - A real BPE tokenizer would require shipping per-model vocabulary files
//     (megabytes per model, redistributed under their own licences) and would
//     add latency to every request to improve an estimate used only for routing
//     and pre-flight rejection.
//
// Every site that cares about exactness (billing, context-limit enforcement) uses
// the provider's reported usage, not this estimate. The estimate is allowed to be
// approximate because being wrong only changes which model is chosen.
package tokens

import (
	"math"
	"strings"
	"unicode/utf8"

	"github.com/shadowsafin/synapass/internal/domain"
)

// charsPerToken is the empirical average for English prose and code. It is
// deliberately on the low side: over-estimating prompt size biases the gateway
// toward larger-context models, whereas under-estimating risks an upstream
// context-length rejection that costs a full round trip.
const charsPerToken = 3.6

// messageOverheadTokens accounts for the per-message role, delimiters and
// control tokens that every chat template adds. OpenAI documents 3 tokens per
// message plus 3 for the reply priming, which matches this closely enough.
const messageOverheadTokens = 4

// replyPrimingTokens is the fixed cost of the assistant turn header.
const replyPrimingTokens = 3

// imageTokensLowDetail is OpenAI's documented cost for a low-detail image. A
// high-detail image costs far more (a function of its resolution), but the
// gateway cannot measure that reliably without fetching the image, so the low
// figure is used and the resulting error is bounded and small relative to a
// request that includes an image at all.
const imageTokensLowDetail = 85

// EstimateText returns the estimated token count for a string.
func EstimateText(s string) int {
	if s == "" {
		return 0
	}
	return EstimateRuneCount(utf8.RuneCountInString(s))
}

// EstimateRuneCount returns the estimated token count for a rune count.
// It shares EstimateText's formula for callers that count runes incrementally
// (for example partial stream delivery) without retaining the text.
func EstimateRuneCount(runes int) int {
	if runes <= 0 {
		return 0
	}
	return int(math.Ceil(float64(runes) / charsPerToken))
}

// EstimateContent estimates a message content value, accounting for images.
func EstimateContent(c domain.MessageContent) int {
	if !c.IsParts {
		return EstimateText(c.Text)
	}
	total := 0
	for _, part := range c.Parts {
		switch part.Type {
		case domain.PartText:
			total += EstimateText(part.Text)
		case domain.PartImageURL:
			total += imageTokensLowDetail
		default:
			// Unknown parts (audio, files) are charged a conservative flat
			// amount so their cost is not silently zero.
			total += imageTokensLowDetail
		}
	}
	return total
}

// EstimateMessages estimates the size of a message list.
func EstimateMessages(msgs []domain.ChatMessage) int {
	total := replyPrimingTokens
	for _, m := range msgs {
		total += messageOverheadTokens
		total += EstimateContent(m.Content)
		if m.Name != "" {
			total += EstimateText(m.Name)
		}
		if m.ToolCallID != "" {
			total += EstimateText(m.ToolCallID)
		}
		for _, tc := range m.ToolCalls {
			total += EstimateText(tc.Function.Name)
			total += EstimateText(tc.Function.Arguments)
		}
	}
	return total
}

// EstimateTools estimates the cost of the tool declarations, which are
// serialized into the prompt and are frequently the largest part of an
// agent-style request.
func EstimateTools(tools []domain.Tool) int {
	total := 0
	for _, t := range tools {
		total += EstimateText(t.Type)
		total += EstimateText(t.Function.Name)
		total += EstimateText(t.Function.Description)
		total += EstimateText(string(t.Function.Parameters))
	}
	return total
}

// EstimateRequest estimates the total prompt size of a chat completion request.
func EstimateRequest(req *domain.ChatCompletionRequest) int {
	if req == nil {
		return 0
	}
	total := EstimateMessages(req.Messages) + EstimateTools(req.Tools)
	// A requested JSON schema is part of the prompt on providers that implement
	// grammar-based decoding via prompt injection.
	if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
		total += EstimateText(string(req.ResponseFormat.JSONSchema))
	}
	return total
}

// EstimateCompletionTokens estimates the completion length for cost projection.
//
// When the client specifies a maximum, that maximum is used because it bounds the
// worst case and cost ceilings must be evaluated against the worst case. When the
// client is silent there is nothing to bound the answer, so a fixed guess is
// used; it only affects the projected cost shown on the decision record.
func EstimateCompletionTokens(requestedMax, policyMax int) int {
	if requestedMax > 0 {
		if policyMax > 0 && requestedMax > policyMax {
			return policyMax
		}
		return requestedMax
	}
	if policyMax > 0 {
		// Clients typically use a fraction of the allowance. Assuming the full
		// ceiling would make every cost estimate alarmingly high, so half is
		// used as a middle ground that still respects the ceiling.
		return policyMax / 2
	}
	return 512
}

// FitsContext reports whether prompt plus completion fits a context window.
func FitsContext(promptTokens, completionTokens, contextWindow int) bool {
	if contextWindow <= 0 {
		return true
	}
	return promptTokens+completionTokens <= contextWindow
}

// TruncateForPrompt returns text truncated to approximately maxTokens tokens.
// It is used only for logging and analysis previews; it never alters the request
// sent to a provider.
func TruncateForPrompt(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	limit := int(float64(maxTokens) * charsPerToken)
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	if limit > len(runes) {
		limit = len(runes)
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
