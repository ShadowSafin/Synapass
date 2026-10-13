package routing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// These tests pin the two defects that made long answers stop mid-sentence:
//
//  1. the soft latency_target_ms was reused as a hard per-attempt deadline, so
//     the default 10s target cancelled every longer generation mid-answer;
//  2. the shared transport set ResponseHeaderTimeout to the first-token budget,
//     which aborts a buffered request before its headers arrive, because a
//     provider only sends headers once the whole answer exists.

// TestLatencyTargetNeverBecomesADeadline is the regression test for defect 1.
func TestLatencyTargetNeverBecomesADeadline(t *testing.T) {
	policy := &domain.RoutingPolicy{
		Enabled: true,
		Limits: domain.PolicyLimits{
			// A client that cares about latency asks for 2s.
			LatencyTargetMS: 2000,
			MaxLatencyMS:    3000,
		},
		Timeout: domain.TimeoutPolicy{
			Total:      10 * time.Minute,
			PerAttempt: 5 * time.Minute,
		},
	}
	policy.Normalize()

	rc := &domain.RequestContext{LatencyTargetMS: 2000}
	desc, t2 := deriveTimeoutStrategy(policy, rc)

	if !strings.Contains(desc, "ranking_only") {
		t.Errorf("decision should record that the target is ranking-only, got %q", desc)
	}
	if t2.PerAttempt <= 3*time.Second {
		t.Fatalf("a latency target must not become the hard deadline: per_attempt = %s", t2.PerAttempt)
	}
	if t2.PerAttempt != 5*time.Minute {
		t.Errorf("per_attempt = %s, want the operator's 5m", t2.PerAttempt)
	}
}

// TestDefaultLatencyTargetDoesNotTruncate guards the specific default that
// caused the reported bug: with no client preference a request must still get a
// budget that can finish a long answer.
func TestDefaultLatencyTargetDoesNotTruncate(t *testing.T) {
	policy := &domain.RoutingPolicy{Enabled: true}
	policy.Normalize()

	// What the router does today: fall back to the engine default target.
	rc := &domain.RequestContext{LatencyTargetMS: 10000}
	_, t2 := deriveTimeoutStrategy(policy, rc)

	if t2.PerAttempt < time.Minute {
		t.Fatalf("a 10s latency target truncated generation: per_attempt = %s", t2.PerAttempt)
	}
}

// TestUnstatedStreamingBudgetGetsGenerationFloor asserts a stream that inherits
// the defaults has room to finish once it has started.
func TestUnstatedStreamingBudgetGetsGenerationFloor(t *testing.T) {
	policy := &domain.RoutingPolicy{Enabled: true}

	_, streaming := deriveTimeoutStrategy(policy, &domain.RequestContext{Stream: true})
	if streaming.PerAttempt < generationFloorSeconds*time.Second {
		t.Errorf("streaming per_attempt = %s, want at least the generation floor", streaming.PerAttempt)
	}
	if streaming.Total < streaming.PerAttempt {
		t.Errorf("total %s must not be below per_attempt %s", streaming.Total, streaming.PerAttempt)
	}
}

// TestExplicitBudgetIsNeverOverruled is the counterweight to the floor: the
// gateway enforces the operator's budget rather than raising it behind their
// back. A truncation they configured is their decision, and it stays visible
// because the response reports it.
func TestExplicitBudgetIsNeverOverruled(t *testing.T) {
	policy := &domain.RoutingPolicy{
		Enabled: true,
		Timeout: domain.TimeoutPolicy{Total: 20 * time.Second, PerAttempt: 15 * time.Second},
	}

	_, streaming := deriveTimeoutStrategy(policy, &domain.RequestContext{Stream: true})
	if streaming.PerAttempt != 15*time.Second {
		t.Errorf("streaming per_attempt = %s, want the operator's 15s untouched", streaming.PerAttempt)
	}
	if streaming.Total != 20*time.Second {
		t.Errorf("streaming total = %s, want the operator's 20s untouched", streaming.Total)
	}

	// A total below the per-attempt budget is a contradiction, and per_attempt
	// wins so the two cannot expire in the wrong order.
	contradictory := &domain.RoutingPolicy{
		Enabled: true,
		Timeout: domain.TimeoutPolicy{Total: 5 * time.Second, PerAttempt: 30 * time.Second},
	}
	_, resolved := deriveTimeoutStrategy(contradictory, &domain.RequestContext{Stream: true})
	if resolved.Total < resolved.PerAttempt {
		t.Errorf("total %s must not be below per_attempt %s", resolved.Total, resolved.PerAttempt)
	}
}

