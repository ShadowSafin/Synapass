package bootstrap

import (
	"context"
	"time"

	"github.com/shadowsafin/synapass/internal/cache"
	"github.com/shadowsafin/synapass/internal/classifier"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/guardrails"
	"github.com/shadowsafin/synapass/internal/policy"
	"github.com/shadowsafin/synapass/internal/scoring"
	"github.com/shadowsafin/synapass/internal/shaping"
	"github.com/shadowsafin/synapass/internal/storage"
)

// Phase2Services bundles intelligence services.
type Phase2Services struct {
	Classifier   *classifier.Classifier
	PolicyEngine *policy.Engine
	Shaper       *shaping.Pipeline
	Cache        *cache.Cache
	Scorer       *scoring.Engine
	ScoreAdapter *ScoreAdapter
	Guardrails   *guardrails.Controller
	ReplayStore  *ReplayAdapter
}

// BuildPhase2 constructs Phase 2 services from config.
func BuildPhase2(cfg *config.Config, redis *storage.Redis) *Phase2Services {
	svc := &Phase2Services{}
	if cfg.Classifier.Enabled {
		svc.Classifier = classifier.New(classifier.Options{
			LongContextTokens: cfg.Classifier.LongContextTokens,
			BatchTokens:       cfg.Classifier.BatchTokens,
		})
	} else {
		svc.Classifier = classifier.New(classifier.DefaultOptions())
	}
	svc.PolicyEngine = policy.NewEngine()
	if cfg.Shaping.Enabled {
		svc.Shaper = shaping.New(shaping.Options{
			Enabled:            true,
			MaxHistoryMessages: cfg.Shaping.MaxHistoryMessages,
			MaxPromptTokens:    cfg.Shaping.MaxPromptTokens,
			SystemPrefix:       cfg.Shaping.SystemPrefix,
			SystemSuffix:       cfg.Shaping.SystemSuffix,
			Guardrails:         cfg.Shaping.Guardrails,
		})
	} else {
		svc.Shaper = shaping.New(shaping.DefaultOptions())
	}
	// Cache: enabled when response cache is on or semantic/prefix enabled.
	// Exact reuse additionally requires ExactEnabled; the master switch stays
	// the kill-switch so operators can disable serving without losing config.
	cacheEnabled := cfg.Cache.ResponseCache || cfg.Cache.SemanticEnabled || cfg.Cache.PrefixEnabled
	threshold := cfg.Cache.SemanticThreshold
	if threshold <= 0 {
		threshold = 0.92
	}
	svc.Cache = cache.New(redisCacheStore{redis: redis}, cache.Options{
		Enabled:               cacheEnabled,
		ExactEnabled:          cfg.Cache.ResponseCache && cfg.Cache.ExactEnabled,
		SemanticEnabled:       cfg.Cache.SemanticEnabled,
		PrefixEnabled:         cfg.Cache.PrefixEnabled,
		TTL:                   cfg.Cache.ResponseTTL.Std(),
		MaxResponseBytes:      cfg.Cache.MaxResponseBytes,
		SemanticThreshold:     threshold,
		PrefixLength:          cfg.Cache.PrefixLength,
		MaxSemanticEntries:    cfg.Cache.MaxSemanticEntries,
		BypassTools:           cfg.Cache.CacheBypassTools(),
		AllowNondeterministic: cfg.Cache.AllowNondeterministic,
		BypassLiveData:        cfg.Cache.CacheBypassLive(),
	})
	if !cfg.Scoring.Enabled {
		// Still construct; engine with no observations simply yields no scores.
	}
	svc.Scorer = scoring.New(scoring.Weights{
		Success:  cfg.Scoring.SuccessWeight,
		Latency:  cfg.Scoring.LatencyWeight,
		Cost:     cfg.Scoring.CostWeight,
		Feedback: cfg.Scoring.FeedbackWeight,
	})
	svc.ScoreAdapter = &ScoreAdapter{engine: svc.Scorer, window: cfg.Scoring.Window.Std()}
	if svc.ScoreAdapter.window <= 0 {
		svc.ScoreAdapter.window = time.Hour
	}
	if cfg.Guardrails.Enabled {
		svc.Guardrails = guardrails.New()
	} else {
		svc.Guardrails = guardrails.New()
	}
	return svc
}

// redisCacheStore adapts *storage.Redis to cache.Store.
type redisCacheStore struct {
	redis *storage.Redis
}

