// Package config defines Synapass's configuration model.
//
// Configuration is resolved in a fixed order so behaviour is predictable in
// every deployment mode:
//
//  1. built-in defaults   (Default)
//  2. the config file     (--config / SYNAPASS_CONFIG_FILE, YAML or JSON)
//  3. environment vars    (SYNAPASS_* prefixed, always win)
//
// The same struct drives the container image, the native systemd unit and a
// local `go run`, which is what lets both deployment paths share one set of
// core code paths and one set of operational semantics.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that (un)marshals from human-readable strings
// like "150ms" or "2m30s". The standard library type cannot be decoded from a
// string by yaml.v3, and operators should never have to express a timeout as a
// nanosecond integer.
type Duration time.Duration

// UnmarshalYAML accepts a duration string or a bare integer interpreted as
// seconds, because both spellings appear in operator config files.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		parsed, perr := time.ParseDuration(s)
		if perr != nil {
			return fmt.Errorf("invalid duration %q: %w", s, perr)
		}
		*d = Duration(parsed)
		return nil
	}
	var secs float64
	if err := value.Decode(&secs); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds")
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalYAML renders the duration in its human-readable form.
func (d Duration) MarshalYAML() (any, error) { return d.Std().String(), nil }

// UnmarshalJSON accepts a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	if parsed, err := time.ParseDuration(s); err == nil {
		*d = Duration(parsed)
		return nil
	}
	var secs float64
	if _, err := fmt.Sscanf(s, "%f", &secs); err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.Std().String() + `"`), nil
}

// Std converts to the standard library type.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the complete runtime configuration.
type Config struct {
	// ConfigFile records the path the configuration was loaded from, for
	// diagnostics and for the startup log line.
	ConfigFile string `yaml:"-" json:"-"`

	App        AppConfig        `yaml:"app" json:"app"`
	HTTP       HTTPConfig       `yaml:"http" json:"http"`
	Database   DatabaseConfig   `yaml:"database" json:"database"`
	Redis      RedisConfig      `yaml:"redis" json:"redis"`
	ClickHouse ClickHouseConfig `yaml:"clickhouse" json:"clickhouse"`
	NATS       NATSConfig       `yaml:"nats" json:"nats"`
	Auth       AuthConfig       `yaml:"auth" json:"auth"`
	Routing    RoutingConfig    `yaml:"routing" json:"routing"`
	Cache      CacheConfig      `yaml:"cache" json:"cache"`
	Tools      ToolsConfig      `yaml:"tools" json:"tools"`
	Tunnel     TunnelConfig     `yaml:"tunnel" json:"tunnel"`
	Classifier ClassifierConfig `yaml:"classifier" json:"classifier"`
	Shaping    ShapingConfig    `yaml:"shaping" json:"shaping"`
	Scoring    ScoringConfig    `yaml:"scoring" json:"scoring"`
	Guardrails GuardrailsConfig `yaml:"guardrails" json:"guardrails"`
	Eval       EvalConfig       `yaml:"eval" json:"eval"`
	// Detection tunes automatic capability detection: asking newly discovered
	// models what they can do so routing does not have to guess from the
	// provider kind.
	Detection DetectionConfig `yaml:"detection" json:"detection"`
	// Benchmark governs the Autonomous Benchmark Engine. Disabled by default:
	// when disabled no scheduler, worker or route is started and the gateway
	// behaves exactly as before.
	Benchmark BenchmarkConfig `yaml:"benchmark" json:"benchmark"`
	Telemetry TelemetryConfig `yaml:"telemetry" json:"telemetry"`
	Logging   LoggingConfig   `yaml:"logging" json:"logging"`
	Admin     AdminConfig     `yaml:"admin" json:"admin"`

	// Providers, Models and Policies are the declarative bootstrap catalogue.
	// They are applied to Postgres on startup when bootstrap is enabled, after
	// which the database is the runtime source of truth. This is what makes a
	// native install reproducible from a single file while still allowing the
	// dashboard to edit configuration at runtime.
	Providers []ProviderConfig `yaml:"providers" json:"providers"`
	Policies  []PolicyConfig   `yaml:"policies" json:"policies"`
}

// AppConfig identifies the deployment.
type AppConfig struct {
	// Name is the service name reported in telemetry.
	Name string `yaml:"name" json:"name"`
	// Environment is one of development, staging, production. It relaxes
	// validation in development (for example allowing a default admin key) and
	// is attached to every telemetry record.
	Environment string `yaml:"environment" json:"environment"`
	// InstanceID distinguishes replicas. It defaults to the hostname.
	InstanceID string `yaml:"instance_id" json:"instance_id"`
	// Region tags telemetry for multi-region deployments.
	Region string `yaml:"region" json:"region"`
	// DefaultTenantSlug is created on first boot so a fresh install is usable
	// without a manual seeding step.
	DefaultTenantSlug string `yaml:"default_tenant_slug" json:"default_tenant_slug"`
	// DefaultTenantName is the display name for that tenant.
	DefaultTenantName string `yaml:"default_tenant_name" json:"default_tenant_name"`
	// BootstrapFromConfig applies the Providers and Policies sections above on
	// every startup, upserting by name.
	BootstrapFromConfig bool `yaml:"bootstrap_from_config" json:"bootstrap_from_config"`
}

