package replay

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// executionTimeout bounds one provider call during replay. Replays are
// background work; a hung provider must stall only its own result, never the
// job.
const executionTimeout = 120 * time.Second

// defaultReplayTokens is the completion allowance when the captured request
// did not pin one. maxReplayTokens caps re-execution spend: replay is
// comparison traffic, and no offline comparison should mint a novel-length
// bill.
const (
	defaultReplayTokens = 1024
	maxReplayTokens     = 4096
)

// PayloadSource reads captured prompts.
type PayloadSource interface {
	Get(ctx context.Context, requestID string) (*domain.RequestPayload, error)
}

// LogLookup reads captured request metadata. It is narrower than
// RequestSource (which also lists) so the concrete log repository satisfies
// it without adaptation; the runner looks up one id at a time and never
// lists.
type LogLookup interface {
	GetByRequestID(ctx context.Context, requestID string) (*domain.RequestLog, error)
}

// AdapterResolver resolves a provider name or id to a live adapter, so
// replay executes through the same provider code as production traffic.
type AdapterResolver interface {
	Resolve(idOrName string) (providers.Adapter, bool)
}

// Pricer costs replayed usage. The gateway wires the versioned-sheet
// resolution live traffic uses (tenant > model > provider > global,
// registry fallback); tests wire a stub. Nil prices everything zero.
type Pricer func(ctx context.Context, tenantID, providerName, modelName string, usage domain.TokenUsage) float64

// Runner executes replay jobs in the gateway process.
//
// Jobs are created through the admin API and run asynchronously in a
// background goroutine (the API layer detaches the request context and owns
// panic recovery). Execution calls provider adapters directly rather than
// looping back through HTTP: replay traffic must not bill, rate-limit, or
// policy-route like production traffic, and it must not require a tenant key.
// Every successful output is scored with the deterministic eval scorers, and a
// completion event is published best-effort for workers and the dashboard.
type Runner struct {
	payloads PayloadSource
	logs     LogLookup
	store    Store
	adapters AdapterResolver
	pricer   Pricer
	scorer   Scorer
	refs     ReferenceSource
	events   Publisher
	logger   *slog.Logger
}

// RunnerDeps wires a Runner. Logs and Store are required; adapters may be nil
// only in tests that never reach execution. Scorer defaults to the eval
// engine, References defaults to no golden reference, and Events defaults to
// no publish; all three are nil-safe.
type RunnerDeps struct {
	Payloads   PayloadSource
	Logs       LogLookup
	Store      Store
	Adapters   AdapterResolver
	Pricer     Pricer
	Scorer     Scorer
	References ReferenceSource
	Events     Publisher
	Logger     *slog.Logger
}

// NewRunner creates a Runner.
func NewRunner(deps RunnerDeps) *Runner {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		payloads: deps.Payloads,
		logs:     deps.Logs,
		store:    deps.Store,
		adapters: deps.Adapters,
		pricer:   deps.Pricer,
		scorer:   deps.Scorer,
		refs:     deps.References,
		events:   deps.Events,
		logger:   logger,
	}
}