func (r redisCacheStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if r.redis == nil {
		return nil, false, nil
	}
	return r.redis.GetResponseCache(ctx, key)
}
func (r redisCacheStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if r.redis == nil {
		return nil
	}
	return r.redis.SetResponseCache(ctx, key, value, ttl)
}
func (r redisCacheStore) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if r.redis == nil {
		return 0, nil
	}
	// Map cache prefixes to the response namespace. Bare kind prefixes and
	// tenant namespaces all live under "response:".
	switch prefix {
	case "exact:", "prefix:", "semantic:", "tenant:":
		return r.redis.DeletePrefix(ctx, "response:"+prefix)
	default:
		if len(prefix) >= 9 && prefix[:9] == "response:" {
			return r.redis.DeletePrefix(ctx, prefix)
		}
		return r.redis.DeletePrefix(ctx, prefix)
	}
}

// ScoreAdapter exposes scoring.Engine as routing.ScoreProvider + api.ScoringService.
type ScoreAdapter struct {
	engine *scoring.Engine
	window time.Duration
}

// Score implements routing.ScoreProvider.
func (s *ScoreAdapter) Score(providerName string) (float64, bool) {
	if s == nil || s.engine == nil {
		return 0, false
	}
	for _, ps := range s.engine.ProviderScores(s.window) {
		key := ps.ProviderName
		if key == "" {
			key = ps.ProviderID
		}
		if key == providerName {
			return ps.Score, true
		}
	}
	return 0, false
}

// ProviderScores implements api.ScoringService.
func (s *ScoreAdapter) ProviderScores() []domain.ProviderScore {
	if s == nil || s.engine == nil {
		return nil
	}
	return s.engine.ProviderScores(s.window)
}

// ModelScores implements api.ScoringService.
func (s *ScoreAdapter) ModelScores() []domain.ModelScore {
	if s == nil || s.engine == nil {
		return nil
	}
	return s.engine.ModelScores(s.window)
}

// RecordOutcome implements api.ScoringService.
func (s *ScoreAdapter) RecordOutcome(provider, model string, task domain.TaskType, success bool, latencyMS int64, costUSD float64, fallback bool, errCode domain.ErrorCode) {
	if s == nil || s.engine == nil {
		return
	}
	s.engine.Record(scoring.Observation{
		Provider: provider, Model: model, Task: task, Success: success,
		Timeout:   errCode == domain.ErrCodeTimeout,
		Refusal:   errCode == domain.ErrCodeContentFiltered,
		LatencyMS: latencyMS, CostUSD: costUSD, FallbackUsed: fallback,
		At: time.Now().UTC(),
	})
}

// ClassifierAdapter adapts *classifier.Classifier to api.ClassifierService
// (method set already matches; this type documents the seam).
type ClassifierAdapter struct {
	Inner *classifier.Classifier
}

// Classify implements api.ClassifierService.
func (a *ClassifierAdapter) Classify(req *domain.ChatCompletionRequest, promptTokens int) domain.TaskClassification {
	if a == nil || a.Inner == nil {
		return domain.TaskClassification{Task: domain.TaskChat, Source: "none"}
	}
	return a.Inner.Classify(req, promptTokens)
}

// PolicyEngineAdapter adapts *policy.Engine to api.PolicyEngineService.
type PolicyEngineAdapter struct {
	Inner *policy.Engine
}

// Evaluate implements api.PolicyEngineService.
func (a *PolicyEngineAdapter) Evaluate(ctx context.Context, pol *domain.RoutingPolicy, rc *domain.RequestContext) *domain.PolicyDecision {
	if a == nil || a.Inner == nil {
		return &domain.PolicyDecision{Allowed: true, UseCache: true}
	}
	var task domain.TaskClassification
	var endpoint *domain.Endpoint
	if rc != nil {
		task = rc.Task
	}
	return a.Inner.Evaluate(ctx, policy.EvaluateInput{Policy: pol, Request: rc, Task: task, Endpoint: endpoint})
}

// ShapingAdapter adapts *shaping.Pipeline to api.ShapingService.
type ShapingAdapter struct {
	Inner *shaping.Pipeline
}

// PlanFor implements api.ShapingService.
func (a *ShapingAdapter) PlanFor(task domain.TaskClassification, pol *domain.RoutingPolicy, dec *domain.PolicyDecision) domain.PromptShapePlan {
	if a == nil || a.Inner == nil {
		return domain.PromptShapePlan{}
	}
	return a.Inner.PlanFor(task, pol, dec)
}

// Apply implements api.ShapingService.
func (a *ShapingAdapter) Apply(req *domain.ChatCompletionRequest, plan domain.PromptShapePlan) (*domain.ChatCompletionRequest, domain.PromptShape) {
	if a == nil || a.Inner == nil {
		return req, domain.PromptShape{}
	}
	return a.Inner.Apply(req, plan)
}

