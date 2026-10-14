package benchmark

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Scheduler ticks due jobs into the queue.
//
// It polls for due jobs on a short interval (never applying backpressure to
// the gateway), enqueues one run per due job, and stamps last/next run times.
// A distributed lock is unnecessary: runs are idempotent queue entries and
// workers deduplicate by run ID.
type Scheduler struct {
	engine  *Engine
	stop    chan struct{}
	stopped chan struct{}
	running atomic.Bool
}

// NewScheduler builds the scheduler.
func NewScheduler(engine *Engine) *Scheduler {
	return &Scheduler{engine: engine, stop: make(chan struct{}), stopped: make(chan struct{})}
}

// Running reports whether the loop is active.
func (s *Scheduler) Running() bool { return s.running.Load() }

// Start begins the tick loop.
func (s *Scheduler) Start(ctx context.Context) {
	if s.running.Swap(true) {
		return
	}
	go s.loop(ctx)
}

// Stop halts the loop.
func (s *Scheduler) Stop() {
	if !s.running.Swap(false) {
		return
	}
	close(s.stop)
	<-s.stopped
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.stopped)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	// Run once immediately so an enabled engine shows activity without
	// waiting a full tick.
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	e := s.engine
	if e == nil || !e.Enabled() || !e.cfg.SchedulerEnabled {
		return
	}
	if e.repos == nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	due, err := e.repos.BenchmarkJobs.ListDue(tctx, time.Now().UTC(), 50)
	if err != nil {
		e.logger.Warn("benchmark scheduler tick failed", "error", err)
		return
	}
	for i := range due {
		job := &due[i]
		// Budget protection: pause automatically, resume next day.
		if over, _ := e.dailyBudgetExceeded(tctx, job.TenantID, job); over {
			e.logger.Warn("benchmark job paused: daily budget exceeded; resumes tomorrow",
				"job", job.Name)
			next := nextDayStart(time.Now().UTC())
			_ = e.repos.BenchmarkJobs.MarkRan(tctx, job.ID, timeOrNow(job.LastRunAt), next)
			e.notify(tctx, job.TenantID, "budget_exceeded", map[string]any{"job": job.Name})
			continue
		}
		run, cerr := e.repos.BenchmarkRuns.Create(tctx, newRunForJob(job))
		if cerr != nil {
			e.logger.Warn("benchmark scheduler could not create run", "job", job.Name, "error", cerr)
			continue
		}
		if err := e.queue.Enqueue(tctx, run.ID); err != nil {
			_ = e.repos.BenchmarkRuns.Finish(tctx, run.ID, "failed", 0, 0, 0, 0, 0, err.Error())
			continue
		}
		now := time.Now().UTC()
		next := NextRunAfter(job.Schedule, job.CronExpression, now)
		_ = e.repos.BenchmarkJobs.MarkRan(tctx, job.ID, now, next)
		e.publish(Event{Type: "run_queued", RunID: run.ID, Data: map[string]any{"job_id": job.ID}, Time: now})
	}
}

func nextDayStart(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// newRunForJob builds the queued run row for a job.
func newRunForJob(job *domain.BenchmarkJob) *domain.BenchmarkRun {
	return &domain.BenchmarkRun{JobID: job.ID, TenantID: job.TenantID, Status: "queued"}
}
