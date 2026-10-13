// Package shaping implements the prompt transformation pipeline.
//
// Shaping runs after classification and policy evaluation but before the
// provider call. Every step is recorded so traces show exactly what changed.
// Transformations are conservative: they never invent content, only normalize,
// trim, compress whitespace, inject guardrails, or adapt to provider quirks.
package shaping

import (
	"fmt"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/tokens"
)

// Options configures shaping defaults.
type Options struct {
	Enabled            bool
	MaxHistoryMessages int
	MaxPromptTokens    int
	SystemPrefix       string
	SystemSuffix       string
	Guardrails         []string
}

// DefaultOptions returns safe defaults.
func DefaultOptions() Options {
	return Options{Enabled: true, MaxHistoryMessages: 50}
}

// Pipeline applies shaping plans to requests.
type Pipeline struct {
	opts Options
}

// New creates a pipeline.
func New(opts Options) *Pipeline { return &Pipeline{opts: opts} }

// PlanFor builds the shaping plan for a request from policy + task signals.
func (p *Pipeline) PlanFor(task domain.TaskClassification, policy *domain.RoutingPolicy, decision *domain.PolicyDecision) domain.PromptShapePlan {
	plan := domain.PromptShapePlan{NormalizeSystem: true, ProviderAdapt: true}
	if p != nil && p.opts.SystemPrefix != "" {
		plan.SystemPrefix = p.opts.SystemPrefix
	}
	if p != nil && p.opts.SystemSuffix != "" {
		plan.SystemSuffix = p.opts.SystemSuffix
	}
	if p != nil && len(p.opts.Guardrails) > 0 {
		plan.Guardrails = append([]string{}, p.opts.Guardrails...)
	}
	if decision != nil {
		// Policy decision already carries a baseline plan.
		base := decision.Shaping
		mergePlan(&plan, base)
	}
	// Task-specific shaping.
	switch task.Task {
	case domain.TaskLongContext:
		plan.TrimContext = true
		plan.SummarizeHistory = true
		if plan.MaxHistoryMessages == 0 {
			plan.MaxHistoryMessages = 20
		}
	case domain.TaskToolUse:
		plan.ToolPrompting = true
	case domain.TaskStructuredOutput:
		plan.FormatStructured = true
	case domain.TaskCoding, domain.TaskReasoning:
		plan.TrimContext = true
	case domain.TaskSummarization, domain.TaskExtraction:
		plan.Compress = true
	}
	if policy != nil {
		if policy.Limits.MaxHistoryMessages > 0 {
			plan.MaxHistoryMessages = policy.Limits.MaxHistoryMessages
		}
		if policy.Limits.MaxPromptTokens > 0 {
			plan.MaxPromptTokens = policy.Limits.MaxPromptTokens
		}
	}
	if p != nil {
		if p.opts.MaxHistoryMessages > 0 && plan.MaxHistoryMessages == 0 {
			plan.MaxHistoryMessages = p.opts.MaxHistoryMessages
		}
		if p.opts.MaxPromptTokens > 0 && plan.MaxPromptTokens == 0 {
			plan.MaxPromptTokens = p.opts.MaxPromptTokens
		}
	}
	plan.Steps = describeSteps(plan)
	return plan
}

func mergePlan(dst *domain.PromptShapePlan, src domain.PromptShapePlan) {
	if src.NormalizeSystem {
		dst.NormalizeSystem = true
	}
	if src.Compress {
		dst.Compress = true
	}
	if src.TrimContext {
		dst.TrimContext = true
	}
	if src.SummarizeHistory {
		dst.SummarizeHistory = true
	}
	if src.FormatStructured {
		dst.FormatStructured = true
	}
	if src.ToolPrompting {
		dst.ToolPrompting = true
	}
	if src.GuardrailInject {
		dst.GuardrailInject = true
	}
	if src.ProviderAdapt {
		dst.ProviderAdapt = true
	}
	if src.MaxHistoryMessages > 0 {
		dst.MaxHistoryMessages = src.MaxHistoryMessages
	}
	if src.MaxPromptTokens > 0 {
		dst.MaxPromptTokens = src.MaxPromptTokens
	}
	if src.SystemPrefix != "" {
		dst.SystemPrefix = src.SystemPrefix
	}
	if src.SystemSuffix != "" {
		dst.SystemSuffix = src.SystemSuffix
	}
	if len(src.Guardrails) > 0 {
		dst.Guardrails = append([]string{}, src.Guardrails...)
	}
}

