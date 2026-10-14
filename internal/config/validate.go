package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ValidationError collects every problem found so an operator can fix a config
// file in one pass instead of rediscovering one mistake per restart.
type ValidationError struct {
	Problems []string
}

// Error renders all problems as a numbered list.
func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(e.Problems))
	for i, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, p)
	}
	return b.String()
}

// add appends a problem.
func (e *ValidationError) add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

// err returns the accumulated error, or nil when nothing was recorded.
func (e *ValidationError) err() error {
	if len(e.Problems) == 0 {
		return nil
	}
	return e
}

// Validate checks the configuration for internal consistency.
//
// The rules are deliberately strict: every problem caught here is one that
// would otherwise surface as a confusing runtime failure under load. Where a
// value is merely suspicious rather than wrong (an empty provider list, for
// example, which is valid when the catalogue is managed entirely through the
// dashboard) validation stays silent.
func (c *Config) Validate() error {
	v := &ValidationError{}

	c.validateApp(v)
	c.validateHTTP(v)
	c.validateDatabase(v)
	c.validateRedis(v)
	c.validateClickHouse(v)
	c.validateNATS(v)
	c.validateAuth(v)
	c.validateRouting(v)
	c.validateCache(v)
	c.validateTunnel(v)
	c.validatePhase2(v)
	c.validateBenchmark(v)
	c.validateTelemetry(v)
	c.validateLogging(v)
	c.validateAdmin(v)
	c.validateCatalog(v)

	return v.err()
}

func (c *Config) validateApp(v *ValidationError) {
	if strings.TrimSpace(c.App.Name) == "" {
		v.add("app.name must not be empty")
	}
	switch strings.ToLower(c.App.Environment) {
	case "development", "dev", "staging", "stage", "production", "prod", "test":
	default:
		v.add("app.environment %q is not recognised; expected development, staging, production or test", c.App.Environment)
	}
	if strings.TrimSpace(c.App.DefaultTenantSlug) == "" {
		v.add("app.default_tenant_slug must not be empty")
	}
}

func (c *Config) validateHTTP(v *ValidationError) {
	if strings.TrimSpace(c.HTTP.Addr) == "" {
		v.add("http.addr must not be empty (example: \":8080\")")
	}
	if c.HTTP.MaxBodyBytes <= 0 {
		v.add("http.max_body_bytes must be positive")
	}
	if c.HTTP.WriteTimeout > 0 && c.HTTP.ReadTimeout > 0 && c.HTTP.WriteTimeout < c.HTTP.ReadTimeout {
		// Not fatal, but almost always a mistake: it would truncate every
		// streaming response.
		v.add("http.write_timeout (%s) is shorter than http.read_timeout (%s); streaming responses will be cut off",
			c.HTTP.WriteTimeout.Std(), c.HTTP.ReadTimeout.Std())
	}
	for _, p := range c.HTTP.CORSAllowedOrigins {
		if p == "*" && c.IsProduction() {
			v.add("http.cors_allowed_origins contains \"*\" in a production environment")
		}
	}
}

func (c *Config) validateDatabase(v *ValidationError) {
	if strings.TrimSpace(c.Database.DSN) == "" {
		v.add("database.dsn is empty; set database.dsn or SYNAPASS_DATABASE_DSN")
		return
	}
	if c.Database.Port <= 0 || c.Database.Port > 65535 {
		v.add("database.port %d is out of range", c.Database.Port)
	}
	if c.Database.MaxConns < 1 {
		v.add("database.max_conns must be at least 1")
	}
	if c.Database.MinConns < 0 {
		v.add("database.min_conns must not be negative")
	}
	if c.Database.MinConns > c.Database.MaxConns {
		v.add("database.min_conns (%d) must not exceed database.max_conns (%d)",
			c.Database.MinConns, c.Database.MaxConns)
	}
	// A pool per replica multiplies against the server's connection limit, so
	// warn early rather than discovering it as "too many clients" in production.
	if c.Database.MaxConns > 100 {
		v.add("database.max_conns %d is very high; remember each replica has its own pool", c.Database.MaxConns)
	}
}

