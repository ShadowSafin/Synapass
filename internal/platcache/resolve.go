package platcache

import (
	"context"
	"strings"
	"time"
)

// TenantResolution is the cached host/path -> tenant mapping.
type TenantResolution struct {
	TenantID    string    `json:"tenant_id"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Route       string    `json:"route,omitempty"`
	ResolvedAt  time.Time `json:"resolved_at"`
}

// WorkspaceLookup is the cached workspace slug -> tenant ID mapping.
type WorkspaceLookup struct {
	WorkspaceID string    `json:"workspace_id"`
	TenantID    string    `json:"tenant_id"`
	Slug        string    `json:"slug"`
	ResolvedAt  time.Time `json:"resolved_at"`
}

// ResolveTenantByHostPath is the gateway fast path: cache first, DB only
// on miss, result cached. Tenant isolation is preserved because the loader
// runs with the request's own host/path and the envelope binds nothing
// cross-tenant: the resolved tenant id is the value, never the key scope.
func (c *Cache) ResolveTenantByHostPath(ctx context.Context, host, path string, load func(ctx context.Context, host, path string) (*TenantResolution, error)) (*TenantResolution, bool, error) {
	if load == nil {
		return nil, false, nil
	}
	host = strings.TrimSpace(host)
	path = strings.TrimSpace(path)
	key := GatewayResolveKey(host, path)
	start := time.Now()
	var out TenantResolution
	hit, stale, err := c.GetOrSetStaleWhileRevalidate(ctx, KindHostResolve, key, "",
		&out,
		func(ctx context.Context) (any, error) {
			res, err := load(ctx, host, path)
			if err != nil {
				return nil, err
			}
			if res == nil {
				return nil, errResolutionNotFound
			}
			if strings.TrimSpace(res.TenantID) == "" {
				return nil, errResolutionNotFound
			}
			res.ResolvedAt = time.Now()
			return res, nil
		},
		"kind:"+string(KindHostResolve), "route:gateway")
	latency := time.Since(start)
	atomicAdd(&c.stats.RouteResolutions, 1)
	c.mu.Lock()
	c.stats.resolutionSamples++
	n := c.stats.resolutionSamples
	ms := float64(latency.Microseconds()) / 1000.0
	if n == 1 {
		c.stats.ResolutionLatencyMS = ms
	} else {
		c.stats.ResolutionLatencyMS += (ms - c.stats.ResolutionLatencyMS) / float64(n)
	}
	c.mu.Unlock()
	c.observer().OnResolve(latency, hit)
	if c.opts.Metrics != nil && c.opts.Metrics.OnResolve != nil {
		c.opts.Metrics.OnResolve(hit, latency.Seconds())
	}
	if err != nil {
		return nil, false, err
	}
	if !hit {
		return &out, false, nil
	}
	_ = stale
	return &out, true, nil
}

// ResolveTenantByWorkspaceSlug caches slug -> tenant ID mappings.
func (c *Cache) ResolveTenantByWorkspaceSlug(ctx context.Context, slug string, load func(ctx context.Context, slug string) (*WorkspaceLookup, error)) (*WorkspaceLookup, bool, error) {
	if load == nil {
		return nil, false, nil
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, false, errResolutionNotFound
	}
	key := WorkspaceSlugKey(slug)
	var out WorkspaceLookup
	hit, _, err := c.GetOrSetStaleWhileRevalidate(ctx, KindWorkspaceLookup, key, "",
		&out,
		func(ctx context.Context) (any, error) {
			res, err := load(ctx, slug)
			if err != nil {
				return nil, err
			}
			if res == nil || strings.TrimSpace(res.TenantID) == "" {
				return nil, errResolutionNotFound
			}
			res.ResolvedAt = time.Now()
			return res, nil
		},
		"kind:"+string(KindWorkspaceLookup))
	if err != nil {
		return nil, false, err
	}
	if !hit && out.TenantID == "" {
		return nil, false, errResolutionNotFound
	}
	return &out, hit, nil
}

// GetTenantMeta is a typed read-through for tenant metadata.
func (c *Cache) GetTenantMeta(ctx context.Context, tenantID string, dst any, load func(ctx context.Context) (any, error)) (bool, error) {
	return c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey(tenantID), tenantID, dst, load,
		"kind:"+string(KindTenantMeta), "tenant:"+normSegment(tenantID))
}

// GetTenantSettings is a typed read-through for tenant settings.
func (c *Cache) GetTenantSettings(ctx context.Context, tenantID string, dst any, load func(ctx context.Context) (any, error)) (bool, error) {
	return c.GetOrSet(ctx, KindTenantSettings, TenantSettingsKey(tenantID), tenantID, dst, load,
		"kind:"+string(KindTenantSettings), "tenant:"+normSegment(tenantID))
}

// GetFeatureFlags reads global flags with SWR (hot key, never block).
func (c *Cache) GetFeatureFlags(ctx context.Context, dst any, load func(ctx context.Context) (any, error)) (bool, bool, error) {
	return c.GetOrSetStaleWhileRevalidate(ctx, KindFeatureFlag, FeatureFlagsGlobalKey(), "", dst, load,
		"kind:"+string(KindFeatureFlag))
}

// GetCatalog reads a catalog snapshot (providers/models/pricing) with SWR.
func (c *Cache) GetCatalog(ctx context.Context, key string, dst any, load func(ctx context.Context) (any, error)) (bool, bool, error) {
	kind := KindModelList
	if key == CatalogPricingKey() {
		kind = KindPricingSnapshot
	}
	return c.GetOrSetStaleWhileRevalidate(ctx, kind, key, "", dst, load, "kind:"+string(kind))
}

// GetHealth reads dependency health with SWR; failures keep serving stale.
func (c *Cache) GetHealth(ctx context.Context, dependency string, dst any, load func(ctx context.Context) (any, error)) (bool, bool, error) {
	return c.GetOrSetStaleWhileRevalidate(ctx, KindHealthStatus, HealthKey(dependency), "", dst, load,
		"kind:"+string(KindHealthStatus))
}
