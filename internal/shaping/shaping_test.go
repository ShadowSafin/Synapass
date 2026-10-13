package shaping

import (
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/tokens"
)

func TestNormalizeMergesSystem(t *testing.T) {
	p := New(DefaultOptions())
	req := &domain.ChatCompletionRequest{
		Model: "m",
		Messages: []domain.ChatMessage{
			{Role: domain.RoleSystem, Content: domain.NewTextContent("a")},
			{Role: domain.RoleSystem, Content: domain.NewTextContent("b")},
			{Role: domain.RoleUser, Content: domain.NewTextContent("hi")},
		},
	}
	plan := domain.PromptShapePlan{NormalizeSystem: true}
	out, shape := p.Apply(req, plan)
	if len(out.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out.Messages))
	}
	if shape.TokensIn == 0 || shape.TokensOut == 0 {
		t.Fatalf("expected token counts")
	}
}

func TestTrimHistory(t *testing.T) {
	p := New(DefaultOptions())
	msgs := []domain.ChatMessage{{Role: domain.RoleSystem, Content: domain.NewTextContent("sys")}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, domain.ChatMessage{Role: domain.RoleUser, Content: domain.NewTextContent("hello")})
	}
	req := &domain.ChatCompletionRequest{Model: "m", Messages: msgs}
	plan := domain.PromptShapePlan{MaxHistoryMessages: 5}
	out, shape := p.Apply(req, plan)
	if len(out.Messages) != 5 {
		t.Fatalf("expected 5, got %d", len(out.Messages))
	}
	if shape.Truncated == 0 {
		t.Fatalf("expected truncated count")
	}
}

func TestPlanForLongContext(t *testing.T) {
	p := New(DefaultOptions())
	plan := p.PlanFor(domain.TaskClassification{Task: domain.TaskLongContext}, nil, nil)
	if !plan.TrimContext || !plan.SummarizeHistory {
		t.Fatalf("long context should trim+summarize: %+v", plan)
	}
}

func summarizeTestMessages(n int) []domain.ChatMessage {
	msgs := []domain.ChatMessage{{Role: domain.RoleSystem, Content: domain.NewTextContent("you are helpful")}}
	for i := 0; i < n; i++ {
		role := domain.RoleUser
		if i%2 == 1 {
			role = domain.RoleAssistant
		}
		msgs = append(msgs, domain.ChatMessage{
			Role:    role,
			Content: domain.NewTextContent(strings.Repeat("discussion of starship navigation hazards ", 8)),
		})
	}
	return msgs
}

func summarizeStep(t *testing.T, shape domain.PromptShape) domain.PromptShapeStep {
	t.Helper()
	for _, s := range shape.Plan.Steps {
		if s.Name == "summarize_history" {
			return s
		}
	}
	t.Fatal("summarize_history step missing from shape record")
	return domain.PromptShapeStep{}
}