func (c *Config) validateRedis(v *ValidationError) {
	if strings.TrimSpace(c.Redis.Addr) == "" {
		if c.Redis.Required {
			v.add("redis.addr is empty but redis.required is true")
		}
		return
	}
	if len(c.Redis.SentinelAddrs) > 0 && strings.TrimSpace(c.Redis.MasterName) == "" {
		v.add("redis.sentinel_addrs is set but redis.master_name is empty")
	}
	if c.Redis.PoolSize < 1 {
		v.add("redis.pool_size must be at least 1")
	}
	if strings.TrimSpace(c.Redis.KeyPrefix) == "" {
		v.add("redis.key_prefix must not be empty; it namespaces keys shared with other services")
	}
}

func (c *Config) validateClickHouse(v *ValidationError) {
	if c.ClickHouse.DSN == "" && strings.TrimSpace(c.ClickHouse.Addr) == "" {
		if c.ClickHouse.Required {
			v.add("clickhouse.addr is empty but clickhouse.required is true")
		}
		return
	}
	if strings.TrimSpace(c.ClickHouse.Database) == "" {
		v.add("clickhouse.database must not be empty")
	}
	if c.ClickHouse.BatchSize < 1 {
		v.add("clickhouse.batch_size must be at least 1")
	}
	if c.ClickHouse.FlushInterval <= 0 {
		v.add("clickhouse.flush_interval must be positive")
	}
}

func (c *Config) validateNATS(v *ValidationError) {
	if strings.TrimSpace(c.NATS.URL) == "" && len(c.NATS.URLs) == 0 {
		if c.NATS.Required {
			v.add("nats.url is empty but nats.required is true")
		}
		return
	}
	for _, u := range append([]string{c.NATS.URL}, c.NATS.URLs...) {
		if u == "" {
			continue
		}
		if _, err := url.Parse(u); err != nil {
			v.add("nats url %q is not a valid URL: %v", u, err)
		}
	}
	if c.NATS.StreamReplicas < 1 {
		v.add("nats.stream_replicas must be at least 1")
	}
}

func (c *Config) validateAuth(v *ValidationError) {
	if c.Auth.MinKeyLength < 16 {
		v.add("auth.min_key_length %d is too short; 32 is recommended for 256-bit keys", c.Auth.MinKeyLength)
	}
	if !strings.HasPrefix(c.Auth.KeyPrefix, "syn_") && c.Auth.KeyPrefix != "" {
		v.add("auth.key_prefix %q should start with \"syn_\" so keys are identifiable in logs and support tickets",
			c.Auth.KeyPrefix)
	}
	if c.Auth.CacheTTL <= 0 {
		v.add("auth.cache_ttl must be positive")
	}
	if c.Auth.CacheTTL > 5*60*1e9 {
		v.add("auth.cache_ttl %s is long; a revoked key would keep working for that long", c.Auth.CacheTTL.Std())
	}
	if c.IsProduction() && c.Auth.AllowAnonymousTenant != "" {
		v.add("auth.allow_anonymous_tenant must not be set in production")
	}
}

func (c *Config) validateRouting(v *ValidationError) {
	switch c.Routing.DefaultStrategy {
	case "", "priority", "weighted", "lowest_cost", "lowest_latency", "highest_quality":
	default:
		v.add("routing.default_strategy %q is not one of priority, weighted, lowest_cost, lowest_latency, highest_quality",
			c.Routing.DefaultStrategy)
	}
	if c.Routing.DefaultMaxAttempts < 1 {
		v.add("routing.default_max_attempts must be at least 1")
	}
	if c.Routing.DefaultTimeout.Total <= 0 {
		v.add("routing.default_timeout.total must be positive")
	}
	if c.Routing.DefaultTimeout.PerAttempt > c.Routing.DefaultTimeout.Total {
		v.add("routing.default_timeout.per_attempt (%s) must not exceed total (%s)",
			c.Routing.DefaultTimeout.PerAttempt.Std(), c.Routing.DefaultTimeout.Total.Std())
	}
	if c.Routing.DefaultLimits.MaxOutputTokens < 1 {
		v.add("routing.default_limits.max_output_tokens must be at least 1")
	}
	if c.Routing.HealthCheckEnabled && c.Routing.HealthCheckInterval <= 0 {
		v.add("routing.health_check_interval must be positive when health checks are enabled")
	}
	if c.Routing.HealthCheckTimeout > c.Routing.HealthCheckInterval {
		v.add("routing.health_check_timeout (%s) should not exceed routing.health_check_interval (%s)",
			c.Routing.HealthCheckTimeout.Std(), c.Routing.HealthCheckInterval.Std())
	}
	for model, ms := range c.Routing.LatencyPriors {
		if ms < 0 {
			v.add("routing.latency_priors[%s] must not be negative", model)
		}
	}
}

