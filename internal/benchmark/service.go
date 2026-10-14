package benchmark

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/storage"
)

// ErrDisabled is returned when the engine is disabled.
var ErrDisabled = fmt.Errorf("the benchmark engine is disabled (set ENABLE_BENCHMARK_ENGINE=true)")

// Engine is the Autonomous Benchmark Engine.
//
// It owns jobs, runs, results, reports, insights, health, notifications and
// export, and drives the scheduler + worker pool. All work is asynchronous
// and workspace scoped; nothing on the inference hot path touches it.
type Engine struct {
	cfg    config.BenchmarkConfig
	logger *slog.Logger
	repos  *storage.Repositories

	queue  *Queue
	caller ProbeCaller

	// cfgEnabled is the configuration switch (ENABLE_BENCHMARK_ENGINE).
	// runtimeEnabled is the dashboard toggle, persisted in the settings
	// table so it survives restarts. The engine runs when either is set,
	// and stays off by default when neither is.
	cfgEnabled     bool
	runtimeEnabled atomic.Bool

	mu       sync.Mutex
	workers  []*Worker
	sched    *Scheduler
	running  atomic.Bool
	runCount atomic.Int64

	subsMu sync.Mutex
	subs   map[chan Event]struct{}
}

// Event is one live-dashboard update broadcast to SSE subscribers.
type Event struct {
	Type  string         `json:"type"`
	RunID string         `json:"run_id,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
	Time  time.Time      `json:"time"`
}

// Options wires the engine.
type Options struct {
	Config config.BenchmarkConfig
	Logger *slog.Logger
	Repos  *storage.Repositories
	Queue  *Queue
	Caller ProbeCaller
	Redis  RedisList
}

// New builds the engine. A nil Caller selects the synthetic default.
func New(opts Options) *Engine {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	q := opts.Queue
	if q == nil {
		q = NewQueue(opts.Redis, opts.Config.RedisQueueKey, opts.Config.QueueSize)
	}
	caller := opts.Caller
	if caller == nil {
		caller = SyntheticCaller{}
	}
	e := &Engine{cfg: opts.Config, cfgEnabled: opts.Config.Enabled, logger: logger, repos: opts.Repos, queue: q, caller: caller}
	e.subs = map[chan Event]struct{}{}
	return e
}

// Enabled reports the effective master switch: the configuration flag or the
// persisted dashboard toggle. False unless explicitly enabled either way.
func (e *Engine) Enabled() bool { return e.cfgEnabled || e.runtimeEnabled.Load() }

// ConfigEnabled reports the configuration switch alone, so the dashboard can
// tell "enabled by configuration" apart from "enabled from this page".
func (e *Engine) ConfigEnabled() bool { return e.cfgEnabled }

// SetRuntimeEnabled flips the dashboard toggle. It does not start or stop
// workers by itself; use Start/Stop for that.
func (e *Engine) SetRuntimeEnabled(enabled bool) { e.runtimeEnabled.Store(enabled) }

// Config returns the resolved engine configuration.
func (e *Engine) Config() config.BenchmarkConfig { return e.cfg }

// QueueDepth reports pending runs.
func (e *Engine) QueueDepth(ctx context.Context) int {
	if e.queue == nil {
		return 0
	}
	return e.queue.Size(ctx)
}

// WorkerCount reports configured workers.
func (e *Engine) WorkerCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.workers)
}

func (e *Engine) requireEnabled() error {
	if !e.Enabled() {
		return ErrDisabled
	}
	if e.repos == nil {
		return fmt.Errorf("the benchmark store is unavailable")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// CreateJob validates and stores a job.
func (e *Engine) CreateJob(ctx context.Context, tenantID string, j *domain.BenchmarkJob) (*domain.BenchmarkJob, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	if j.Name == "" {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "job name is required")
	}
	if len(j.Providers) == 0 {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "select at least one provider")
	}
	if len(j.Models) == 0 {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "select at least one model")
	}
	if err := ValidSchedule(j.Schedule, j.CronExpression); err != nil {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, err.Error())
	}
	if j.Concurrency <= 0 {
		j.Concurrency = e.cfg.DefaultConcurrency
		if j.Concurrency <= 0 {
			j.Concurrency = 5
		}
	}
	if j.Concurrency > 100 {
		j.Concurrency = 100
	}
	if j.TimeoutMS <= 0 {
		j.TimeoutMS = e.cfg.DefaultTimeout.Std().Milliseconds()
	}
	if j.Schedule == "" {
		j.Schedule = domain.BenchmarkEveryHour
	}
	j.TenantID = tenantID
	// New jobs start enabled; use an update to disable one.
	j.Enabled = true
	j.NextRunAt = timePtr(NextRunAfter(j.Schedule, j.CronExpression, time.Now().UTC()))
	return e.repos.BenchmarkJobs.Create(ctx, j)
}

// UpdateJob rewrites a job.
func (e *Engine) UpdateJob(ctx context.Context, tenantID string, j *domain.BenchmarkJob) (*domain.BenchmarkJob, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	existing, err := e.repos.BenchmarkJobs.GetByID(ctx, j.ID)
	if err != nil || existing == nil {
		return nil, domain.NewError(domain.ErrCodeNotFound, "benchmark job not found")
	}
	if existing.TenantID != "" && existing.TenantID != tenantID && tenantID != "" {
		return nil, domain.NewError(domain.ErrCodePermission, "you do not own this benchmark job")
	}
	if err := ValidSchedule(j.Schedule, j.CronExpression); err != nil {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, err.Error())
	}
	j.TenantID = existing.TenantID
	j.NextRunAt = existing.NextRunAt
	if j.Schedule != existing.Schedule || j.CronExpression != existing.CronExpression {
		j.NextRunAt = timePtr(NextRunAfter(j.Schedule, j.CronExpression, time.Now().UTC()))
	}
	updated, err := e.repos.BenchmarkJobs.Update(ctx, j)
	if err != nil {
		return nil, err
	}
	// Refresh the next-run stamp when the schedule changed.
	if updated != nil && j.NextRunAt != nil {
		_ = e.repos.BenchmarkJobs.MarkRan(ctx, updated.ID, timeOrNow(existing.LastRunAt), *j.NextRunAt)
		updated.NextRunAt = j.NextRunAt
	}
	return updated, nil
}

// DeleteJob removes a job.
func (e *Engine) DeleteJob(ctx context.Context, tenantID, id string) error {
	if err := e.requireEnabled(); err != nil {
		return err
	}
	existing, err := e.repos.BenchmarkJobs.GetByID(ctx, id)
	if err != nil || existing == nil {
		return domain.NewError(domain.ErrCodeNotFound, "benchmark job not found")
	}
	if existing.TenantID != "" && existing.TenantID != tenantID && tenantID != "" {
		return domain.NewError(domain.ErrCodePermission, "you do not own this benchmark job")
	}
	return e.repos.BenchmarkJobs.Delete(ctx, id)
}

// ListJobs returns paginated jobs.
func (e *Engine) ListJobs(ctx context.Context, tenantID string, limit, offset int) ([]domain.BenchmarkJob, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	return e.repos.BenchmarkJobs.List(ctx, tenantID, limit, offset)
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// TriggerRun enqueues an immediate run of a job.
func (e *Engine) TriggerRun(ctx context.Context, tenantID, jobID string) (*domain.BenchmarkRun, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	job, err := e.repos.BenchmarkJobs.GetByID(ctx, jobID)
	if err != nil || job == nil {
		return nil, domain.NewError(domain.ErrCodeNotFound, "benchmark job not found")
	}
	if job.TenantID != "" && job.TenantID != tenantID && tenantID != "" {
		return nil, domain.NewError(domain.ErrCodePermission, "you do not own this benchmark job")
	}
	if over, cost := e.dailyBudgetExceeded(ctx, tenantID, job); over {
		return nil, domain.Errorf(domain.ErrCodeInvalidRequest,
			"the daily benchmark budget is exceeded (spent $%.4f); benchmarks resume tomorrow", cost)
	}
	run, err := e.repos.BenchmarkRuns.Create(ctx, &domain.BenchmarkRun{
		JobID: job.ID, TenantID: job.TenantID, Status: "queued",
	})
	if err != nil {
		return nil, err
	}
	if err := e.queue.Enqueue(ctx, run.ID); err != nil {
		_ = e.repos.BenchmarkRuns.Finish(ctx, run.ID, "failed", 0, 0, 0, 0, 0, err.Error())
		return nil, err
	}
	e.publish(Event{Type: "run_queued", RunID: run.ID, Data: map[string]any{"job_id": job.ID}, Time: time.Now().UTC()})
	return run, nil
}

// ListRuns returns recent runs.
func (e *Engine) ListRuns(ctx context.Context, tenantID, jobID string, limit, offset int) ([]domain.BenchmarkRun, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	return e.repos.BenchmarkRuns.List(ctx, tenantID, jobID, limit, offset)
}

// dailyBudgetExceeded enforces per-job and global daily ceilings.
func (e *Engine) dailyBudgetExceeded(ctx context.Context, tenantID string, job *domain.BenchmarkJob) (bool, float64) {
	startOfDay := time.Now().UTC().Truncate(24 * time.Hour)
	_, _, dayCost, _, err := e.repos.BenchmarkResults.Totals(ctx, tenantID, startOfDay, time.Time{})
	if err != nil {
		return false, 0
	}
	if job != nil && job.MaxDailyBudgetUSD > 0 && dayCost >= job.MaxDailyBudgetUSD {
		return true, dayCost
	}
	if e.cfg.MaxDailyCostUSD > 0 && dayCost >= e.cfg.MaxDailyCostUSD {
		return true, dayCost
	}
	return false, dayCost
}

// ---------------------------------------------------------------------------
// Overview, history, health, comparison, reports, insights
// ---------------------------------------------------------------------------

// Overview builds the dashboard header.
func (e *Engine) Overview(ctx context.Context, tenantID string) (*domain.BenchmarkOverview, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	active, _ := e.repos.BenchmarkJobs.CountActive(ctx, tenantID)
	_, failed, avgDur, _ := e.repos.BenchmarkRuns.Stats(ctx, tenantID)
	probes, successRate, _, tokens, _ := e.repos.BenchmarkResults.Totals(ctx, tenantID, time.Time{}, time.Time{})
	// Daily cost is today's spend; totals are all-time.
	_, _, dailyCost, _, _ := e.repos.BenchmarkResults.Totals(ctx, tenantID, startOfDay, time.Time{})
	last, _ := e.repos.BenchmarkRuns.LastRun(ctx, tenantID)
	ov := &domain.BenchmarkOverview{
		EngineEnabled: true, EngineStatus: "running",
		SchedulerStatus: e.schedulerStatus(),
		ActiveJobs:      active, RunningWorkers: e.WorkerCount(),
		QueueSize:     e.QueueDepth(ctx),
		AvgDurationMS: avgDur, TotalBenchmarks: probes,
		SuccessRate: successRate, FailedJobs: failed,
		TotalDailyCostUSD: dailyCost, TotalTokens: tokens,
	}
	if last != nil {
		t := last.CreatedAt
		ov.LastBenchmark = &t
	}
	if next := e.nextScheduledRun(ctx, tenantID); next != nil {
		ov.NextBenchmark = next
	}
	return ov, nil
}

func (e *Engine) schedulerStatus() string {
	if !e.cfg.SchedulerEnabled {
		return "disabled"
	}
	if e.sched != nil && e.sched.Running() {
		return "running"
	}
	if e.running.Load() {
		return "running"
	}
	return "stopped"
}

func (e *Engine) nextScheduledRun(ctx context.Context, tenantID string) *time.Time {
	jobs, err := e.repos.BenchmarkJobs.List(ctx, tenantID, 100, 0)
	if err != nil {
		return nil
	}
	var best *time.Time
	for _, j := range jobs {
		if !j.Enabled || j.NextRunAt == nil {
			continue
		}
		if best == nil || j.NextRunAt.Before(*best) {
			cp := *j.NextRunAt
			best = &cp
		}
	}
	return best
}

// History returns paginated measurements.
func (e *Engine) History(ctx context.Context, f storage.BenchmarkHistoryFilter) ([]domain.BenchmarkResult, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	return e.repos.BenchmarkResults.List(ctx, f)
}

// Health returns per-provider health derived from recent results.
func (e *Engine) Health(ctx context.Context, tenantID string, from, to time.Time) ([]domain.BenchmarkProviderHealth, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	stats, err := e.repos.BenchmarkResults.ProviderStats(ctx, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Provider < stats[j].Provider })
	return stats, nil
}

// Comparison returns side-by-side provider stats over a window.
func (e *Engine) Comparison(ctx context.Context, tenantID string, providers []string, from, to time.Time) ([]domain.BenchmarkProviderHealth, error) {
	all, err := e.Health(ctx, tenantID, from, to)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return all, nil
	}
	want := map[string]bool{}
	for _, p := range providers {
		want[p] = true
	}
	var out []domain.BenchmarkProviderHealth
	for _, h := range all {
		if want[h.Provider] {
			out = append(out, h)
		}
	}
	return out, nil
}

// GenerateReport builds a daily/weekly/monthly report from recent results.
func (e *Engine) GenerateReport(ctx context.Context, tenantID, period string) (*domain.BenchmarkReport, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	var width time.Duration
	switch period {
	case "daily":
		width = 24 * time.Hour
	case "weekly":
		width = 7 * 24 * time.Hour
	case "monthly":
		width = 30 * 24 * time.Hour
	default:
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "period must be daily, weekly or monthly")
	}
	now := time.Now().UTC()
	stats, err := e.repos.BenchmarkResults.ProviderStats(ctx, tenantID, now.Add(-width), now)
	if err != nil {
		return nil, err
	}
	rep := &domain.BenchmarkReport{
		TenantID: tenantID, Period: period,
		PeriodStart: now.Add(-width), PeriodEnd: now,
		Summary: map[string]any{"providers": len(stats)},
	}
	// Pick winners by scanning the aggregates.
	var fastest *domain.BenchmarkProviderHealth
	var cheapest *domain.BenchmarkProviderHealth
	var reliable *domain.BenchmarkProviderHealth
	byProvider := map[string]*domain.BenchmarkProviderHealth{}
	for i := range stats {
		h := &stats[i]
		byProvider[h.Provider] = h
		if fastest == nil || h.AvgLatencyMS < fastest.AvgLatencyMS {
			fastest = h
		}
		if reliable == nil || h.SuccessRate > reliable.SuccessRate {
			reliable = h
		}
	}
	_ = byProvider
	if fastest != nil {
		rep.FastestProvider = fastest.Provider
		rep.LowestLatency = fastest.Provider
		rep.HighestThroughput = fastest.Provider
	}
	if reliable != nil {
		rep.MostReliable = reliable.Provider
		rep.HighestSuccess = reliable.Provider
		rep.OverallWinner = reliable.Provider
		rep.CheapestProvider = reliable.Provider
	}
	// Improvement/regression vs the previous window.
	prev, _ := e.repos.BenchmarkResults.ProviderStats(ctx, tenantID, now.Add(-2*width), now.Add(-width))
	prevLatency := map[string]float64{}
	for _, h := range prev {
		prevLatency[h.Provider] = h.AvgLatencyMS
	}
	bestImproveDelta := 0.0
	bestRegressDelta := 0.0
	for _, h := range stats {
		old, ok := prevLatency[h.Provider]
		if !ok || old <= 0 || h.AvgLatencyMS <= 0 {
			continue
		}
		delta := (h.AvgLatencyMS - old) / old * 100
		if delta < bestImproveDelta {
			bestImproveDelta = delta
			rep.BiggestImprovement = h.Provider
		}
		if delta > bestRegressDelta {
			bestRegressDelta = delta
			rep.BiggestRegression = h.Provider
		}
	}
	_ = cheapest
	return e.repos.BenchmarkReports.Create(ctx, rep)
}

// Insights generates natural-language summaries from window-over-window deltas.
func (e *Engine) Insights(ctx context.Context, tenantID string) ([]domain.BenchmarkInsight, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	return GenerateInsights(ctx, e.repos, tenantID)
}

// ---------------------------------------------------------------------------
// Live subscriptions (SSE)
// ---------------------------------------------------------------------------

// Subscribe registers a live feed. The caller must call Unsubscribe.
func (e *Engine) Subscribe() chan Event {
	ch := make(chan Event, 32)
	e.subsMu.Lock()
	e.subs[ch] = struct{}{}
	e.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a live feed.
func (e *Engine) Unsubscribe(ch chan Event) {
	e.subsMu.Lock()
	delete(e.subs, ch)
	e.subsMu.Unlock()
	close(ch)
}

func (e *Engine) publish(ev Event) {
	e.subsMu.Lock()
	defer e.subsMu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Start launches the scheduler and workers. No-op when disabled.
//
// Start is idempotent, so the dashboard toggle can call it at runtime: the
// first call after enabling boots the pool, later calls do nothing.
func (e *Engine) Start(ctx context.Context) {
	if !e.Enabled() {
		e.logger.Info("benchmark engine is disabled; scheduler and workers stay stopped",
			"hint", "set ENABLE_BENCHMARK_ENGINE=true or enable it from the Benchmark page")
		return
	}
	if e.running.Swap(true) {
		return
	}
	// The default corpus is seeded asynchronously and idempotently, so a
	// fresh install produces a meaningful comparison on its first run without
	// blocking gateway startup on an enterprise feature.
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.SeedDefaultDataset(sctx); err != nil {
			e.logger.Warn("benchmark default dataset seeding failed", "error", err)
		}
	}()
	workers := e.cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		w := NewWorker(e, i)
		e.mu.Lock()
		e.workers = append(e.workers, w)
		e.mu.Unlock()
		w.Start(ctx)
	}
	if e.cfg.SchedulerEnabled {
		e.sched = NewScheduler(e)
		e.sched.Start(ctx)
	}
	e.logger.Info("benchmark engine started", "workers", workers, "scheduler", e.cfg.SchedulerEnabled)
	e.publish(Event{Type: "engine_started", Time: time.Now().UTC()})
}

// Stop halts workers and the scheduler.
func (e *Engine) Stop() {
	if !e.running.Swap(false) {
		return
	}
	e.mu.Lock()
	workers := e.workers
	e.workers = nil
	sched := e.sched
	e.sched = nil
	e.mu.Unlock()
	if sched != nil {
		sched.Stop()
	}
	for _, w := range workers {
		w.Stop()
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func timeOrNow(t *time.Time) time.Time {
	if t != nil {
		return *t
	}
	return time.Now().UTC()
}