func describeSteps(plan domain.PromptShapePlan) []domain.PromptShapeStep {
	steps := []domain.PromptShapeStep{}
	add := func(name, detail string, on bool) {
		steps = append(steps, domain.PromptShapeStep{Name: name, Detail: detail, Applied: on})
	}
	add("normalize_system", "merge duplicate system messages", plan.NormalizeSystem)
	add("compress", "collapse whitespace", plan.Compress)
	add("trim_context", "enforce history/token budgets", plan.TrimContext)
	add("summarize_history", "summarize dropped history into a system note (extractive)", plan.SummarizeHistory)
	add("structured_format", "enforce JSON schema hint", plan.FormatStructured)
	add("tool_prompting", "render tool declarations", plan.ToolPrompting)
	add("guardrails", strings.Join(plan.Guardrails, ";"), plan.GuardrailInject || len(plan.Guardrails) > 0)
	add("provider_adapt", "provider-specific tweaks", plan.ProviderAdapt)
	return steps
}

// Apply executes the plan against a request, returning the shaped copy and a
// record of what changed. The input is never mutated.
func (p *Pipeline) Apply(req *domain.ChatCompletionRequest, plan domain.PromptShapePlan) (*domain.ChatCompletionRequest, domain.PromptShape) {
	if req == nil {
		return req, domain.PromptShape{}
	}
	tokensIn := tokens.EstimateRequest(req)
	out := *req
	out.Messages = append([]domain.ChatMessage{}, req.Messages...)

	shape := domain.PromptShape{TokensIn: tokensIn}
	truncated := 0

	if plan.NormalizeSystem {
		out.Messages = normalizeSystem(out.Messages)
	}
	if plan.Compress {
		out.Messages = compressMessages(out.Messages)
	}
	// History and token budgets drop the middle of the conversation. The
	// dropped messages are collected so that, when summarization is planned,
	// they are compacted into a system note instead of vanishing silently.
	var droppedAll []domain.ChatMessage
	if plan.MaxHistoryMessages > 0 && len(out.Messages) > plan.MaxHistoryMessages {
		// Keep system messages plus the tail.
		dropped, kept := splitTail(out.Messages, plan.MaxHistoryMessages)
		truncated = len(out.Messages) - len(kept)
		out.Messages = kept
		droppedAll = append(droppedAll, dropped...)
	}
	if plan.MaxPromptTokens > 0 {
		var removed []domain.ChatMessage
		out.Messages, removed = trimToTokens(out.Messages, plan.MaxPromptTokens, &truncated)
		droppedAll = append(droppedAll, removed...)
	}
	summarized := false
	summaryDetail := ""
	if plan.SummarizeHistory && len(droppedAll) > 0 {
		note, detail := summarizeDropped(droppedAll)
		out.Messages = insertSummaryNote(out.Messages, note)
		summarized = true
		summaryDetail = detail
	}
	if plan.SystemPrefix != "" || plan.SystemSuffix != "" {
		out.Messages = injectSystem(out.Messages, plan.SystemPrefix, plan.SystemSuffix)
	}
	if len(plan.Guardrails) > 0 || plan.GuardrailInject {
		out.Messages = injectGuardrails(out.Messages, plan.Guardrails)
	}
	if plan.FormatStructured && out.ResponseFormat == nil {
		out.ResponseFormat = &domain.ResponseFormat{Type: "json_object"}
	}

	shape.TokensOut = tokens.EstimateRequest(&out)
	shape.Truncated = truncated
	shape.Plan = plan
	// Mark applied steps with token deltas. The summarization step reports
	// what actually happened: planned-but-empty (nothing was dropped) is
	// recorded as not applied, so a trace never claims a summary that does
	// not exist.
	for i := range plan.Steps {
		plan.Steps[i].TokensBefore = tokensIn
		plan.Steps[i].TokensAfter = shape.TokensOut
		if plan.Steps[i].Name == "summarize_history" && plan.SummarizeHistory {
			if summarized {
				plan.Steps[i].Applied = true
				plan.Steps[i].Detail = summaryDetail
			} else {
				plan.Steps[i].Applied = false
				plan.Steps[i].Detail = "no messages dropped; nothing to summarize"
			}
		}
	}
	shape.Plan.Steps = plan.Steps
	return &out, shape
}