func TestSummarizeHistoryInsertsNote(t *testing.T) {
	p := New(DefaultOptions())
	msgs := summarizeTestMessages(10)
	req := &domain.ChatCompletionRequest{Model: "m", Messages: msgs}
	plan := domain.PromptShapePlan{MaxHistoryMessages: 5, SummarizeHistory: true}
	plan.Steps = describeSteps(plan)

	out, shape := p.Apply(req, plan)

	// 1 system + 4 tail kept, plus exactly one summary note.
	if len(out.Messages) != 6 {
		t.Fatalf("expected 6 messages (5 kept + note), got %d", len(out.Messages))
	}
	if shape.Truncated != 6 {
		t.Fatalf("expected 6 truncated, got %d", shape.Truncated)
	}
	// The input must never be mutated.
	if len(req.Messages) != 11 {
		t.Fatalf("input was mutated: %d messages", len(req.Messages))
	}
	// The note is a system message describing the cut.
	var note *domain.ChatMessage
	for i := range out.Messages {
		if strings.Contains(out.Messages[i].Text(), "history summarized") {
			note = &out.Messages[i]
			break
		}
	}
	if note == nil {
		t.Fatalf("no history summary note found: %+v", out.Messages)
	}
	if note.Role != domain.RoleSystem {
		t.Fatalf("summary note should be a system message, got %q", note.Role)
	}
	body := note.Text()
	if !strings.Contains(body, "6 earlier messages") {
		t.Fatalf("note should carry the dropped count, got %q", body)
	}
	if !strings.Contains(body, "First dropped") || !strings.Contains(body, "Last dropped") {
		t.Fatalf("note should preserve first/last excerpts, got %q", body)
	}
	// Token accounting uses the shared estimator and shows a real saving.
	if shape.TokensIn != tokens.EstimateRequest(req) {
		t.Fatalf("TokensIn %d != estimate %d", shape.TokensIn, tokens.EstimateRequest(req))
	}
	if shape.TokensOut != tokens.EstimateRequest(out) {
		t.Fatalf("TokensOut %d != estimate %d", shape.TokensOut, tokens.EstimateRequest(out))
	}
	if shape.TokensOut >= shape.TokensIn {
		t.Fatalf("expected net token saving, in=%d out=%d", shape.TokensIn, shape.TokensOut)
	}
	// The shape record is honest: applied, no placeholder wording.
	step := summarizeStep(t, shape)
	if !step.Applied {
		t.Fatalf("summarize step should be applied: %+v", step)
	}
	if strings.Contains(step.Detail, "placeholder") {
		t.Fatalf("placeholder wording must be gone, got %q", step.Detail)
	}
	if !strings.Contains(step.Detail, "6 dropped") {
		t.Fatalf("step detail should carry the count, got %q", step.Detail)
	}
}

func TestSummarizeHistoryNothingDropped(t *testing.T) {
	p := New(DefaultOptions())
	req := &domain.ChatCompletionRequest{Model: "m", Messages: summarizeTestMessages(2)}
	plan := domain.PromptShapePlan{SummarizeHistory: true}
	plan.Steps = describeSteps(plan)

	out, shape := p.Apply(req, plan)

	if len(out.Messages) != len(req.Messages) {
		t.Fatalf("no drops expected, got %d messages", len(out.Messages))
	}
	for _, m := range out.Messages {
		if strings.Contains(m.Text(), "history summarized") {
			t.Fatalf("no summary note should be inserted when nothing dropped")
		}
	}
	step := summarizeStep(t, shape)
	if step.Applied {
		t.Fatalf("summarize step must not claim applied work: %+v", step)
	}
	if !strings.Contains(step.Detail, "nothing to summarize") {
		t.Fatalf("step detail should say so honestly, got %q", step.Detail)
	}
}

func TestSummarizeHistoryTokenBudget(t *testing.T) {
	p := New(DefaultOptions())
	req := &domain.ChatCompletionRequest{Model: "m", Messages: summarizeTestMessages(10)}
	budget := tokens.EstimateRequest(req) / 3
	plan := domain.PromptShapePlan{MaxPromptTokens: budget, SummarizeHistory: true}
	plan.Steps = describeSteps(plan)

	out, shape := p.Apply(req, plan)

	if shape.Truncated == 0 {
		t.Fatalf("expected token-budget drops")
	}
	found := false
	for _, m := range out.Messages {
		if strings.Contains(m.Text(), "history summarized") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("token-budget drops should also be summarized")
	}
	if step := summarizeStep(t, shape); !step.Applied {
		t.Fatalf("summarize step should be applied: %+v", step)
	}
}

func TestHistoryTrimWithoutSummarizeStaysSilent(t *testing.T) {
	p := New(DefaultOptions())
	req := &domain.ChatCompletionRequest{Model: "m", Messages: summarizeTestMessages(10)}
	plan := domain.PromptShapePlan{MaxHistoryMessages: 5}
	plan.Steps = describeSteps(plan)

	out, _ := p.Apply(req, plan)

	if len(out.Messages) != 5 {
		t.Fatalf("expected 5, got %d", len(out.Messages))
	}
	for _, m := range out.Messages {
		if strings.Contains(m.Text(), "history summarized") {
			t.Fatalf("no summary note without SummarizeHistory")
		}
	}
}
