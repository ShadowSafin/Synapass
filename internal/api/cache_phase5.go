package api

import (
	"context"
	"net/http"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// cacheFlushBestEffort invalidates a scope after a catalogue change without
// ever failing the admin operation that triggered it. Every flush is
// audited, metered and published so the dashboard and the event stream agree
// about what happened.
func (s *Server) cacheFlushBestEffort(ctx context.Context, scope, tenantID, target, reason, actor string) {
	removed := 0
	if s.cache != nil {
		if n, err := s.cache.InvalidateScope(ctx, scope, tenantID, targetIf(scope == domain.CacheScopeModel, target), targetIf(scope == domain.CacheScopeProvider, target), targetIf(scope == domain.CacheScopeKey, target), reason); err == nil {
			removed = n
		} else {
			s.logger.Warn("cache flush failed", "scope", scope, "target", target, "error", err)
		}
	}
	// Mirror the flush into the platform cache (tenant/catalog/route/flags)
	// so both layers converge on the same write. Nil-safe and best-effort.
	s.platFlushBestEffort(ctx, scope, tenantID, target)
	if s.repos != nil && s.repos.CacheEntries != nil {
		if scope == domain.CacheScopeModel {
			_, _ = s.repos.CacheEntries.DeleteScope(ctx, tenantID, target, "", "")
		} else if scope == domain.CacheScopeProvider {
			_, _ = s.repos.CacheEntries.DeleteScope(ctx, tenantID, "", target, "")
		} else if scope == domain.CacheScopeTenant {
			_, _ = s.repos.CacheEntries.DeleteScope(ctx, tenantID, "", "", "")
		}
	}
	if s.repos != nil && s.repos.CacheInvalidations != nil {
		_ = s.repos.CacheInvalidations.Record(ctx, &domain.CacheInvalidationEvent{
			TenantID: tenantID, Scope: scope, Target: target,
			Reason: reason, Actor: actor, Removed: removed,
		})
	}
	if s.metrics != nil {
		s.metrics.ObserveCacheInvalidation(scope, reason)
	}
	if s.nats != nil {
		s.nats.PublishCacheInvalidated(&domain.CacheEvent{
			Event: "invalidated", Scope: scope, Target: target, Reason: reason,
			TenantID: tenantID, Actor: actor, Removed: removed,
			CreatedAt: domain.Now(),
		})
	}
}

func targetIf(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}



// cachePolicyScope maps a rule to the flush scope its change affects.
func cachePolicyScope(p *domain.CachePolicy) string {
	if p == nil {
		return domain.CacheScopeAll
	}
	if p.Model != "" {
		return domain.CacheScopeModel
	}
	if p.Provider != "" {
		return domain.CacheScopeProvider
	}
	if p.TenantID != "" || p.EndpointID != "" || p.APIKeyID != "" {
		return domain.CacheScopeTenant
	}
	return domain.CacheScopeAll
}

// --- Cache inspect ---

func (s *Server) handleAdminCacheInspect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.CacheEntries == nil {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []domain.CacheEntry{}})
		return
	}
	q := r.URL.Query()
	entries, err := s.repos.CacheEntries.Inspect(ctx,
		q.Get("tenant_id"), q.Get("model"), q.Get("provider"),
		parseIntParam(r, "limit", 25))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// --- Cache policies ---

func (s *Server) handleAdminListCachePolicies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.CachePolicies == nil {
		writeJSON(w, http.StatusOK, map[string]any{"policies": []domain.CachePolicy{}})
		return
	}
	policies, err := s.repos.CachePolicies.List(ctx, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

func (s *Server) handleAdminUpsertCachePolicy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.CachePolicies == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the cache policy store is unavailable"), metaFromContext(rc, nil))
		return
	}
	var p domain.CachePolicy
	if err := decodeLenient(r, 1<<20, &p); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if p.Name == "" {
		p.Name = "cache-policy"
	}
	saved, err := s.repos.CachePolicies.Upsert(ctx, &p)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceCache, saved.ID,
		nil, map[string]any{"policy": saved.Name, "enabled": saved.Enabled})
	// A rule change must not serve answers cached under the old rule.
	actor := ""
	if pr := principal(ctx); pr != nil {
		actor = pr.Label()
	}
	s.cacheFlushBestEffort(ctx, cachePolicyScope(saved), saved.TenantID, saved.Name, domain.CacheInvalidatePolicyChange, actor)
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleAdminDeleteCachePolicy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.CachePolicies == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the cache policy store is unavailable"), metaFromContext(rc, nil))
		return
	}
	id := chiURLParam(r, "id")
	if err := s.repos.CachePolicies.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceCache, id, nil, nil)
	actor := ""
	if pr := principal(ctx); pr != nil {
		actor = pr.Label()
	}
	s.cacheFlushBestEffort(ctx, domain.CacheScopeAll, "", id, domain.CacheInvalidatePolicyChange, actor)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// --- Cache invalidations ---

func (s *Server) handleAdminListCacheInvalidations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.repos == nil || s.repos.CacheInvalidations == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []domain.CacheInvalidationEvent{}})
		return
	}
	events, err := s.repos.CacheInvalidations.ListRecent(ctx,
		r.URL.Query().Get("tenant_id"), parseIntParam(r, "limit", 20))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
