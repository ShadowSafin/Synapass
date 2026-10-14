package benchmark

import (
	"context"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
)

// Disabled by default: the engine must refuse work and start nothing.
func TestEngineDisabledByDefault(t *testing.T) {
	cfg := config.Default()
	if cfg.Benchmark.Enabled {
		t.Fatal("Default().Benchmark.Enabled must be false; the engine is opt-in")
	}
	eng := New(Options{Config: cfg.Benchmark})
	if eng.Enabled() {
		t.Fatal("a default engine must report disabled")
	}
	ctx := context.Background()
	if _, err := eng.CreateJob(ctx, "", &domain.BenchmarkJob{Name: "x"}); err == nil {
		t.Fatal("CreateJob on a disabled engine must fail")
	}
	if _, err := eng.Overview(ctx, ""); err == nil {
		t.Fatal("Overview on a disabled engine must fail")
	}
	// Start must be a no-op that leaves no workers behind.
	eng.Start(ctx)
	if eng.WorkerCount() != 0 {
		t.Fatalf("disabled engine started %d workers", eng.WorkerCount())
	}
	eng.Stop()
}

func TestNextRunAfterPresets(t *testing.T) {
	after := time.Date(2026, 10, 14, 10, 7, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		domain.BenchmarkEveryMinute:   time.Minute,
		domain.BenchmarkEvery5Minutes: 5 * time.Minute,
		domain.BenchmarkEveryHour:     time.Hour,
		domain.BenchmarkEvery24Hours:  24 * time.Hour,
	}
	for schedule, step := range cases {
		next := NextRunAfter(schedule, "", after)
		if !next.After(after) {
			t.Errorf("schedule %s: next %v is not after %v", schedule, next, after)
		}
		if next.Sub(after) > step {
			t.Errorf("schedule %s: next %v is more than one step %v ahead", schedule, next, step)
		}
	}
}

func TestValidSchedule(t *testing.T) {
	if err := ValidSchedule(domain.BenchmarkEveryHour, ""); err != nil {
		t.Errorf("preset must validate: %v", err)
	}
	if err := ValidSchedule(domain.BenchmarkCustomCron, "*/5 * * * *"); err != nil {
		t.Errorf("valid cron must validate: %v", err)
	}
	if err := ValidSchedule(domain.BenchmarkCustomCron, ""); err == nil {
		t.Error("custom without an expression must fail")
	}
	if err := ValidSchedule("every_fortnight", ""); err == nil {
		t.Error("unknown preset must fail")
	}
}

func TestQueueEnqueueDequeue(t *testing.T) {
	q := NewQueue(nil, "test", 10)
	ctx := context.Background()
	if err := q.Enqueue(ctx, "run-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := q.Size(ctx); got != 1 {
		t.Fatalf("size = %d, want 1", got)
	}
	id, ok := q.Dequeue(ctx, time.Second)
	if !ok || id != "run-1" {
		t.Fatalf("dequeue = %q, %v; want run-1, true", id, ok)
	}
}

func TestExportFormats(t *testing.T) {
	results := []domain.BenchmarkResult{
		{ID: "r1", Provider: "openai", Model: "gpt-4o-mini", TTFTMS: 120, LatencyMS: 800, Success: true},
	}
	for _, format := range []ExportFormat{ExportCSV, ExportJSON, ExportExcel, ExportPDF} {
		raw, contentType, err := ExportResults(results, format)
		if err != nil {
			t.Errorf("format %s: %v", format, err)
			continue
		}
		if len(raw) == 0 || contentType == "" {
			t.Errorf("format %s: empty output", format)
		}
	}
	if _, _, err := ExportResults(results, "parquet"); err == nil {
		t.Error("unknown format must fail")
	}
}

func TestSyntheticCaller(t *testing.T) {
	var c SyntheticCaller
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := c.Probe(ctx, ProbeRequest{
		Provider: "openai", Model: "gpt-4o-mini",
		Prompt:    domain.BenchmarkPrompt{Prompt: "Hello", Category: domain.BenchmarkCatReasoning},
		MaxTokens: 128,
	})
	if !res.Success {
		t.Fatalf("synthetic probe must succeed, got %q", res.Error)
	}
	if res.LatencyMS <= 0 || res.TotalTokens <= 0 {
		t.Errorf("synthetic probe must report metrics: %+v", res)
	}
}

func TestDefaultPromptsCoverCategories(t *testing.T) {
	prompts := DefaultPrompts()
	if len(prompts) < 10 {
		t.Fatalf("default corpus has %d prompts, want at least 10", len(prompts))
	}
	seen := map[domain.BenchmarkPromptCategory]bool{}
	for _, p := range prompts {
		if p.Prompt == "" {
			t.Error("default prompt must not be empty")
		}
		seen[p.Category] = true
	}
	for _, cat := range []domain.BenchmarkPromptCategory{
		domain.BenchmarkCatCoding, domain.BenchmarkCatReasoning, domain.BenchmarkCatMath,
		domain.BenchmarkCatJSONMode, domain.BenchmarkCatToolCalling,
	} {
		if !seen[cat] {
			t.Errorf("default corpus is missing category %q", cat)
		}
	}
}