// CacheAdapter adapts *cache.Cache to api.ResponseCacheService.
type CacheAdapter struct {
	Inner *cache.Cache
}

// Enabled implements api.ResponseCacheService.
func (a *CacheAdapter) Enabled() bool {
	return a != nil && a.Inner != nil && a.Inner.Enabled()
}

// Lookup implements api.ResponseCacheService (legacy minimal key).
func (a *CacheAdapter) Lookup(ctx context.Context, tenant, model string, req *domain.ChatCompletionRequest, bypass bool, bypassReason string, sensitive bool) domain.CacheLookupResult {
	if a == nil || a.Inner == nil {
		return domain.CacheLookupResult{Hit: false, BypassReason: "cache_disabled"}
	}
	return a.Inner.Lookup(ctx, fullKeyInput(tenant, "", model, req, "", 0, "", "", ""), bypass, bypassReason, sensitive)
}

// Store implements api.ResponseCacheService (legacy minimal key).
func (a *CacheAdapter) Store(ctx context.Context, tenant, model string, req *domain.ChatCompletionRequest, body []byte, sensitive bool) string {
	if a == nil || a.Inner == nil {
		return ""
	}
	return a.Inner.StoreResponse(ctx, fullKeyInput(tenant, "", model, req, "", 0, "", "", ""), body, sensitive)
}

// LookupFull implements api.ResponseCacheService with the full Phase 5 key.
func (a *CacheAdapter) LookupFull(ctx context.Context, tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, policyID string, policyVersion int, endpointID, sensitivity, user string) domain.CacheLookupResult {
	return a.LookupFullWithTiers(ctx, tenantID, apiKeyID, model, req, policyID, policyVersion, endpointID, sensitivity, user, true, true, true, 0)
}

// LookupFullWithTiers implements api.ResponseCacheService with per-request
// tier enforcement.
func (a *CacheAdapter) LookupFullWithTiers(ctx context.Context, tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, policyID string, policyVersion int, endpointID, sensitivity, user string, allowExact, allowPrefix, allowSemantic bool, semThreshold float64) domain.CacheLookupResult {
	if a == nil || a.Inner == nil {
		return domain.CacheLookupResult{Hit: false, BypassReason: "cache_disabled"}
	}
	return a.Inner.LookupWithTiers(ctx, fullKeyInput(tenantID, apiKeyID, model, req, policyID, policyVersion, endpointID, sensitivity, user), false, "", false, allowExact, allowPrefix, allowSemantic, semThreshold)
}

// StoreFull implements api.ResponseCacheService with serving metadata.
func (a *CacheAdapter) StoreFull(ctx context.Context, tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, body []byte, meta domain.CacheHitMeta) string {
	if a == nil || a.Inner == nil {
		return ""
	}
	// Sensitivity and user scope the exact key, so they must match the
	// lookup path exactly: LookupFull builds its key from the request's
	// live sensitivity/user, and the store must use the same values or
	// scoped traffic would never hit (and scopes would blur).
	return a.Inner.StoreResponseWithMeta(ctx, fullKeyInput(tenantID, apiKeyID, model, req, meta.PolicyID, meta.PolicyVersion, meta.EndpointID, meta.Sensitivity, meta.User), body, false, meta)
}

// Evaluate implements api.ResponseCacheService policy decisions.
func (a *CacheAdapter) Evaluate(in domain.CacheEvalInput) domain.CacheDecision {
	if a == nil || a.Inner == nil {
		return domain.CacheDecision{Cacheable: false, BypassReason: domain.CacheBypassDisabled}
	}
	opts := a.Inner.Options()
	return cache.Evaluate(cache.PolicyInput{
		TenantID: in.TenantID, EndpointID: in.EndpointID, Model: in.Model,
		Stream: in.Stream, CacheBypass: in.CacheBypass, Sensitive: in.Sensitive,
		HasTools: in.HasTools, ToolsSafe: in.ToolsSafe, HasImages: in.HasImages,
		N: in.N, Temperature: in.Temperature, Seed: in.Seed,
		PolicyUseCache: in.PolicyUseCache, PolicyID: in.PolicyID,
		EndpointCache: in.EndpointCache,
		Request: cache.KeyInput{Messages: messagePreview(in.PromptText)},
	}, cache.PolicyConfig{
		Enabled: opts.Enabled, ExactEnabled: opts.ExactEnabled,
		SemanticEnabled: opts.SemanticEnabled, PrefixEnabled: opts.PrefixEnabled,
		TTL: opts.TTL, SemanticThreshold: opts.SemanticThreshold,
		BypassTools: opts.BypassTools, AllowNondeterministic: opts.AllowNondeterministic,
		BypassLiveData: opts.BypassLiveData,
	})
}