// HTTPConfig configures the public listener.
type HTTPConfig struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string `yaml:"addr" json:"addr"`
	// ReadHeaderTimeout guards against slowloris-style clients.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout" json:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout" json:"read_timeout"`
	// WriteTimeout must exceed the longest streaming response. It is deliberately
	// generous because a streaming completion can legitimately run for minutes.
	WriteTimeout Duration `yaml:"write_timeout" json:"write_timeout"`
	IdleTimeout  Duration `yaml:"idle_timeout" json:"idle_timeout"`
	// ShutdownTimeout bounds graceful drain on SIGTERM.
	ShutdownTimeout Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`
	// MaxBodyBytes caps request size, protecting the gateway from oversized
	// prompts before any provider sees them.
	MaxBodyBytes int64 `yaml:"max_body_bytes" json:"max_body_bytes"`
	// TrustedProxies lists CIDRs whose X-Forwarded-For header is honoured.
	TrustedProxies []string `yaml:"trusted_proxies" json:"trusted_proxies"`
	// CORSAllowedOrigins enables the browser-facing dashboard to call the API.
	CORSAllowedOrigins []string `yaml:"cors_allowed_origins" json:"cors_allowed_origins"`
	// EnablePprof exposes /debug/pprof. It must stay off in production because
	// the endpoints are unauthenticated.
	EnablePprof bool `yaml:"enable_pprof" json:"enable_pprof"`
}

// DatabaseConfig configures the PostgreSQL system of record.
type DatabaseConfig struct {
	// DSN is a libpq/pgx connection string. When empty it is assembled from the
	// discrete fields below, which is more convenient for systemd drop-ins.
	DSN      string `yaml:"dsn" json:"dsn"`
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	User     string `yaml:"user" json:"user"`
	Password string `yaml:"password" json:"password"`
	Name     string `yaml:"name" json:"name"`
	SSLMode  string `yaml:"ssl_mode" json:"ssl_mode"`

	MaxConns         int32    `yaml:"max_conns" json:"max_conns"`
	MinConns         int32    `yaml:"min_conns" json:"min_conns"`
	MaxConnLifetime  Duration `yaml:"max_conn_lifetime" json:"max_conn_lifetime"`
	MaxConnIdleTime  Duration `yaml:"max_conn_idle_time" json:"max_conn_idle_time"`
	ConnectTimeout   Duration `yaml:"connect_timeout" json:"connect_timeout"`
	StatementTimeout Duration `yaml:"statement_timeout" json:"statement_timeout"`
	// AutoMigrate applies pending migrations at startup. Convenient for
	// self-hosting; operators with strict change control run the migrate
	// subcommand instead.
	AutoMigrate bool `yaml:"auto_migrate" json:"auto_migrate"`
	// MigrationsDir is where the .sql files live for a native install.
	MigrationsDir string `yaml:"migrations_dir" json:"migrations_dir"`
}

// RedisConfig configures rate limiting, caching and coordination.
type RedisConfig struct {
	Addr     string `yaml:"addr" json:"addr"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
	DB       int    `yaml:"db" json:"db"`
	// SentinelAddrs enables sentinel-based failover; when set, MasterName is
	// required.
	SentinelAddrs []string `yaml:"sentinel_addrs" json:"sentinel_addrs"`
	MasterName    string   `yaml:"master_name" json:"master_name"`
	PoolSize      int      `yaml:"pool_size" json:"pool_size"`
	DialTimeout   Duration `yaml:"dial_timeout" json:"dial_timeout"`
	ReadTimeout   Duration `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout  Duration `yaml:"write_timeout" json:"write_timeout"`
	// KeyPrefix namespaces every key so one Redis instance can serve several
	// Synapass deployments.
	KeyPrefix string `yaml:"key_prefix" json:"key_prefix"`
	// TLS enables TLS for managed Redis providers.
	TLS bool `yaml:"tls" json:"tls"`
	// Required fails startup when Redis is unreachable. Rate limiting degrades
	// to a local in-process limiter when false.
	Required bool `yaml:"required" json:"required"`
}

// ClickHouseConfig configures the analytics store.
type ClickHouseConfig struct {
	DSN      string `yaml:"dsn" json:"dsn"`
	Addr     string `yaml:"addr" json:"addr"`
	Database string `yaml:"database" json:"database"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"`
	// TLS enables a secure connection to ClickHouse Cloud.
	TLS bool `yaml:"tls" json:"tls"`
	// BatchSize and FlushInterval control write batching. Batching is essential:
	// ClickHouse performs poorly with one INSERT per row.
	BatchSize     int      `yaml:"batch_size" json:"batch_size"`
	FlushInterval Duration `yaml:"flush_interval" json:"flush_interval"`
	MaxRetries    int      `yaml:"max_retries" json:"max_retries"`
	// Required fails startup when ClickHouse is unreachable. When false,
	// telemetry writes are dropped and counted.
	Required bool `yaml:"required" json:"required"`
}

// NATSConfig configures the asynchronous messaging layer.
type NATSConfig struct {
	URL string `yaml:"url" json:"url"`
	// URLs allows a cluster seed list.
	URLs []string `yaml:"urls" json:"urls"`
	// Name is the connection name shown in NATS server monitoring.
	Name string `yaml:"name" json:"name"`
	// CredentialsFile points at a .creds file for decentralized JWT auth.
	CredentialsFile string `yaml:"credentials_file" json:"credentials_file"`
	// NKeySeedFile and Token are the alternative credential forms.
	NKeySeedFile string `yaml:"nkey_seed_file" json:"nkey_seed_file"`
	Token        string `yaml:"token" json:"token"`
	// JetStream enables durable streams for replay and evaluation jobs.
	JetStream bool `yaml:"jetstream" json:"jetstream"`
	// StreamReplicas is the JetStream replication factor.
	StreamReplicas int `yaml:"stream_replicas" json:"stream_replicas"`
	// MaxReconnects of -1 retries forever, which is correct for a long-lived
	// service.
	MaxReconnects int      `yaml:"max_reconnects" json:"max_reconnects"`
	ReconnectWait Duration `yaml:"reconnect_wait" json:"reconnect_wait"`
	// Required fails startup when NATS is unreachable. When false the gateway
	// continues and async work is dropped, with the drop counter exported.
	Required bool `yaml:"required" json:"required"`
}

// AuthConfig configures API key handling.
type AuthConfig struct {
	// HeaderName overrides the Authorization header for deployments behind a
	// gateway that reserves it.
	HeaderName string `yaml:"header_name" json:"header_name"`
	// RequireKeyOfLength rejects short keys early. Real minted keys are longer;
	// this catches hand-crafted placeholders in development.
	MinKeyLength int `yaml:"min_key_length" json:"min_key_length"`
	// KeyPrefix is prepended to generated tokens, e.g. "syn_live_".
	KeyPrefix string `yaml:"key_prefix" json:"key_prefix"`
	// CacheTTL is how long a validated key is cached in Redis. Short values
	// bound the window in which a revoked key still works.
	CacheTTL Duration `yaml:"cache_ttl" json:"cache_ttl"`
	// AdminKeyEnv names an environment variable holding a bootstrap admin key.
	// It exists so a fresh install can be administered before any key is stored.
	AdminKeyEnv string `yaml:"admin_key_env" json:"admin_key_env"`
	// AdminKey is the resolved bootstrap key, populated at load time.
	AdminKey string `yaml:"-" json:"-"`
	// AllowAnonymousTenant, when set, permits unauthenticated calls to be
	// attributed to a single tenant. Development convenience only.
	AllowAnonymousTenant string `yaml:"allow_anonymous_tenant" json:"allow_anonymous_tenant"`
}

// RoutingConfig holds engine-wide defaults applied when a policy is silent.
type RoutingConfig struct {
	DefaultStrategy    string        `yaml:"default_strategy" json:"default_strategy"`
	DefaultMaxAttempts int           `yaml:"default_max_attempts" json:"default_max_attempts"`
	DefaultRetry       RetryConfig   `yaml:"default_retry" json:"default_retry"`
	DefaultTimeout     TimeoutConfig `yaml:"default_timeout" json:"default_timeout"`
	DefaultLimits      LimitsConfig  `yaml:"default_limits" json:"default_limits"`
	// FallbackOnAllProvidersFailed keeps trying even when a provider reports a
	// non-retryable error, which is occasionally desirable for local runtimes
	// whose error classification is unreliable.
	FallbackOnAllProvidersFailed bool `yaml:"fallback_on_all_providers_failed" json:"fallback_on_all_providers_failed"`
	// HealthCheckInterval is how often the active probe loop runs.
	HealthCheckInterval Duration `yaml:"health_check_interval" json:"health_check_interval"`
	// HealthCheckTimeout bounds a single probe.
	HealthCheckTimeout Duration `yaml:"health_check_timeout" json:"health_check_timeout"`
	// HealthCheckEnabled turns the probe loop off entirely, leaving health to
	// passive observation.
	HealthCheckEnabled bool `yaml:"health_check_enabled" json:"health_check_enabled"`
	// HealthWindow is the rolling window used for success rates.
	HealthWindow Duration `yaml:"health_window" json:"health_window"`
	// LatencyPriors are static per-provider-model latency estimates used before
	// real observations exist, keyed by the model name.
	LatencyPriors map[string]int `yaml:"latency_priors" json:"latency_priors"`
}

