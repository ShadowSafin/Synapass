package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is the namespace for Synapass's own environment variables. Every
// setting is reachable as SYNAPASS_<PATH>, so a container or systemd unit can be fully
// configured without a config file.
const EnvPrefix = "SYNAPASS_"

// firstEnv returns the value of the first environment variable that is set to a
// non-empty string.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func envString(dst *string, names ...string) {
	if v := firstEnv(names...); v != "" {
		*dst = v
	}
}

func envBool(dst *bool, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
		*dst = b
	}
}

// envBoolPtr sets an optional *bool from the environment, allocating only
// when the variable is present so a missing var keeps the nil default.
func envBoolPtr(dst **bool, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
		*dst = &b
	}
}

func envInt(dst *int, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil {
		*dst = n
	}
}

func envInt32(dst *int32, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 32); err == nil {
		*dst = int32(n)
	}
}

func envInt64(dst *int64, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		*dst = n
	}
}

func envFloat(dst *float64, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		*dst = f
	}
}

func envDuration(dst *Duration, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	if d, err := time.ParseDuration(v); err == nil {
		*dst = Duration(d)
		return
	}
	// Bare numbers are seconds, matching the config file's behaviour.
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		*dst = Duration(time.Duration(secs * float64(time.Second)))
	}
}

// envSlice splits a comma-separated list, trimming whitespace and dropping empty
// entries so trailing commas in a systemd Environment= line are harmless.
func envSlice(dst *[]string, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	*dst = out
}

// envMap parses "key=value,key=value" pairs.
func envMap(dst *map[string]string, names ...string) {
	v := firstEnv(names...)
	if v == "" {
		return
	}
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k, val = strings.TrimSpace(k), strings.TrimSpace(val)
		if k == "" {
			continue
		}
		out[k] = val
	}
	*dst = out
}