// TestExecutorLetsALongStreamFinish is the end-to-end regression for defect 1:
// a stream that runs past any chat-shaped budget must still deliver its complete
// answer, rather than being cancelled part way through.
func TestExecutorLetsALongStreamFinish(t *testing.T) {
	adapter := newStubAdapter("openai", domain.ProviderOpenAI)
	// Every chunk is a real call into the client handler, so a cancelled
	// attempt shows up as missing content rather than as a silent success.
	adapter.onStream = func(handler providers.StreamHandler, req *providers.Request) error {
		for i := 0; i < 25; i++ {
			if err := handler(providers.Chunk{
				ID:    "chunk",
				Model: req.Model,
				Index: 0,
				Delta: domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent("ab")},
			}); err != nil {
				return err
			}
		}
		return nil
	}

	clock := newTestClock()
	rc := &domain.RequestContext{
		RequestID:       domain.RequestID("long-stream"),
		ReceivedAt:      clock.now(),
		RequestType:     domain.RequestTypeChatCompletion,
		RequestedModel:  "gpt-4o",
		Stream:          true,
		PromptTokens:    100,
		MaxOutputTokens: 8192,
		// The client allowed an hour; the policy per-attempt budget is the
		// binding constraint and must not be shrunk by a latency target.
		Deadline: clock.now().Add(time.Hour),
	}
	rc.Resolution = &domain.RouteDecision{
		RequestID: rc.RequestID,
		Strategy:  domain.StrategyPriority,
		Chosen:    domain.RouteTarget{ProviderID: openaiID, ProviderName: "openai", Model: "gpt-4o"},
		Chain:     domain.FallbackChain{Targets: []domain.RouteTarget{{ProviderID: openaiID, ProviderName: "openai", Model: "gpt-4o"}}, MaxAttempts: 1},
		Retry:     domain.RetryPolicy{MaxAttempts: 1},
		Timeout:   domain.DefaultTimeoutPolicy(),
		Fallback:  domain.FallbackPolicy{Enabled: false, MaxAttempts: 1},
	}

	registry := newRegistry(registration{provider: testProvider(openaiID, "openai", domain.ProviderOpenAI, 1), adapter: adapter})
	executor := NewExecutor(registry, NewHealthTracker(HealthConfig{}), WithClock(clock.now, clock.sleep))

	var got strings.Builder
	result, err := executor.Execute(context.Background(), rc,
		&providers.Request{Model: "gpt-4o", Stream: true},
		func(chunk providers.Chunk) error {
			got.WriteString(chunk.Delta.Text())
			return nil
		})
	if err != nil {
		t.Fatalf("a long stream must not be cancelled: %v", err)
	}
	if result.Response == nil {
		t.Fatal("expected a response")
	}
	if want := strings.Repeat("ab", 25); got.String() != want {
		t.Errorf("delivered %d chars, want %d: the answer was cut short",
			got.Len(), len(want))
	}
}

// TestSilentClientGetsNoInjectedMaxTokens pins the third defect: the router
// turned its own policy ceiling into the request's max_tokens, so a client that
// asked for no limit was silently given one and the answer stopped early with
// nothing reporting it.
func TestSilentClientGetsNoInjectedMaxTokens(t *testing.T) {
	policy := &domain.RoutingPolicy{
		Enabled: true,
		Limits:  domain.PolicyLimits{MaxOutputTokens: 512},
	}
	engine := NewEngine(nil, nil, nil)

	rc := testRequest("gpt-4o") // no MaxOutputTokens: the client said nothing
	engine.applyLimits(rc, policy)

	if rc.RequestedOutputTokens != 0 {
		t.Errorf("requested_output_tokens = %d, want 0 for a silent client", rc.RequestedOutputTokens)
	}
	if rc.MaxOutputTokens != 512 {
		t.Errorf("max_output_tokens = %d, want the policy ceiling 512 for costing", rc.MaxOutputTokens)
	}
	if got := effectiveMaxTokens(domain.RouteTarget{}, rc); got != 0 {
		t.Errorf("upstream max_tokens = %d, want 0 so the provider applies its own default", got)
	}
}