// RetryConfig is the config-file form of the retry policy.
type RetryConfig struct {
	MaxAttempts     int      `yaml:"max_attempts" json:"max_attempts"`
	InitialBackoff  Duration `yaml:"initial_backoff" json:"initial_backoff"`
	MaxBackoff      Duration `yaml:"max_backoff" json:"max_backoff"`
	Multiplier      float64  `yaml:"multiplier" json:"multiplier"`
	Jitter          *bool    `yaml:"jitter" json:"jitter"`
	RetryOn         []string `yaml:"retry_on" json:"retry_on"`
	HonorRetryAfter *bool    `yaml:"honor_retry_after" json:"honor_retry_after"`
}

// TimeoutConfig is the config-file form of the timeout policy.
type TimeoutConfig struct {
	Total      Duration `yaml:"total" json:"total"`
	PerAttempt Duration `yaml:"per_attempt" json:"per_attempt"`
	Connect    Duration `yaml:"connect" json:"connect"`
	StreamIdle Duration `yaml:"stream_idle" json:"stream_idle"`
	FirstToken Duration `yaml:"first_token" json:"first_token"`
}

// FallbackConfig is the config-file form of the fallback policy.
type FallbackConfig struct {
	Enabled               *bool    `yaml:"enabled" json:"enabled"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
	OnErrorCodes          []string `yaml:"on_error_codes" json:"on_error_codes"`
	OnStatusCodes         []int    `yaml:"on_status_codes" json:"on_status_codes"`
	BudgetAware           *bool    `yaml:"budget_aware" json:"budget_aware"`
	BackoffBeforeFailover Duration `yaml:"backoff_before_failover" json:"backoff_before_failover"`
}

// LimitsConfig is the config-file form of policy limits.
type LimitsConfig struct {
	MaxCostPerRequestUSD float64  `yaml:"max_cost_per_request_usd" json:"max_cost_per_request_usd"`
	MaxPromptTokens      int      `yaml:"max_prompt_tokens" json:"max_prompt_tokens"`
	MaxOutputTokens      int      `yaml:"max_output_tokens" json:"max_output_tokens"`
	LatencyTargetMS      int      `yaml:"latency_target_ms" json:"latency_target_ms"`
	MaxLatencyMS         int      `yaml:"max_latency_ms" json:"max_latency_ms"`
	RequestsPerMinute    int      `yaml:"requests_per_minute" json:"requests_per_minute"`
	TokensPerMinute      int      `yaml:"tokens_per_minute" json:"tokens_per_minute"`
	DailyBudgetUSD       float64  `yaml:"daily_budget_usd" json:"daily_budget_usd"`
	MonthlyBudgetUSD     float64  `yaml:"monthly_budget_usd" json:"monthly_budget_usd"`
	AllowedModels        []string `yaml:"allowed_models" json:"allowed_models"`
	DeniedModels         []string `yaml:"denied_models" json:"denied_models"`
	// Phase 2: provider/region/sensitivity/size/mode/cache/shaping guards.
	AllowedProviders            []string `yaml:"allowed_providers" json:"allowed_providers"`
	DeniedProviders             []string `yaml:"denied_providers" json:"denied_providers"`
	AllowedRegions              []string `yaml:"allowed_regions" json:"allowed_regions"`
	DeniedRegions               []string `yaml:"denied_regions" json:"denied_regions"`
	DeniedSensitive             []string `yaml:"denied_sensitive" json:"denied_sensitive"`
	MaxRequestBytes             int      `yaml:"max_request_bytes" json:"max_request_bytes"`
	BatchOnly                   bool     `yaml:"batch_only" json:"batch_only"`
	InteractiveOnly             bool     `yaml:"interactive_only" json:"interactive_only"`
	CacheEnabled                *bool    `yaml:"cache_enabled" json:"cache_enabled"`
	BypassCacheForSensitive     bool     `yaml:"bypass_cache_for_sensitive" json:"bypass_cache_for_sensitive"`
	RequireCacheBypassSensitive bool     `yaml:"require_cache_bypass_sensitive" json:"require_cache_bypass_sensitive"`
	MaxHistoryMessages          int      `yaml:"max_history_messages" json:"max_history_messages"`
}

// ProviderConfig declares an upstream inference provider.
type ProviderConfig struct {
	Name string `yaml:"name" json:"name"`
	// Kind selects the adapter: openai, anthropic, ollama, vllm or
	// openai_compatible.
	Kind    string `yaml:"kind" json:"kind"`
	BaseURL string `yaml:"base_url" json:"base_url"`
	// APIKeyEnv is the preferred credential mechanism: it keeps the secret out
	// of the config file so the file can live in version control.
	APIKeyEnv string `yaml:"api_key_env" json:"api_key_env"`
	// APIKey is an inline credential. Intended for local runtimes and for
	// development; it is redacted from every API response.
	APIKey string `yaml:"api_key" json:"api_key"`
	// AuthStyle is bearer, header or none.
	AuthStyle  string            `yaml:"auth_style" json:"auth_style"`
	HeaderName string            `yaml:"header_name" json:"header_name"`
	Headers    map[string]string `yaml:"headers" json:"headers"`
	// Organization and Project are OpenAI routing identifiers.
	Organization string `yaml:"organization" json:"organization"`
	Project      string `yaml:"project" json:"project"`
	// Capabilities defaults to the kind's natural capability set.
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	// Status defaults to active. Set to disabled to keep a provider configured
	// but out of rotation.
	Status string `yaml:"status" json:"status"`
	// Enabled is a convenience alias for status: active|disabled.
	Enabled *bool `yaml:"enabled" json:"enabled"`
	Weight  int   `yaml:"weight" json:"weight"`
	// Priority orders providers when a policy omits an explicit order.
	Priority       int               `yaml:"priority" json:"priority"`
	Timeout        Duration          `yaml:"timeout" json:"timeout"`
	MaxConcurrency int               `yaml:"max_concurrency" json:"max_concurrency"`
	Region         string            `yaml:"region" json:"region"`
	Labels         map[string]string `yaml:"labels" json:"labels"`
	// Models lists the servable models on this provider.
	Models []ModelConfig `yaml:"models" json:"models"`
}

// ModelConfig declares a servable model.
type ModelConfig struct {
	// Name is the upstream model identifier.
	Name string `yaml:"name" json:"name"`
	// Aliases are additional client-facing names that resolve to this model.
	Aliases     []string `yaml:"aliases" json:"aliases"`
	DisplayName string   `yaml:"display_name" json:"display_name"`
	Version     string   `yaml:"version" json:"version"`
	// ContextWindow is the total token capacity.
	ContextWindow   int `yaml:"context_window" json:"context_window"`
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens"`
	// Capabilities narrows the provider set for this model.
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	// Pricing is USD per one million tokens.
	InputCostPerMillion       float64 `yaml:"input_cost_per_million" json:"input_cost_per_million"`
	OutputCostPerMillion      float64 `yaml:"output_cost_per_million" json:"output_cost_per_million"`
	CachedInputCostPerMillion float64 `yaml:"cached_input_cost_per_million" json:"cached_input_cost_per_million"`
	// QualityTier is an operator-assigned 1..5 signal.
	QualityTier int `yaml:"quality_tier" json:"quality_tier"`
	// Status defaults to active.
	Status       string            `yaml:"status" json:"status"`
	RateLimitRPM int               `yaml:"rate_limit_rpm" json:"rate_limit_rpm"`
	RateLimitTPM int               `yaml:"rate_limit_tpm" json:"rate_limit_tpm"`
	Metadata     map[string]string `yaml:"metadata" json:"metadata"`
}

