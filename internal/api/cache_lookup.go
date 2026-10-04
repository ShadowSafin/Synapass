package api

import (
	"context"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/cache"
	"github.com/shadowsafin/synapass/internal/domain"
)

// cacheLookup is the Phase 5 request-flow cache check.
//
// It evaluates the policy-aware decision first (tenant/endpoint policy,
// streaming, sensitivity, tools, live-data, determinism), then performs the
// full-key lookup (tenant, key, model, content, tools, settings, contract,
// policy, endpoint, sensitivity). Endpoint scopes are namespaced in the key
// rather than bypassed, so scoped traffic caches safely per scope.
func (s *Server) cacheLookup(
	ctx context.Context,
	rc *domain.RequestContext,
	body *domain.ChatCompletionRequest,
	policy *domain.RoutingPolicy,
	toolCtx *toolContext,
) (*domain.CacheLookupResult, *domain.CacheDecision) {
	// Bypass accounting lives in exactly two places: the in-process
	// Cache.Stats (via RecordBypass, served by the dashboard) and the
	// Prometheus counters (via the recorder at outcome time). Nothing is
	// emitted here so a hit is not counted once at lookup and again at
	// record time.
	//
	// Streams are looked up like any other request: a hit replays as SSE
	// and a clean completion is stored. There is no streaming bypass.
	if s.cache == nil || !s.cache.Enabled() {
		dec := &domain.CacheDecision{Cacheable: false, BypassReason: domain.CacheBypassDisabled}
		rc.CacheDecision = dec
		if s.cache != nil {
			s.cache.RecordBypass(domain.CacheBypassDisabled)
		}
		return &domain.CacheLookupResult{Hit: false, BypassReason: domain.CacheBypassDisabled}, dec
	}

	sensitive, sensitivity := cache.SensitivityOf(rc.DataSensitivity)
	policyUseCache := true
	policyID := ""
	policyVersion := 0
	if rc.PolicyDecision != nil {
		policyUseCache = rc.PolicyDecision.UseCache
		policyID = rc.PolicyDecision.PolicyID
		policyVersion = rc.PolicyDecision.PolicyVersion
	} else if policy != nil {
		policyID = policy.ID
		policyVersion = policy.Version
	}
	var endpointCache *bool
	if rc.EndpointOverride != nil {
		endpointCache = rc.EndpointOverride.UseCache
	}

	toolsSafe := cache.ToolsSafeForCache(body.Tools, safeRegistryNames(toolCtx))
	hasImages := hasImageParts(body)

	n := rc.N
	if body.N != nil {
		n = *body.N
	}

	dec := s.cache.Evaluate(domain.CacheEvalInput{
		TenantID: rc.TenantID(), APIKeyID: rc.APIKeyID(),
		EndpointID: rc.EndpointID, Model: body.Model, Stream: rc.Stream,
		CacheBypass: rc.CacheBypass, Sensitive: sensitive, Sensitivity: sensitivity,
		HasTools: len(body.Tools) > 0, ToolsSafe: toolsSafe, HasImages: hasImages,
		N: n, Temperature: body.Temperature, Seed: body.Seed,
		PolicyUseCache: policyUseCache, PolicyID: policyID,
		EndpointCache: endpointCache, PromptText: body.PromptText(),
	})
	// Overlay the most specific durable cache rule, if any. Global config
	// stays the default; a matching row tightens or relaxes it per scope.
	var matched *domain.CachePolicy
	if s.repos != nil && s.repos.CachePolicies != nil {
		if m, err := s.repos.CachePolicies.MatchForRequest(ctx, rc.TenantID(), rc.EndpointID, rc.APIKeyID(), "", body.Model); err == nil {
			matched = m
		}
	}
	if matched != nil {
		applyCachePolicy(&dec, matched)
	}
	rc.CacheDecision = &dec
	if !dec.Cacheable {
		s.cache.RecordBypass(dec.BypassReason)
		return &domain.CacheLookupResult{Hit: false, BypassReason: dec.BypassReason}, &dec
	}

	// Tier allowances combine the global decision with the most specific
	// durable rule: a rule that disables a tier turns its entries into
	// misses before any store read, so the counters never record a hit
	// that was never served.
	allowExact, allowPrefix, allowSemantic := dec.AllowExact, dec.AllowPrefix, dec.AllowSemantic
	semThreshold := 0.0
	if matched != nil {
		allowSemantic = tierAllowed(domain.CacheSemantic, matched, dec)
		allowPrefix = tierAllowed(domain.CachePrefix, matched, dec)
		allowExact = tierAllowed(domain.CacheExact, matched, dec)
		semThreshold = matched.Threshold
	}
	lookup := s.cache.LookupFullWithTiers(ctx, rc.TenantID(), rc.APIKeyID(), body.Model,
		body, policyID, policyVersion, rc.EndpointID, sensitivity, body.User,
		allowExact, allowPrefix, allowSemantic, semThreshold)
	rc.CacheLookupMS = lookup.LookupMS
	lookup.Trace = &domain.CacheTrace{
		Hit: lookup.Hit, Kind: string(lookup.Kind), Key: lookup.Key,
		LookupMS: lookup.LookupMS, Similarity: lookup.Similarity,
		BypassReason: lookup.BypassReason, ReuseCount: lookup.ReuseCount,
		LatencySavedMS: lookup.LatencySavedMS,
	}
	return &lookup, &dec
}

