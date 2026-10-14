package benchmark

import (
	"context"
	"fmt"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/storage"
)

// GenerateInsights produces natural-language summaries from window deltas.
//
// Examples: "Claude became 14% faster.", "Gemini latency increased by 9%.",
// "HopsAPI experienced two outages.", "Groq is now the cheapest provider.",
// "OpenAI provides the highest overall reliability."
func GenerateInsights(ctx context.Context, repos *storage.Repositories, tenantID string) ([]domain.BenchmarkInsight, error) {
	now := time.Now().UTC()
	week := 7 * 24 * time.Hour
	current, err := repos.BenchmarkResults.ProviderStats(ctx, tenantID, now.Add(-week), now)
	if err != nil {
		return nil, err
	}
	previous, err := repos.BenchmarkResults.ProviderStats(ctx, tenantID, now.Add(-2*week), now.Add(-week))
	if err != nil {
		return nil, err
	}
	prevByProvider := map[string]domain.BenchmarkProviderHealth{}
	for _, h := range previous {
		prevByProvider[h.Provider] = h
	}
	var out []domain.BenchmarkInsight
	for _, h := range current {
		old, ok := prevByProvider[h.Provider]
		if ok && old.AvgLatencyMS > 0 && h.AvgLatencyMS > 0 {
			delta := (old.AvgLatencyMS - h.AvgLatencyMS) / old.AvgLatencyMS * 100
			switch {
			case delta >= 5:
				out = append(out, domain.BenchmarkInsight{
					Text:     fmt.Sprintf("%s became %.0f%% faster.", h.Provider, delta),
					Severity: "info", Provider: h.Provider, Metric: "latency", DeltaPct: delta, CreatedAt: now,
				})
			case delta <= -5:
				out = append(out, domain.BenchmarkInsight{
					Text:     fmt.Sprintf("%s latency increased by %.0f%%.", h.Provider, -delta),
					Severity: "warning", Provider: h.Provider, Metric: "latency", DeltaPct: delta, CreatedAt: now,
				})
			}
			oldRel, newRel := old.SuccessRate, h.SuccessRate
			if oldRel >= 0.99 && newRel < 0.9 && h.TotalProbes >= 3 {
				out = append(out, domain.BenchmarkInsight{
					Text:     fmt.Sprintf("%s experienced outages (success rate %.0f%%).", h.Provider, newRel*100),
					Severity: "critical", Provider: h.Provider, Metric: "availability", CreatedAt: now,
				})
			}
		}
	}
	if len(current) > 0 {
		cheapest, reliable := current[0], current[0]
		for _, h := range current[1:] {
			if h.AvgLatencyMS < cheapest.AvgLatencyMS {
				cheapest = h
			}
			if h.SuccessRate > reliable.SuccessRate {
				reliable = h
			}
		}
		out = append(out, domain.BenchmarkInsight{
			Text:     fmt.Sprintf("%s is now the fastest provider.", cheapest.Provider),
			Severity: "info", Provider: cheapest.Provider, Metric: "latency", CreatedAt: now,
		})
		out = append(out, domain.BenchmarkInsight{
			Text:     fmt.Sprintf("%s provides the highest overall reliability.", reliable.Provider),
			Severity: "info", Provider: reliable.Provider, Metric: "reliability", CreatedAt: now,
		})
	}
	if len(out) == 0 {
		out = append(out, domain.BenchmarkInsight{
			Text:     "Not enough benchmark history yet to generate insights.",
			Severity: "info", CreatedAt: now,
		})
	}
	return out, nil
}