// InvalidateScope implements api.ResponseCacheService scoped flushes.
func (a *CacheAdapter) InvalidateScope(ctx context.Context, scope, tenantID, model, provider, key, reason string) (int, error) {
	if a == nil || a.Inner == nil {
		return 0, nil
	}
	return a.Inner.Invalidate(ctx, cache.InvalidateScope{
		Scope: scope, TenantID: tenantID, Model: model, Provider: provider, Key: key, Reason: reason,
	})
}

func fullKeyInput(tenantID, apiKeyID, model string, req *domain.ChatCompletionRequest, policyID string, policyVersion int, endpointID, sensitivity, user string) cache.KeyInput {
	if req == nil {
		return cache.KeyInput{TenantID: tenantID, APIKeyID: apiKeyID, Model: model}
	}
	return cache.KeyInputFromRequest(tenantID, apiKeyID, model, req, policyID, policyVersion, endpointID, sensitivity, user)
}

func messagePreview(text string) []domain.ChatMessage {
	if text == "" {
		return nil
	}
	return []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(text)}}
}

// Stats implements api.ResponseCacheService.
func (a *CacheAdapter) Stats() domain.CacheStats {
	if a == nil || a.Inner == nil {
		return domain.CacheStats{}
	}
	return a.Inner.Stats()
}

// RecordBypass implements api.ResponseCacheService.
func (a *CacheAdapter) RecordBypass(reason string) {
	if a == nil || a.Inner == nil {
		return
	}
	a.Inner.RecordBypass(reason)
}

// InvalidateTenant implements api.ResponseCacheService.
func (a *CacheAdapter) InvalidateTenant(ctx context.Context, tenantID string) (int, error) {
	if a == nil || a.Inner == nil {
		return 0, nil
	}
	return a.Inner.InvalidateTenant(ctx, tenantID)
}

// ReplayAdapter adapts storage repos to api.ReplayService.
type ReplayAdapter struct {
	Replay   *storage.ReplayRepository
	Feedback *storage.FeedbackRepository
}

// CreateReplayJob implements api.ReplayService.
func (a *ReplayAdapter) CreateReplayJob(ctx context.Context, job *domain.ReplayJob) (*domain.ReplayJob, error) {
	if a == nil || a.Replay == nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "replay store is unavailable")
	}
	if job.ID == "" {
		job.ID = domain.NewID()
	}
	if job.Status == "" {
		job.Status = "queued"
	}
	job.CreatedAt = domain.Now()
	job.UpdatedAt = domain.Now()
	job.Total = len(job.RequestIDs)
	if err := a.Replay.CreateJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// GetReplayJob implements api.ReplayService.
func (a *ReplayAdapter) GetReplayJob(ctx context.Context, id string) (*domain.ReplayJob, error) {
	if a == nil || a.Replay == nil {
		return nil, nil
	}
	return a.Replay.GetJob(ctx, id)
}

// ListReplayJobs implements api.ReplayService.
func (a *ReplayAdapter) ListReplayJobs(ctx context.Context, tenantID string, limit int) ([]domain.ReplayJob, error) {
	if a == nil || a.Replay == nil {
		return nil, nil
	}
	return a.Replay.ListJobs(ctx, tenantID, limit)
}

// ListEvalRuns implements api.ReplayService.
func (a *ReplayAdapter) ListEvalRuns(ctx context.Context, tenantID string, limit int) ([]domain.EvaluationRun, error) {
	if a == nil || a.Replay == nil {
		return nil, nil
	}
	return a.Replay.ListRuns(ctx, tenantID, limit)
}

// GetEvalRun implements api.ReplayService.
func (a *ReplayAdapter) GetEvalRun(ctx context.Context, id string) (*domain.EvaluationRun, error) {
	if a == nil || a.Replay == nil {
		return nil, nil
	}
	return a.Replay.GetRun(ctx, id)
}

// ListEvalResults implements api.ReplayService.
func (a *ReplayAdapter) ListEvalResults(ctx context.Context, evaluationID string, limit int) ([]domain.EvaluationResult, error) {
	if a == nil || a.Replay == nil {
		return nil, nil
	}
	return a.Replay.ListResults(ctx, evaluationID, limit)
}

// SubmitFeedback implements api.ReplayService.
func (a *ReplayAdapter) SubmitFeedback(ctx context.Context, e *domain.FeedbackEvent) error {
	if a == nil || a.Feedback == nil {
		return domain.NewError(domain.ErrCodeInternal, "feedback store is unavailable")
	}
	return a.Feedback.Insert(ctx, e)
}
