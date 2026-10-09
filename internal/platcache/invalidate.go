package platcache

import (
	"context"
	"strings"
)

// Explicit invalidation on write paths. Call these immediately after the DB
// write commits; they are best-effort and never fail the write itself.
// Every helper clears L1 synchronously and L2 best-effort, then emits the
// observability event, so dashboards and logs agree about what happened.

// InvalidateTenantCache drops all of one tenant's platform entries:
// metadata, routes, settings, usage aggregates, graph, flags.
func (c *Cache) InvalidateTenantCache(ctx context.Context, tenantID string) (int, error) {
	if c == nil || strings.TrimSpace(tenantID) == "" {
		return 0, nil
	}
	total := 0
	n, _ := c.InvalidateByTag(ctx, "tenant:"+normSegment(tenantID))
	total += n
	for _, k := range []string{
		TenantMetaKey(tenantID), TenantRoutesKey(tenantID),
		TenantSettingsKey(tenantID), TenantGraphKey(tenantID),
		FeatureFlagsTenantKey(tenantID),
	} {
		m, _ := c.InvalidateByKey(ctx, k)
		total += m
	}
	m, _ := c.InvalidateByPrefix(ctx, "tenant:"+normSegment(tenantID)+":")
	total += m
	m, _ = c.InvalidateByPrefix(ctx, "usage:"+normSegment(tenantID)+":")
	total += m
	c.emitInvalidateMetric("tenant", "tenant-changed")
	c.observer().OnInvalidate(nil, []string{"tenant:" + normSegment(tenantID)}, "tenant-changed", total)
	return total, nil
}

// InvalidateWorkspaceCache drops workspace metadata + slug mappings.
func (c *Cache) InvalidateWorkspaceCache(ctx context.Context, workspaceID string) (int, error) {
	if c == nil || strings.TrimSpace(workspaceID) == "" {
		return 0, nil
	}
	total := 0
	for _, k := range []string{WorkspaceTenantKey(workspaceID), WorkspaceMetaKey(workspaceID)} {
		m, _ := c.InvalidateByKey(ctx, k)
		total += m
	}
	m, _ := c.InvalidateByPrefix(ctx, "workspace:"+normSegment(workspaceID))
	total += m
	c.emitInvalidateMetric("workspace", "workspace-changed")
	return total, nil
}

// InvalidateRouteCache drops one gateway host/path resolution + tenant routes.
func (c *Cache) InvalidateRouteCache(ctx context.Context, host, path string) (int, error) {
	if c == nil {
		return 0, nil
	}
	total := 0
	m, _ := c.InvalidateByKey(ctx, GatewayResolveKey(host, path))
	total += m
	// Route definitions changed: retire all gateway resolutions (bounded:
	// L1 prefix + L2 prefix scan) so no stale host/path mapping survives.
	n, _ := c.InvalidateByPrefix(ctx, "gateway:resolve:")
	total += n
	c.emitInvalidateMetric("route", "route-changed")
	return total, nil
}

// InvalidateProviderCache drops one provider's caps + the shared catalog.
func (c *Cache) InvalidateProviderCache(ctx context.Context, providerID string) (int, error) {
	if c == nil {
		return 0, nil
	}
	total := 0
	if strings.TrimSpace(providerID) != "" {
		m, _ := c.InvalidateByKey(ctx, ProviderCapsKey(providerID))
		total += m
	}
	m, _ := c.InvalidateCatalogCache(ctx)
	total += m
	c.emitInvalidateMetric("provider", "provider-changed")
	return total, nil
}

// InvalidateCatalogCache drops model/provider/pricing snapshots.
func (c *Cache) InvalidateCatalogCache(ctx context.Context) (int, error) {
	if c == nil {
		return 0, nil
	}
	total := 0
	for _, k := range []string{CatalogProvidersKey(), CatalogModelsKey(), CatalogPricingKey()} {
		m, _ := c.InvalidateByKey(ctx, k)
		total += m
	}
	m, _ := c.InvalidateByPrefix(ctx, "modelcatalog:")
	total += m
	c.emitInvalidateMetric("catalog", "catalog-changed")
	return total, nil
}

// InvalidateTenantGraphCache drops the resolved routing graph for a tenant.
func (c *Cache) InvalidateTenantGraphCache(ctx context.Context, tenantID string) (int, error) {
	if c == nil || strings.TrimSpace(tenantID) == "" {
		return 0, nil
	}
	n, _ := c.InvalidateByKey(ctx, TenantGraphKey(tenantID))
	n2, _ := c.InvalidateByKey(ctx, TenantRoutesKey(tenantID))
	c.emitInvalidateMetric("tenant-graph", "graph-changed")
	return n + n2, nil
}

// InvalidateFeatureFlags drops global and optionally one tenant's flags.
func (c *Cache) InvalidateFeatureFlags(ctx context.Context, tenantID string) (int, error) {
	if c == nil {
		return 0, nil
	}
	total := 0
	m, _ := c.InvalidateByKey(ctx, FeatureFlagsGlobalKey())
	total += m
	if strings.TrimSpace(tenantID) != "" {
		n, _ := c.InvalidateByKey(ctx, FeatureFlagsTenantKey(tenantID))
		total += n
	} else {
		n, _ := c.InvalidateByPrefix(ctx, "featureflags:")
		total += n
	}
	c.emitInvalidateMetric("flags", "flags-changed")
	return total, nil
}

// InvalidateSettings drops tenant settings.
func (c *Cache) InvalidateSettings(ctx context.Context, tenantID string) (int, error) {
	if c == nil || strings.TrimSpace(tenantID) == "" {
		return 0, nil
	}
	n, _ := c.InvalidateByKey(ctx, TenantSettingsKey(tenantID))
	c.emitInvalidateMetric("settings", "settings-changed")
	return n, nil
}

// InvalidateHealth drops one or all dependency health entries.
func (c *Cache) InvalidateHealth(ctx context.Context, dependency string) (int, error) {
	if c == nil {
		return 0, nil
	}
	if strings.TrimSpace(dependency) == "" {
		return c.InvalidateByPrefix(ctx, "health:")
	}
	n, _ := c.InvalidateByKey(ctx, HealthKey(dependency))
	return n, nil
}
