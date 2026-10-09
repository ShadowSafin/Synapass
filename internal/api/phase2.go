package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/cost"
	"github.com/shadowsafin/synapass/internal/domain"
)

// priceReplayUsage costs one replayed execution through the versioned-sheet
// resolution live traffic uses, degrading to registry rates exactly as the
// request path does. Wired into the replay runner at server construction.
func (s *Server) priceReplayUsage(ctx context.Context, tenantID, providerName, modelName string, usage domain.TokenUsage) float64 {
	price := s.resolvePrice(ctx, tenantID, providerName, modelName,
		domain.RouteTarget{ProviderName: providerName, Model: modelName})
	bd := cost.Compute(usage, price, []cost.AttemptUsage{{Label: "replay", Usage: usage}})
	return bd.TotalUSD
}

// Phase 2 service contracts. Defined here (consumer side) so the api package
// does not import classifier/shaping/cache/scoring/guardrails/replay
// implementations directly; wiring adapters satisfy these.

// ClassifierService infers task types.
type ClassifierService interface {
	Classify(req *domain.ChatCompletionRequest, promptTokens int) domain.TaskClassification
}

// PolicyEngineService evaluates fine-grained policy decisions.
type PolicyEngineService interface {
	Evaluate(ctx context.Context, policy *domain.RoutingPolicy, rc *domain.RequestContext) *domain.PolicyDecision
}

// ShapingService plans and applies prompt transformations.
type ShapingService interface {
	PlanFor(task domain.TaskClassification, policy *domain.RoutingPolicy, decision *domain.PolicyDecision) domain.PromptShapePlan
	Apply(req *domain.ChatCompletionRequest, plan domain.PromptShapePlan) (*domain.ChatCompletionRequest, domain.PromptShape)
}

// ResponseCacheService is the exact/prefix/semantic cache.
//
// Lookup/Store are the legacy minimal-key entry points, kept for
// compatibility. New code uses LookupFull/StoreFull with the full key input
// (tenant, key, model, tools, settings, contract, policy, endpoint,
// sensitivity) plus Evaluate for the policy-aware bypass decision.
type ResponseCacheService interface {
	Enabled() bool
	Lookup(ctx context.Context, tenant, model string, req *domain.ChatCompletionRequest, bypass bool, bypassReason string, sensitive bool) domain.CacheLookupResult
	Store(ctx context.Context, tenant, model string, req *domain.ChatCompletionRequest, body []byte, sensitive bool) string
	LookupFull(ctx context.Context, tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, policyID string, policyVersion int, endpointID, sensitivity, user string) domain.CacheLookupResult
	// LookupFullWithTiers is LookupFull with per-request tier enforcement
	// (scoped policy overrides) applied before any store read, so a
	// tier-disabled entry is a miss rather than a downgraded hit.
	LookupFullWithTiers(ctx context.Context, tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, policyID string, policyVersion int, endpointID, sensitivity, user string, allowExact, allowPrefix, allowSemantic bool, semThreshold float64) domain.CacheLookupResult
	StoreFull(ctx context.Context, tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, body []byte, meta domain.CacheHitMeta) string
	Evaluate(in domain.CacheEvalInput) domain.CacheDecision
	Stats() domain.CacheStats
	// RecordBypass records a bypass decided outside Lookup (streaming,
	// policy-disabled, cache-disabled) so the in-process dashboard
	// counters agree with the request flow.
	RecordBypass(reason string)
	InvalidateTenant(ctx context.Context, tenantID string) (int, error)
	InvalidateScope(ctx context.Context, scope, tenantID, model, provider, key, reason string) (int, error)
}

// ScoringService exposes provider/model scores.
type ScoringService interface {
	ProviderScores() []domain.ProviderScore
	ModelScores() []domain.ModelScore
	RecordOutcome(provider, model string, task domain.TaskType, success bool, latencyMS int64, costUSD float64, fallback bool, errCode domain.ErrorCode)
}

// GuardrailService enforces production controls.
type GuardrailService interface {
	CheckProvider(providerID, providerName string) error
	CheckTenant(tenantID string) error
	Killed(provider string) (bool, string)
	Kill(provider, reason string)
	Revive(provider string)
	BlockFallback(scope, reason string)
	UnblockFallback(scope string)
	FallbackBlocked(scope string) (bool, string)
	SetHardCap(tenantID string, maxUSD float64)
	HardCap(tenantID string) (float64, bool)
	EffectiveCap(tenantID string, policyCeiling float64) float64
}

