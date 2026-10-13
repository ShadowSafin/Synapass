// Package analytics turns stored telemetry into the aggregates the analytics
// console renders. It is deliberately pure: every function here takes rows that
// have already been read and returns derived figures, so the arithmetic can be
// unit tested without a database and cannot accidentally re-query on render.
//
// Nothing in this package decides routing or talks to providers. It reads what
// already happened.
package analytics

import (
	"sort"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Report is one complete analytics payload for a window.
//
// It is a single document rather than a dozen endpoints because the console
// renders every section together: returning them in one response keeps the
// sections mutually consistent (they all describe the same instant) and avoids
// a page that flickers section by section as separate requests land.
type Report struct {
	Window   domain.TimeRange    `json:"window"`
	Interval string              `json:"interval"`
	Summary  domain.UsageSummary `json:"summary"`
	Previous domain.UsageSummary `json:"previous_summary"`
	Buckets  []domain.TimeBucket `json:"buckets"`
	Compared []MetricDelta       `json:"comparison"`

	Providers    []domain.DimensionRow `json:"providers"`
	Models       []domain.DimensionRow `json:"models"`
	Tenants      []domain.DimensionRow `json:"tenants"`
	Policies     []domain.DimensionRow `json:"policies"`
	RequestTypes []domain.DimensionRow `json:"request_types"`
	Outcomes     []domain.DimensionRow `json:"outcomes"`
	Errors       []domain.DimensionRow `json:"errors"`

	Cache   CacheReport   `json:"cache"`
	Routing RoutingReport `json:"routing"`

	// GeneratedAt lets the UI say how fresh the figures are without guessing.
	GeneratedAt time.Time `json:"generated_at"`
}

// MetricDelta is one metric compared across the current and previous windows.
//
// HigherIsWorse is carried so the UI never has to hardcode which direction is
// bad: a rising error rate and a rising throughput are the same shape of
// number and must be colored differently.
type MetricDelta struct {
	Metric        string  `json:"metric"`
	Current       float64 `json:"current"`
	Previous      float64 `json:"previous"`
	Change        float64 `json:"change"`
	ChangeRatio   float64 `json:"change_ratio"`
	HigherIsWorse bool    `json:"higher_is_worse"`
	// Unit is a hint for formatting: "count", "ratio", "ms", "usd".
	Unit string `json:"unit"`
}

// CacheReport answers whether caching is actually helping.
type CacheReport struct {
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
	// HitRate is over all requests in the window, hits / (hits + misses).
	HitRate float64 `json:"hit_rate"`

	HitAvgLatencyMS  float64 `json:"hit_avg_latency_ms"`
	MissAvgLatencyMS float64 `json:"miss_avg_latency_ms"`
	// LatencySavedPerHitMS is the mean latency a hit avoided, estimated from the
	// provider-served population. It is an estimate and is labelled as one: a
	// hit has no counterfactual to measure.
	LatencySavedPerHitMS float64 `json:"latency_saved_per_hit_ms"`
	// LatencySavedTotalMS is that figure applied to the hits in the window.
	LatencySavedTotalMS float64 `json:"latency_saved_total_ms"`
	// CostSavedUSD applies the mean cost of a provider-served request to the
	// hits. Estimated, same caveat.
	CostSavedUSD float64 `json:"cost_saved_usd"`

	// CachedPromptTokens is provider-side prompt caching, a different mechanism
	// from the response cache and reported separately so the two are not
	// conflated into one "cache" number.
	CachedPromptTokens int64   `json:"cached_prompt_tokens"`
	PromptCacheShare   float64 `json:"prompt_cache_share"`

	// Active is false when the window contains no cache telemetry at all, which
	// is different from a cache that is on and missing.
	Active bool `json:"active"`
}

// RoutingReport describes how the router behaved.
type RoutingReport struct {
	Strategies []domain.StrategyRow `json:"strategies"`
	// Requests and Fallbacks are totals across the strategies.
	Requests     int64   `json:"requests"`
	Fallbacks    int64   `json:"fallbacks"`
	Errors       int64   `json:"errors"`
	FallbackRate float64 `json:"fallback_rate"`
	AvgAttempts  float64 `json:"avg_attempts"`
	// ProviderSwitchRate is the share of requests where the router did not stay
	// with its first choice. This is the honest measure of "how often does the
	// router change its mind".
	ProviderSwitchRate float64 `json:"provider_switch_rate"`
}

// Enrich fills in the derived metrics on dimension rows.
//
// Every rate is computed from the counts on the same row, so a consumer can
// never divide a numerator from one aggregate by a denominator from another.
func Enrich(rows []domain.DimensionRow) []domain.DimensionRow {
	out := make([]domain.DimensionRow, len(rows))
	for i, row := range rows {
		out[i] = EnrichRow(row)
	}
	return out
}

// EnrichRow derives the rates on a single row.
func EnrichRow(row domain.DimensionRow) domain.DimensionRow {
	if row.Requests > 0 {
		denominator := float64(row.Requests)
		row.SuccessRate = float64(row.Successes) / denominator
		row.ErrorRate = float64(row.Errors+row.Rejections) / denominator
		row.FallbackRate = float64(row.Fallbacks) / denominator
		row.CacheHitRate = float64(row.CacheHits) / denominator
		row.CostPerSuccessUSD = row.CostUSD / denominator
		row.TokensPerRequest = float64(row.TotalTokens) / denominator
	}
	if row.Successes > 0 {
		// Cost per successful request is the number an operator budgets against:
		// spend on failed attempts is real but is not what a caller bought.
		row.CostPerSuccessUSD = row.CostUSD / float64(row.Successes)
	}
	return row
}

// Compare builds the current-vs-previous deltas for the headline metrics.
//
// The order is fixed and intentional: it matches the order the KPI cards read,
// so the UI can pair them positionally.
func Compare(current, previous domain.UsageSummary) []MetricDelta {
	deltas := []MetricDelta{
		delta("requests", float64(current.Requests), float64(previous.Requests), false, "count"),
		delta("success_rate", successRate(current), successRate(previous), false, "ratio"),
		delta("error_rate", current.ErrorRate, previous.ErrorRate, true, "ratio"),
		delta("fallback_rate", current.FallbackRate, previous.FallbackRate, true, "ratio"),
		delta("avg_latency_ms", current.AvgLatencyMS, previous.AvgLatencyMS, true, "ms"),
		delta("p95_latency_ms", float64(current.LatencyP95MS), float64(previous.LatencyP95MS), true, "ms"),
		delta("total_cost_usd", current.TotalCostUSD, previous.TotalCostUSD, true, "usd"),
		delta("cost_per_request_usd", current.AvgCostUSD, previous.AvgCostUSD, true, "usd"),
		delta("total_tokens", float64(current.TotalTokens), float64(previous.TotalTokens), true, "count"),
		delta("unique_tenants", float64(current.UniqueTenants), float64(previous.UniqueTenants), false, "count"),
	}
	return deltas
}

func delta(metric string, current, previous float64, higherIsWorse bool, unit string) MetricDelta {
	d := MetricDelta{
		Metric:        metric,
		Current:       current,
		Previous:      previous,
		Change:        current - previous,
		HigherIsWorse: higherIsWorse,
		Unit:          unit,
	}
	// A ratio against zero is undefined, not infinite. Reporting 0 keeps the UI
	// from rendering "+Inf%" the first time a window has traffic and the one
	// before it did not.
	if previous != 0 {
		d.ChangeRatio = (current - previous) / previous
	}
	return d
}

func successRate(s domain.UsageSummary) float64 {
	if s.Requests == 0 {
		return 0
	}
	return float64(s.Successes) / float64(s.Requests)
}

// BuildCache turns the cached/provider-served split into a report with the
// savings estimates that follow from it.
func BuildCache(split domain.CacheSplit) CacheReport {
	total := split.Hits + split.Misses
	report := CacheReport{
		Hits:               split.Hits,
		Misses:             split.Misses,
		HitAvgLatencyMS:    split.HitAvgMS,
		MissAvgLatencyMS:   split.MissAvgMS,
		CachedPromptTokens: split.CachedPromptTokens,
	}
	if total > 0 {
		report.HitRate = float64(split.Hits) / float64(total)
	}
	// "Active" means we saw cache decisions at all. A window with only misses is
	// an active cache that is not helping — a very different finding from a
	// cache that is switched off, and the UI says which.
	report.Active = split.Hits > 0 || split.Misses > 0

	if split.MissAvgMS > split.HitAvgMS {
		report.LatencySavedPerHitMS = split.MissAvgMS - split.HitAvgMS
	}
	report.LatencySavedTotalMS = report.LatencySavedPerHitMS * float64(split.Hits)
	report.CostSavedUSD = split.MissCostUSD * float64(split.Hits)

	allTokens := split.HitTokens + split.MissTokens
	if allTokens > 0 {
		report.PromptCacheShare = float64(split.CachedPromptTokens) / float64(allTokens)
	}
	return report
}

// BuildRouting summarizes the per-strategy routing rows.
func BuildRouting(rows []domain.StrategyRow) RoutingReport {
	report := RoutingReport{Strategies: rows}
	var weightedAttempts float64
	for _, row := range rows {
		report.Requests += row.Requests
		report.Fallbacks += row.Fallbacks
		report.Errors += row.Errors
		weightedAttempts += row.AvgAttempts * float64(row.Requests)
	}
	if report.Requests > 0 {
		report.FallbackRate = float64(report.Fallbacks) / float64(report.Requests)
		report.AvgAttempts = weightedAttempts / float64(report.Requests)
	}
	report.ProviderSwitchRate = report.FallbackRate
	return report
}

// SortRows orders dimension rows by a named metric, descending.
//
// Unknown sort keys fall back to requests rather than erroring: a URL parameter
// should not be able to break a page, and requests is the least surprising
// default ordering.
func SortRows(rows []domain.DimensionRow, key string) []domain.DimensionRow {
	out := make([]domain.DimensionRow, len(rows))
	copy(out, rows)
	less := func(i, j int) bool { return out[i].Requests > out[j].Requests }
	switch key {
	case "cost":
		less = func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD }
	case "errors":
		less = func(i, j int) bool { return out[i].Errors > out[j].Errors }
	case "latency":
		less = func(i, j int) bool { return out[i].LatencyP95MS > out[j].LatencyP95MS }
	case "tokens":
		less = func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens }
	case "error_rate":
		less = func(i, j int) bool { return out[i].ErrorRate > out[j].ErrorRate }
	case "cost_per_success":
		less = func(i, j int) bool { return out[i].CostPerSuccessUSD > out[j].CostPerSuccessUSD }
	}
	sort.SliceStable(out, less)
	return out
}
