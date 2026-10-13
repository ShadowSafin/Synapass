package storage

import (
	"context"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Pinned sheet: $2/M input, $6/M output. 1000 prompt + 500 completion tokens
// bills exactly 2.0 + 3.0 = $5.00 after micro-dollar rounding.
func pricedAttempt() domain.TraceAttempt {
	return domain.TraceAttempt{
		Number: 1,
		Target: domain.RouteTarget{
			ProviderName:         "acme",
			Model:                "large",
			InputCostPerMillion:  2000,
			OutputCostPerMillion: 6000,
		},
		Usage: domain.TokenUsage{PromptTokens: 1000, CompletionTokens: 500},
	}
}

func TestAttemptRegistryCostUSD(t *testing.T) {
	if got := attemptRegistryCostUSD(pricedAttempt()); got != 5.0 {
		t.Fatalf("expected $5.00, got %v", got)
	}
}

func TestAttemptRegistryCostUSDUnpriced(t *testing.T) {
	// A target with no registry rates (local runtime, unknown model) costs
	// exactly zero rather than an invented number.
	attempt := domain.TraceAttempt{
		Number: 1,
		Target: domain.RouteTarget{ProviderName: "local", Model: "tiny"},
		Usage:  domain.TokenUsage{PromptTokens: 1000, CompletionTokens: 500},
	}
	if got := attemptRegistryCostUSD(attempt); got != 0 {
		t.Fatalf("expected $0, got %v", got)
	}
}

func TestAttemptRegistryCostUSDMicroRounding(t *testing.T) {
	// 3 prompt tokens at $1/M is $0.000003: the engine's single rounding rule
	// must hold it exactly, not a float dust value.
	attempt := domain.TraceAttempt{
		Number: 1,
		Target: domain.RouteTarget{
			ProviderName:         "acme",
			Model:                "cheap",
			InputCostPerMillion:  1,
			OutputCostPerMillion: 0,
		},
		Usage: domain.TokenUsage{PromptTokens: 3},
	}
	if got := attemptRegistryCostUSD(attempt); got != 0.000003 {
		t.Fatalf("expected $0.000003, got %v", got)
	}
}

func TestAttemptCostFallsBackWithoutPricer(t *testing.T) {
	ch := &ClickHouse{}
	if got := ch.attemptCost(context.Background(), "tenant-1", pricedAttempt()); got != 5.0 {
		t.Fatalf("nil pricer should fall back to registry rates ($5.00), got %v", got)
	}
}

func TestAttemptCostUsesPricerWhenWired(t *testing.T) {
	ch := &ClickHouse{}
	var seenTenant string
	ch.SetPricer(func(_ context.Context, tenantID string, _ domain.RouteTarget, _ domain.TokenUsage) float64 {
		seenTenant = tenantID
		return 42.5
	})
	if got := ch.attemptCost(context.Background(), "tenant-9", pricedAttempt()); got != 42.5 {
		t.Fatalf("wired pricer should win, got %v", got)
	}
	if seenTenant != "tenant-9" {
		t.Fatalf("pricer should see the trace tenant, got %q", seenTenant)
	}
}

func TestSetPricerNilReceiver(t *testing.T) {
	var ch *ClickHouse
	ch.SetPricer(nil) // must not panic; unwired writers record registry cost
}
