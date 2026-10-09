package platcache

import (
	"fmt"
	"strings"
)

// Key versions. Bumping a version retires every key of that family without
// a manual flush; schema changes must bump the family's version.
const (
	VersionTenant    = "v1"
	VersionWorkspace = "v1"
	VersionGateway   = "v1"
	VersionDashboard = "v1"
	VersionCatalog   = "v3"
	VersionPricing   = "v3"
	VersionFlags     = "v2"
	VersionSettings  = "v1"
	VersionUsage     = "v1"
	VersionHealth    = "v1"
	VersionResolve   = "v1"
)

// Kind identifies a cacheable data family. It drives TTL selection,
// safety checks, and tag-based invalidation.
type Kind string

const (
	KindTenantMeta      Kind = "tenant-meta"
	KindWorkspaceMeta   Kind = "workspace-meta"
	KindGatewayRoute    Kind = "gateway-route"
	KindDashboardRoute  Kind = "dashboard-route"
	KindProviderCaps    Kind = "provider-caps"
	KindModelList       Kind = "model-list"
	KindPricingSnapshot Kind = "pricing-snapshot"
	KindFeatureFlag     Kind = "feature-flag"
	KindTenantSettings  Kind = "tenant-settings"
	KindUsageAggregate  Kind = "usage-aggregate"
	KindHealthStatus    Kind = "health-status"
	KindHostResolve     Kind = "host-resolve"
	KindWorkspaceLookup Kind = "workspace-lookup"
	KindTenantGraph     Kind = "tenant-graph"
	KindHTTPResponse    Kind = "http-response"
)

// forbiddenSubstrings are matched (case-insensitively) against every key
// and payload family before anything is cached. A match refuses the write.
// This is the last line of defence against caching secrets; callers must
// still avoid passing secrets in the first place.
var forbiddenSubstrings = []string{
	"api_key", "apikey", "api-key",
	"auth_token", "authtoken", "auth-token",
	"access_token", "refresh_token", "id_token",
	"bearer", "session_secret", "sessionsecret",
	"password", "passwd", "payment", "card_number", "cardnumber",
	"credential", "private_key", "privatekey", "secret_key", "secretkey",
	"webhook_payload", "webhookpayload",
	"raw_body", "rawbody", "raw_prompt", "rawprompt",
	"raw_response",
}

// tenantScopedKinds must carry a non-empty tenant id in their key.
var tenantScopedKinds = map[Kind]bool{
	KindTenantMeta:      true,
	KindWorkspaceMeta:   true,
	KindGatewayRoute:    true,
	KindDashboardRoute:  true,
	KindTenantSettings:  true,
	KindUsageAggregate:  true,
	KindHostResolve:     false,
	KindWorkspaceLookup: false,
	KindProviderCaps:    false,
	KindModelList:       false,
	KindPricingSnapshot: false,
	KindFeatureFlag:     false,
	KindHealthStatus:    false,
	KindTenantGraph:     true,
	KindHTTPResponse:    false,
}

// Key builders. Every key is namespaced, tenant-aware where applicable,
// and versioned. Keep the exact example shapes stable:
// tenant:{tenantId}:meta:v1, gateway:resolve:{host}:{path}:v1, ...

// TenantMetaKey is tenant:{tenantId}:meta:v1.
func TenantMetaKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:meta:%s", normSegment(tenantID), VersionTenant)
}

// TenantRoutesKey is tenant:{tenantId}:routes:v1.
func TenantRoutesKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:routes:%s", normSegment(tenantID), VersionTenant)
}

// TenantSettingsKey is tenant:{tenantId}:settings:v1.
func TenantSettingsKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:settings:%s", normSegment(tenantID), VersionSettings)
}

// TenantGraphKey is tenant:{tenantId}:graph:v1 (resolved routing graph).
func TenantGraphKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:graph:%s", normSegment(tenantID), VersionTenant)
}

// WorkspaceTenantKey is workspace:{workspaceId}:tenant:v1.
func WorkspaceTenantKey(workspaceID string) string {
	return fmt.Sprintf("workspace:%s:tenant:%s", normSegment(workspaceID), VersionWorkspace)
}

// WorkspaceMetaKey is workspace:{workspaceId}:meta:v1.
func WorkspaceMetaKey(workspaceID string) string {
	return fmt.Sprintf("workspace:%s:meta:%s", normSegment(workspaceID), VersionWorkspace)
}