// ReplayService manages replay/eval jobs.
type ReplayService interface {
	CreateReplayJob(ctx context.Context, job *domain.ReplayJob) (*domain.ReplayJob, error)
	GetReplayJob(ctx context.Context, id string) (*domain.ReplayJob, error)
	ListReplayJobs(ctx context.Context, tenantID string, limit int) ([]domain.ReplayJob, error)
	ListEvalRuns(ctx context.Context, tenantID string, limit int) ([]domain.EvaluationRun, error)
	GetEvalRun(ctx context.Context, id string) (*domain.EvaluationRun, error)
	ListEvalResults(ctx context.Context, evaluationID string, limit int) ([]domain.EvaluationResult, error)
	SubmitFeedback(ctx context.Context, e *domain.FeedbackEvent) error
}

// decodeStrict decodes JSON without DisallowUnknownFields, for admin payloads
// that may carry forward-compatible fields.
func decodeLenient(r *http.Request, limit int64, target any) error {
	if r.Body == nil {
		return domain.NewError(domain.ErrCodeInvalidRequest, "a request body is required")
	}
	if limit > 0 {
		r.Body = http.MaxBytesReader(nil, r.Body, limit)
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(target); err != nil {
		return domain.Errorf(domain.ErrCodeInvalidRequest, "failed to parse the request body as JSON: %v", err)
	}
	return nil
}

// --- Routing explanation ---

// handleAdminExplain serves GET /admin/v1/requests/{requestID}/explain.
func (s *Server) handleAdminExplain(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Logs == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the request log store is unavailable"), metaFromContext(rc, nil))
		return
	}
	requestID := chiURLParam(r, "requestID")
	entry, err := s.repos.Logs.GetByRequestID(ctx, requestID)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if entry == nil {
		writeError(w, domain.Errorf(domain.ErrCodeNotFound, "request %q was not found", requestID), metaFromContext(rc, nil))
		return
	}
	expl := domain.RoutingExplanation{
		RequestID: requestID,
		Reason:    "reconstructed from stored decision",
	}
	if entry.Decision != nil {
		expl.Decision = entry.Decision
		expl.Task = entry.Decision.Task
		expl.Shaping = entry.Decision.Shaping
		expl.Reason = entry.Decision.Reason
		expl.Rejected = entry.Decision.Rejected
	}
	writeJSON(w, http.StatusOK, expl)
}

// --- Scores ---

func (s *Server) handleAdminProviderScores(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	_ = ctx
	if s.scorer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"providers": []domain.ProviderScore{}, "models": []domain.ModelScore{}})
		return
	}
	_ = rc
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": s.scorer.ProviderScores(),
		"models":    s.scorer.ModelScores(),
	})
}

// --- Cache ---

