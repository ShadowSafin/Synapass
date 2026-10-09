package telemetry

import (
	"strings"
	"testing"
)

// TestStreamMetricsObserve proves every streaming collector is registered,
// accepts observations without panicking, and exports its family once used.
// Vector families (histograms, counters) expose nothing before their first
// observation, which is why a fresh /metrics shows only the gauge.
func TestStreamMetricsObserve(t *testing.T) {
	m := NewMetrics(MetricsConfig{Enabled: true, Namespace: "synapass"})
	if !m.Enabled() {
		t.Fatalf("metrics must be enabled")
	}

	m.StreamActiveInc()
	m.StreamActiveDec()
	m.ObserveStreamTTFT("openai", 0.12)
	m.ObserveStreamTTFT("", -1) // negative durations are dropped, not recorded
	m.ObserveStreamFirstText("anthropic", 0.2)
	m.ObserveStreamEnd("openai", "completed", 1.5)
	m.ObserveStreamEnd("openai", "cancelled", 0.4)
	m.ObserveStreamEnd("openai", "truncated", 2.0)
	m.ObserveStreamEnd("openai", "error", 0.1)
	m.ObserveStreamError("openai", "upstream")
	m.ObserveStreamError("openai", "downstream")
	m.ObserveStreamError("openai", "timeout")
	m.ObserveStreamError("openai", "bogus-stage") // clamped to upstream
	m.ObserveStreamCancel("openai")
	m.ObserveStreamBackpressure("openai")

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"synapass_stream_ttft_seconds":       false,
		"synapass_stream_first_text_seconds": false,
		"synapass_stream_duration_seconds":   false,
		"synapass_stream_active":             false,
		"synapass_stream_errors_total":       false,
		"synapass_stream_cancels_total":      false,
		"synapass_stream_backpressure_total": false,
	}
	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	var missing []string
	for name, seen := range want {
		if !seen {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("missing stream families: %s", strings.Join(missing, ", "))
	}
}

// TestStreamMetricsDisabledIsNoOp proves every method is safe on a disabled
// or nil Metrics, so instrumentation calls need no guards.
func TestStreamMetricsDisabledIsNoOp(t *testing.T) {
	var nilMetrics *Metrics
	nilMetrics.StreamActiveInc()
	nilMetrics.StreamActiveDec()
	nilMetrics.ObserveStreamTTFT("p", 1)
	nilMetrics.ObserveStreamFirstText("p", 1)
	nilMetrics.ObserveStreamEnd("p", "completed", 1)
	nilMetrics.ObserveStreamError("p", "upstream")
	nilMetrics.ObserveStreamCancel("p")
	nilMetrics.ObserveStreamBackpressure("p")

	off := NewMetrics(MetricsConfig{Enabled: false})
	off.StreamActiveInc()
	off.ObserveStreamTTFT("p", 1)
	off.ObserveStreamEnd("p", "completed", 1)
	off.ObserveStreamError("p", "upstream")
	off.ObserveStreamCancel("p")
	off.ObserveStreamBackpressure("p")
}