// Run executes a job to completion, recording one evaluation result per
// request per target and updating job progress as it goes. It returns the
// evaluation run; a job whose every execution failed is marked failed,
// partial success is marked completed with the first error on the run.
func (r *Runner) Run(ctx context.Context, jobID string) (*domain.EvaluationRun, error) {
	if r.store == nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "replay store is unavailable")
	}
	job, err := r.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, domain.NewError(domain.ErrCodeNotFound, "replay job not found")
	}

	ids := job.RequestIDs
	max := job.MaxRequests
	if max <= 0 {
		max = 100
	}
	if max > 1000 {
		max = 1000
	}
	if len(ids) > max {
		ids = ids[:max]
	}

	run := &domain.EvaluationRun{
		ID: domain.NewID(), TenantID: job.TenantID, ReplayJobID: job.ID,
		Dataset: job.Dataset, Status: "running", CreatedAt: domain.Now(),
		CreatedBy: job.CreatedBy,
	}
	if err := r.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	markJob := func(status, jobErr string) {
		job.Status = status
		job.Error = jobErr
		job.UpdatedAt = domain.Now()
		if status == "completed" || status == "failed" {
			now := domain.Now()
			job.FinishedAt = &now
		}
		if uerr := r.store.UpdateJob(ctx, job); uerr != nil {
			r.logger.Warn("failed to update replay job", "job_id", job.ID, "error", uerr)
		}
	}
	markJob("running", "")

	// Total counts executions (requests x targets), not requests, so the
	// progress bar ends at 100% instead of past it when targets multiply.
	total := 0
	results := 0
	var firstErr error
	targetsByID := make(map[string][]domain.RouteTarget, len(ids))
	for _, id := range ids {
		entry, lerr := r.logs.GetByRequestID(ctx, id)
		if lerr != nil || entry == nil {
			r.logger.Warn("replay request log missing; recording as failed",
				"job_id", job.ID, "request_id", id)
			total++
			if firstErr == nil {
				firstErr = domain.Errorf(domain.ErrCodeNotFound, "request %q was not found", id)
			}
			if r.addErrorResult(ctx, run.ID, id, "", "", "request log not found; it may have been pruned") {
				results++
			}
			continue
		}
		targets := executionTargets(job, entry)
		if len(targets) == 0 {
			total++
			if firstErr == nil {
				firstErr = domain.Errorf(domain.ErrCodeInvalidRequest,
					"request %q has no replay target: it never reached a provider and the job pins none", id)
			}
			if r.addErrorResult(ctx, run.ID, id, "", "", "no replay target: the request never reached a provider and the job pins none") {
				results++
			}
			continue
		}
		targetsByID[id] = targets
		total += len(targets)
	}
	job.Total = total
	job.Progress = results
	markJob("running", "")

	succeeded := 0
	for _, id := range ids {
		targets, ok := targetsByID[id]
		if !ok {
			continue
		}
		payload, perr := r.payloads.Get(ctx, id)
		if perr != nil {
			r.logger.Warn("replay payload lookup failed; recording as failed",
				"job_id", job.ID, "request_id", id, "error", perr)
		}
		reference := lookupReference(ctx, r.refs, job.Dataset, id)
		for _, tgt := range targets {
			res := r.execute(ctx, run.ID, job.TenantID, id, payload, reference, tgt)
			if res.execErr != nil {
				if firstErr == nil {
					firstErr = res.execErr
				}
			} else {
				succeeded++
			}
			if rerr := r.store.AddResult(ctx, res.result); rerr != nil {
				r.logger.Warn("failed to store a replay result",
					"job_id", job.ID, "request_id", id, "error", rerr)
			} else {
				results++
			}
			job.Progress = results
			markJob("running", "")
		}
	}

	// Error rows are results for progress purposes but not success: a job
	// that produced no provider output failed, even though every request is
	// accounted for.
	now := domain.Now()
	run.FinishedAt = &now
	if succeeded == 0 {
		run.Status = "failed"
		if firstErr != nil {
			run.Error = firstErr.Error()
		} else if total == 0 {
			run.Error = "the job selected no requests"
		} else {
			run.Error = "no execution produced output"
		}
	} else {
		run.Status = "completed"
		if firstErr != nil {
			run.Error = firstErr.Error()
		}
	}
	if uerr := r.store.UpdateRun(ctx, run); uerr != nil {
		r.logger.Warn("failed to update evaluation run", "run_id", run.ID, "error", uerr)
	}
	if run.Status == "failed" {
		markJob("failed", run.Error)
	} else {
		markJob("completed", "")
	}
	publishCompletion(r.events, job, run, succeeded)
	return run, nil
}

// executionTargets resolves what one captured request re-executes against:
// the job's provider/model cross product when pinned, otherwise the original
// serving target so "paste an id" replays exactly what served it.
func executionTargets(job *domain.ReplayJob, entry *domain.RequestLog) []domain.RouteTarget {
	if len(job.Providers) > 0 || len(job.Models) > 0 {
		out := []domain.RouteTarget{}
		providers := job.Providers
		if len(providers) == 0 {
			providers = []string{entry.Provider}
		}
		models := job.Models
		if len(models) == 0 {
			models = []string{replayModel(entry)}
		}
		for _, p := range providers {
			if p == "" {
				continue
			}
			for _, m := range models {
				if m == "" {
					continue
				}
				out = append(out, domain.RouteTarget{ProviderName: p, Model: m})
			}
		}
		return out
	}
	if entry.Provider == "" {
		return nil
	}
	return []domain.RouteTarget{{ProviderName: entry.Provider, Model: replayModel(entry)}}
}

