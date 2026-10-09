package platcache

import (
	"context"
	"strings"
)

// PrewarmTenant writes a new tenant's most important entries right after
// provisioning succeeds: meta, settings, routes/graph placeholder, flags.
// Call sequence after provisioning: write DB state -> invalidate relevant
// keys -> prewarm. Loaders read from the just-committed DB rows.
func (c *Cache) PrewarmTenant(ctx context.Context, tenantID string, loads TenantPrewarmLoads) error {
	if c == nil || strings.TrimSpace(tenantID) == "" {
		return nil
	}
	ctx = withTenantCacheContext(ctx, tenantID)
	if loads.Meta != nil {
		if v, err := loads.Meta(ctx); err == nil && v != nil {
			_ = c.Set(ctx, KindTenantMeta, TenantMetaKey(tenantID), tenantID, v,
				"kind:"+string(KindTenantMeta), "tenant:"+normSegment(tenantID))
		}
	}
	if loads.Settings != nil {
		if v, err := loads.Settings(ctx); err == nil && v != nil {
			_ = c.Set(ctx, KindTenantSettings, TenantSettingsKey(tenantID), tenantID, v,
				"kind:"+string(KindTenantSettings), "tenant:"+normSegment(tenantID))
		}
	}
	if loads.Flags != nil {
		if v, err := loads.Flags(ctx); err == nil && v != nil {
			_ = c.Set(ctx, KindFeatureFlag, FeatureFlagsTenantKey(tenantID), tenantID, v,
				"kind:"+string(KindFeatureFlag), "tenant:"+normSegment(tenantID))
		}
	}
	if loads.Graph != nil {
		if v, err := loads.Graph(ctx); err == nil && v != nil {
			_ = c.Set(ctx, KindTenantGraph, TenantGraphKey(tenantID), tenantID, v,
				"kind:"+string(KindTenantGraph), "tenant:"+normSegment(tenantID))
		}
	}
	atomicAdd(&c.stats.Prewarms, 1)
	c.log.Info("platcache prewarmed tenant", "tenant", tenantID)
	return nil
}

// TenantPrewarmLoads carries the DB loaders for prewarming. Any nil loader
// is skipped; loader errors are logged and skipped (never fail provisioning).
type TenantPrewarmLoads struct {
	Meta     func(ctx context.Context) (any, error)
	Settings func(ctx context.Context) (any, error)
	Flags    func(ctx context.Context) (any, error)
	Graph    func(ctx context.Context) (any, error)
}

// PrewarmCatalog warms the frequently accessed provider/model/pricing data.
func (c *Cache) PrewarmCatalog(ctx context.Context, providers, models, pricing any) error {
	if c == nil {
		return nil
	}
	if providers != nil {
		_ = c.Set(ctx, KindModelList, CatalogProvidersKey(), "", providers, "kind:"+string(KindModelList))
	}
	if models != nil {
		_ = c.Set(ctx, KindModelList, CatalogModelsKey(), "", models, "kind:"+string(KindModelList))
	}
	if pricing != nil {
		_ = c.Set(ctx, KindPricingSnapshot, CatalogPricingKey(), "", pricing, "kind:"+string(KindPricingSnapshot))
	}
	atomicAdd(&c.stats.Prewarms, 1)
	return nil
}

// PrewarmFeatureFlags warms the global flags entry.
func (c *Cache) PrewarmFeatureFlags(ctx context.Context, flags any) error {
	if c == nil || flags == nil {
		return nil
	}
	_ = c.Set(ctx, KindFeatureFlag, FeatureFlagsGlobalKey(), "", flags, "kind:"+string(KindFeatureFlag))
	atomicAdd(&c.stats.Prewarms, 1)
	return nil
}

// PrewarmWorkspace warms a new workspace's entries after creation.
func (c *Cache) PrewarmWorkspace(ctx context.Context, workspaceID, tenantID string, meta any) error {
	if c == nil || strings.TrimSpace(workspaceID) == "" {
		return nil
	}
	if meta != nil {
		_ = c.Set(ctx, KindWorkspaceMeta, WorkspaceMetaKey(workspaceID), tenantID, meta,
			"kind:"+string(KindWorkspaceMeta))
	}
	atomicAdd(&c.stats.Prewarms, 1)
	return nil
}