// PolicyConfig declares a routing policy.
type PolicyConfig struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Priority    int    `yaml:"priority" json:"priority"`
	// Enabled defaults to true so a policy without the field is active.
	Enabled *bool `yaml:"enabled" json:"enabled"`
	// TenantSlug scopes the policy to a tenant; empty means platform-wide.
	TenantSlug string            `yaml:"tenant_slug" json:"tenant_slug"`
	Match      PolicyMatchConfig `yaml:"match" json:"match"`
	Strategy   string            `yaml:"strategy" json:"strategy"`
	// Targets is the ordered candidate list.
	Targets  []TargetConfig  `yaml:"targets" json:"targets"`
	Fallback *FallbackConfig `yaml:"fallback" json:"fallback"`
	Retry    *RetryConfig    `yaml:"retry" json:"retry"`
	Timeout  *TimeoutConfig  `yaml:"timeout" json:"timeout"`
	Limits   *LimitsConfig   `yaml:"limits" json:"limits"`
}

// PolicyMatchConfig is the config-file form of a policy match.
type PolicyMatchConfig struct {
	Models               []string `yaml:"models" json:"models"`
	RequestTypes         []string `yaml:"request_types" json:"request_types"`
	TenantSlugs          []string `yaml:"tenant_slugs" json:"tenant_slugs"`
	APIKeyPrefixes       []string `yaml:"api_key_prefixes" json:"api_key_prefixes"`
	EndpointSlugs        []string `yaml:"endpoint_slugs" json:"endpoint_slugs"`
	MinPromptTokens      int      `yaml:"min_prompt_tokens" json:"min_prompt_tokens"`
	MaxPromptTokens      int      `yaml:"max_prompt_tokens" json:"max_prompt_tokens"`
	RequiredCapabilities []string `yaml:"required_capabilities" json:"required_capabilities"`
	Streaming            *bool    `yaml:"streaming" json:"streaming"`
	// Phase 2: task, batch, region, sensitivity scoping.
	TaskTypes       []string `yaml:"task_types" json:"task_types"`
	Batch           *bool    `yaml:"batch" json:"batch"`
	Regions         []string `yaml:"regions" json:"regions"`
	DataSensitivity []string `yaml:"data_sensitivity" json:"data_sensitivity"`
}

// TargetConfig is one entry in a policy's target list. A target may name either
// a specific provider/model pair or just a model, in which case every provider
// serving that model becomes a candidate.
type TargetConfig struct {
	// Provider is the provider name as declared in providers[].name.
	Provider string `yaml:"provider" json:"provider"`
	// Model is the model name or alias.
	Model string `yaml:"model" json:"model"`
	// Weight biases weighted routing.
	Weight int `yaml:"weight" json:"weight"`
	// Priority overrides list position.
	Priority        int `yaml:"priority" json:"priority"`
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens"`
}

// CacheConfig configures response caching. Phase 1 wires the configuration and
// the store; the gateway records cache-hit telemetry as false until the
// response cache is enabled. Phase 5 adds per-layer switches and safety
// defaults so exact reuse ships first and semantic/prefix stay opt-in.
type CacheConfig struct {
	// ResponseCache enables exact-match response caching in Redis.
	ResponseCache bool `yaml:"response_cache" json:"response_cache"`
	// ResponseTTL bounds cache entry lifetime.
	ResponseTTL Duration `yaml:"response_ttl" json:"response_ttl"`
	// MaxResponseBytes skips caching of very large completions.
	MaxResponseBytes int `yaml:"max_response_bytes" json:"max_response_bytes"`
	// ProviderHealthTTL is how long a health snapshot lives in Redis.
	ProviderHealthTTL Duration `yaml:"provider_health_ttl" json:"provider_health_ttl"`
	// PolicyCacheTTL caches resolved policies in process.
	PolicyCacheTTL Duration `yaml:"policy_cache_ttl" json:"policy_cache_ttl"`
	// RegistryCacheTTL caches the model registry in process.
	RegistryCacheTTL Duration `yaml:"registry_cache_ttl" json:"registry_cache_ttl"`
	// Phase 2: semantic + prefix caches.
	SemanticEnabled   bool    `yaml:"semantic_enabled" json:"semantic_enabled"`
	PrefixEnabled     bool    `yaml:"prefix_enabled" json:"prefix_enabled"`
	SemanticThreshold float64 `yaml:"semantic_threshold" json:"semantic_threshold"`
	PrefixLength      int     `yaml:"prefix_length" json:"prefix_length"`
	// Phase 5: layered switches and safety defaults.
	// ExactEnabled gates exact response reuse independently of the master
	// ResponseCache switch; both must be true for an exact hit to serve.
	ExactEnabled bool `yaml:"exact_enabled" json:"exact_enabled"`
	// BypassToolRequests skips caching for any request carrying tools
	// (default true: tool results may depend on definitions/results that the
	// key cannot fully capture for state-changing tools).
	BypassToolRequests *bool `yaml:"bypass_tool_requests" json:"bypass_tool_requests"`
	// AllowNondeterministic permits caching when temperature is non-zero or N>1
	// (default false: only deterministic or seeded requests are reusable).
	AllowNondeterministic bool `yaml:"allow_nondeterministic" json:"allow_nondeterministic"`
	// BypassLiveData skips caching when the prompt looks like a live-data
	// request (default true).
	BypassLiveData *bool `yaml:"bypass_live_data" json:"bypass_live_data"`
	// MaxSemanticEntries bounds the in-process semantic index.
	MaxSemanticEntries int `yaml:"max_semantic_entries" json:"max_semantic_entries"`
}

// CacheExactEnabled reports whether exact response reuse may serve.
func (c CacheConfig) CacheExactEnabled() bool {
	if !c.ResponseCache {
		return false
	}
	return c.ExactEnabled
}