func (s *Server) handleAdminCacheStats(w http.ResponseWriter, r *http.Request) {
	rc := requestContext(r.Context())
	if s.cache == nil || !s.cache.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "stats": domain.CacheStats{},
			"platform": s.PlatformCacheHealth(r.Context()),
		})
		return
	}
	_ = rc
	stats := s.cache.Stats()
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	tenantID := r.URL.Query().Get("tenant_id")
	if s.repos != nil && s.repos.CacheEntries != nil {
		if top, err := s.repos.CacheEntries.TopPrompts(ctx, tenantID, 10); err == nil {
			for _, e := range top {
				hits := int64(e.HitCount) + e.ReuseCount
				stats.TopPrompts = append(stats.TopPrompts, domain.CacheTopPrompt{
					PromptHash: e.PromptHash, PromptPreview: e.PromptPreview,
					Model: e.Model, Hits: hits,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"stats":   stats,
		"config":  s.config.Cache,
		"platform": s.PlatformCacheHealth(r.Context()),
	})
}

func (s *Server) handleAdminCacheInvalidate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	var body struct {
		TenantID string `json:"tenant_id"`
		Scope    string `json:"scope"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Key      string `json:"key"`
		Reason   string `json:"reason"`
	}
	_ = decodeLenient(r, 1<<16, &body)
	scope := body.Scope
	if scope == "" {
		scope = domain.CacheScopeTenant
		if body.TenantID == "" && body.Model == "" && body.Provider == "" && body.Key == "" {
			scope = domain.CacheScopeAll
		}
	}
	if body.Key != "" {
		scope = domain.CacheScopeKey
	} else if body.Model != "" {
		scope = domain.CacheScopeModel
	} else if body.Provider != "" {
		scope = domain.CacheScopeProvider
	}
	reason := body.Reason
	if reason == "" {
		reason = domain.CacheInvalidateManual
	}
	removed := 0
	if s.cache != nil {
		n, err := s.cache.InvalidateScope(ctx, scope, body.TenantID, body.Model, body.Provider, body.Key, reason)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		removed = n
	}
	// Drive the platform cache from the same manual flush (tenant/provider/
	// model/all scopes). Key scope stays response-cache-only: key material
	// is never platform-cached. Provider/model targets double as the
	// platform provider id / catalog generation, so pass them through.
	platTarget := body.Provider
	if body.Model != "" {
		platTarget = body.Model
	}
	s.platFlushBestEffort(ctx, scope, body.TenantID, platTarget)
	if s.repos != nil && s.repos.CacheEntries != nil {
		if n, err := s.repos.CacheEntries.DeleteScope(ctx, body.TenantID, body.Model, body.Provider, body.Key); err == nil {
			_ = n
		}
	}
	target := body.TenantID
	if body.Model != "" {
		target = body.Model
	}
	if body.Provider != "" {
		target = body.Provider
	}
	if body.Key != "" {
		target = body.Key
	}
	if s.repos != nil && s.repos.CacheInvalidations != nil {
		actor := ""
		if p := principal(ctx); p != nil {
			actor = p.Label()
		}
		_ = s.repos.CacheInvalidations.Record(ctx, &domain.CacheInvalidationEvent{
			TenantID: body.TenantID, Scope: scope, Target: target,
			Reason: reason, Actor: actor, Removed: removed,
		})
	}
	if s.metrics != nil {
		s.metrics.ObserveCacheInvalidation(scope, reason)
	}
	if s.nats != nil {
		actor := ""
		if p := principal(ctx); p != nil {
			actor = p.Label()
		}
		event := &domain.CacheEvent{
			Event: "invalidated", Scope: scope, Target: target, Reason: reason,
			TenantID: body.TenantID, Actor: actor, Removed: removed,
			CreatedAt: domain.Now(),
		}
		if rc != nil {
			event.RequestID = rc.RequestID.String()
		}
		s.nats.PublishCacheInvalidated(event)
	}
	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceCache, target,
		nil, map[string]any{"invalidated": removed, "scope": scope, "reason": reason})
	writeJSON(w, http.StatusOK, map[string]any{"invalidated": removed, "scope": scope})
}

// --- Replay / eval ---

func (s *Server) handleAdminCreateReplay(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.replaySvc == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "replay is not configured"), metaFromContext(rc, nil))
		return
	}
	var job domain.ReplayJob
	if err := decodeLenient(r, 1<<20, &job); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if p := principal(ctx); p != nil {
		job.CreatedBy = p.Label()
	}
	// Fail fast on unreplayable input: a queued job that can never run is
	// worse than a 400 that says why, and the dashboard surfaces this text
	// next to the form.
	if err := s.validateReplayJob(ctx, &job); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	created, err := s.replaySvc.CreateReplayJob(ctx, &job)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if s.metrics != nil && s.metrics.ReplayJobsTotal != nil {
		s.metrics.ReplayJobsTotal.WithLabelValues("created").Inc()
	}
	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceReplayJob, created.ID, nil, map[string]any{"name": created.Name})
	s.startReplayJob(created.ID)
	writeJSON(w, http.StatusCreated, created)
}

// validateReplayJob normalizes a replay request and proves every id can
// actually run: the request must exist in the logs and its prompt must have
// been captured. Traffic that predates prompt capture fails here with the
// recovery step, not later as a silent empty job.
func (s *Server) validateReplayJob(ctx context.Context, job *domain.ReplayJob) error {
	cleaned := make([]string, 0, len(job.RequestIDs))
	seen := map[string]bool{}
	for _, id := range job.RequestIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
	}
	job.RequestIDs = cleaned
	if len(job.RequestIDs) == 0 {
		if strings.TrimSpace(job.Dataset) != "" {
			return domain.NewError(domain.ErrCodeInvalidRequest,
				"dataset replay is not supported yet: pass request_ids")
		}
		return domain.NewError(domain.ErrCodeInvalidRequest, "request_ids is required")
	}
	if job.MaxRequests <= 0 {
		job.MaxRequests = 100
	}
	if job.MaxRequests > 1000 {
		job.MaxRequests = 1000
	}
	if s.repos == nil || s.repos.Logs == nil {
		return domain.NewError(domain.ErrCodeInternal, "the request log store is unavailable")
	}
	if s.repos.Payloads == nil {
		return domain.NewError(domain.ErrCodeInternal, "prompt capture is unavailable")
	}
	for _, id := range job.RequestIDs {
		entry, err := s.repos.Logs.GetByRequestID(ctx, id)
		if err != nil {
			return err
		}
		if entry == nil {
			return domain.Errorf(domain.ErrCodeNotFound, "request %q was not found", id)
		}
		payload, err := s.repos.Payloads.Get(ctx, id)
		if err != nil {
			return err
		}
		if payload == nil || len(payload.Messages) == 0 {
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"request %q has no captured prompt: it predates prompt capture; send new traffic and retry", id)
		}
	}
	return nil
}

// startReplayJob runs a created job in the background. The context is
// detached from the HTTP request so a closed dashboard tab cannot cancel an
// offline job; per-execution timeouts still bound every provider call.
func (s *Server) startReplayJob(jobID string) {
	if s.replayRunner == nil {
		return
	}
	observe := func(status string) {
		if s.metrics != nil && s.metrics.ReplayJobsTotal != nil {
			s.metrics.ReplayJobsTotal.WithLabelValues(status).Inc()
		}
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("replay job panicked", "job_id", jobID, "panic", rec)
				observe("failed")
				if s.repos != nil && s.repos.Replay != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if job, _ := s.repos.Replay.GetJob(ctx, jobID); job != nil {
						now := domain.Now()
						job.Status = "failed"
						job.Error = "the replay worker panicked; check the gateway log"
						job.FinishedAt = &now
						job.UpdatedAt = now
						_ = s.repos.Replay.UpdateJob(ctx, job)
					}
				}
			}
		}()
		run, err := s.replayRunner.Run(context.Background(), jobID)
		if err != nil {
			s.logger.Error("replay job failed", "job_id", jobID, "error", err)
			observe("failed")
			return
		}
		observe(run.Status)
	}()
}

func (s *Server) handleAdminListReplay(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.replaySvc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": []domain.ReplayJob{}})
		return
	}
	jobs, err := s.replaySvc.ListReplayJobs(ctx, r.URL.Query().Get("tenant_id"), parseIntParam(r, "limit", 50))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleAdminGetReplay(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.replaySvc == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "replay job not found"), metaFromContext(rc, nil))
		return
	}
	job, err := s.replaySvc.GetReplayJob(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if job == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "replay job not found"), metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleAdminListEvals(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.replaySvc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []domain.EvaluationRun{}})
		return
	}
	runs, err := s.replaySvc.ListEvalRuns(ctx, r.URL.Query().Get("tenant_id"), parseIntParam(r, "limit", 50))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleAdminGetEval(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.replaySvc == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "evaluation not found"), metaFromContext(rc, nil))
		return
	}
	id := chiURLParam(r, "id")
	run, err := s.replaySvc.GetEvalRun(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if run == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "evaluation not found"), metaFromContext(rc, nil))
		return
	}
	results, err := s.replaySvc.ListEvalResults(ctx, id, parseIntParam(r, "limit", 100))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "results": results})
}

// --- Provider health overrides / kill switches ---

func (s *Server) handleAdminKillProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.guard == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "guardrails are not configured"), metaFromContext(rc, nil))
		return
	}
	id := chiURLParam(r, "id")
	var body struct {
		Reason string `json:"reason"`
		Kill   *bool  `json:"kill"`
	}
	_ = decodeLenient(r, 1<<16, &body)
	kill := true
	if body.Kill != nil {
		kill = *body.Kill
	}
	if kill {
		reason := body.Reason
		if reason == "" {
			reason = "operator kill switch"
		}
		s.guard.Kill(id, reason)
		if s.metrics != nil {
			s.metrics.ObserveGuardrailBlock("kill_switch")
		}
	} else {
		s.guard.Revive(id)
	}
	var actor string
	if p := principal(ctx); p != nil {
		actor = p.Label()
	}
	if s.repos != nil && s.repos.Overrides != nil {
		_ = s.repos.Overrides.Create(ctx, &domain.AuditOverride{
			Kind: "kill_switch", Target: id, Enabled: kill, Reason: body.Reason, Actor: actor,
		})
	}
	s.audit(ctx, rc, domain.AuditOverridden, domain.ResourceOverride, id,
		nil, map[string]any{"kill": kill, "reason": body.Reason})
	writeJSON(w, http.StatusOK, map[string]any{"provider": id, "killed": kill})
}

func (s *Server) handleAdminGuardrails(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	_ = ctx
	killed := map[string]string{}
	if s.guard != nil {
		// Expose via provider list; here return empty + per-provider checks are
		// done by the dashboard against /providers with health.
		_ = killed
	}
	overrides := []domain.AuditOverride{}
	if s.repos != nil && s.repos.Overrides != nil {
		if list, err := s.repos.Overrides.List(ctx, 50); err == nil {
			overrides = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": overrides})
}

// --- Tenant budget management ---

func (s *Server) handleAdminUpdateTenantBudget(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.Budgets == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the budget store is unavailable"), metaFromContext(rc, nil))
		return
	}
	var body struct {
		TenantID   string  `json:"tenant_id"`
		Period     string  `json:"period"`
		LimitUSD   float64 `json:"limit_usd"`
		Enforced   bool    `json:"enforced"`
		HardCapUSD float64 `json:"hard_cap_usd,omitempty"`
	}
	if err := decodeLenient(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.TenantID == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "'tenant_id' is required"), metaFromContext(rc, nil))
		return
	}
	period := domain.BudgetPeriod(body.Period)
	if period == "" {
		period = domain.BudgetMonthly
	}
	budget := &domain.Budget{
		ID: domain.NewID(), TenantID: body.TenantID, Scope: "tenant",
		Period: period, LimitUSD: body.LimitUSD, Enforced: body.Enforced,
		CreatedAt: domain.Now(), UpdatedAt: domain.Now(),
	}
	// Upsert via the budgets repository; hard caps flow to guardrails.
	if s.guard != nil && body.HardCapUSD > 0 {
		s.guard.SetHardCap(body.TenantID, body.HardCapUSD)
	}
	saved, err := s.repos.Budgets.Upsert(ctx, budget)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceBudget, saved.ID,
		nil, map[string]any{"tenant_id": body.TenantID, "limit_usd": body.LimitUSD})
	writeJSON(w, http.StatusOK, saved)
}

// --- Feedback ---

func (s *Server) handleSubmitFeedback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	var body struct {
		RequestID string  `json:"request_id"`
		Score     float64 `json:"score"`
		Comment   string  `json:"comment"`
	}
	if err := decodeLenient(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.RequestID == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "'request_id' is required"), metaFromContext(rc, nil))
		return
	}
	event := &domain.FeedbackEvent{
		RequestID: domain.RequestID(body.RequestID), Score: body.Score, Comment: body.Comment,
	}
	if rc != nil {
		event.TenantID = rc.TenantID()
	}
	if s.replaySvc != nil {
		if err := s.replaySvc.SubmitFeedback(ctx, event); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
	} else if s.repos != nil && s.repos.Feedback != nil {
		if err := s.repos.Feedback.Insert(ctx, event); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
	}
	if s.metrics != nil && s.metrics.FeedbackTotal != nil {
		tenant := ""
		if rc != nil {
			tenant = rc.TenantID()
		}
		s.metrics.FeedbackTotal.WithLabelValues(tenant).Inc()
	}
	writeJSON(w, http.StatusCreated, event)
}

// --- Endpoints ---

func (s *Server) handleAdminListEndpoints(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.Endpoints == nil {
		writeJSON(w, http.StatusOK, map[string]any{"endpoints": []domain.Endpoint{}})
		return
	}
	endpoints, err := s.repos.Endpoints.List(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": endpoints})
}

func (s *Server) handleAdminUpsertEndpoint(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.Endpoints == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the endpoint store is unavailable"), metaFromContext(rc, nil))
		return
	}
	var ep domain.Endpoint
	if err := decodeLenient(r, 1<<20, &ep); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if ep.Slug == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "'slug' is required"), metaFromContext(rc, nil))
		return
	}
	saved, err := s.repos.Endpoints.Upsert(ctx, &ep)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceEndpoint, saved.ID, nil, map[string]any{"slug": saved.Slug})
	s.cacheFlushBestEffort(ctx, domain.CacheScopeTenant, saved.TenantID, saved.Slug, domain.CacheInvalidateEndpointChange, actorLabel(ctx))
	writeJSON(w, http.StatusOK, saved)
}
