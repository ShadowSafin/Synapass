package benchmark

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Worker drains run IDs from the queue and executes them.
//
// Workers scale horizontally: every replica runs its own pool against the
// same Redis queue. Execution is fully asynchronous and bounded by the job's
// concurrency and timeout so a slow provider can never stall the gateway.
type Worker struct {
	engine *Engine
	index  int
	id     string

	stop    chan struct{}
	stopped chan struct{}
	wg      sync.WaitGroup
}

// NewWorker builds a worker.
func NewWorker(engine *Engine, index int) *Worker {
	return &Worker{
		engine: engine, index: index,
		id:   uuid.NewString()[:8],
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

// Start begins the drain loop.
func (w *Worker) Start(ctx context.Context) {
	w.wg.Add(1)
	go w.loop(ctx)
}

// Stop halts the worker.
func (w *Worker) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	defer w.wg.Done()
	defer close(w.stopped)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		default:
		}
		runID, ok := w.engine.queue.Dequeue(ctx, 2*time.Second)
		if !ok {
			continue
		}
		w.execute(ctx, runID)
	}
}

// execute runs one benchmark: for each provider, for each model, run every
// prompt, collect metrics, store results.
func (w *Worker) execute(ctx context.Context, runID string) {
	e := w.engine
	start := time.Now().UTC()
	log := func(level, msg string) {
		if e.repos != nil && e.repos.BenchmarkWorkerLogs != nil {
			e.repos.BenchmarkWorkerLogs.Append(context.Background(), w.id, runID, level, msg)
		}
	}
	run, err := e.repos.BenchmarkRuns.GetByID(ctx, runID)
	if err != nil || run == nil {
		log("warn", "run not found: "+runID)
		return
	}
	job, err := e.repos.BenchmarkJobs.GetByID(ctx, run.JobID)
	if err != nil || job == nil {
		_ = e.repos.BenchmarkRuns.Finish(ctx, runID, "failed", 0, 0, 0, 0, 0, "benchmark job not found")
		return
	}
	e.publish(Event{Type: "run_started", RunID: runID, Data: map[string]any{"job_id": job.ID}, Time: time.Now().UTC()})
	_ = e.repos.BenchmarkRuns.SetStatus(ctx, runID, "running")
	prompts := e.promptsForJob(ctx, job)
	concurrency := job.Concurrency
	if concurrency <= 0 {
		concurrency = 5
	}
	if concurrency > 100 {
		concurrency = 100
	}
	timeout := time.Duration(job.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	type task struct {
		provider string
		model    string
		prompt   domain.BenchmarkPrompt
	}
	var tasks []task
	for _, p := range job.Providers {
		for _, m := range job.Models {
			for _, pr := range prompts {
				tasks = append(tasks, task{provider: p, model: m, prompt: pr})
			}
		}
	}
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	probes, successes, failures := 0, 0, 0
	var cost float64
	var tokens int64
	var wg sync.WaitGroup
	for _, t := range tasks {
		t := t
		// Budget protection mid-run: stop queuing new probes once over budget.
		if over, _ := e.dailyBudgetExceeded(ctx, job.TenantID, job); over {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// Retry loop per probe.
			attempts := job.RetryCount + 1
			if attempts < 1 {
				attempts = 1
			}
			var pres ProbeResult
			for a := 0; a < attempts; a++ {
				pctx, cancel := context.WithTimeout(ctx, timeout)
				pres = e.caller.Probe(pctx, ProbeRequest{
					Provider: t.provider, Model: t.model, Prompt: t.prompt,
					Timeout: timeout, RetryCount: a,
					Temperature: t.prompt.Temperature, MaxTokens: t.prompt.MaxTokens,
				})
				cancel()
				pres.RetryCount = a
				if pres.Success {
					break
				}
			}
			res := &domain.BenchmarkResult{
				RunID: runID, JobID: job.ID, TenantID: job.TenantID,
				Provider: t.provider, Model: t.model, PromptID: t.prompt.ID,
				Category: string(t.prompt.Category),
				TTFTMS:   pres.TTFTMS, LatencyMS: pres.LatencyMS,
				TokensPerSecond: pres.TokensPerSecond,
				PromptTokens:    pres.PromptTokens, CompletionTokens: pres.CompletionTokens,
				TotalTokens: pres.TotalTokens, CostUSD: pres.CostUSD,
				Success: pres.Success, Error: pres.Error, ErrorType: pres.ErrorType,
				HTTPStatus: pres.HTTPStatus, RetryCount: pres.RetryCount,
				FinishReason: pres.FinishReason, StreamingSpeed: pres.StreamingSpeed,
				JSONSuccess: pres.JSONSuccess, ToolCallingSuccess: pres.ToolCallingSuccess,
				ContextLength: pres.ContextLength, ProviderMetadata: pres.Metadata,
			}
			_ = e.repos.BenchmarkResults.Insert(ctx, res)
			mu.Lock()
			probes++
			if pres.Success {
				successes++
			} else {
				failures++
			}
			cost += pres.CostUSD
			tokens += int64(pres.TotalTokens)
			mu.Unlock()
		}()
	}
	wg.Wait()
	status := "completed"
	runErr := ""
	if probes > 0 && failures == probes {
		status = "failed"
		runErr = "all benchmark probes failed"
	}
	_ = e.repos.BenchmarkRuns.Finish(ctx, runID, status, probes, successes, failures, cost, tokens, runErr)
	duration := time.Since(start)
	_ = duration
	log("info", "run "+status)
	e.publish(Event{Type: "run_finished", RunID: runID, Data: map[string]any{
		"job_id": job.ID, "status": status, "probes": probes,
		"successes": successes, "failures": failures, "cost_usd": cost,
	}, Time: time.Now().UTC()})
	if status == "completed" {
		e.notify(ctx, job.TenantID, "benchmark_completed", map[string]any{"job": job.Name, "run_id": runID})
		// Down-provider detection for notifications.
		e.checkProviderDown(ctx, job.TenantID, job)
	}
}

func (e *Engine) promptsForJob(ctx context.Context, job *domain.BenchmarkJob) []domain.BenchmarkPrompt {
	if e.repos == nil || e.repos.BenchmarkPrompts == nil {
		return DefaultPrompts()
	}
	if job.DatasetID != "" {
		if prompts, err := e.repos.BenchmarkPrompts.ListByDataset(ctx, job.DatasetID); err == nil && len(prompts) > 0 {
			return prompts
		}
	}
	// Fall back to the default dataset, then the built-in corpus.
	if e.repos.BenchmarkDatasets != nil {
		if datasets, err := e.repos.BenchmarkDatasets.List(ctx, job.TenantID); err == nil {
			for _, d := range datasets {
				if d.IsDefault {
					if prompts, err := e.repos.BenchmarkPrompts.ListByDataset(ctx, d.ID); err == nil && len(prompts) > 0 {
						return prompts
					}
				}
			}
		}
	}
	return DefaultPrompts()
}

// checkProviderDown notifies when a provider's recent success rate collapses.
func (e *Engine) checkProviderDown(ctx context.Context, tenantID string, job *domain.BenchmarkJob) {
	stats, err := e.repos.BenchmarkResults.ProviderStats(ctx, tenantID, time.Now().UTC().Add(-time.Hour), time.Time{})
	if err != nil {
		return
	}
	for _, h := range stats {
		if h.TotalProbes >= 3 && h.SuccessRate < 0.5 {
			e.notify(ctx, tenantID, "provider_down", map[string]any{"provider": h.Provider})
		} else if h.TotalProbes >= 3 && h.SuccessRate < 0.9 {
			e.notify(ctx, tenantID, "high_error_rate", map[string]any{"provider": h.Provider})
		}
		if h.AvgLatencyMS > 10000 {
			e.notify(ctx, tenantID, "high_latency", map[string]any{"provider": h.Provider})
		}
	}
}
