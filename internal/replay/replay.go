// Package replay re-executes captured traffic safely against multiple
// providers for offline comparison.
//
// Replay never touches live traffic: it reads request logs, rebuilds the
// original prompt preferring the captured payload messages, executes through
// the in-process Runner against the live provider adapters, scores every
// successful output with the deterministic eval scorers, and publishes a
// completion event for workers and the dashboard to observe.
package replay

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// RequestSource reads captured requests.
type RequestSource interface {
	GetByRequestID(ctx context.Context, requestID string) (*domain.RequestLog, error)
	List(ctx context.Context, filter ListFilter) ([]domain.RequestLog, error)
}

// ListFilter scopes a replay selection.
type ListFilter struct {
	TenantID string
	From     time.Time
	To       time.Time
	Limit    int
}

// Executor runs one prompt against one target (provider adapter in prod,
// stub in tests).
type Executor func(ctx context.Context, prompt domain.ChatCompletionRequest, target domain.RouteTarget) (output string, latencyMS int64, costUSD float64, err error)

// Store persists jobs and results.
type Store interface {
	CreateJob(ctx context.Context, job *domain.ReplayJob) error
	UpdateJob(ctx context.Context, job *domain.ReplayJob) error
	GetJob(ctx context.Context, id string) (*domain.ReplayJob, error)
	ListJobs(ctx context.Context, tenantID string, limit int) ([]domain.ReplayJob, error)
	CreateRun(ctx context.Context, run *domain.EvaluationRun) error
	UpdateRun(ctx context.Context, run *domain.EvaluationRun) error
	AddResult(ctx context.Context, res *domain.EvaluationResult) error
	ListResults(ctx context.Context, evaluationID string, limit int) ([]domain.EvaluationResult, error)
}

// Service orchestrates replay jobs. It is the legacy synchronous path (a stub
// Executor, retained for tests and embedders); production traffic runs
// through Runner, which executes against the live provider adapters.
type Service struct {
	requests RequestSource
	exec     Executor
	store    Store
	payloads PayloadSource
	scorer   Scorer
	refs     ReferenceSource
	events   Publisher
	logger   *slog.Logger
}

// ServiceDeps wires a Service. Only Requests, Exec and Store are required;
// Payloads, Scorer, References and Events are optional and degrade gracefully
// (stub fallback, default eval scorer, empty reference, no publish).
type ServiceDeps struct {
	Requests   RequestSource
	Exec       Executor
	Store      Store
	Payloads   PayloadSource
	Scorer     Scorer
	References ReferenceSource
	Events     Publisher
	Logger     *slog.Logger
}

// New creates a service.
func New(requests RequestSource, exec Executor, store Store) *Service {
	return NewWithDeps(ServiceDeps{Requests: requests, Exec: exec, Store: store})
}

// NewWithDeps creates a service with scoring, payload and event wiring.
func NewWithDeps(deps ServiceDeps) *Service {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		requests: deps.Requests, exec: deps.Exec, store: deps.Store,
		payloads: deps.Payloads, scorer: deps.Scorer, refs: deps.References,
		events: deps.Events, logger: logger,
	}
}

// CreateJob validates and persists a job.
func (s *Service) CreateJob(ctx context.Context, job *domain.ReplayJob) (*domain.ReplayJob, error) {
	if job == nil {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "'job' is required")
	}
	if job.ID == "" {
		job.ID = domain.NewID()
	}
	if len(job.RequestIDs) == 0 && job.Dataset == "" {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "either 'request_ids' or 'dataset' is required")
	}
	if job.MaxRequests <= 0 {
		job.MaxRequests = 100
	}
	if job.MaxRequests > 1000 {
		job.MaxRequests = 1000
	}
	job.Status = "queued"
	now := domain.Now()
	job.CreatedAt = now
	job.UpdatedAt = now
	job.Total = len(job.RequestIDs)
	if s.store != nil {
		if err := s.store.CreateJob(ctx, job); err != nil {
			return nil, err
		}
	}
	return job, nil
}