// replayModel prefers the model the client asked for over the one that
// served: aliases resolve at replay time exactly as they do live.
func replayModel(entry *domain.RequestLog) string {
	if entry.RequestedModel != "" {
		return entry.RequestedModel
	}
	return entry.RoutedModel
}

// execution is one recorded provider call.
type execution struct {
	result  *domain.EvaluationResult
	execErr error
}

// execute replays one prompt against one target through the live adapter,
// scoring the output against the request's golden reference (empty when none
// is known). Executions that fail before producing output are stored unscored:
// scoring an error string would manufacture a quality measurement out of a
// zero.
func (r *Runner) execute(ctx context.Context, evaluationID, tenantID, id string, payload *domain.RequestPayload, reference string, tgt domain.RouteTarget) execution {
	fail := func(err error, preview string) execution {
		if preview == "" {
			preview = "error: " + err.Error()
		}
		return execution{
			result: &domain.EvaluationResult{
				ID: domain.NewID(), EvaluationID: evaluationID, RequestID: id,
				Provider: tgt.ProviderName, Model: tgt.Model,
				OutputPreview: truncate(preview, 500),
				CreatedAt:     domain.Now(),
			},
			execErr: err,
		}
	}
	if payload == nil || len(payload.Messages) == 0 {
		return fail(fmt.Errorf("no captured prompt for request %q: it predates prompt capture; send new traffic and retry", id), "")
	}
	if r.adapters == nil {
		return fail(fmt.Errorf("no provider adapters are configured"), "")
	}
	adapter, ok := r.adapters.Resolve(tgt.ProviderName)
	if !ok || adapter == nil {
		return fail(fmt.Errorf("unknown provider %q", tgt.ProviderName), "")
	}
	if !adapter.Enabled() {
		return fail(fmt.Errorf("provider %q is disabled", tgt.ProviderName), "")
	}

	maxTokens := payload.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = defaultReplayTokens
	}
	if maxTokens > maxReplayTokens {
		maxTokens = maxReplayTokens
	}
	params := &domain.ChatCompletionRequest{
		Model:     tgt.Model,
		Messages:  payload.Messages,
		MaxTokens: &maxTokens,
	}
	req := &providers.Request{
		Model:     tgt.Model,
		Params:    params,
		Ref:       domain.ModelRef{ProviderName: tgt.ProviderName, Model: tgt.Model},
		MaxTokens: maxTokens,
	}

	step, cancel := context.WithTimeout(ctx, executionTimeout)
	defer cancel()
	start := time.Now()
	resp, err := adapter.ChatCompletion(step, req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return fail(err, "")
	}
	if resp == nil {
		return fail(fmt.Errorf("provider %q returned no response", tgt.ProviderName), "")
	}
	usage := resp.Usage.Normalize()
	var costUSD float64
	if r.pricer != nil {
		costUSD = r.pricer(ctx, tenantID, tgt.ProviderName, tgt.Model, usage)
	}
	content := resp.Content()
	score, metrics := scoreOutput(ctx, r.scorer, r.logger, reference, content,
		map[string]float64{
			"prompt_tokens":     float64(usage.PromptTokens),
			"completion_tokens": float64(usage.CompletionTokens),
			"total_tokens":      float64(usage.TotalTokens),
		})
	res := &domain.EvaluationResult{
		ID: domain.NewID(), EvaluationID: evaluationID, RequestID: id,
		Provider: tgt.ProviderName, Model: tgt.Model,
		Score:         score,
		LatencyMS:     latency,
		CostUSD:       costUSD,
		OutputPreview: truncate(content, 500),
		Metrics:       metrics,
		CreatedAt:     domain.Now(),
	}
	return execution{result: res}
}

// addErrorResult records a request the runner could not execute at all, so
// progress and totals account for it instead of silently dropping it. It
// reports whether the row was stored, so progress counts only real rows.
func (r *Runner) addErrorResult(ctx context.Context, evaluationID, id, provider, model, preview string) bool {
	res := &domain.EvaluationResult{
		ID: domain.NewID(), EvaluationID: evaluationID, RequestID: id,
		Provider: provider, Model: model,
		OutputPreview: truncate("error: "+preview, 500),
		CreatedAt:     domain.Now(),
	}
	if err := r.store.AddResult(ctx, res); err != nil {
		r.logger.Warn("failed to store a replay error result",
			"evaluation_id", evaluationID, "request_id", id, "error", err)
		return false
	}
	return true
}