// CacheBypassTools reports whether tool-carrying requests skip the cache.
func (c CacheConfig) CacheBypassTools() bool {
	if c.BypassToolRequests == nil {
		return true
	}
	return *c.BypassToolRequests
}

// CacheBypassLive reports whether live-data-looking prompts skip the cache.
func (c CacheConfig) CacheBypassLive() bool {
	if c.BypassLiveData == nil {
		return true
	}
	return *c.BypassLiveData
}

// ToolsConfig governs gateway-side tool execution (Phase 4).
//
// The default is client-executed tools only: the gateway forwards tool
// definitions to the provider and returns the model's tool calls untouched,
// so agentic apps (OpenCode, Claude Code, custom agents) run their own
// tools. Gateway execution is opt-in because a gateway that runs tools on a
// streamed request cannot un-send frames, and because silently executing
// tools on behalf of a client that expected to run them itself is a
// correctness hazard.
type ToolsConfig struct {
	// GatewayExecution allows automatic mode: the gateway executes
	// registered, approved tools itself in a bounded loop. When false, an
	// automatic request is clamped to manual (the model still emits tool
	// calls; the client runs them), registry tools are never advertised
	// onto tool-less requests, and built-in tools are not seeded.
	GatewayExecution bool `yaml:"gateway_execution" json:"gateway_execution"`
}

// TunnelConfig governs temporary public tunnels (Cloudflare quick tunnels).
//
// The feature is opt-in: nothing is exposed until an operator creates a
// tunnel through the dashboard or admin API. A tunnel mints a disposable
// *.trycloudflare.com URL that proxies to one local Synapass service; all
// Synapass authentication, policy and rate limiting still apply through it.
type TunnelConfig struct {
	// Enabled allows tunnel creation. When false every tunnel endpoint
	// reports the feature as disabled rather than failing obscurely later.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Binary is the cloudflared executable. An absolute path pins a managed
	// install; the bare name resolves through PATH.
	Binary string `yaml:"binary" json:"binary"`
	// DefaultTarget is used when a create request names no target. One of
	// "gateway", "dashboard" or a loopback "host:port".
	DefaultTarget string `yaml:"default_target" json:"default_target"`
	// DashboardTarget is the local address the "dashboard" target resolves
	// to. Inside Docker this is the compose service name; natively it is
	// usually http://127.0.0.1:3000.
	DashboardTarget string `yaml:"dashboard_target" json:"dashboard_target"`
	// AllowCustomTargets permits explicit loopback "host:port" targets.
	// Named targets always work; this only gates free-form ones.
	AllowCustomTargets bool `yaml:"allow_custom_targets" json:"allow_custom_targets"`
	// AutoRestart respawns the cloudflared process when it exits
	// unexpectedly, keeping the same session row and counting reconnects.
	AutoRestart bool `yaml:"auto_restart" json:"auto_restart"`
	// StopOnShutdown tears the tunnel down during gateway shutdown so a
	// public URL never outlives the process that served it.
	StopOnShutdown bool `yaml:"stop_on_shutdown" json:"stop_on_shutdown"`
	// StartupTimeout bounds how long creation waits for cloudflared to print
	// the public URL before failing the session.
	StartupTimeout Duration `yaml:"startup_timeout" json:"startup_timeout"`
}

// ClassifierConfig tunes task classification.
type ClassifierConfig struct {
	Enabled           bool `yaml:"enabled" json:"enabled"`
	LongContextTokens int  `yaml:"long_context_tokens" json:"long_context_tokens"`
	BatchTokens       int  `yaml:"batch_tokens" json:"batch_tokens"`
}

// ShapingConfig tunes prompt shaping.
type ShapingConfig struct {
	Enabled            bool     `yaml:"enabled" json:"enabled"`
	MaxHistoryMessages int      `yaml:"max_history_messages" json:"max_history_messages"`
	MaxPromptTokens    int      `yaml:"max_prompt_tokens" json:"max_prompt_tokens"`
	SystemPrefix       string   `yaml:"system_prefix" json:"system_prefix"`
	SystemSuffix       string   `yaml:"system_suffix" json:"system_suffix"`
	Guardrails         []string `yaml:"guardrails" json:"guardrails"`
}

// ScoringConfig tunes provider scoring.
type ScoringConfig struct {
	Enabled        bool     `yaml:"enabled" json:"enabled"`
	Window         Duration `yaml:"window" json:"window"`
	SuccessWeight  float64  `yaml:"success_weight" json:"success_weight"`
	LatencyWeight  float64  `yaml:"latency_weight" json:"latency_weight"`
	CostWeight     float64  `yaml:"cost_weight" json:"cost_weight"`
	FeedbackWeight float64  `yaml:"feedback_weight" json:"feedback_weight"`
}

// GuardrailsConfig toggles production controls.
type GuardrailsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// EvalConfig tunes eval/replay.
type EvalConfig struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	MaxRequests int  `yaml:"max_requests" json:"max_requests"`
	WorkerPool  int  `yaml:"worker_pool" json:"worker_pool"`
}

// DetectionConfig tunes automatic capability detection.
//
// Probing spends real upstream tokens — four minimal completions per model by
// default — so everything here is off or capped. The endpoint that runs
// detection stays available regardless, because an explicit operator action
// needs no further permission.
type DetectionConfig struct {
	// Enabled runs detection automatically after model discovery for models
	// that declare no capabilities. Off by default: it is the operator's call
	// whether a discovery run may spend upstream tokens.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MaxModelsPerRun caps how many models one automatic run touches.
	MaxModelsPerRun int `yaml:"max_models_per_run" json:"max_models_per_run"`
	// Concurrency caps simultaneous probe calls in one run.
	Concurrency int `yaml:"concurrency" json:"concurrency"`
	// TimeoutPerModel bounds all probes for one model, e.g. "90s".
	TimeoutPerModel Duration `yaml:"timeout_per_model" json:"timeout_per_model"`
}

// BenchmarkConfig governs the Autonomous Benchmark Engine.
//
// The engine is a fully independent, enterprise-grade module that continuously
// benchmarks providers and models, stores every result permanently, and powers
// a Grafana-style analytics dashboard. It is DISABLED by default and must
// never affect gateway performance unless explicitly enabled via
// ENABLE_BENCHMARK_ENGINE=true (or benchmark.enabled: true).
//
// Architecture when enabled:
//
//	Dashboard → Benchmark API → Scheduler → Redis Queue → Workers → Gateway → Providers → Database
//
// The module is self-contained (internal/benchmark) so it can later be
// extracted into its own service or sold as a premium enterprise feature.
type BenchmarkConfig struct {
	// Enabled is the master switch. False (default) hides the Benchmark page,
	// starts no scheduler or workers, and disables every benchmark API route.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// SchedulerEnabled gates the cron-style scheduler independently. It only
	// takes effect when Enabled is true.
	SchedulerEnabled bool `yaml:"scheduler_enabled" json:"scheduler_enabled"`
	// Workers is the number of in-process benchmark workers. Workers also
	// scale horizontally: every replica drains the same Redis queue.
	Workers int `yaml:"workers" json:"workers"`
	// QueueSize bounds the in-memory fallback queue when Redis is absent.
	QueueSize int `yaml:"queue_size" json:"queue_size"`
	// DefaultTimeout bounds a single benchmark probe, e.g. "60s".
	DefaultTimeout Duration `yaml:"default_timeout" json:"default_timeout"`
	// DefaultConcurrency caps simultaneous probes inside one run.
	DefaultConcurrency int `yaml:"default_concurrency" json:"default_concurrency"`
	// MaxDailyCostUSD is the global daily spend ceiling for benchmarks.
	// 0 means unlimited; per-job ceilings still apply.
	MaxDailyCostUSD float64 `yaml:"max_daily_cost_usd" json:"max_daily_cost_usd"`
	// RetentionDays bounds pruning of benchmark results. 0 keeps everything
	// forever (the default: results are never overwritten).
	RetentionDays int `yaml:"retention_days" json:"retention_days"`
	// RedisQueueKey is the Redis list key for the benchmark queue.
	RedisQueueKey string `yaml:"redis_queue_key" json:"redis_queue_key"`
}