// Run executes a job synchronously, scoring every successful output with the
// deterministic eval scorers and publishing a completion event. Failed
// executions are stored unscored: scoring an error string against a reference
// would produce a number that looks like a quality measurement and is really
// a zero.
func (s *Service) Run(ctx context.Context, jobID string) (*domain.EvaluationRun, error) {
	if s.store == nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "replay store is unavailable")
	}
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, domain.NewError(domain.ErrCodeNotFound, "replay job not found")
	}
	run := &domain.EvaluationRun{
		ID: domain.NewID(), TenantID: job.TenantID, ReplayJobID: job.ID,
		Dataset: job.Dataset, Status: "running", CreatedAt: domain.Now(),
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	job.Status = "running"
	job.UpdatedAt = domain.Now()
	_ = s.store.UpdateJob(ctx, job)

	ids := job.RequestIDs
	if len(ids) > job.MaxRequests && job.MaxRequests > 0 {
		ids = ids[:job.MaxRequests]
	}
	targets := buildTargets(job)

	var mu sync.Mutex
	results := 0
	succeeded := 0
	var firstErr error

	for _, id := range ids {
		entry, err := s.requests.GetByRequestID(ctx, id)
		if err != nil || entry == nil {
			continue
		}
		var payload *domain.RequestPayload
		if s.payloads != nil {
			// A lookup failure degrades to the stub reconstruction below,
			// never to a dropped request.
			payload, _ = s.payloads.Get(ctx, id)
		}
		req := rebuildRequest(entry, payload)
		reference := lookupReference(ctx, s.refs, job.Dataset, id)
		for _, tgt := range targets {
			output, lat, cost, execErr := s.exec(ctx, req, tgt)
			res := &domain.EvaluationResult{
				ID: domain.NewID(), EvaluationID: run.ID, RequestID: id,
				Provider: tgt.ProviderName, Model: tgt.Model,
				LatencyMS: lat, CostUSD: cost,
				OutputPreview: truncate(output, 500),
				CreatedAt:     domain.Now(),
			}
			if execErr != nil {
				res.OutputPreview = "error: " + execErr.Error()
				mu.Lock()
				if firstErr == nil {
					firstErr = execErr
				}
				mu.Unlock()
			} else {
				res.Score, res.Metrics = scoreOutput(ctx, s.scorer, s.logger, reference, output, nil)
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
			_ = s.store.AddResult(ctx, res)
			mu.Lock()
			results++
			mu.Unlock()
		}
		job.Progress = results
		job.UpdatedAt = domain.Now()
		_ = s.store.UpdateJob(ctx, job)
	}

	now := domain.Now()
	run.FinishedAt = &now
	if firstErr != nil && results == 0 {
		run.Status = "failed"
		run.Error = firstErr.Error()
	} else {
		run.Status = "completed"
	}
	_ = s.store.UpdateRun(ctx, run)
	job.Status = run.Status
	job.FinishedAt = run.FinishedAt
	job.UpdatedAt = domain.Now()
	_ = s.store.UpdateJob(ctx, job)
	publishCompletion(s.events, job, run, succeeded)
	return run, nil
}

func buildTargets(job *domain.ReplayJob) []domain.RouteTarget {
	out := []domain.RouteTarget{}
	for _, p := range job.Providers {
		model := ""
		if len(job.Models) > 0 {
			model = job.Models[0]
		}
		out = append(out, domain.RouteTarget{ProviderName: p, Model: model})
	}
	if len(out) == 0 {
		for _, m := range job.Models {
			out = append(out, domain.RouteTarget{Model: m})
		}
	}
	if len(out) == 0 {
		return []domain.RouteTarget{{ProviderName: "openai", Model: "gpt-4o-mini"}}
	}
	return out
}

func rebuildRequest(entry *domain.RequestLog, payload *domain.RequestPayload) domain.ChatCompletionRequest {
	// The captured payload carries the real prompt when available; prefer it
	// so replay re-executes what actually served. Fall back to a minimal
	// reconstruction so replay never fails for lack of detail.
	if payload != nil && len(payload.Messages) > 0 {
		model := entry.RequestedModel
		if model == "" {
			model = payload.Model
		}
		return domain.ChatCompletionRequest{Model: model, Messages: payload.Messages}
	}
	return domain.ChatCompletionRequest{
		Model:    entry.RequestedModel,
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("[replay:" + entry.RequestID + "]")}},
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("…")
}