func normalizeSystem(msgs []domain.ChatMessage) []domain.ChatMessage {
	// Merge consecutive system messages into one.
	out := make([]domain.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(out) > 0 && m.Role == domain.RoleSystem && out[len(out)-1].Role == domain.RoleSystem {
			prev := out[len(out)-1]
			merged := prev.Text() + "\n" + m.Text()
			prev.Content = domain.NewTextContent(strings.TrimSpace(merged))
			out[len(out)-1] = prev
			continue
		}
		out = append(out, m)
	}
	return out
}

func compressMessages(msgs []domain.ChatMessage) []domain.ChatMessage {
	out := make([]domain.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Content.IsParts {
			out = append(out, m)
			continue
		}
		c := collapseWhitespace(m.Content.Text)
		m.Content = domain.NewTextContent(c)
		out = append(out, m)
	}
	return out
}

func collapseWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func keepTail(msgs []domain.ChatMessage, max int) []domain.ChatMessage {
	_, kept := splitTail(msgs, max)
	return kept
}

// splitTail divides messages into the dropped middle and the kept head+tail
// under the history budget: leading system messages are preserved plus the
// most recent tail up to max. It is the single rule behind both trimming and
// summarization, so the summary always describes exactly what trimming
// removed.
func splitTail(msgs []domain.ChatMessage, max int) (dropped, kept []domain.ChatMessage) {
	if len(msgs) <= max {
		return nil, msgs
	}
	// Preserve leading system messages.
	var system []domain.ChatMessage
	var rest []domain.ChatMessage
	for _, m := range msgs {
		if m.Role == domain.RoleSystem && len(system) < 2 {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}
	need := max - len(system)
	if need < 0 {
		return append([]domain.ChatMessage{}, msgs[:len(msgs)-max]...), msgs[len(msgs)-max:]
	}
	if len(rest) <= need {
		return nil, append(system, rest...)
	}
	dropped = append([]domain.ChatMessage{}, rest[:len(rest)-need]...)
	return dropped, append(system, rest[len(rest)-need:]...)
}

func trimToTokens(msgs []domain.ChatMessage, maxTokens int, truncated *int) ([]domain.ChatMessage, []domain.ChatMessage) {
	// Drop oldest non-system messages until under budget, collecting the
	// removed ones for the history summary. System messages (including a
	// summary note) are exempt: the note must survive to describe the cut.
	var removed []domain.ChatMessage
	for tokens.EstimateMessages(msgs) > maxTokens {
		idx := -1
		for i, m := range msgs {
			if m.Role != domain.RoleSystem {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		removed = append(removed, msgs[idx])
		msgs = append(msgs[:idx], msgs[idx+1:]...)
		if truncated != nil {
			*truncated++
		}
		if len(msgs) == 0 {
			break
		}
	}
	return msgs, removed
}

// summaryExcerptTokens bounds each verbatim excerpt in the history note. Two
// short excerpts keep the note itself small relative to the history it
// replaces, which is what makes the cut a net saving.
const summaryExcerptTokens = 30

// summarizeDropped compacts dropped middle messages into one system note plus
// the step detail for the shape record. The summary is extractive: it reports
// the dropped count by role and the dropped token volume, and preserves
// verbatim excerpts of the first and last dropped messages. It never invents
// content. All sizes use the shared tokens estimator, so the reported savings
// agree with the shape token deltas.
func summarizeDropped(dropped []domain.ChatMessage) (domain.ChatMessage, string) {
	droppedTokens := tokens.EstimateMessages(dropped)

	var order []domain.MessageRole
	seen := map[domain.MessageRole]bool{}
	for _, m := range dropped {
		r := m.Role.Normalize()
		if !seen[r] {
			seen[r] = true
			order = append(order, r)
		}
	}
	counts := map[domain.MessageRole]int{}
	for _, m := range dropped {
		counts[m.Role.Normalize()]++
	}
	parts := make([]string, 0, len(order))
	for _, r := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[r], r))
	}

	firstMsg := dropped[0]
	lastMsg := dropped[len(dropped)-1]
	text := fmt.Sprintf(
		"[history summarized: %d earlier messages (%s; ~%d tokens) compacted to fit context. "+
			"First dropped [%s]: %q. Last dropped [%s]: %q.]",
		len(dropped), strings.Join(parts, ", "), droppedTokens,
		firstMsg.Role, excerptOf(firstMsg), lastMsg.Role, excerptOf(lastMsg))
	note := domain.ChatMessage{Role: domain.RoleSystem, Content: domain.NewTextContent(text)}

	noteTokens := tokens.EstimateMessages([]domain.ChatMessage{note})
	detail := fmt.Sprintf("extractive summary of %d dropped messages (~%d tokens -> ~%d-token note, net ~%d saved)",
		len(dropped), droppedTokens, noteTokens, droppedTokens-noteTokens)
	return note, detail
}