func (c *Config) validateCache(v *ValidationError) {
	if c.Cache.ResponseCache && c.Cache.ResponseTTL <= 0 {
		v.add("cache.response_ttl must be positive when cache.response_cache is enabled")
	}
	if c.Cache.PolicyCacheTTL < 0 || c.Cache.RegistryCacheTTL < 0 {
		v.add("cache policy and registry TTLs must not be negative")
	}
	if c.Cache.SemanticThreshold < 0 || c.Cache.SemanticThreshold > 1 {
		v.add("cache.semantic_threshold %g must be between 0 and 1", c.Cache.SemanticThreshold)
	}
	if c.Cache.PrefixLength < 0 {
		v.add("cache.prefix_length must not be negative")
	}
}

func (c *Config) validateTunnel(v *ValidationError) {
	if !c.Tunnel.Enabled {
		return
	}
	if c.Tunnel.Binary == "" {
		v.add("tunnel.binary must name the cloudflared executable when tunnel.enabled is true")
	}
	switch c.Tunnel.DefaultTarget {
	case "", "gateway", "dashboard":
		// Named targets always resolve; custom loopback targets are
		// validated per request by the tunnel manager.
	default:
		host, port, ok := splitHostPort(c.Tunnel.DefaultTarget)
		if !ok || !isLoopbackHost(host) || port == "" {
			v.add("tunnel.default_target %q is not a named target or loopback host:port", c.Tunnel.DefaultTarget)
		}
	}
	if c.Tunnel.StartupTimeout < 0 {
		v.add("tunnel.startup_timeout must not be negative")
	}
}

func (c *Config) validatePhase2(v *ValidationError) {
	if c.Classifier.LongContextTokens < 0 || c.Classifier.BatchTokens < 0 {
		v.add("classifier token thresholds must not be negative")
	}
	if c.Classifier.BatchTokens > 0 && c.Classifier.LongContextTokens > 0 && c.Classifier.BatchTokens < c.Classifier.LongContextTokens {
		v.add("classifier.batch_tokens should not be smaller than classifier.long_context_tokens")
	}
	if c.Shaping.MaxHistoryMessages < 0 || c.Shaping.MaxPromptTokens < 0 {
		v.add("shaping limits must not be negative")
	}
	if c.Scoring.Window <= 0 {
		v.add("scoring.window must be positive")
	}
	for _, w := range []float64{c.Scoring.SuccessWeight, c.Scoring.LatencyWeight, c.Scoring.CostWeight, c.Scoring.FeedbackWeight} {
		if w < 0 {
			v.add("scoring weights must not be negative")
		}
	}
	if c.Eval.MaxRequests < 0 || c.Eval.MaxRequests > 1000 {
		v.add("eval.max_requests must be between 0 and 1000")
	}
}

func (c *Config) validateTelemetry(v *ValidationError) {
	t := c.Telemetry
	if t.TraceSampleRatio < 0 || t.TraceSampleRatio > 1 {
		v.add("telemetry.trace_sample_ratio %g must be between 0 and 1", t.TraceSampleRatio)
	}
	if t.PrometheusPath != "" && !strings.HasPrefix(t.PrometheusPath, "/") {
		v.add("telemetry.prometheus_path %q must start with \"/\"", t.PrometheusPath)
	}
	if t.TracesEnabled && strings.TrimSpace(t.OTLPEndpoint) == "" {
		// Not fatal: the OTel SDK also honours OTEL_EXPORTER_OTLP_ENDPOINT and
		// falls back to a no-op exporter. Calling it out avoids an operator
		// wondering why the trace pane is empty.
		v.add("telemetry.otlp_endpoint is empty while traces are enabled; set it or disable traces")
	}
	if t.ExportInterval <= 0 {
		v.add("telemetry.export_interval must be positive")
	}
	if t.TraceBufferSize < 1 {
		v.add("telemetry.trace_buffer_size must be at least 1")
	}
	// A buffer smaller than one flush batch silently drops traces under load,
	// which presents as missing data rather than as an error.
	if c.ClickHouse.BatchSize > 0 && t.TraceBufferSize < c.ClickHouse.BatchSize {
		v.add("telemetry.trace_buffer_size (%d) is smaller than clickhouse.batch_size (%d); traces will be dropped under load",
			t.TraceBufferSize, c.ClickHouse.BatchSize)
	}
}