// cacheStore stores a completed non-streaming response when the request's
// cache decision allows it. Serving metadata (provider, model, policy,
// endpoint, tools hash, usage, latency) travels in the envelope so future
// hits can validate that the world has not changed.
func (s *Server) cacheStore(
	ctx context.Context,
	rc *domain.RequestContext,
	body *domain.ChatCompletionRequest,
	payload []byte,
	provider, model, policyID string,
	policyVersion int,
	usage domain.TokenUsage,
	costUSD float64,
	providerLatencyMS int64,
) {
	if s.cache == nil || !s.cache.Enabled() {
		return
	}
	// No stream exclusion: a cleanly completed stream is a complete
	// response. storeCompletedStream already proved completion; the
	// decision and sensitivity gates below are the remaining guards.
	if rc.CacheBypass {
		return
	}
	dec := rc.CacheDecision
	if dec == nil || !dec.Cacheable {
		return
	}
	sensitive, sensitivityLabel := cache.SensitivityOf(rc.DataSensitivity)
	if sensitive {
		return
	}
	meta := domain.CacheHitMeta{
		Model: model, Provider: provider, PolicyID: policyID,
		PolicyVersion: policyVersion, EndpointID: rc.EndpointID,
		Sensitivity: sensitivityLabel, User: body.User,
		Usage: usage, CostUSD: costUSD, ProviderLatencyMS: providerLatencyMS,
	}
	if dec != nil && dec.TTLSeconds > 0 {
		meta.ExpiresAt = domain.Now().Add(time.Duration(dec.TTLSeconds) * time.Second)
	}
	key := s.cache.StoreFull(ctx, rc.TenantID(), rc.APIKeyID(), body.Model, body, payload, meta)
	if key != "" && s.repos != nil && s.repos.CacheEntries != nil {
		bctx, cancel := bookkeepingContext()
		defer cancel()
		_ = s.repos.CacheEntries.UpsertMeta(bctx, &domain.CacheEntry{
			TenantID: rc.TenantID(), Kind: domain.CacheExact, CacheKey: key,
			Model: model, Provider: provider, PolicyID: policyID,
			EndpointID: rc.EndpointID, APIKeyID: rc.APIKeyID(),
			PromptHash: cache.KeyInputFromRequest(rc.TenantID(), rc.APIKeyID(),
				body.Model, body, policyID, policyVersion, rc.EndpointID, "", body.User).PromptHash(),
			PromptPreview: cache.KeyInputFromRequest(rc.TenantID(), rc.APIKeyID(),
				body.Model, body, policyID, policyVersion, rc.EndpointID, "", body.User).PromptPreview(120),
			HitCount: 0,
		})
	}
}

// safeRegistryNames maps registry tool names to their safety verdict.
func safeRegistryNames(toolCtx *toolContext) map[string]bool {
	if toolCtx == nil || toolCtx.Registry == nil {
		return nil
	}
	out := map[string]bool{}
	for _, t := range toolCtx.Registry.All() {
		// Only deterministic read-only built-ins are safe to reuse.
		// "now" is read-only but time-dependent: an answer built on the
		// current time goes stale immediately, so it is never safe.
		safe := t.Executable && t.SafetyLevel == domain.SafetySafe && !strings.EqualFold(t.Name, "now")
		out[t.Name] = safe
	}
	return out
}

func hasImageParts(body *domain.ChatCompletionRequest) bool {
	if body == nil {
		return false
	}
	for _, m := range body.Messages {
		if m.Content.HasImages() {
			return true
		}
	}
	return false
}

// applyCachePolicy overlays a durable per-scope rule onto the global
// decision. A disabled rule bypasses; TTL and tier flags narrow or widen
// the global defaults without ever widening tenant isolation.
func applyCachePolicy(dec *domain.CacheDecision, matched *domain.CachePolicy) {
	if dec == nil || matched == nil {
		return
	}
	if !matched.Enabled {
		dec.Cacheable = false
		dec.BypassReason = domain.CacheBypassPolicyDisabled
		dec.Scope = cachePolicyScope(matched)
		return
	}
	if matched.TTLSeconds > 0 {
		dec.TTLSeconds = matched.TTLSeconds
	}
	if matched.Semantic != nil {
		dec.AllowSemantic = *matched.Semantic
	}
	if matched.Prefix != nil {
		dec.AllowPrefix = *matched.Prefix
	}
	if matched.BypassTools != nil && !*matched.BypassTools {
		// An explicit opt-in to tool reuse: only still-safe requests benefit,
		// the Evaluate verdict for unsafe tools stands.
	}
	if matched.AllowNonDet != nil && *matched.AllowNonDet {
		if dec.BypassReason == domain.CacheBypassNondeterministic {
			dec.Cacheable = true
			dec.BypassReason = ""
		}
	}
	dec.Scope = cachePolicyScope(matched)
}

// tierAllowed reports whether a hit kind survives the rule's tier flags.
func tierAllowed(kind domain.CacheKind, matched *domain.CachePolicy, dec domain.CacheDecision) bool {
	switch kind {
	case domain.CacheSemantic:
		if matched.Semantic != nil {
			return *matched.Semantic
		}
		return dec.AllowSemantic
	case domain.CachePrefix:
		if matched.Prefix != nil {
			return *matched.Prefix
		}
		return dec.AllowPrefix
	default:
		return dec.AllowExact
	}
}
