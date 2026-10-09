package api

import (
	"context"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/platcache"
)

// PlatformCacheService is satisfied by *platcache.Cache. It is declared here
// (consumer side) so wiring can pass the concrete cache without the api
// package depending on construction details.
type PlatformCacheService interface {
	Stats() platcache.Stats
	HealthSnapshot(ctx context.Context) map[string]any
	InvalidateTenantCache(ctx context.Context, tenantID string) (int, error)
	InvalidateWorkspaceCache(ctx context.Context, workspaceID string) (int, error)
	InvalidateRouteCache(ctx context.Context, host, path string) (int, error)
	InvalidateProviderCache(ctx context.Context, providerID string) (int, error)
	InvalidateCatalogCache(ctx context.Context) (int, error)
	InvalidateTenantGraphCache(ctx context.Context, tenantID string) (int, error)
	InvalidateFeatureFlags(ctx context.Context, tenantID string) (int, error)
	InvalidateSettings(ctx context.Context, tenantID string) (int, error)
	InvalidateByTag(ctx context.Context, tag string) (int, error)
	PrewarmTenant(ctx context.Context, tenantID string, loads platcache.TenantPrewarmLoads) error
}

func resolvePlatformCache(svc PlatformCacheService) *platcache.Cache {
	if c, ok := svc.(*platcache.Cache); ok {
		return c
	}
	return nil
}

// SetPlatformCache attaches the multi-layer platform cache at runtime.
// Nil detaches it; every hook is nil-safe.
func (s *Server) SetPlatformCache(c *platcache.Cache) {
	if s == nil {
		return
	}
	s.plat = c
}

// PlatformCache exposes the attached platform cache (may be nil).
func (s *Server) PlatformCache() *platcache.Cache {
	if s == nil {
		return nil
	}
	return s.plat
}

// PlatformCacheHealth returns the platform cache health snapshot for
// dashboards. Nil cache reports disabled rather than erroring.
func (s *Server) PlatformCacheHealth(ctx context.Context) map[string]any {
	if s == nil || s.plat == nil {
		return map[string]any{"enabled": false}
	}
	snap := s.plat.HealthSnapshot(ctx)
	snap["enabled"] = true
	return snap
}

// platFlushBestEffort mirrors a response-cache flush into the platform
// cache without ever failing the admin operation that triggered it.
// Scope mapping:
//
//	tenant   -> InvalidateTenantCache (meta, routes, settings, graph, flags)
//	provider -> InvalidateProviderCache (caps + shared catalog)
//	model    -> InvalidateCatalogCache (model list + pricing snapshots)
//	all      -> catalog + flags (global generations retire together)
//	key      -> response-cache only (key material is never platform-cached)
func (s *Server) platFlushBestEffort(ctx context.Context, scope, tenantID, target string) {
	if s == nil || s.plat == nil {
		return
	}
	var err error
	switch scope {
	case domain.CacheScopeTenant:
		_, err = s.plat.InvalidateTenantCache(ctx, tenantID)
	case domain.CacheScopeProvider:
		_, err = s.plat.InvalidateProviderCache(ctx, target)
	case domain.CacheScopeModel:
		_, err = s.plat.InvalidateCatalogCache(ctx)
	case domain.CacheScopeKey:
		return
	default:
		_, err = s.plat.InvalidateCatalogCache(ctx)
		if err == nil {
			_, err = s.plat.InvalidateFeatureFlags(ctx, "")
		}
	}
	if err != nil {
		s.logger.Warn("platform cache flush failed", "scope", scope, "target", target, "error", err)
	}
}

// platTenantChanged invalidates one tenant's platform entries after a
// tenant or provisioning write. Best-effort: never fails the write.
func (s *Server) platTenantChanged(ctx context.Context, tenantID string) {
	if s == nil || s.plat == nil || tenantID == "" {
		return
	}
	if _, err := s.plat.InvalidateTenantCache(ctx, tenantID); err != nil {
		s.logger.Warn("platform tenant cache flush failed", "tenant", tenantID, "error", err)
	}
}

// platTenantPrewarm prewarms a newly provisioned tenant's entries from the
// just-committed DB rows. Loaders run synchronously but are cheap point
// reads; failures are logged and skipped, never failing provisioning.
func (s *Server) platTenantPrewarm(ctx context.Context, tenantID string) {
	if s == nil || s.plat == nil || tenantID == "" {
		return
	}
	if s.repos == nil || s.repos.Tenants == nil {
		return
	}
	err := s.plat.PrewarmTenant(ctx, tenantID, platcache.TenantPrewarmLoads{
		Meta: func(ctx context.Context) (any, error) {
			return s.repos.Tenants.GetByID(ctx, tenantID)
		},
		Settings: func(ctx context.Context) (any, error) {
			t, err := s.repos.Tenants.GetByID(ctx, tenantID)
			if err != nil || t == nil {
				return nil, err
			}
			// Tenant-scoped settings today live on the tenant row
			// (labels/plan/policy); snapshot them so the hot path avoids
			// the row read.
			return map[string]any{
				"labels": t.Labels, "plan": t.Plan,
				"default_routing_policy_id": t.DefaultRoutingPolicyID,
			}, nil
		},
		Graph: func(ctx context.Context) (any, error) {
			if s.repos.Policies == nil {
				return nil, nil
			}
			policies, err := s.repos.Policies.ListPolicies(ctx)
			if err != nil {
				return nil, err
			}
			var mine []string
			for _, p := range policies {
				if p.TenantID == "" || p.TenantID == tenantID {
					mine = append(mine, p.ID)
				}
			}
			return map[string]any{"policies": mine}, nil
		},
	})
	if err != nil {
		s.logger.Warn("platform tenant prewarm failed", "tenant", tenantID, "error", err)
	}
}