// BenchmarkEnabled reports whether the engine may run at all.
func (c BenchmarkConfig) BenchmarkEnabled() bool { return c.Enabled }

// TelemetryConfig configures observability.
type TelemetryConfig struct {
	// ServiceName overrides app.name for OTel resource attributes.
	ServiceName string `yaml:"service_name" json:"service_name"`
	// OTLPEndpoint is the collector endpoint, e.g. "localhost:4318".
	OTLPEndpoint string `yaml:"otlp_endpoint" json:"otlp_endpoint"`
	// OTLPInsecure disables TLS to the collector, correct for a sidecar.
	OTLPInsecure bool `yaml:"otlp_insecure" json:"otlp_insecure"`
	// OTLPHeaders are sent on every export request, e.g. an API key.
	OTLPHeaders map[string]string `yaml:"otlp_headers" json:"otlp_headers"`
	// TracesEnabled, MetricsEnabled and LogsEnabled gate each signal.
	TracesEnabled  bool `yaml:"traces_enabled" json:"traces_enabled"`
	MetricsEnabled bool `yaml:"metrics_enabled" json:"metrics_enabled"`
	LogsEnabled    bool `yaml:"logs_enabled" json:"logs_enabled"`
	// TraceSampleRatio is the head sampling ratio in [0,1].
	TraceSampleRatio float64 `yaml:"trace_sample_ratio" json:"trace_sample_ratio"`
	// PrometheusEnabled exposes /metrics.
	PrometheusEnabled bool `yaml:"prometheus_enabled" json:"prometheus_enabled"`
	// PrometheusPath is the metrics route.
	PrometheusPath string `yaml:"prometheus_path" json:"prometheus_path"`
	// ExportInterval is the OTLP metric export cadence.
	ExportInterval Duration `yaml:"export_interval" json:"export_interval"`
	// TraceBufferSize is the in-memory ClickHouse trace buffer capacity.
	TraceBufferSize int `yaml:"trace_buffer_size" json:"trace_buffer_size"`
	// PersistTraces stores full traces in ClickHouse in addition to OTel export.
	PersistTraces bool `yaml:"persist_traces" json:"persist_traces"`
}

// LoggingConfig configures the structured logger.
type LoggingConfig struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level" json:"level"`
	// Format is json or console.
	Format string `yaml:"format" json:"format"`
	// AddSource includes the caller file and line, which is useful in
	// development and costly in production.
	AddSource bool `yaml:"add_source" json:"add_source"`
	// RedactHeaders lists header names removed from logs.
	RedactHeaders []string `yaml:"redact_headers" json:"redact_headers"`
	// LogRequestBodies enables request body logging. It must stay off by
	// default: prompts routinely contain personal data.
	LogRequestBodies bool `yaml:"log_request_bodies" json:"log_request_bodies"`
	// SlowRequestThreshold logs a warning for requests slower than this.
	SlowRequestThreshold Duration `yaml:"slow_request_threshold" json:"slow_request_threshold"`
}

// AdminConfig configures administrative surface area.
type AdminConfig struct {
	// Enabled exposes the /admin endpoints.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// RequireScope enforces ScopeAdminAll on every admin route.
	RequireScope bool `yaml:"require_scope" json:"require_scope"`
	// UsageRetentionDays controls pruning of the usage table.
	UsageRetentionDays int `yaml:"usage_retention_days" json:"usage_retention_days"`
	// TraceRetentionDays controls pruning of ClickHouse traces.
	TraceRetentionDays int `yaml:"trace_retention_days" json:"trace_retention_days"`
	// LogRetentionDays controls pruning of the request log table.
	LogRetentionDays int `yaml:"log_retention_days" json:"log_retention_days"`
	// DashboardAuth configures the console's own username/password login.
	//
	// This is a separate credential type from API keys on purpose: an operator
	// chooses a password, so it is stretched with Argon2id and the account is
	// lockable, whereas a key is high-entropy material verified by digest lookup.
	DashboardAuth DashboardAuthConfig `yaml:"dashboard_auth" json:"dashboard_auth"`
}

// DashboardAuthConfig governs human operator login for the dashboard.
type DashboardAuthConfig struct {
	// Enabled exposes the setup, login and session endpoints.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// CookieName is the session cookie. Renaming it logs everyone out.
	CookieName string `yaml:"cookie_name" json:"cookie_name"`
	// CookieSecure forces the Secure attribute. It is set automatically when the
	// gateway is in production, and can be forced on for a TLS-terminating proxy
	// in development.
	CookieSecure bool `yaml:"cookie_secure" json:"cookie_secure"`
	// SessionTTL is the absolute session lifetime.
	SessionTTL Duration `yaml:"session_ttl" json:"session_ttl"`
	// IdleTTL expires a session not seen for this long. Zero disables it.
	IdleTTL Duration `yaml:"idle_ttl" json:"idle_ttl"`
	// MinPasswordLength is the enforced minimum. The floor is 12: a shorter
	// minimum is not a configuration choice, it is a weakness.
	MinPasswordLength int `yaml:"min_password_length" json:"min_password_length"`
	// MaxPasswordLength bounds the Argon2 input.
	MaxPasswordLength int `yaml:"max_password_length" json:"max_password_length"`
	// MaxFailedAttempts is the consecutive-failure count that triggers a lockout.
	MaxFailedAttempts int `yaml:"max_failed_attempts" json:"max_failed_attempts"`
	// LockoutDuration is the base lockout window.
	LockoutDuration Duration `yaml:"lockout_duration" json:"lockout_duration"`
	// MaxLockoutDuration bounds the doubling that punishes sustained guessing.
	MaxLockoutDuration Duration `yaml:"max_lockout_duration" json:"max_lockout_duration"`
}

