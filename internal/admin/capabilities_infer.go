package admin

import (
	"sort"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// CapabilitiesSourceInferred records that a model's capability list was
// guessed from its family name, not read from a catalogue or proven by a
// probe. It sits alongside CapabilitiesSourceProvider, CapabilitiesSourceProbed
// and CapabilitiesSourceDeclared in model metadata. Trust order, lowest to
// highest: inferred < provider catalogue < probed < declared. Anything above
// inferred replaces it: a catalogue backfill on re-sync, a probe is never
// spent on it (a non-empty list is never probed), and an operator edit wins
// over everything.
const CapabilitiesSourceInferred = "inferred"

// inferRule maps a model-name prefix to the capabilities its family is known
// to serve through an OpenAI-compatible API. The table is deliberately
// conservative: a wrongly inferred capability routes traffic the upstream
// rejects, while a missing one merely waits for a probe or an edit. Only
// well-established family traits go here.
type inferRule struct {
	prefix string
	caps   []domain.Capability
}

// nonChatPrefixes names models that are not chat completions at all (image,
// video, audio, embeddings). They are matched first and left unknown: marking
// an image model as chat/tools would route it text traffic it can never
// serve. Leaving the list empty keeps today's behaviour for them.
var nonChatPrefixes = []string{
	"gpt-image", "dall-e", "imagen-", "nano-banana", "flux", "midjourney",
	"stable-diffusion", "kling-", "veo-", "sora", "runway", "pika", "wan-",
	"whisper", "tts-", "embed-",
}

// inferTable is checked in order; the first prefix match wins.
var inferTable = []inferRule{
	{prefix: "claude-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapParallelTool,
		domain.CapVision, domain.CapJSONMode, domain.CapLongContext, domain.CapReasoning,
	}},
	{prefix: "gpt-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapParallelTool,
		domain.CapVision, domain.CapJSONMode, domain.CapJSONSchema,
		domain.CapLongContext, domain.CapReasoning, domain.CapSeed,
	}},
	{prefix: "gemini-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapParallelTool,
		domain.CapVision, domain.CapJSONMode, domain.CapJSONSchema,
		domain.CapLongContext, domain.CapReasoning,
	}},
	{prefix: "deepseek-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapParallelTool,
		domain.CapJSONMode, domain.CapLongContext, domain.CapReasoning,
	}},
	{prefix: "grok-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapParallelTool,
		domain.CapVision, domain.CapJSONMode, domain.CapLongContext, domain.CapReasoning,
	}},
	{prefix: "kimi-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapParallelTool,
		domain.CapVision, domain.CapJSONMode, domain.CapLongContext,
	}},
	{prefix: "minimax-", caps: []domain.Capability{
		domain.CapChat, domain.CapStreaming, domain.CapTools, domain.CapLongContext,
	}},
}

// nonChatModel reports whether a model name belongs to a family that is not
// a chat completion model at all (image, video, audio, embeddings). Callers
// that spend upstream tokens (probing) must skip these: a chat probe against
// an image endpoint is pure waste, and a shape it happens to accept would
// mislabel the row as chat.
func nonChatModel(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if name == "" {
		return false
	}
	for _, prefix := range nonChatPrefixes {
		if strings.HasPrefix(name, prefix) || strings.Contains(name, "-"+prefix) {
			return true
		}
	}
	return false
}

// InferCapabilities guesses a model's capabilities from its family name. It
// costs no upstream tokens: no request is sent. It returns nil when nothing
// is known, in which case the caller leaves the row alone (empty means
// "unknown, use the kind default", which probing or an edit can resolve
// later). The returned list is sorted and safe for the caller to store.
func InferCapabilities(modelName string) []domain.Capability {
	if nonChatModel(modelName) {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(modelName))
	if name == "" {
		return nil
	}
	for _, rule := range inferTable {
		if strings.HasPrefix(name, rule.prefix) {
			out := make([]domain.Capability, len(rule.caps))
			copy(out, rule.caps)
			sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
			return out
		}
	}
	return nil
}

// capabilitiesSource reads where a model's capability list came from. Empty
// means a legacy row that predates provenance tracking; it is treated as a
// declaration (never overwritten by inference, only by an operator or a
// provider catalogue that proves more).
func capabilitiesSource(m domain.Model) string {
	if m.Metadata == nil {
		return ""
	}
	return m.Metadata[CapabilitiesSourceKey]
}