// WorkspaceSlugKey is workspace:slug:{slug}:tenant:v1.
func WorkspaceSlugKey(slug string) string {
	return fmt.Sprintf("workspace:slug:%s:tenant:%s", normSegment(slug), VersionResolve)
}

// GatewayResolveKey is gateway:resolve:{host}:{path}:v1.
func GatewayResolveKey(host, path string) string {
	return fmt.Sprintf("gateway:resolve:%s:%s:%s", normSegment(host), normPath(path), VersionGateway)
}

// DashboardRouteKey is dashboard:route:{name}:v1.
func DashboardRouteKey(name string) string {
	return fmt.Sprintf("dashboard:route:%s:%s", normSegment(name), VersionDashboard)
}

// ProviderCapsKey is modelcatalog:provider:{providerId}:caps:v3.
func ProviderCapsKey(providerID string) string {
	return fmt.Sprintf("modelcatalog:provider:%s:caps:%s", normSegment(providerID), VersionCatalog)
}

// CatalogProvidersKey is modelcatalog:providers:v3.
func CatalogProvidersKey() string { return "modelcatalog:providers:" + VersionCatalog }

// CatalogPricingKey is modelcatalog:pricing:v3.
func CatalogPricingKey() string { return "modelcatalog:pricing:" + VersionPricing }

// CatalogModelsKey is modelcatalog:models:v3.
func CatalogModelsKey() string { return "modelcatalog:models:" + VersionCatalog }

// FeatureFlagsGlobalKey is featureflags:global:v2.
func FeatureFlagsGlobalKey() string { return "featureflags:global:" + VersionFlags }

// FeatureFlagsTenantKey is featureflags:tenant:{tenantId}:v2.
func FeatureFlagsTenantKey(tenantID string) string {
	return fmt.Sprintf("featureflags:tenant:%s:%s", normSegment(tenantID), VersionFlags)
}

// UsageAggregateKey is usage:{tenantId}:{window}:v1.
func UsageAggregateKey(tenantID, window string) string {
	return fmt.Sprintf("usage:%s:%s:%s", normSegment(tenantID), normSegment(window), VersionUsage)
}

// HealthKey is health:{dependency}:v1.
func HealthKey(dependency string) string {
	return fmt.Sprintf("health:%s:%s", normSegment(dependency), VersionHealth)
}

// HTTPResponseKey is http:get:{tenantOrPublic}:{pathHash}:v1. The path hash
// keeps query strings out of the keyspace while staying deterministic.
func HTTPResponseKey(scope, pathHash string) string {
	return fmt.Sprintf("http:get:%s:%s:%s", normSegment(scope), normSegment(pathHash), VersionTenant)
}

// TagsFor returns tag labels for tag/index-based invalidation.
func TagsFor(kind Kind, tenantID, extra string) []string {
	var tags []string
	tags = append(tags, "kind:"+string(kind))
	if tenantID != "" {
		tags = append(tags, "tenant:"+normSegment(tenantID))
	}
	if extra != "" {
		tags = append(tags, extra)
	}
	return tags
}

// ValidateKey enforces namespacing, tenant-awareness, and safety.
// It returns an error when the key must not be cached.
func ValidateKey(kind Kind, key, tenantID string) error {
	if key == "" {
		return fmt.Errorf("platcache: empty key")
	}
	if strings.Contains(key, " ") || strings.Contains(key, "\n") {
		return fmt.Errorf("platcache: key contains whitespace")
	}
	lower := strings.ToLower(key)
	for _, bad := range forbiddenSubstrings {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("platcache: refusing to cache secret-like key %q (matched %q)", key, bad)
		}
	}
	if tenantScopedKinds[kind] && strings.TrimSpace(tenantID) == "" {
		return fmt.Errorf("platcache: kind %q requires a tenant id", kind)
	}
	if tenantScopedKinds[kind] && !strings.Contains(key, normSegment(tenantID)) {
		return fmt.Errorf("platcache: tenant-scoped key %q does not contain tenant %q", key, tenantID)
	}
	return nil
}

// AssertSafePayload refuses payload families that must never be cached,
// such as raw bodies or secret material. Pass a short family label
// (e.g. "tenant-meta", "raw_body") rather than the payload itself.
func AssertSafePayload(family string) error {
	lower := strings.ToLower(family)
	for _, bad := range forbiddenSubstrings {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("platcache: refusing to cache payload family %q (matched %q)", family, bad)
		}
	}
	return nil
}

func normSegment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == ':':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "root"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return normSegment(p)
}