// Default returns a configuration with every field set to a usable value.
//
// Defaults favour "works out of the box for a single-host install" (localhost
// datastores, dev environment, no mandatory external dependency) because the
// native install path must not require a config file to boot. Production
// deployments override via environment variables or a config file.
func Default() *Config {
	return &Config{
		App: AppConfig{
			Name:                "synapass",
			Environment:         "development",
			DefaultTenantSlug:   "default",
			DefaultTenantName:   "Default Tenant",
			BootstrapFromConfig: true,
		},
		HTTP: HTTPConfig{
			Addr:               ":8080",
			ReadHeaderTimeout:  Duration(10 * time.Second),
			ReadTimeout:        Duration(30 * time.Second),
			WriteTimeout:       Duration(15 * time.Minute),
			IdleTimeout:        Duration(90 * time.Second),
			ShutdownTimeout:    Duration(30 * time.Second),
			MaxBodyBytes:       8 << 20, // 8 MiB is generous for a long prompt.
			CORSAllowedOrigins: []string{"http://localhost:3000"},
		},
		Database: DatabaseConfig{
			Host:             "localhost",
			Port:             5432,
			User:             "synapass",
			Password:         "synapass",
			Name:             "synapass",
			SSLMode:          "disable",
			MaxConns:         20,
			MinConns:         2,
			MaxConnLifetime:  Duration(time.Hour),
			MaxConnIdleTime:  Duration(30 * time.Minute),
			ConnectTimeout:   Duration(10 * time.Second),
			StatementTimeout: Duration(30 * time.Second),
			AutoMigrate:      true,
			MigrationsDir:    "migrations",
		},
		Redis: RedisConfig{
			Addr:         "localhost:6379",
			PoolSize:     20,
			DialTimeout:  Duration(5 * time.Second),
			ReadTimeout:  Duration(3 * time.Second),
			WriteTimeout: Duration(3 * time.Second),
			KeyPrefix:    "synapass",
		},
		ClickHouse: ClickHouseConfig{
			Addr:          "localhost:9000",
			Database:      "synapass",
			BatchSize:     500,
			FlushInterval: Duration(2 * time.Second),
			MaxRetries:    3,
		},
		NATS: NATSConfig{
			URL:            "nats://localhost:4222",
			Name:           "synapass-gateway",
			JetStream:      true,
			StreamReplicas: 1,
			MaxReconnects:  -1,
			ReconnectWait:  Duration(2 * time.Second),
		},
		Auth: AuthConfig{
			MinKeyLength: 20,
			KeyPrefix:    "syn_live_",
			CacheTTL:     Duration(30 * time.Second),
			AdminKeyEnv:  "SYNAPASS_ADMIN_KEY",
		},
		Routing: RoutingConfig{
			DefaultStrategy:    "priority",
			DefaultMaxAttempts: 2,
			DefaultRetry: RetryConfig{
				MaxAttempts:    2,
				InitialBackoff: Duration(150 * time.Millisecond),
				MaxBackoff:     Duration(2 * time.Second),
				Multiplier:     2.0,
			},
			DefaultTimeout: TimeoutConfig{
				// Generous enough that a long answer completes; tight enough that a
				// stuck upstream is abandoned. Per-request policy timeouts override.
				Total:      Duration(10 * time.Minute),
				PerAttempt: Duration(5 * time.Minute),
				Connect:    Duration(10 * time.Second),
				StreamIdle: Duration(90 * time.Second),
				FirstToken: Duration(60 * time.Second),
			},
			DefaultLimits: LimitsConfig{
				// 32768 is a ceiling, not a target: it stops a runaway generation
				// without cutting off an ordinary long answer. The client may ask
				// for less; a client asking for more is clamped to this.
				MaxOutputTokens:   32768,
				LatencyTargetMS:   10000,
				RequestsPerMinute: 600,
			},
			HealthCheckEnabled:  true,
			HealthCheckInterval: Duration(30 * time.Second),
			HealthCheckTimeout:  Duration(10 * time.Second),
			HealthWindow:        Duration(5 * time.Minute),
		},
		Cache: CacheConfig{
			ResponseCache:         false,
			ResponseTTL:           Duration(5 * time.Minute),
			MaxResponseBytes:      256 << 10,
			ProviderHealthTTL:     Duration(90 * time.Second),
			PolicyCacheTTL:        Duration(30 * time.Second),
			RegistryCacheTTL:      Duration(30 * time.Second),
			SemanticEnabled:       true,
			PrefixEnabled:         true,
			SemanticThreshold:     0.92,
			PrefixLength:          256,
			ExactEnabled:          true,
			AllowNondeterministic: false,
			MaxSemanticEntries:    2000,
		},
		Tools: ToolsConfig{
			// Off by default: agentic clients run their own tools. Set
			// tools.gateway_execution: true (or SYNAPASS_TOOLS_GATEWAY_EXECUTION=true)
			// to let the gateway execute registered tools itself.
			GatewayExecution: false,
		},
		Tunnel: TunnelConfig{
			// Conservative floor for a config file that omits the key. Every
			// template the project ships (config.example.yaml and both native
			// installers) sets this true, so a stock install is enabled; only a
			// hand-written config that leaves it out lands here.
			//
			// Enabled is not the same as exposed: nothing is public until an
			// operator creates a tunnel from the dashboard or the admin API.
			Enabled:            false,
			Binary:             "cloudflared",
			DefaultTarget:      "gateway",
			DashboardTarget:    "http://127.0.0.1:3000",
			AllowCustomTargets: true,
			AutoRestart:        true,
			StopOnShutdown:     true,
			StartupTimeout:     Duration(60 * time.Second),
		},
		Classifier: ClassifierConfig{
			Enabled:           true,
			LongContextTokens: 32000,
			BatchTokens:       64000,
		},
		Shaping: ShapingConfig{
			Enabled:            true,
			MaxHistoryMessages: 50,
		},
		Scoring: ScoringConfig{
			Enabled:        true,
			Window:         Duration(time.Hour),
			SuccessWeight:  0.5,
			LatencyWeight:  0.25,
			CostWeight:     0.15,
			FeedbackWeight: 0.1,
		},
		Guardrails: GuardrailsConfig{Enabled: true},
		Eval: EvalConfig{
			Enabled:     true,
			MaxRequests: 100,
			WorkerPool:  4,
		},
		Detection: DetectionConfig{
			Enabled:         false,
			MaxModelsPerRun: 10,
			Concurrency:     2,
			TimeoutPerModel: Duration(90 * time.Second),
		},
		Benchmark: BenchmarkConfig{
			// Disabled by default: zero impact on the inference gateway
			// unless the operator opts in explicitly.
			Enabled:            false,
			SchedulerEnabled:   true,
			Workers:            2,
			QueueSize:          1000,
			DefaultTimeout:     Duration(60 * time.Second),
			DefaultConcurrency: 5,
			MaxDailyCostUSD:    0,
			RetentionDays:      0,
			RedisQueueKey:      "benchmark:queue",
		},
		Telemetry: TelemetryConfig{
			TracesEnabled:     true,
			MetricsEnabled:    true,
			LogsEnabled:       false,
			TraceSampleRatio:  1.0,
			PrometheusEnabled: true,
			PrometheusPath:    "/metrics",
			ExportInterval:    Duration(15 * time.Second),
			TraceBufferSize:   1024,
			PersistTraces:     true,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
			RedactHeaders: []string{
				"authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization",
			},
			SlowRequestThreshold: Duration(10 * time.Second),
		},
		Admin: AdminConfig{
			Enabled:            true,
			RequireScope:       true,
			UsageRetentionDays: 90,
			TraceRetentionDays: 30,
			LogRetentionDays:   30,
			DashboardAuth: DashboardAuthConfig{
				Enabled:            true,
				CookieName:         "synapass_session",
				CookieSecure:       false,
				SessionTTL:         Duration(12 * time.Hour),
				IdleTTL:            Duration(2 * time.Hour),
				MinPasswordLength:  12,
				MaxPasswordLength:  256,
				MaxFailedAttempts:  5,
				LockoutDuration:    Duration(30 * time.Second),
				MaxLockoutDuration: Duration(15 * time.Minute),
			},
		},
	}
}

