package benchmark

import (
	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ProbeRequest is one provider/model/prompt measurement to perform.
type ProbeRequest struct {
	Provider    string
	Model       string
	Prompt      domain.BenchmarkPrompt
	Timeout     time.Duration
	RetryCount  int
	Temperature float64
	MaxTokens   int
}

// ProbeResult carries the collected metrics for one probe.
type ProbeResult struct {
	TTFTMS             int64
	LatencyMS          int64
	TokensPerSecond    float64
	PromptTokens       int
	CompletionTokens   int
	TotalTokens        int
	CostUSD            float64
	Success            bool
	Error              string
	ErrorType          string
	HTTPStatus         int
	RetryCount         int
	FinishReason       string
	StreamingSpeed     float64
	JSONSuccess        *bool
	ToolCallingSuccess *bool
	ContextLength      int
	Metadata           map[string]string
}

// ProbeCaller executes one probe against a provider.
//
// The gateway wires this to its live provider adapters so benchmarks measure
// real upstream behaviour through the same path inference uses. The default
// caller below performs a synthetic measurement (timed, priced, deterministic
// in shape) so the engine works without any provider configured and its tests
// never need network access.
type ProbeCaller interface {
	Probe(ctx context.Context, req ProbeRequest) ProbeResult
}

// SyntheticCaller measures a deterministic synthetic probe. It is the safe
// default: no network, no spend, but real timing/throughput plumbing.
type SyntheticCaller struct{}

// Probe runs the synthetic measurement.
func (SyntheticCaller) Probe(ctx context.Context, req ProbeRequest) ProbeResult {
	start := time.Now()
	// Simulate time-to-first-token and total latency deterministically from
	// the input sizes so repeated runs are comparable.
	promptLen := len(req.Prompt.Prompt)
	if promptLen == 0 {
		promptLen = 64
	}
	ttft := time.Duration(40+promptLen%120) * time.Millisecond
	total := ttft + time.Duration(req.MaxTokens%400)*time.Millisecond + 120*time.Millisecond
	select {
	case <-ctx.Done():
		return ProbeResult{Success: false, Error: ctx.Err().Error(), ErrorType: "timeout", RetryCount: req.RetryCount}
	case <-time.After(minDuration(total, 900*time.Millisecond)):
	}
	latency := time.Since(start)
	promptTokens := promptLen / 4
	if promptTokens < 1 {
		promptTokens = 8
	}
	completionTokens := req.MaxTokens
	if completionTokens <= 0 || completionTokens > 256 {
		completionTokens = 128
	}
	totalTokens := promptTokens + completionTokens
	secs := latency.Seconds()
	if secs <= 0 {
		secs = 0.2
	}
	tps := float64(completionTokens) / secs
	cost := float64(promptTokens)/1e6*0.5 + float64(completionTokens)/1e6*1.5
	jsonOK, toolOK := probeQualitySignals(req.Prompt)
	return ProbeResult{
		TTFTMS: ttft.Milliseconds(), LatencyMS: latency.Milliseconds(),
		TokensPerSecond: tps, PromptTokens: promptTokens, CompletionTokens: completionTokens,
		TotalTokens: totalTokens, CostUSD: cost, Success: true, HTTPStatus: 200,
		FinishReason: "stop", StreamingSpeed: tps,
		JSONSuccess: jsonOK, ToolCallingSuccess: toolOK,
		ContextLength: totalTokens,
		Metadata:      map[string]string{"caller": "synthetic"},
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func probeQualitySignals(p domain.BenchmarkPrompt) (*bool, *bool) {
	var jsonOK, toolOK *bool
	t, f := true, false
	switch p.Category {
	case domain.BenchmarkCatJSONMode:
		// Synthetic output is well-formed JSON often enough to exercise the flag.
		if rand.Intn(10) < 8 { //nolint:gosec // synthetic benchmark signal only
			jsonOK = &t
		} else {
			jsonOK = &f
		}
	case domain.BenchmarkCatToolCalling, domain.BenchmarkCatFunctionCalling:
		if rand.Intn(10) < 8 { //nolint:gosec // synthetic benchmark signal only
			toolOK = &t
		} else {
			toolOK = &f
		}
	}
	_ = strings.TrimSpace
	return jsonOK, toolOK
}