// excerptOf returns a short verbatim excerpt of a message for the history
// note, bounded through the shared token estimator so excerpt sizes stay in
// the same unit as every other shaping measurement.
func excerptOf(m domain.ChatMessage) string {
	text := strings.TrimSpace(m.Text())
	if text == "" {
		return "(no text content)"
	}
	return tokens.TruncateForPrompt(text, summaryExcerptTokens)
}

// insertSummaryNote places the history note after leading system messages so
// provider system handling is unaffected and the note reads as prior context.
// The note is inserted after all budget cuts, so it always survives them; the
// shaped message count may therefore exceed MaxHistoryMessages by exactly one.
func insertSummaryNote(msgs []domain.ChatMessage, note domain.ChatMessage) []domain.ChatMessage {
	idx := 0
	for idx < len(msgs) && msgs[idx].Role == domain.RoleSystem && idx < 2 {
		idx++
	}
	out := make([]domain.ChatMessage, 0, len(msgs)+1)
	out = append(out, msgs[:idx]...)
	out = append(out, note)
	out = append(out, msgs[idx:]...)
	return out
}

func injectSystem(msgs []domain.ChatMessage, prefix, suffix string) []domain.ChatMessage {
	if prefix == "" && suffix == "" {
		return msgs
	}
	text := strings.TrimSpace(prefix + "\n" + suffix)
	if text == "" {
		return msgs
	}
	sys := domain.ChatMessage{Role: domain.RoleSystem, Content: domain.NewTextContent(text)}
	// Prepend: system prefix must be first for providers to honor it.
	return append([]domain.ChatMessage{sys}, msgs...)
}

func injectGuardrails(msgs []domain.ChatMessage, guardrails []string) []domain.ChatMessage {
	if len(guardrails) == 0 {
		return msgs
	}
	text := "Guardrails: " + strings.Join(guardrails, "; ")
	sys := domain.ChatMessage{Role: domain.RoleSystem, Content: domain.NewTextContent(text)}
	return append([]domain.ChatMessage{sys}, msgs...)
}
