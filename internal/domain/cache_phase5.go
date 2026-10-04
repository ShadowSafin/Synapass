package domain

import "time"

// ---------------------------------------------------------------------------
// Phase 5: local request cache — policy, decisions and durable events.
//
// The live body always lives in Redis. PostgreSQL holds policy rows,
// invalidation audit rows and lightweight entry metadata used by the
// dashboard (top prompts, hit counts). Nothing here changes the public
// inference contract: a cache hit returns the same JSON shape as a live
// response with `synapass.cache_hit: true` plus the kind/similarity.
// ---------------------------------------------------------------------------

// Cache bypass reasons. Machine-readable and surfaced in metrics, traces
// and the dashboard so a miss is explainable rather than opaque.
const (
	CacheBypassRequested        = "bypass_requested"
	CacheBypassStreaming        = "streaming"
	CacheBypassSensitive        = "sensitive_request"
	CacheBypassPolicyDisabled   = "policy_disabled"
	CacheBypassEndpointDisabled = "endpoint_cache_disabled"
	CacheBypassProviderDisabled = "provider_cache_disabled"
	CacheBypassToolRequest      = "tool_request"
	CacheBypassLiveData         = "live_data_request"
	CacheBypassNondeterministic = "nondeterministic_request"
	CacheBypassMultiSample      = "multi_sample_request"
	CacheBypassMultimodal       = "multimodal_request"
	CacheBypassTooLarge         = "response_too_large"
	CacheBypassDisabled         = "cache_disabled"
)

// Cache invalidation scopes and reasons.
const (
	CacheScopeTenant   = "tenant"
	CacheScopeModel    = "model"
	CacheScopeProvider = "provider"
	CacheScopeKey      = "key"
	CacheScopeAll      = "all"
)

const (
	CacheInvalidateManual          = "manual_flush"
	CacheInvalidateProviderChange  = "provider_changed"
	CacheInvalidateModelChange     = "model_changed"
	CacheInvalidatePolicyChange    = "policy_changed"
	CacheInvalidateTenantSettings  = "tenant_settings_changed"
	CacheInvalidateToolChange      = "tool_definition_changed"
	CacheInvalidateExpired         = "expired"
	CacheInvalidateUnsafe          = "unsafe_to_reuse"
	CacheInvalidateConfigChange    = "config_changed"
	CacheInvalidateEndpointChange  = "endpoint_changed"
)

