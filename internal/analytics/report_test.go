package analytics

import (
	"math"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEnrichRowDerivesRatesFromItsOwnCounts(t *testing.T) {
	row := EnrichRow(domain.DimensionRow{
		Key:         "openai",
		Requests:    100,
		Successes:   80,
		Errors:      15,
		Rejections:  5,
		Fallbacks:   20,
		CacheHits:   25,
		CostUSD:     10,
		TotalTokens: 1000,
	})

	if !almostEqual(row.SuccessRate, 0.8) {
		t.Fatalf("SuccessRate = %v, want 0.8", row.SuccessRate)
	}
	// Non-success outcomes are errors plus rejections; cancelations belong to
	// neither and must not be invented here.
	if !almostEqual(row.ErrorRate, 0.2) {
		t.Fatalf("ErrorRate = %v, want 0.2", row.ErrorRate)
	}
	if !almostEqual(row.FallbackRate, 0.2) {
		t.Fatalf("FallbackRate = %v, want 0.2", row.FallbackRate)
	}
	if !almostEqual(row.CacheHitRate, 0.25) {
		t.Fatalf("CacheHitRate = %v, want 0.25", row.CacheHitRate)
	}
	// Cost per success divides by successes, not requests: failures are not what
	// a caller paid for.
	if !almostEqual(row.CostPerSuccessUSD, 10.0/80.0) {
		t.Fatalf("CostPerSuccessUSD = %v, want %v", row.CostPerSuccessUSD, 10.0/80.0)
	}
	if !almostEqual(row.TokensPerRequest, 10) {
		t.Fatalf("TokensPerRequest = %v, want 10", row.TokensPerRequest)
	}
}

func TestEnrichRowOnEmptyRowDoesNotDivideByZero(t *testing.T) {
	// A dimension value with no requests (for example an error code that only
	// appears in a previous window) must leave every derived metric at zero
	// rather than produce NaN, which JSON cannot even encode.
	row := EnrichRow(domain.DimensionRow{Key: "unattributed"})
	for name, value := range map[string]float64{
		"SuccessRate":       row.SuccessRate,
		"ErrorRate":         row.ErrorRate,
		"FallbackRate":      row.FallbackRate,
		"CacheHitRate":      row.CacheHitRate,
		"CostPerSuccessUSD": row.CostPerSuccessUSD,
		"TokensPerRequest":  row.TokensPerRequest,
	} {
		if value != 0 {
			t.Fatalf("%s = %v, want 0", name, value)
		}
	}
}

func TestEnrichReturnsOneRowPerInputAndKeepsOrder(t *testing.T) {
	rows := []domain.DimensionRow{
		{Key: "a", Requests: 2, Successes: 2},
		{Key: "b", Requests: 4, Successes: 1},
	}
	out := Enrich(rows)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Key != "a" || out[1].Key != "b" {
		t.Fatalf("order changed: %s, %s", out[0].Key, out[1].Key)
	}
	if !almostEqual(out[1].SuccessRate, 0.25) {
		t.Fatalf("SuccessRate = %v, want 0.25", out[1].SuccessRate)
	}
}

func TestCompareDirectionFlagsAndRatio(t *testing.T) {
	current := domain.UsageSummary{Requests: 150, Successes: 150, AvgLatencyMS: 120, TotalCostUSD: 3}
	previous := domain.UsageSummary{Requests: 100, Successes: 90, AvgLatencyMS: 100, TotalCostUSD: 2}

	byMetric := map[string]MetricDelta{}
	for _, d := range Compare(current, previous) {
		byMetric[d.Metric] = d
	}

	requests := byMetric["requests"]
	if requests.Change != 50 || !almostEqual(requests.ChangeRatio, 0.5) {
		t.Fatalf("requests delta = %+v, want +50 / +0.5", requests)
	}
	if requests.HigherIsWorse {
		t.Fatal("rising request volume must not be flagged as bad")
	}
	if !byMetric["error_rate"].HigherIsWorse {
		t.Fatal("rising error rate must be flagged as bad")
	}
	if !byMetric["avg_latency_ms"].HigherIsWorse {
		t.Fatal("rising latency must be flagged as bad")
	}
}

func TestCompareRatioAgainstZeroPreviousIsZeroNotInfinite(t *testing.T) {
	deltas := Compare(domain.UsageSummary{Requests: 42}, domain.UsageSummary{})
	var requests MetricDelta
	for _, d := range deltas {
		if d.Metric == "requests" {
			requests = d
		}
	}
	if requests.ChangeRatio != 0 {
		t.Fatalf("ChangeRatio = %v, want 0 when the previous window was empty", requests.ChangeRatio)
	}
	if requests.Change != 42 {
		t.Fatalf("Change = %v, want 42", requests.Change)
	}
}

func TestBuildCacheEstimatesSavingsFromTheMissPopulation(t *testing.T) {
	report := BuildCache(domain.CacheSplit{
		Hits:        40,
		Misses:      60,
		HitAvgMS:    10,
		MissAvgMS:   110,
		MissCostUSD: 0.02,
		HitTokens:   100,
		MissTokens:  900,
	})

	if !almostEqual(report.HitRate, 0.4) {
		t.Fatalf("HitRate = %v, want 0.4", report.HitRate)
	}
	if !report.Active {
		t.Fatal("a window with hits and misses must read as an active cache")
	}
	if !almostEqual(report.LatencySavedPerHitMS, 100) {
		t.Fatalf("LatencySavedPerHitMS = %v, want 100", report.LatencySavedPerHitMS)
	}
	if !almostEqual(report.LatencySavedTotalMS, 4000) {
		t.Fatalf("LatencySavedTotalMS = %v, want 4000", report.LatencySavedTotalMS)
	}
	if !almostEqual(report.CostSavedUSD, 0.8) {
		t.Fatalf("CostSavedUSD = %v, want 0.8", report.CostSavedUSD)
	}
}

func TestBuildCacheNeverReportsNegativeSavings(t *testing.T) {
	// A cache that is somehow slower than the provider must not report a
	// negative saving, which would render as "saved -30ms" and read as a bug.
	report := BuildCache(domain.CacheSplit{Hits: 5, Misses: 5, HitAvgMS: 200, MissAvgMS: 100})
	if report.LatencySavedPerHitMS != 0 {
		t.Fatalf("LatencySavedPerHitMS = %v, want 0", report.LatencySavedPerHitMS)
	}
	if report.LatencySavedTotalMS != 0 {
		t.Fatalf("LatencySavedTotalMS = %v, want 0", report.LatencySavedTotalMS)
	}
}

func TestBuildCacheOnSilentWindowIsInactive(t *testing.T) {
	report := BuildCache(domain.CacheSplit{})
	if report.Active {
		t.Fatal("a window with no cache decisions must not read as an active cache")
	}
	if report.HitRate != 0 {
		t.Fatalf("HitRate = %v, want 0", report.HitRate)
	}
}

func TestBuildRoutingWeightsAttemptsByVolume(t *testing.T) {
	rows := []domain.StrategyRow{
		{Strategy: "lowest_latency", Requests: 90, Fallbacks: 9, AvgAttempts: 1.0},
		{Strategy: "cost_aware", Requests: 10, Fallbacks: 5, AvgAttempts: 3.0},
	}
	report := BuildRouting(rows)

	if report.Requests != 100 || report.Fallbacks != 14 {
		t.Fatalf("totals = %d/%d, want 100/14", report.Requests, report.Fallbacks)
	}
	if !almostEqual(report.FallbackRate, 0.14) {
		t.Fatalf("FallbackRate = %v, want 0.14", report.FallbackRate)
	}
	// A plain mean would give 2.0; volume-weighted gives 1.2. The weighted figure
	// is the one that describes the traffic.
	if !almostEqual(report.AvgAttempts, 1.2) {
		t.Fatalf("AvgAttempts = %v, want 1.2", report.AvgAttempts)
	}
	if !almostEqual(report.ProviderSwitchRate, report.FallbackRate) {
		t.Fatal("provider switch rate must equal the fallback rate")
	}
}

func TestBuildRoutingOnEmptyRowsIsZeroed(t *testing.T) {
	report := BuildRouting(nil)
	if report.Requests != 0 || report.FallbackRate != 0 || report.AvgAttempts != 0 {
		t.Fatalf("empty routing report is not zeroed: %+v", report)
	}
}

func TestSortRowsOrdersByRequestedMetricAndDoesNotMutateInput(t *testing.T) {
	rows := []domain.DimensionRow{
		{Key: "a", Requests: 100, CostUSD: 1, Errors: 1, LatencyP95MS: 10, TotalTokens: 10},
		{Key: "b", Requests: 10, CostUSD: 50, Errors: 9, LatencyP95MS: 900, TotalTokens: 90},
	}

	if got := SortRows(rows, "cost")[0].Key; got != "b" {
		t.Fatalf("cost sort put %q first, want b", got)
	}
	if got := SortRows(rows, "errors")[0].Key; got != "b" {
		t.Fatalf("errors sort put %q first, want b", got)
	}
	if got := SortRows(rows, "latency")[0].Key; got != "b" {
		t.Fatalf("latency sort put %q first, want b", got)
	}
	if got := SortRows(rows, "requests")[0].Key; got != "a" {
		t.Fatalf("requests sort put %q first, want a", got)
	}
	// An unknown key must not panic or reorder unpredictably.
	if got := SortRows(rows, "nonsense")[0].Key; got != "a" {
		t.Fatalf("unknown sort key put %q first, want the requests ordering", got)
	}
	if rows[0].Key != "a" {
		t.Fatal("SortRows mutated its input")
	}
}