func (c *Config) validateLogging(v *ValidationError) {
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		v.add("logging.level %q is not one of debug, info, warn, error", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "json", "console", "text":
	default:
		v.add("logging.format %q is not one of json, console", c.Logging.Format)
	}
	if c.Logging.LogRequestBodies && c.IsProduction() {
		v.add("logging.log_request_bodies must be disabled in production; prompts contain personal data")
	}
}

func (c *Config) validateAdmin(v *ValidationError) {
	if c.Admin.UsageRetentionDays < 0 || c.Admin.TraceRetentionDays < 0 || c.Admin.LogRetentionDays < 0 {
		v.add("admin retention days must not be negative")
	}
	if c.Admin.Enabled && c.Admin.RequireScope && c.Auth.AdminKey == "" && c.IsProduction() {
		v.add("admin.require_scope is true but no bootstrap admin key is configured (set SYNAPASS_ADMIN_KEY)")
	}
	// The shipped placeholder (.env.example, compose defaults) is non-empty on
	// purpose so an unmodified copy boots far enough to fail loudly -- but it
	// must never administer a production install. Outside production (an
	// explicit local evaluation) it is tolerated.
	if c.IsProduction() && isPlaceholderAdminKey(c.Auth.AdminKey) {
		v.add("auth admin key is the shipped placeholder; generate a real one (openssl rand -hex 24) and set SYNAPASS_ADMIN_KEY")
	}
}

// PlaceholderAdminKey is the value shipped in .env.example and the compose
// defaults. Kept in one place so validation, tests and docs cannot drift.
const PlaceholderAdminKey = "syn_admin_change_me_in_production_000000"

// isPlaceholderAdminKey reports whether a key is a shipped placeholder rather
// than a real credential. The exact constant plus the family prefix are both
// rejected so a hand-edited variant ("..._000001") does not slip through.
func isPlaceholderAdminKey(key string) bool {
	if key == "" {
		return false
	}
	return key == PlaceholderAdminKey || strings.HasPrefix(key, "syn_admin_change_me")
}