// TestExplicitMaxTokensSurvivesClamping asserts a real client request is still
// honoured and still clamped by policy.
func TestExplicitMaxTokensSurvivesClamping(t *testing.T) {
	engine := NewEngine(nil, nil, nil)

	within := testRequest("gpt-4o")
	within.MaxOutputTokens = 256
	engine.applyLimits(within, &domain.RoutingPolicy{Enabled: true, Limits: domain.PolicyLimits{MaxOutputTokens: 512}})
	if got := effectiveMaxTokens(domain.RouteTarget{}, within); got != 256 {
		t.Errorf("upstream max_tokens = %d, want the client's 256", got)
	}

	over := testRequest("gpt-4o")
	over.MaxOutputTokens = 100_000
	engine.applyLimits(over, &domain.RoutingPolicy{Enabled: true, Limits: domain.PolicyLimits{MaxOutputTokens: 512}})
	if over.RequestedOutputTokens != 100_000 {
		t.Errorf("requested_output_tokens = %d, want the client's 100000", over.RequestedOutputTokens)
	}
	if got := effectiveMaxTokens(domain.RouteTarget{}, over); got != 512 {
		t.Errorf("upstream max_tokens = %d, want the policy clamp 512", got)
	}
}

// TestTargetOverrideStillBoundsAnExplicitRequest keeps the per-target override
// working for operators who cap one model.
func TestTargetOverrideStillBoundsAnExplicitRequest(t *testing.T) {
	rc := testRequest("gpt-4o")
	rc.MaxOutputTokens = 8000
	rc.RequestedOutputTokens = 8000
	target := domain.RouteTarget{MaxOutputTokens: 1024}
	if got := effectiveMaxTokens(target, rc); got != 1024 {
		t.Errorf("upstream max_tokens = %d, want the target override 1024", got)
	}
}

// TestFallbackDoesNotAppendAfterStreamStarted pins the fallback rule: once
// bytes are on the wire the executor must fail the stream rather than retry, so
// a client can never receive two attempts concatenated into one answer.
func TestFallbackDoesNotAppendAfterStreamStarted(t *testing.T) {
	flaky := newStubAdapter("openai", domain.ProviderOpenAI)
	flaky.onStream = func(handler providers.StreamHandler, req *providers.Request) error {
		if err := handler(providers.Chunk{
			ID:    "chunk-1",
			Model: req.Model,
			Index: 0,
			Delta: domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent("partial")},
		}); err != nil {
			return err
		}
		return domain.NewError(domain.ErrCodeUpstream, "connection reset mid-stream")
	}
	backup := newStubAdapter("anthropic", domain.ProviderAnthropic)

	rc := &domain.RequestContext{
		RequestID:       domain.RequestID("no-append"),
		RequestType:     domain.RequestTypeChatCompletion,
		RequestedModel:  "gpt-4o",
		Stream:          true,
		MaxOutputTokens: 4096,
		Deadline:        time.Now().Add(time.Minute),
	}
	rc.Resolution = &domain.RouteDecision{
		RequestID: rc.RequestID,
		Strategy:  domain.StrategyPriority,
		Chosen:    domain.RouteTarget{ProviderID: openaiID, ProviderName: "openai", Model: "gpt-4o"},
		Chain: domain.FallbackChain{Targets: []domain.RouteTarget{
			{ProviderID: openaiID, ProviderName: "openai", Model: "gpt-4o"},
			{ProviderID: anthropicID, ProviderName: "anthropic", Model: "gpt-4o"},
		}, MaxAttempts: 2},
		Retry:    domain.RetryPolicy{MaxAttempts: 2},
		Timeout:  domain.DefaultTimeoutPolicy(),
		Fallback: domain.FallbackPolicy{Enabled: true, MaxAttempts: 2},
	}

	registry := newRegistry(
		registration{provider: testProvider(openaiID, "openai", domain.ProviderOpenAI, 1), adapter: flaky},
		registration{provider: testProvider(anthropicID, "anthropic", domain.ProviderAnthropic, 2), adapter: backup},
	)
	executor := NewExecutor(registry, NewHealthTracker(HealthConfig{}), WithJitter(false))

	var received []string
	_, err := executor.Execute(context.Background(), rc,
		&providers.Request{Model: "gpt-4o", Stream: true},
		func(chunk providers.Chunk) error {
			received = append(received, chunk.Delta.Text())
			return nil
		})

	if err == nil {
		t.Fatal("a mid-stream failure must be reported, not silently repaired")
	}
	if !IsStreamStarted(err) {
		t.Fatalf("expected a stream-started error, got %T: %v", err, err)
	}
	if backup.callCount() != 0 {
		t.Error("the backup provider was called after bytes reached the client; that would duplicate the answer")
	}
	if len(received) != 1 {
		t.Errorf("client received %d chunks, want only the first attempt's 1", len(received))
	}
}