// applyEnv overlays environment variables onto the configuration.
//
// The mapping is written out explicitly rather than derived by reflecting over
// struct tags. That is more code, but it means the set of environment variables
// is discoverable by reading one file, and a renamed struct field cannot
// silently change the operator-facing interface.
func (c *Config) applyEnv() error {
	// ---- app ----
	envString(&c.App.Name, "SYNAPASS_APP_NAME")
	envString(&c.App.Environment, "SYNAPASS_ENVIRONMENT", "SYNAPASS_ENV")
	envString(&c.App.InstanceID, "SYNAPASS_INSTANCE_ID")
	envString(&c.App.Region, "SYNAPASS_REGION")
	envString(&c.App.DefaultTenantSlug, "SYNAPASS_DEFAULT_TENANT_SLUG")
	envString(&c.App.DefaultTenantName, "SYNAPASS_DEFAULT_TENANT_NAME")
	envBool(&c.App.BootstrapFromConfig, "SYNAPASS_BOOTSTRAP_FROM_CONFIG")

	// ---- http ----
	envString(&c.HTTP.Addr, "SYNAPASS_HTTP_ADDR", "SYNAPASS_LISTEN_ADDR", "PORT_ADDR")
	envDuration(&c.HTTP.ReadHeaderTimeout, "SYNAPASS_HTTP_READ_HEADER_TIMEOUT")
	envDuration(&c.HTTP.ReadTimeout, "SYNAPASS_HTTP_READ_TIMEOUT")
	envDuration(&c.HTTP.WriteTimeout, "SYNAPASS_HTTP_WRITE_TIMEOUT")
	envDuration(&c.HTTP.IdleTimeout, "SYNAPASS_HTTP_IDLE_TIMEOUT")
	envDuration(&c.HTTP.ShutdownTimeout, "SYNAPASS_HTTP_SHUTDOWN_TIMEOUT")
	envInt64(&c.HTTP.MaxBodyBytes, "SYNAPASS_HTTP_MAX_BODY_BYTES")
	envSlice(&c.HTTP.TrustedProxies, "SYNAPASS_HTTP_TRUSTED_PROXIES")
	envSlice(&c.HTTP.CORSAllowedOrigins, "SYNAPASS_HTTP_CORS_ALLOWED_ORIGINS", "SYNAPASS_CORS_ALLOWED_ORIGINS")
	envBool(&c.HTTP.EnablePprof, "SYNAPASS_HTTP_ENABLE_PPROF")

	// ---- database ----
	// A single DATABASE_URL is the conventional container spelling; the SYNAPASS_
	// prefixed aliases exist so the gateway can share a host with other
	// services without variable collisions.
	envString(&c.Database.DSN, "SYNAPASS_DATABASE_DSN", "SYNAPASS_POSTGRES_DSN", "DATABASE_URL")
	envString(&c.Database.Host, "SYNAPASS_POSTGRES_HOST", "SYNAPASS_DATABASE_HOST")
	envInt(&c.Database.Port, "SYNAPASS_POSTGRES_PORT", "SYNAPASS_DATABASE_PORT")
	envString(&c.Database.User, "SYNAPASS_POSTGRES_USER", "SYNAPASS_DATABASE_USER")
	envString(&c.Database.Password, "SYNAPASS_POSTGRES_PASSWORD", "SYNAPASS_DATABASE_PASSWORD")
	envString(&c.Database.Name, "SYNAPASS_POSTGRES_DB", "SYNAPASS_POSTGRES_NAME", "SYNAPASS_DATABASE_NAME")
	envString(&c.Database.SSLMode, "SYNAPASS_POSTGRES_SSL_MODE", "SYNAPASS_DATABASE_SSL_MODE")
	envInt32(&c.Database.MaxConns, "SYNAPASS_POSTGRES_MAX_CONNS")
	envInt32(&c.Database.MinConns, "SYNAPASS_POSTGRES_MIN_CONNS")
	envDuration(&c.Database.MaxConnLifetime, "SYNAPASS_POSTGRES_MAX_CONN_LIFETIME")
	envDuration(&c.Database.MaxConnIdleTime, "SYNAPASS_POSTGRES_MAX_CONN_IDLE_TIME")
	envDuration(&c.Database.ConnectTimeout, "SYNAPASS_POSTGRES_CONNECT_TIMEOUT")
	envDuration(&c.Database.StatementTimeout, "SYNAPASS_POSTGRES_STATEMENT_TIMEOUT")
	envBool(&c.Database.AutoMigrate, "SYNAPASS_POSTGRES_AUTO_MIGRATE", "SYNAPASS_AUTO_MIGRATE")
	envString(&c.Database.MigrationsDir, "SYNAPASS_MIGRATIONS_DIR")

	// ---- redis ----
	envString(&c.Redis.Addr, "SYNAPASS_REDIS_ADDR", "REDIS_URL")
	envString(&c.Redis.Username, "SYNAPASS_REDIS_USERNAME")
	envString(&c.Redis.Password, "SYNAPASS_REDIS_PASSWORD")
	envInt(&c.Redis.DB, "SYNAPASS_REDIS_DB")
	envSlice(&c.Redis.SentinelAddrs, "SYNAPASS_REDIS_SENTINEL_ADDRS")
	envString(&c.Redis.MasterName, "SYNAPASS_REDIS_MASTER_NAME")
	envInt(&c.Redis.PoolSize, "SYNAPASS_REDIS_POOL_SIZE")
	envDuration(&c.Redis.DialTimeout, "SYNAPASS_REDIS_DIAL_TIMEOUT")
	envDuration(&c.Redis.ReadTimeout, "SYNAPASS_REDIS_READ_TIMEOUT")
	envDuration(&c.Redis.WriteTimeout, "SYNAPASS_REDIS_WRITE_TIMEOUT")
	envString(&c.Redis.KeyPrefix, "SYNAPASS_REDIS_KEY_PREFIX")
	envBool(&c.Redis.TLS, "SYNAPASS_REDIS_TLS")
	envBool(&c.Redis.Required, "SYNAPASS_REDIS_REQUIRED")

	// ---- clickhouse ----
	envString(&c.ClickHouse.DSN, "SYNAPASS_CLICKHOUSE_DSN")
	envString(&c.ClickHouse.Addr, "SYNAPASS_CLICKHOUSE_ADDR")
	envString(&c.ClickHouse.Database, "SYNAPASS_CLICKHOUSE_DB", "SYNAPASS_CLICKHOUSE_DATABASE")
	envString(&c.ClickHouse.Username, "SYNAPASS_CLICKHOUSE_USERNAME", "SYNAPASS_CLICKHOUSE_USER")
	envString(&c.ClickHouse.Password, "SYNAPASS_CLICKHOUSE_PASSWORD")
	envBool(&c.ClickHouse.TLS, "SYNAPASS_CLICKHOUSE_TLS")
	envInt(&c.ClickHouse.BatchSize, "SYNAPASS_CLICKHOUSE_BATCH_SIZE")
	envDuration(&c.ClickHouse.FlushInterval, "SYNAPASS_CLICKHOUSE_FLUSH_INTERVAL")
	envInt(&c.ClickHouse.MaxRetries, "SYNAPASS_CLICKHOUSE_MAX_RETRIES")
	envBool(&c.ClickHouse.Required, "SYNAPASS_CLICKHOUSE_REQUIRED")

	// ---- nats ----
	envString(&c.NATS.URL, "SYNAPASS_NATS_URL", "NATS_URL")
	envSlice(&c.NATS.URLs, "SYNAPASS_NATS_URLS")
	envString(&c.NATS.Name, "SYNAPASS_NATS_NAME")
	envString(&c.NATS.CredentialsFile, "SYNAPASS_NATS_CREDENTIALS_FILE")
	envString(&c.NATS.NKeySeedFile, "SYNAPASS_NATS_NKEY_SEED_FILE")
	envString(&c.NATS.Token, "SYNAPASS_NATS_TOKEN")
	envBool(&c.NATS.JetStream, "SYNAPASS_NATS_JETSTREAM")
	envInt(&c.NATS.StreamReplicas, "SYNAPASS_NATS_STREAM_REPLICAS")
	envInt(&c.NATS.MaxReconnects, "SYNAPASS_NATS_MAX_RECONNECTS")
	envDuration(&c.NATS.ReconnectWait, "SYNAPASS_NATS_RECONNECT_WAIT")
	envBool(&c.NATS.Required, "SYNAPASS_NATS_REQUIRED")

	// ---- auth ----
	envString(&c.Auth.HeaderName, "SYNAPASS_AUTH_HEADER_NAME")
	envInt(&c.Auth.MinKeyLength, "SYNAPASS_AUTH_MIN_KEY_LENGTH")
	envString(&c.Auth.KeyPrefix, "SYNAPASS_AUTH_KEY_PREFIX")
	envDuration(&c.Auth.CacheTTL, "SYNAPASS_AUTH_CACHE_TTL")
	envString(&c.Auth.AdminKeyEnv, "SYNAPASS_ADMIN_KEY_ENV")
	// Accept the admin key directly as well, which is how the container image is
	// configured by most orchestrators.
	envString(&c.Auth.AdminKey, "SYNAPASS_ADMIN_KEY")
	envString(&c.Auth.AllowAnonymousTenant, "SYNAPASS_AUTH_ALLOW_ANONYMOUS_TENANT")

	// ---- routing defaults ----
	envString(&c.Routing.DefaultStrategy, "SYNAPASS_ROUTING_DEFAULT_STRATEGY")
	envInt(&c.Routing.DefaultMaxAttempts, "SYNAPASS_ROUTING_DEFAULT_MAX_ATTEMPTS")
	envInt(&c.Routing.DefaultRetry.MaxAttempts, "SYNAPASS_ROUTING_RETRY_MAX_ATTEMPTS")
	envDuration(&c.Routing.DefaultRetry.InitialBackoff, "SYNAPASS_ROUTING_RETRY_INITIAL_BACKOFF")
	envDuration(&c.Routing.DefaultRetry.MaxBackoff, "SYNAPASS_ROUTING_RETRY_MAX_BACKOFF")
	envFloat(&c.Routing.DefaultRetry.Multiplier, "SYNAPASS_ROUTING_RETRY_MULTIPLIER")
	envDuration(&c.Routing.DefaultTimeout.Total, "SYNAPASS_ROUTING_TIMEOUT_TOTAL")
	envDuration(&c.Routing.DefaultTimeout.PerAttempt, "SYNAPASS_ROUTING_TIMEOUT_PER_ATTEMPT")
	envDuration(&c.Routing.DefaultTimeout.Connect, "SYNAPASS_ROUTING_TIMEOUT_CONNECT")
	envDuration(&c.Routing.DefaultTimeout.StreamIdle, "SYNAPASS_ROUTING_TIMEOUT_STREAM_IDLE")
	envDuration(&c.Routing.DefaultTimeout.FirstToken, "SYNAPASS_ROUTING_TIMEOUT_FIRST_TOKEN")
	envInt(&c.Routing.DefaultLimits.MaxOutputTokens, "SYNAPASS_ROUTING_MAX_OUTPUT_TOKENS")
	envInt(&c.Routing.DefaultLimits.MaxPromptTokens, "SYNAPASS_ROUTING_MAX_PROMPT_TOKENS")
	envInt(&c.Routing.DefaultLimits.RequestsPerMinute, "SYNAPASS_ROUTING_REQUESTS_PER_MINUTE")
	envInt(&c.Routing.DefaultLimits.TokensPerMinute, "SYNAPASS_ROUTING_TOKENS_PER_MINUTE")
	envFloat(&c.Routing.DefaultLimits.MaxCostPerRequestUSD, "SYNAPASS_ROUTING_MAX_COST_PER_REQUEST_USD")
	envFloat(&c.Routing.DefaultLimits.DailyBudgetUSD, "SYNAPASS_ROUTING_DAILY_BUDGET_USD")
	envFloat(&c.Routing.DefaultLimits.MonthlyBudgetUSD, "SYNAPASS_ROUTING_MONTHLY_BUDGET_USD")
	envBool(&c.Routing.HealthCheckEnabled, "SYNAPASS_ROUTING_HEALTH_CHECK_ENABLED")
	envDuration(&c.Routing.HealthCheckInterval, "SYNAPASS_ROUTING_HEALTH_CHECK_INTERVAL")
	envDuration(&c.Routing.HealthCheckTimeout, "SYNAPASS_ROUTING_HEALTH_CHECK_TIMEOUT")
	envDuration(&c.Routing.HealthWindow, "SYNAPASS_ROUTING_HEALTH_WINDOW")

	// ---- cache ----
	envBool(&c.Cache.ResponseCache, "SYNAPASS_CACHE_RESPONSE_ENABLED")
	envDuration(&c.Cache.ResponseTTL, "SYNAPASS_CACHE_RESPONSE_TTL")
	envInt(&c.Cache.MaxResponseBytes, "SYNAPASS_CACHE_MAX_RESPONSE_BYTES")
	envDuration(&c.Cache.ProviderHealthTTL, "SYNAPASS_CACHE_PROVIDER_HEALTH_TTL")
	envDuration(&c.Cache.PolicyCacheTTL, "SYNAPASS_CACHE_POLICY_TTL")
	envDuration(&c.Cache.RegistryCacheTTL, "SYNAPASS_CACHE_REGISTRY_TTL")
	envBool(&c.Cache.SemanticEnabled, "SYNAPASS_CACHE_SEMANTIC_ENABLED")
	envBool(&c.Cache.PrefixEnabled, "SYNAPASS_CACHE_PREFIX_ENABLED")
	envFloat(&c.Cache.SemanticThreshold, "SYNAPASS_CACHE_SEMANTIC_THRESHOLD")
	envInt(&c.Cache.PrefixLength, "SYNAPASS_CACHE_PREFIX_LENGTH")
	envBool(&c.Cache.ExactEnabled, "SYNAPASS_CACHE_EXACT_ENABLED")
	envBoolPtr(&c.Cache.BypassToolRequests, "SYNAPASS_CACHE_BYPASS_TOOLS")
	envBool(&c.Cache.AllowNondeterministic, "SYNAPASS_CACHE_ALLOW_NONDETERMINISTIC")
	envBoolPtr(&c.Cache.BypassLiveData, "SYNAPASS_CACHE_BYPASS_LIVE_DATA")
	envInt(&c.Cache.MaxSemanticEntries, "SYNAPASS_CACHE_MAX_SEMANTIC_ENTRIES")

	// ---- tools ----
	envBool(&c.Tools.GatewayExecution, "SYNAPASS_TOOLS_GATEWAY_EXECUTION")

	// ---- tunnel ----
	envBool(&c.Tunnel.Enabled, "SYNAPASS_TUNNEL_ENABLED")
	envString(&c.Tunnel.Binary, "SYNAPASS_TUNNEL_BINARY")
	envString(&c.Tunnel.DefaultTarget, "SYNAPASS_TUNNEL_DEFAULT_TARGET")
	envString(&c.Tunnel.DashboardTarget, "SYNAPASS_TUNNEL_DASHBOARD_TARGET")
	envBool(&c.Tunnel.AllowCustomTargets, "SYNAPASS_TUNNEL_ALLOW_CUSTOM_TARGETS")
	envBool(&c.Tunnel.AutoRestart, "SYNAPASS_TUNNEL_AUTO_RESTART")
	envBool(&c.Tunnel.StopOnShutdown, "SYNAPASS_TUNNEL_STOP_ON_SHUTDOWN")
	envDuration(&c.Tunnel.StartupTimeout, "SYNAPASS_TUNNEL_STARTUP_TIMEOUT")

	// ---- phase 2: classifier / shaping / scoring / guardrails / eval ----
	envBool(&c.Classifier.Enabled, "SYNAPASS_CLASSIFIER_ENABLED")
	envInt(&c.Classifier.LongContextTokens, "SYNAPASS_CLASSIFIER_LONG_CONTEXT_TOKENS")
	envInt(&c.Classifier.BatchTokens, "SYNAPASS_CLASSIFIER_BATCH_TOKENS")
	envBool(&c.Shaping.Enabled, "SYNAPASS_SHAPING_ENABLED")
	envInt(&c.Shaping.MaxHistoryMessages, "SYNAPASS_SHAPING_MAX_HISTORY")
	envInt(&c.Shaping.MaxPromptTokens, "SYNAPASS_SHAPING_MAX_PROMPT_TOKENS")
	envString(&c.Shaping.SystemPrefix, "SYNAPASS_SHAPING_SYSTEM_PREFIX")
	envString(&c.Shaping.SystemSuffix, "SYNAPASS_SHAPING_SYSTEM_SUFFIX")
	envSlice(&c.Shaping.Guardrails, "SYNAPASS_SHAPING_GUARDRAILS")
	envBool(&c.Scoring.Enabled, "SYNAPASS_SCORING_ENABLED")
	envDuration(&c.Scoring.Window, "SYNAPASS_SCORING_WINDOW")
	envFloat(&c.Scoring.SuccessWeight, "SYNAPASS_SCORING_SUCCESS_WEIGHT")
	envFloat(&c.Scoring.LatencyWeight, "SYNAPASS_SCORING_LATENCY_WEIGHT")
	envFloat(&c.Scoring.CostWeight, "SYNAPASS_SCORING_COST_WEIGHT")
	envFloat(&c.Scoring.FeedbackWeight, "SYNAPASS_SCORING_FEEDBACK_WEIGHT")
	envBool(&c.Guardrails.Enabled, "SYNAPASS_GUARDRAILS_ENABLED")
	envBool(&c.Eval.Enabled, "SYNAPASS_EVAL_ENABLED")
	envInt(&c.Eval.MaxRequests, "SYNAPASS_EVAL_MAX_REQUESTS")
	envInt(&c.Eval.WorkerPool, "SYNAPASS_EVAL_WORKER_POOL")
	envBool(&c.Detection.Enabled, "SYNAPASS_DETECTION_ENABLED")
	envInt(&c.Detection.MaxModelsPerRun, "SYNAPASS_DETECTION_MAX_MODELS")
	envInt(&c.Detection.Concurrency, "SYNAPASS_DETECTION_CONCURRENCY")
	envDuration(&c.Detection.TimeoutPerModel, "SYNAPASS_DETECTION_TIMEOUT_PER_MODEL")

	// ---- benchmark engine (disabled by default) ----
	// ENABLE_BENCHMARK_ENGINE is the documented global switch (spec); the
	// SYNAPASS_ spelling is the namespaced alias so every setting stays
	// reachable under the standard prefix as well.
	if v := firstEnv("ENABLE_BENCHMARK_ENGINE", "SYNAPASS_BENCHMARK_ENABLED", "SYNAPASS_BENCHMARK_ENGINE_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
			c.Benchmark.Enabled = b
		}
	}
	envBool(&c.Benchmark.SchedulerEnabled, "SYNAPASS_BENCHMARK_SCHEDULER_ENABLED")
	envInt(&c.Benchmark.Workers, "SYNAPASS_BENCHMARK_WORKERS")
	envInt(&c.Benchmark.QueueSize, "SYNAPASS_BENCHMARK_QUEUE_SIZE")
	envDuration(&c.Benchmark.DefaultTimeout, "SYNAPASS_BENCHMARK_DEFAULT_TIMEOUT")
	envInt(&c.Benchmark.DefaultConcurrency, "SYNAPASS_BENCHMARK_DEFAULT_CONCURRENCY")
	envFloat(&c.Benchmark.MaxDailyCostUSD, "SYNAPASS_BENCHMARK_MAX_DAILY_COST_USD")
	envInt(&c.Benchmark.RetentionDays, "SYNAPASS_BENCHMARK_RETENTION_DAYS")
	envString(&c.Benchmark.RedisQueueKey, "SYNAPASS_BENCHMARK_REDIS_QUEUE_KEY")

	// ---- telemetry ----
	envString(&c.Telemetry.ServiceName, "SYNAPASS_OTEL_SERVICE_NAME")
	envString(&c.Telemetry.OTLPEndpoint, "SYNAPASS_OTEL_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	envBool(&c.Telemetry.OTLPInsecure, "SYNAPASS_OTEL_INSECURE", "OTEL_EXPORTER_OTLP_INSECURE")
	envMap(&c.Telemetry.OTLPHeaders, "SYNAPASS_OTEL_HEADERS")
	envBool(&c.Telemetry.TracesEnabled, "SYNAPASS_OTEL_TRACES_ENABLED")
	envBool(&c.Telemetry.MetricsEnabled, "SYNAPASS_OTEL_METRICS_ENABLED")
	envBool(&c.Telemetry.LogsEnabled, "SYNAPASS_OTEL_LOGS_ENABLED")
	envFloat(&c.Telemetry.TraceSampleRatio, "SYNAPASS_OTEL_TRACE_SAMPLE_RATIO")
	envBool(&c.Telemetry.PrometheusEnabled, "SYNAPASS_PROMETHEUS_ENABLED")
	envString(&c.Telemetry.PrometheusPath, "SYNAPASS_PROMETHEUS_PATH")
	envDuration(&c.Telemetry.ExportInterval, "SYNAPASS_OTEL_EXPORT_INTERVAL")
	envInt(&c.Telemetry.TraceBufferSize, "SYNAPASS_TELEMETRY_TRACE_BUFFER_SIZE")
	envBool(&c.Telemetry.PersistTraces, "SYNAPASS_TELEMETRY_PERSIST_TRACES")

	// ---- logging ----
	envString(&c.Logging.Level, "SYNAPASS_LOG_LEVEL", "LOG_LEVEL")
	envString(&c.Logging.Format, "SYNAPASS_LOG_FORMAT", "LOG_FORMAT")
	envBool(&c.Logging.AddSource, "SYNAPASS_LOG_ADD_SOURCE")
	envSlice(&c.Logging.RedactHeaders, "SYNAPASS_LOG_REDACT_HEADERS")
	envBool(&c.Logging.LogRequestBodies, "SYNAPASS_LOG_REQUEST_BODIES")
	envDuration(&c.Logging.SlowRequestThreshold, "SYNAPASS_LOG_SLOW_REQUEST_THRESHOLD")

	// ---- admin ----
	envBool(&c.Admin.Enabled, "SYNAPASS_ADMIN_ENABLED")
	envBool(&c.Admin.RequireScope, "SYNAPASS_ADMIN_REQUIRE_SCOPE")
	envInt(&c.Admin.UsageRetentionDays, "SYNAPASS_ADMIN_USAGE_RETENTION_DAYS")
	envInt(&c.Admin.TraceRetentionDays, "SYNAPASS_ADMIN_TRACE_RETENTION_DAYS")
	envInt(&c.Admin.LogRetentionDays, "SYNAPASS_ADMIN_LOG_RETENTION_DAYS")

	// Console operator login. A separate credential type from API keys: an
	// operator chooses a password, so it is stretched and lockable, while a key is
	// high-entropy material verified by digest lookup.
	envBool(&c.Admin.DashboardAuth.Enabled, "SYNAPASS_ADMIN_DASHBOARD_ENABLED")
	envString(&c.Admin.DashboardAuth.CookieName, "SYNAPASS_ADMIN_DASHBOARD_COOKIE_NAME")
	envBool(&c.Admin.DashboardAuth.CookieSecure, "SYNAPASS_ADMIN_DASHBOARD_COOKIE_SECURE")
	envDuration(&c.Admin.DashboardAuth.SessionTTL, "SYNAPASS_ADMIN_DASHBOARD_SESSION_TTL")
	envDuration(&c.Admin.DashboardAuth.IdleTTL, "SYNAPASS_ADMIN_DASHBOARD_IDLE_TTL")
	envInt(&c.Admin.DashboardAuth.MinPasswordLength, "SYNAPASS_ADMIN_DASHBOARD_MIN_PASSWORD_LENGTH")
	envInt(&c.Admin.DashboardAuth.MaxPasswordLength, "SYNAPASS_ADMIN_DASHBOARD_MAX_PASSWORD_LENGTH")
	envInt(&c.Admin.DashboardAuth.MaxFailedAttempts, "SYNAPASS_ADMIN_DASHBOARD_MAX_FAILED_ATTEMPTS")
	envDuration(&c.Admin.DashboardAuth.LockoutDuration, "SYNAPASS_ADMIN_DASHBOARD_LOCKOUT_DURATION")
	envDuration(&c.Admin.DashboardAuth.MaxLockoutDuration, "SYNAPASS_ADMIN_DASHBOARD_MAX_LOCKOUT_DURATION")

	return nil
}