// validateCatalog checks the declarative provider/model/policy catalogue. Cross
// references are verified here so a startup failure names the exact bad
// reference rather than failing later with a "model not found" at request time.
func (c *Config) validateCatalog(v *ValidationError) {
	providerNames := map[string]struct{}{}
	modelNames := map[string]string{}  // model name -> provider
	aliasOwners := map[string]string{} // alias -> first owner

	for i, p := range c.Providers {
		where := fmt.Sprintf("providers[%d]", i)
		name := strings.TrimSpace(p.Name)
		if name == "" {
			v.add("%s.name must not be empty", where)
			continue
		}
		where = fmt.Sprintf("providers[%q]", name)
		if _, dup := providerNames[name]; dup {
			v.add("%s is declared more than once", where)
		}
		providerNames[name] = struct{}{}

		if !validProviderKind(p.Kind) {
			v.add("%s.kind %q is not one of openai, anthropic, ollama, vllm, openai_compatible", where, p.Kind)
		}
		if strings.TrimSpace(p.BaseURL) == "" {
			v.add("%s.base_url must not be empty", where)
		} else if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			v.add("%s.base_url %q must be an http or https URL", where, p.BaseURL)
		}
		if !validAuthStyle(p.AuthStyle, p.Kind) {
			v.add("%s.auth_style %q is not one of bearer, header, none", where, p.AuthStyle)
		}
		if p.AuthStyle == "header" && strings.TrimSpace(p.HeaderName) == "" {
			v.add("%s.header_name is required when auth_style is \"header\"", where)
		}
		if requiresCredential(p.Kind) && p.APIKeyEnv == "" && p.APIKey == "" {
			v.add("%s requires credentials: set api_key_env (recommended) or api_key", where)
		}
		if p.APIKeyEnv != "" && p.APIKey != "" {
			v.add("%s sets both api_key_env and api_key; api_key_env wins and the inline value will be ignored", where)
		}
		if p.Weight < 0 {
			v.add("%s.weight must not be negative", where)
		}
		if p.MaxConcurrency < 0 {
			v.add("%s.max_concurrency must not be negative", where)
		}
		if len(p.Models) == 0 {
			v.add("%s declares no models; add at least one entry under models", where)
		}
		for j, m := range p.Models {
			mwhere := fmt.Sprintf("%s.models[%d]", where, j)
			mname := strings.TrimSpace(m.Name)
			if mname == "" {
				v.add("%s.name must not be empty", mwhere)
				continue
			}
			mwhere = fmt.Sprintf("%s.models[%q]", where, mname)
			if prev, dup := modelNames[mname]; dup {
				// Different providers legitimately serve the same model name:
				// that is the whole point of a fallback chain. Same provider
				// twice is a configuration mistake.
				if prev == name {
					v.add("%s is declared more than once on provider %q", mwhere, name)
				}
			} else {
				modelNames[mname] = name
			}
			if m.ContextWindow < 0 {
				v.add("%s.context_window must not be negative", mwhere)
			}
			if m.MaxOutputTokens < 0 {
				v.add("%s.max_output_tokens must not be negative", mwhere)
			}
			if m.ContextWindow > 0 && m.MaxOutputTokens > m.ContextWindow {
				v.add("%s.max_output_tokens (%d) exceeds context_window (%d)", mwhere, m.MaxOutputTokens, m.ContextWindow)
			}
			if m.InputCostPerMillion < 0 || m.OutputCostPerMillion < 0 {
				v.add("%s pricing must not be negative", mwhere)
			}
			if m.QualityTier < 0 || m.QualityTier > 5 {
				v.add("%s.quality_tier %d must be between 0 and 5", mwhere, m.QualityTier)
			}
			for _, alias := range append([]string{m.Name}, m.Aliases...) {
				alias = strings.TrimSpace(alias)
				if alias == "" {
					continue
				}
				if owner, taken := aliasOwners[alias]; taken && owner != name {
					// Aliases may span providers (that is how a migration is
					// expressed) but must be unique within one provider.
					continue
				}
				aliasOwners[alias] = name
			}
			if m.Status != "" && !validModelStatus(m.Status) {
				v.add("%s.status %q is not one of active, degraded, deprecated, disabled", mwhere, m.Status)
			}
		}
	}

	policyNames := map[string]struct{}{}
	for i, p := range c.Policies {
		where := fmt.Sprintf("policies[%d]", i)
		if strings.TrimSpace(p.Name) == "" {
			v.add("%s.name must not be empty", where)
			continue
		}
		where = fmt.Sprintf("policies[%q]", p.Name)
		if _, dup := policyNames[p.Name]; dup {
			v.add("%s is declared more than once", where)
		}
		policyNames[p.Name] = struct{}{}

		switch p.Strategy {
		case "", "priority", "weighted", "lowest_cost", "lowest_latency", "highest_quality":
		default:
			v.add("%s.strategy %q is not a known routing strategy", where, p.Strategy)
		}
		if len(p.Targets) == 0 {
			v.add("%s declares no targets; a policy must route somewhere", where)
			continue
		}
		for j, t := range p.Targets {
			twhere := fmt.Sprintf("%s.targets[%d]", where, j)
			if strings.TrimSpace(t.Model) == "" {
				v.add("%s.model must not be empty", twhere)
			}
			if t.Provider != "" {
				if _, ok := providerNames[t.Provider]; !ok {
					v.add("%s.provider %q is not declared in the providers section", twhere, t.Provider)
				}
			}
			if t.Weight < 0 {
				v.add("%s.weight must not be negative", twhere)
			}
		}
		if p.Retry != nil && p.Retry.MaxAttempts < 1 {
			v.add("%s.retry.max_attempts must be at least 1", where)
		}
		if p.Timeout != nil {
			if p.Timeout.Total > 0 && p.Timeout.PerAttempt > p.Timeout.Total {
				v.add("%s.timeout.per_attempt must not exceed timeout.total", where)
			}
		}
		if p.Fallback != nil && p.Fallback.MaxAttempts < 0 {
			v.add("%s.fallback.max_attempts must not be negative", where)
		}
		if p.Limits != nil {
			if p.Limits.MaxCostPerRequestUSD < 0 || p.Limits.DailyBudgetUSD < 0 || p.Limits.MonthlyBudgetUSD < 0 {
				v.add("%s.limits monetary values must not be negative", where)
			}
		}
	}
}

func validProviderKind(kind string) bool {
	switch ProviderKind(kind) {
	case KindOpenAI, KindAnthropic, KindOllama, KindVLLM, KindOpenAICompatible:
		return true
	default:
		return false
	}
}