// Load resolves configuration from defaults, an optional file and the
// environment.
//
// A missing file is not an error when the path came from the default search
// order, because a native install may legitimately be configured entirely
// through environment variables. An explicitly requested path that does not
// exist is an error, since silently ignoring it would hide a typo.
func Load(path string, explicit bool) (*Config, error) {
	cfg, err := LoadUnchecked(path, explicit)
	if err != nil {
		return nil, err
	}
	if err := cfg.Finalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadUnchecked resolves configuration from defaults, an optional file and the
// environment without finalizing it: no materialization, no validation.
//
// It exists for tooling that must run against a broken configuration —
// `synapass native doctor` reports validation problems instead of refusing
// to start, and `native install` writes the files that fix them. Callers take
// over the Finalize step and decide what a validation failure means for them.
func LoadUnchecked(path string, explicit bool) (*Config, error) {
	cfg := Default()

	resolved, err := ResolveConfigPath(path, explicit)
	if err != nil {
		return nil, err
	}
	if resolved != "" {
		if err := cfg.loadFile(resolved); err != nil {
			return nil, err
		}
		cfg.ConfigFile = resolved
	}

	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DefaultSearchPaths returns the config file locations tried in order when no
// path is given. The systemd unit uses /etc/synapass; the container image
// ships /etc/synapass/config.yaml; a developer typically relies on
// synapass.yaml in the working directory.
func DefaultSearchPaths() []string {
	return []string{
		"synapass.yaml",
		"synapass.yml",
		"synapass.json",
		filepath.Join("config", "synapass.yaml"),
		"/etc/synapass/config.yaml",
		"/etc/synapass/config.yml",
	}
}

// ResolveConfigPath determines which config file to load.
func ResolveConfigPath(path string, explicit bool) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			if explicit {
				return "", fmt.Errorf("config file %q: %w", path, err)
			}
			return "", nil
		}
		return path, nil
	}
	if envPath := firstEnv("SYNAPASS_CONFIG_FILE"); envPath != "" {
		if _, err := os.Stat(envPath); err != nil {
			return "", fmt.Errorf("config file from SYNAPASS_CONFIG_FILE %q: %w", envPath, err)
		}
		return envPath, nil
	}
	for _, candidate := range DefaultSearchPaths() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", nil
}

// loadFile reads and unmarshals a YAML or JSON config file into cfg, on top of
// the defaults already present.
//
// Decoding into the existing struct (rather than into a fresh one) is what makes
// partial config files work: a file that sets only database.host leaves every
// other default intact.
func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	// yaml.v3 parses JSON too, because JSON is a subset of YAML, so a single
	// decoder handles both extensions without sniffing the content.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	return nil
}

// Finalize normalizes derived values, resolves secrets from the environment and
// validates the result. It runs after every source has been applied, so it is
// the single place that guarantees a post-condition-valid configuration.
func (c *Config) Finalize() error {
	if c.App.InstanceID == "" {
		if host, err := os.Hostname(); err == nil {
			c.App.InstanceID = host
		} else {
			c.App.InstanceID = "unknown"
		}
	}
	if c.Telemetry.ServiceName == "" {
		c.Telemetry.ServiceName = c.App.Name
	}
	if c.Auth.AdminKey == "" && c.Auth.AdminKeyEnv != "" {
		c.Auth.AdminKey = os.Getenv(c.Auth.AdminKeyEnv)
	}
	// Materialize the DSN once so every consumer sees identical connection
	// settings regardless of which form the operator used.
	if c.Database.DSN == "" {
		c.Database.DSN = c.Database.buildDSN()
	}
	if len(c.NATS.URLs) > 0 && c.NATS.URL == "" {
		c.NATS.URL = c.NATS.URLs[0]
	}
	return c.Validate()
}

// buildDSN assembles a libpq connection string from the discrete fields.
func (d DatabaseConfig) buildDSN() string {
	ssl := d.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s&connect_timeout=%d&statement_timeout=%d",
		urlEscape(d.User), urlEscape(d.Password), d.Host, d.Port, d.Name, ssl,
		int(d.ConnectTimeout.Std().Seconds()), int(d.StatementTimeout.Std().Milliseconds()),
	)
}

// urlEscape percent-encodes the characters that would otherwise terminate a
// userinfo or query component in a connection URI. Credentials with an '@' or
// ':' are common enough that skipping this breaks real deployments.
func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// Redacted returns a copy safe to log or return over the admin API: every
// secret is replaced with a presence marker rather than its value.
func (c *Config) Redacted() *Config {
	clone := *c
	clone.Database.Password = redactSecret(c.Database.Password)
	clone.Database.DSN = redactDSN(c.Database.DSN)
	clone.Redis.Password = redactSecret(c.Redis.Password)
	clone.ClickHouse.Password = redactSecret(c.ClickHouse.Password)
	clone.ClickHouse.DSN = redactDSN(c.ClickHouse.DSN)
	clone.NATS.Token = redactSecret(c.NATS.Token)
	clone.Auth.AdminKey = redactSecret(c.Auth.AdminKey)

	clone.Providers = make([]ProviderConfig, len(c.Providers))
	for i, p := range c.Providers {
		p.APIKey = redactSecret(p.APIKey)
		clone.Providers[i] = p
	}
	return &clone
}

func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// redactDSN removes the password component of a connection URI while keeping
// the host and database visible, which is what an operator needs to diagnose a
// misconfiguration.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	schemeEnd := strings.Index(dsn, "://")
	if schemeEnd < 0 || schemeEnd > at {
		return "***"
	}
	userinfo := dsn[schemeEnd+3 : at]
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return dsn
	}
	return dsn[:schemeEnd+3] + userinfo[:colon] + ":***" + dsn[at:]
}

// IsProduction reports whether the deployment should enforce strict settings.
func (c *Config) IsProduction() bool {
	return strings.EqualFold(c.App.Environment, "production") ||
		strings.EqualFold(c.App.Environment, "prod")
}

// IsDevelopment reports whether relaxed development conveniences apply.
func (c *Config) IsDevelopment() bool {
	return !c.IsProduction()
}