// CachePolicy is the durable per-scope caching rule.
//
// Resolution order for a request: key scope > endpoint scope > tenant scope >
// provider scope > global config. The first matching enabled row wins; a
// matching disabled row bypasses with the scope's reason. When no row
// matches, the global config defaults apply.
type CachePolicy struct {
	ID          string  `json:"id"`
	TenantID    string  `json:"tenant_id,omitempty"`
	EndpointID  string  `json:"endpoint_id,omitempty"`
	APIKeyID    string  `json:"api_key_id,omitempty"`
	Provider    string  `json:"provider,omitempty"`
	Model       string  `json:"model,omitempty"`
	Name        string  `json:"name"`
	Enabled     bool    `json:"enabled"`
	TTLSeconds  int     `json:"ttl_seconds"`
	// Semantic and Prefix are optional overrides: nil means "inherit the
	// global tier switch", so a TTL-only rule never disables a tier by
	// accident.
	Semantic    *bool   `json:"semantic_enabled,omitempty"`
	Threshold   float64 `json:"semantic_threshold,omitempty"`
	Prefix      *bool   `json:"prefix_enabled,omitempty"`
	BypassTools *bool   `json:"bypass_tool_requests,omitempty"`
	AllowNonDet *bool   `json:"allow_nondeterministic,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SemanticAllowed reports whether the semantic tier applies under this rule.
func (p *CachePolicy) SemanticAllowed(global bool) bool {
	if p == nil || p.Semantic == nil {
		return global
	}
	return *p.Semantic
}

// PrefixAllowed reports whether the prefix tier applies under this rule.
func (p *CachePolicy) PrefixAllowed(global bool) bool {
	if p == nil || p.Prefix == nil {
		return global
	}
	return *p.Prefix
}

// CacheDecision is the policy engine output for one request.
type CacheDecision struct {
	Cacheable    bool   `json:"cacheable"`
	BypassReason string `json:"bypass_reason,omitempty"`
	TTL          time.Duration `json:"-"`
	TTLSeconds   int    `json:"ttl_seconds,omitempty"`
	AllowExact   bool   `json:"allow_exact"`
	AllowPrefix  bool   `json:"allow_prefix"`
	AllowSemantic bool  `json:"allow_semantic"`
	PolicyID     string `json:"policy_id,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// IsCacheable reports whether the request may use the cache at all.
func (d CacheDecision) IsCacheable() bool { return d.Cacheable }

// CacheHitMeta is the serving metadata stored alongside a cached body.
// It is what lets a hit validate that the world has not changed since
// the store: same model, same provider availability, same policy version,
// same tool definitions hash.
type CacheHitMeta struct {
	Model       string    `json:"model"`
	Provider    string    `json:"provider,omitempty"`
	PolicyID    string    `json:"policy_id,omitempty"`
	PolicyVersion int     `json:"policy_version,omitempty"`
	EndpointID  string    `json:"endpoint_id,omitempty"`
	ToolsHash   string    `json:"tools_hash,omitempty"`
	// SettingsHash covers generation settings and the response contract
	// (temperature, top-p, seed, max tokens, response format, stop, ...).
	// A settings change turns a would-be hit into a miss.
	SettingsHash string   `json:"settings_hash,omitempty"`
	// Sensitivity is the request sensitivity label the entry was stored
	// under. Labels isolate entries exactly like tenants do.
	Sensitivity string    `json:"sensitivity,omitempty"`
	// User is the end-user identifier the entry was stored under.
	User        string    `json:"user,omitempty"`
	// APIKeyID is the calling key the entry was stored under. Exact keys
	// already hash it; this field lets the prefix/semantic validators
	// enforce the same isolation for non-exact tiers.
	APIKeyID    string    `json:"api_key_id,omitempty"`
	PromptHash  string    `json:"prompt_hash"`
	Usage       TokenUsage `json:"usage,omitempty"`
	CostUSD     float64   `json:"cost_usd,omitempty"`
	ProviderLatencyMS int64 `json:"provider_latency_ms,omitempty"`
	StoredAt    time.Time `json:"stored_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	ReuseCount  int64     `json:"reuse_count,omitempty"`
}

// CachedPayload is the Redis envelope: the exact response bytes plus the
// metadata needed to validate and explain a future hit.
type CachedPayload struct {
	Body []byte      `json:"body"`
	Meta CacheHitMeta `json:"meta"`
}

// CacheInspectEntry is one cache entry for the admin inspect view.
// The body is never returned in lists; only metadata and a preview hash.
type CacheInspectEntry struct {
	CacheKey    string    `json:"cache_key"`
	Kind        CacheKind `json:"kind"`
	TenantID    string    `json:"tenant_id"`
	Model       string    `json:"model,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	PromptHash  string    `json:"prompt_hash"`
	PromptPreview string  `json:"prompt_preview,omitempty"`
	HitCount    int       `json:"hit_count"`
	ReuseCount  int64     `json:"reuse_count,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// CacheInvalidationEvent is the durable audit row for a flush.
type CacheInvalidationEvent struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id,omitempty"`
	Scope     string    `json:"scope"`
	Target    string    `json:"target,omitempty"`
	Reason    string    `json:"reason"`
	Actor     string    `json:"actor,omitempty"`
	Removed   int       `json:"removed"`
	CreatedAt time.Time `json:"created_at"`
}

// CacheEvent is the NATS payload for cache invalidations (and, when
// enabled, notable hits). Consumers get the same summary the audit row has.
type CacheEvent struct {
	Event     string    `json:"event"`
	Scope     string    `json:"scope,omitempty"`
	Target    string    `json:"target,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	TenantID  string    `json:"tenant_id,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	Removed   int       `json:"removed,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CacheBypassEvent records why one request skipped the cache. It is emitted
// into the request trace and counted in metrics; durable storage is the
// request log itself rather than a separate table.
type CacheBypassEvent struct {
	RequestID string `json:"request_id"`
	TenantID  string `json:"tenant_id,omitempty"`
	Reason    string `json:"reason"`
	Kind      string `json:"kind,omitempty"`
}

// CacheEvalInput carries the full request context for a cache policy
// decision. It lives in domain so the api package can pass it without
// importing the cache implementation.
type CacheEvalInput struct {
	TenantID       string
	APIKeyID       string
	EndpointID     string
	Model          string
	Stream         bool
	CacheBypass    bool
	Sensitive      bool
	Sensitivity    string
	HasTools       bool
	ToolsSafe      bool
	HasImages      bool
	N              int
	Temperature    *float64
	Seed           *int
	PolicyUseCache bool
	PolicyID       string
	EndpointCache  *bool
	PromptText     string
}

// CacheTrace carries the per-request cache observability attached to traces
// and the dashboard request view.
type CacheTrace struct {
	Hit            bool    `json:"hit"`
	Kind           string  `json:"kind,omitempty"`
	Key            string  `json:"key,omitempty"`
	LookupMS       float64 `json:"lookup_ms,omitempty"`
	Similarity     float64 `json:"similarity,omitempty"`
	BypassReason   string  `json:"bypass_reason,omitempty"`
	ReuseCount     int64   `json:"reuse_count,omitempty"`
	LatencySavedMS int64   `json:"latency_saved_ms,omitempty"`
	Invalidated    string  `json:"invalidation_reason,omitempty"`
}