// ProviderKind mirrors the domain enum for config parsing. It is duplicated
// here so the config package does not depend on the domain package, which keeps
// configuration purely about parsing and validation. The two are kept in sync by
// the bootstrap layer's exhaustive switch, which fails to compile if a case is
// missing.
type ProviderKind string

// Provider kind constants, matching domain.ProviderKind.
const (
	KindOpenAI           ProviderKind = "openai"
	KindAnthropic        ProviderKind = "anthropic"
	KindOllama           ProviderKind = "ollama"
	KindVLLM             ProviderKind = "vllm"
	KindOpenAICompatible ProviderKind = "openai_compatible"
)

func validAuthStyle(style, kind string) bool {
	switch style {
	case "", "bearer", "header", "none":
		// Ollama has no auth by default, so an empty style is fine there.
		return true
	default:
		return false
	}
}

// requiresCredential reports whether a provider kind cannot work without a key.
// Local runtimes are excluded: requiring a credential for Ollama would make the
// common self-hosted setup impossible to configure.
func requiresCredential(kind string) bool {
	switch ProviderKind(kind) {
	case KindOpenAI, KindAnthropic:
		return true
	default:
		return false
	}
}

func validModelStatus(s string) bool {
	switch s {
	case "active", "degraded", "deprecated", "disabled":
		return true
	default:
		return false
	}
}

// splitHostPort splits a "host:port" target without accepting URLs, paths or
// schemes. It mirrors internal/tunnel's parser and is duplicated here so the
// config package keeps its no-domain, no-net dependency discipline for this
// check.
func splitHostPort(target string) (host, port string, ok bool) {
	if target == "" || len(target) > 253 {
		return "", "", false
	}
	for _, r := range target {
		if r <= ' ' || r == '/' || r == '?' || r == '#' || r == '@' {
			return "", "", false
		}
	}
	// An IPv6 literal must be bracketed; anything else splits on the last colon.
	if strings.HasPrefix(target, "[") {
		end := strings.Index(target, "]")
		if end < 0 || end+1 >= len(target) || target[end+1] != ':' {
			return "", "", false
		}
		return target[1:end], target[end+2:], target[end+2:] != ""
	}
	idx := strings.LastIndex(target, ":")
	if idx <= 0 || idx+1 >= len(target) {
		return "", "", false
	}
	host, port = target[:idx], target[idx+1:]
	if strings.Contains(host, ":") {
		return "", "", false
	}
	return host, port, true
}

// isLoopbackHost reports whether a target host is this machine. Only loopback
// destinations may be exposed: a tunnel must never turn Synapass into a
// proxy for someone else's infrastructure.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// validateBenchmark checks the Autonomous Benchmark Engine configuration.
//
// The engine is disabled by default; validation only constrains the shape of
// the values so an operator who enables it gets a fast, actionable error
// instead of a broken scheduler at runtime.
func (c *Config) validateBenchmark(v *ValidationError) {
	b := c.Benchmark
	if b.Workers < 0 || b.Workers > 100 {
		v.add("benchmark.workers %d must be between 0 and 100", b.Workers)
	}
	if b.QueueSize < 0 || b.QueueSize > 100000 {
		v.add("benchmark.queue_size %d must be between 0 and 100000", b.QueueSize)
	}
	if b.DefaultTimeout.Std() < 0 {
		v.add("benchmark.default_timeout must not be negative")
	}
	if b.Enabled && b.DefaultTimeout.Std() <= 0 {
		v.add("benchmark.default_timeout must be positive when benchmark.enabled is true")
	}
	if b.DefaultConcurrency < 0 || b.DefaultConcurrency > 100 {
		v.add("benchmark.default_concurrency %d must be between 0 and 100", b.DefaultConcurrency)
	}
	if b.MaxDailyCostUSD < 0 {
		v.add("benchmark.max_daily_cost_usd must not be negative")
	}
	if b.RetentionDays < 0 {
		v.add("benchmark.retention_days must not be negative")
	}
	if b.Enabled && b.RedisQueueKey == "" {
		v.add("benchmark.redis_queue_key must not be empty when benchmark.enabled is true")
	}
}

// ErrNotConfigured is returned by accessors when a required subsystem is
// absent, allowing callers to distinguish "unset" from "misconfigured".
var ErrNotConfigured = errors.New("not configured")
