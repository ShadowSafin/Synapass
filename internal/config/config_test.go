package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Defaults and finalization
// ---------------------------------------------------------------------------

// deployableDefaults returns the shipped defaults adjusted into a state that
// passes validation, which is what any real deployment does: point telemetry at a
// collector and supply a bootstrap administrator credential.
func deployableDefaults() *Config {
	cfg := Default()
	cfg.Telemetry.OTLPEndpoint = "localhost:4318"
	cfg.Auth.AdminKey = "syn_admin_test_key_value_000000000000"
	return cfg
}

func TestShippedDefaultsFailFastOnIncompleteTelemetry(t *testing.T) {
	// The defaults enable traces without naming a collector. Validation refuses
	// that rather than starting a gateway that silently exports nothing: an
	// operator who enabled tracing should be told, not left guessing.
	cfg := Default()
	cfg.Auth.AdminKey = "syn_admin_test_key_value_000000000000"

	err := cfg.Finalize()
	if err == nil {
		t.Fatal("enabling traces without an endpoint must be rejected")
	}
	if !strings.Contains(err.Error(), "otlp_endpoint") {
		t.Errorf("error %q should name the missing endpoint", err.Error())
	}

	// Disabling traces is the documented alternative.
	cfg.Telemetry.TracesEnabled = false
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("disabling traces should make the defaults valid: %v", err)
	}
}

func TestShippedDefaultsFailFastWithoutABootstrapAdminKey(t *testing.T) {
	// Scope enforcement without a bootstrap key leaves a fresh production install
	// unadministrable, so it is refused at startup. In development the same
	// combination is tolerated, which is why this only applies to production.
	cfg := Default()
	cfg.Telemetry.OTLPEndpoint = "localhost:4318"
	cfg.App.Environment = "production"

	err := cfg.Finalize()
	if err == nil {
		t.Fatal("requiring admin scopes with no admin key must be rejected")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("error %q should name the admin key requirement", err.Error())
	}
}

func TestPlaceholderAdminKeyRejectedInProduction(t *testing.T) {
	// The shipped placeholder is non-empty, so the empty-key check above does
	// not catch it. Production must refuse it explicitly; an explicit local
	// evaluation (development) may still use it.
	cfg := Default()
	cfg.Telemetry.OTLPEndpoint = "localhost:4318"
	cfg.App.Environment = "production"
	cfg.Auth.AdminKey = PlaceholderAdminKey

	if err := cfg.Finalize(); err == nil {
		t.Fatal("the shipped placeholder admin key must be rejected in production")
	} else if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error %q should name the placeholder", err.Error())
	}

	cfg.App.Environment = "development"
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("an explicit local evaluation may use the placeholder: %v", err)
	}
}

func TestPlaceholderAdminKeyRejectedInStaging(t *testing.T) {
	// Staging used to be treated as development, so a deployed environment
	// could boot with the key documented in the repository. Only an explicitly
	// local environment may use it now.
	cfg := Default()
	cfg.Telemetry.OTLPEndpoint = "localhost:4318"
	cfg.App.Environment = "staging"
	cfg.Auth.AdminKey = PlaceholderAdminKey

	err := cfg.Finalize()
	if err == nil {
		t.Fatal("the shipped placeholder admin key must be rejected in staging")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error %q should name the placeholder", err.Error())
	}

	// The documented escape hatch exists for CI harnesses that must run the
	// compose file verbatim.
	t.Setenv("SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY", "true")
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("the explicit opt-in should permit the placeholder: %v", err)
	}
}

func TestStagingRejectsAnonymousTenantAndCORS(t *testing.T) {
	cfg := Default()
	cfg.Telemetry.OTLPEndpoint = "localhost:4318"
	cfg.App.Environment = "staging"
	cfg.Auth.AdminKey = "syn_staging_abcdefghijklmnopqrstuvwxyz0123456789"
	cfg.Auth.AllowAnonymousTenant = "default"
	cfg.HTTP.CORSAllowedOrigins = []string{"*"}

	err := cfg.Finalize()
	if err == nil {
		t.Fatal("staging must not keep the development auth and CORS relaxations")
	}
	if !strings.Contains(err.Error(), "allow_anonymous_tenant") {
		t.Errorf("error %q should name the anonymous tenant setting", err.Error())
	}
	if !strings.Contains(err.Error(), "cors_allowed_origins") {
		t.Errorf("error %q should name the CORS wildcard", err.Error())
	}

	// The same settings remain valid for a local evaluation.
	cfg.App.Environment = "development"
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("development may keep the relaxations: %v", err)
	}
}

func TestDefaultConfigFinalizesAndValidates(t *testing.T) {
	cfg := deployableDefaults()
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("a deployable configuration must validate: %v", err)
	}

	if cfg.App.InstanceID == "" {
		t.Error("finalization must assign an instance id for telemetry")
	}
	if cfg.Telemetry.ServiceName != cfg.App.Name {
		t.Errorf("service name = %q, want it defaulted from app.name %q", cfg.Telemetry.ServiceName, cfg.App.Name)
	}
	if !strings.HasPrefix(cfg.Database.DSN, "postgres://") {
		t.Errorf("dsn = %q, want a materialized connection string", cfg.Database.DSN)
	}
	if cfg.HTTP.MaxBodyBytes <= 0 {
		t.Error("a request body limit must be configured by default")
	}
}

func TestFinalizeBuildsDSNFromDiscreteFields(t *testing.T) {
	cfg := deployableDefaults()
	cfg.Database.DSN = ""
	cfg.Database.User = "app"
	cfg.Database.Password = "p@ss:word" // needs escaping in a URI userinfo section
	cfg.Database.Host = "db.internal"
	cfg.Database.Port = 5433
	cfg.Database.Name = "synapass"

	if err := cfg.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	dsn := cfg.Database.DSN
	for _, want := range []string{"app", "db.internal", "5433", "synapass", "sslmode="} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q is missing %q", dsn, want)
		}
	}
	// An unescaped '@' or ':' in the password would terminate the userinfo
	// component and send the client to the wrong host.
	if strings.Contains(dsn, "p@ss:word") {
		t.Errorf("credentials must be percent-encoded in the dsn, got %q", dsn)
	}
	if !strings.Contains(dsn, "p%40ss%3Aword") {
		t.Errorf("dsn %q should carry the percent-encoded password", dsn)
	}
}

func TestFinalizeDoesNotOverrideAnExplicitDSN(t *testing.T) {
	cfg := deployableDefaults()
	cfg.Database.DSN = "postgres://custom:secret@elsewhere:5432/custom"
	cfg.Database.Host = "ignored"

	if err := cfg.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if cfg.Database.DSN != "postgres://custom:secret@elsewhere:5432/custom" {
		t.Errorf("an operator-supplied dsn must be preserved, got %q", cfg.Database.DSN)
	}
}

func TestAdminKeyIsResolvedFromItsEnvironmentVariable(t *testing.T) {
	t.Setenv("SYNAPASS_TEST_ADMIN_KEY", "syn_admin_from_env")

	cfg := deployableDefaults()
	cfg.Auth.AdminKey = ""
	cfg.Auth.AdminKeyEnv = "SYNAPASS_TEST_ADMIN_KEY"

	if err := cfg.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if cfg.Auth.AdminKey != "syn_admin_from_env" {
		t.Errorf("admin key = %q, want the value read from the named variable", cfg.Auth.AdminKey)
	}
}

func TestFirstNATSURLWins(t *testing.T) {
	cfg := deployableDefaults()
	cfg.NATS.URL = ""
	cfg.NATS.URLs = []string{"nats://one:4222", "nats://two:4222"}

	if err := cfg.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if cfg.NATS.URL != "nats://one:4222" {
		t.Errorf("nats url = %q, want the first of the pool", cfg.NATS.URL)
	}
}

// ---------------------------------------------------------------------------
// Environment overrides
// ---------------------------------------------------------------------------

func TestEnvironmentOverridesEveryLayer(t *testing.T) {
	env := map[string]string{
		"SYNAPASS_APP_NAME":                 "synapass-edge",
		"SYNAPASS_ENVIRONMENT":              "staging",
		"SYNAPASS_HTTP_ADDR":                ":9090",
		"SYNAPASS_HTTP_MAX_BODY_BYTES":      "1048576",
		"SYNAPASS_HTTP_TRUSTED_PROXIES":     "10.0.0.0/8,192.168.0.0/16",
		"SYNAPASS_DATABASE_DSN":             "postgres://u:p@db:5432/cr",
		"SYNAPASS_REDIS_ADDR":               "redis:6379",
		"SYNAPASS_REDIS_REQUIRED":           "true",
		"SYNAPASS_CLICKHOUSE_ADDR":          "clickhouse:9000",
		"SYNAPASS_NATS_URL":                 "nats://nats:4222",
		"SYNAPASS_AUTH_MIN_KEY_LENGTH":      "24",
		"SYNAPASS_ADMIN_ENABLED":            "false",
		"SYNAPASS_ROUTING_DEFAULT_STRATEGY": "lowest_cost", "SYNAPASS_ROUTING_MAX_OUTPUT_TOKENS": "2048",
		"SYNAPASS_ROUTING_TIMEOUT_TOTAL":       "45s",
		"SYNAPASS_ROUTING_TIMEOUT_PER_ATTEMPT": "30s",
		"SYNAPASS_PROMETHEUS_ENABLED":          "false",
		"SYNAPASS_LOG_LEVEL":                   "debug", "SYNAPASS_OTEL_ENDPOINT": "collector:4318",
		"SYNAPASS_OTEL_TRACE_SAMPLE_RATIO": "0.25",
		"SYNAPASS_CORS_ALLOWED_ORIGINS":    "https://console.example,https://ops.example",
	}
	for name, value := range env {
		t.Setenv(name, value)
	}

	cfg := deployableDefaults()
	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if cfg.App.Name != "synapass-edge" {
		t.Errorf("app name = %q", cfg.App.Name)
	}
	if cfg.App.Environment != "staging" {
		t.Errorf("environment = %q", cfg.App.Environment)
	}
	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("http addr = %q", cfg.HTTP.Addr)
	}
	if cfg.HTTP.MaxBodyBytes != 1048576 {
		t.Errorf("max body bytes = %d", cfg.HTTP.MaxBodyBytes)
	}
	if len(cfg.HTTP.TrustedProxies) != 2 {
		t.Errorf("trusted proxies = %v", cfg.HTTP.TrustedProxies)
	}
	if cfg.Database.DSN != "postgres://u:p@db:5432/cr" {
		t.Errorf("dsn = %q", cfg.Database.DSN)
	}
	if cfg.Redis.Addr != "redis:6379" || !cfg.Redis.Required {
		t.Errorf("redis = %+v", cfg.Redis)
	}
	if cfg.ClickHouse.Addr != "clickhouse:9000" {
		t.Errorf("clickhouse addr = %q", cfg.ClickHouse.Addr)
	}
	if cfg.NATS.URL != "nats://nats:4222" {
		t.Errorf("nats url = %q", cfg.NATS.URL)
	}
	if cfg.Auth.MinKeyLength != 24 {
		t.Errorf("min key length = %d", cfg.Auth.MinKeyLength)
	}
	if cfg.Admin.Enabled {
		t.Error("admin should have been disabled by the environment")
	}
	if cfg.Routing.DefaultStrategy != "lowest_cost" {
		t.Errorf("strategy = %q", cfg.Routing.DefaultStrategy)
	}
	if cfg.Routing.DefaultLimits.MaxOutputTokens != 2048 {
		t.Errorf("max output tokens = %d", cfg.Routing.DefaultLimits.MaxOutputTokens)
	}
	if cfg.Routing.DefaultTimeout.Total.Std() != 45*time.Second {
		t.Errorf("total timeout = %v", cfg.Routing.DefaultTimeout.Total.Std())
	}
	if cfg.Telemetry.PrometheusEnabled {
		t.Error("prometheus should have been disabled by the environment")
	}
	if cfg.Telemetry.OTLPEndpoint != "collector:4318" {
		t.Errorf("otlp endpoint = %q", cfg.Telemetry.OTLPEndpoint)
	}
	if cfg.Telemetry.TraceSampleRatio != 0.25 {
		t.Errorf("sample ratio = %v", cfg.Telemetry.TraceSampleRatio)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("log level = %q", cfg.Logging.Level)
	}
	if len(cfg.HTTP.CORSAllowedOrigins) != 2 {
		t.Errorf("cors origins = %v", cfg.HTTP.CORSAllowedOrigins)
	}
}

func TestMalformedEnvironmentValueIsIgnoredRatherThanCorruptingConfig(t *testing.T) {
	// A bad numeric value must leave the default in place. Overwriting with the
	// zero value would silently disable a limit because of a typo in a YAML file
	// or a compose file.
	t.Setenv("SYNAPASS_AUTH_MIN_KEY_LENGTH", "not-a-number")
	t.Setenv("SYNAPASS_HTTP_MAX_BODY_BYTES", "8MiB")
	t.Setenv("SYNAPASS_ADMIN_ENABLED", "maybe")

	cfg := Default()
	want := cfg.Auth.MinKeyLength
	wantBody := cfg.HTTP.MaxBodyBytes
	wantAdmin := cfg.Admin.Enabled

	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}

	if cfg.Auth.MinKeyLength != want {
		t.Errorf("min key length = %d, want the default %d", cfg.Auth.MinKeyLength, want)
	}
	if cfg.HTTP.MaxBodyBytes != wantBody {
		t.Errorf("max body bytes = %d, want the default %d", cfg.HTTP.MaxBodyBytes, wantBody)
	}
	if cfg.Admin.Enabled != wantAdmin {
		t.Errorf("admin enabled = %v, want the default %v", cfg.Admin.Enabled, wantAdmin)
	}
}

func TestEnvironmentIsCaseInsensitiveForBooleans(t *testing.T) {
	t.Setenv("SYNAPASS_REDIS_REQUIRED", "TRUE")
	cfg := Default()
	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if !cfg.Redis.Required {
		t.Error("TRUE should parse as true")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestValidationRejectsInvalidConfigurations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "empty app name",
			mutate: func(c *Config) { c.App.Name = "  " },
			want:   "app.name",
		},
		{
			name:   "missing database dsn",
			mutate: func(c *Config) { c.Database.DSN = "" },
			want:   "database.dsn",
		},
		{
			name:   "unknown routing strategy",
			mutate: func(c *Config) { c.Routing.DefaultStrategy = "magic" },
			want:   "routing.default_strategy",
		},
		{
			name:   "unknown log level",
			mutate: func(c *Config) { c.Logging.Level = "verbose" },
			want:   "logging.level",
		},
		{
			name:   "weak minimum key length",
			mutate: func(c *Config) { c.Auth.MinKeyLength = 8 },
			want:   "auth.min_key_length",
		},
		{
			name:   "sample ratio above one",
			mutate: func(c *Config) { c.Telemetry.TraceSampleRatio = 4 },
			want:   "trace_sample_ratio",
		},
		{
			name:   "negative retention",
			mutate: func(c *Config) { c.Admin.LogRetentionDays = -1 },
			want:   "retention",
		},
		{
			name:   "empty listen address",
			mutate: func(c *Config) { c.HTTP.Addr = "" },
			want:   "http.addr",
		},
		{
			name: "required dependency left unconfigured",
			mutate: func(c *Config) {
				c.Redis.Addr = ""
				c.Redis.Required = true
			},
			want: "redis.addr",
		},
		{
			name: "response cache without a ttl",
			mutate: func(c *Config) {
				c.Cache.ResponseCache = true
				c.Cache.ResponseTTL = 0
			},
			want: "cache.response_ttl",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			// A DSN is required, so seed one before applying the mutation.
			cfg.Database.DSN = "postgres://u:p@localhost:5432/synapass"
			tc.mutate(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidationAggregatesEveryProblem(t *testing.T) {
	// An operator fixing a config file should see every mistake at once rather
	// than discovering them one restart at a time.
	cfg := Default()
	cfg.App.Name = ""
	cfg.Database.DSN = ""
	cfg.Logging.Level = "chatty"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	message := err.Error()
	for _, want := range []string{"app.name", "database.dsn", "logging.level"} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q should mention %q", message, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Reconciled defaults: strategy catalogue and tunnel floor
// ---------------------------------------------------------------------------

func TestAllShippedStrategiesValidate(t *testing.T) {
	// The strategy catalogue lives in internal/domain and internal/routing;
	// validation and the shipped example must accept every member of it.
	// config.example.yaml documents the same five names in its
	// routing.default_strategy comment.
	for _, strategy := range []string{"priority", "weighted", "lowest_cost", "lowest_latency", "highest_quality"} {
		t.Run(strategy, func(t *testing.T) {
			cfg := deployableDefaults()
			cfg.Routing.DefaultStrategy = strategy
			cfg.Policies = []PolicyConfig{{
				Name:     "test",
				Strategy: strategy,
				Targets:  []TargetConfig{{Model: "gpt-4o-mini"}},
			}}
			if err := cfg.Finalize(); err != nil {
				t.Errorf("strategy %q must validate: %v", strategy, err)
			}
		})
	}
}

func TestTunnelDefaultsToDisabledSafeFloor(t *testing.T) {
	// Reconciled truth: code defaults tunnel.enabled to false (a hand-written
	// config that omits the key creates nothing), while every shipped template
	// (config.example.yaml, compose, desktop/native installers) sets it true
	// explicitly. Pin the floor so a future default change is deliberate.
	cfg := Default()
	if cfg.Tunnel.Enabled {
		t.Error("Default().Tunnel.Enabled must be false; shipped templates opt in explicitly")
	}
}

func TestTunnelEnabledViaEnvironment(t *testing.T) {
	// The compose/docker path enables tunnels through the environment, which
	// must override the disabled floor without a config file change.
	t.Setenv("SYNAPASS_TUNNEL_ENABLED", "true")
	cfg := deployableDefaults()
	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if !cfg.Tunnel.Enabled {
		t.Error("SYNAPASS_TUNNEL_ENABLED=true must enable tunnels over the disabled default")
	}
}

// ---------------------------------------------------------------------------
// Secret handling
// ---------------------------------------------------------------------------

func TestRedactedHidesEverySecret(t *testing.T) {
	cfg := Default()
	cfg.Database.Password = "hunter2"
	cfg.Database.DSN = "postgres://app:hunter2@db:5432/synapass"
	cfg.Redis.Password = "redis-secret"
	cfg.ClickHouse.Password = "ch-secret"
	cfg.NATS.Token = "nats-token"
	cfg.Auth.AdminKey = "syn_admin_super_secret"

	redacted := cfg.Redacted()
	encoded := redactedToJSON(t, redacted)

	for _, secret := range []string{"hunter2", "redis-secret", "ch-secret", "nats-token", "syn_admin_super_secret"} {
		if strings.Contains(encoded, secret) {
			t.Errorf("the redacted configuration still contains %q: %s", secret, encoded)
		}
	}

	// Redaction must not mutate the live configuration, or the running gateway
	// would lose its own credentials the first time an admin read the config.
	if cfg.Database.Password != "hunter2" {
		t.Error("Redacted must return a copy, not mutate the receiver")
	}
	if cfg.Auth.AdminKey != "syn_admin_super_secret" {
		t.Error("Redacted must not clear the live admin key")
	}
}

func TestRedactedKeepsNonSecrets(t *testing.T) {
	cfg := Default()
	cfg.App.Name = "synapass-edge"
	cfg.Database.Host = "db.internal"

	redacted := cfg.Redacted()
	if redacted.App.Name != "synapass-edge" {
		t.Errorf("app name = %q", redacted.App.Name)
	}
	if redacted.Database.Host != "db.internal" {
		t.Errorf("host = %q", redacted.Database.Host)
	}
}

// ---------------------------------------------------------------------------
// File loading
// ---------------------------------------------------------------------------

func TestLoadReadsAYAMLFile(t *testing.T) {
	// The administrator credential is deliberately not settable from a file: the
	// file names an environment variable that holds it, so a checked-in config can
	// be committed without carrying a secret.
	t.Setenv("SYNAPASS_TEST_LOAD_ADMIN_KEY", "syn_admin_from_the_environment")

	dir := t.TempDir()
	path := filepath.Join(dir, "synapass.yaml")
	writeFile(t, path, `
app:
  name: from-file
  environment: production
http:
  addr: ":18080"
database:
  dsn: "postgres://file:file@filehost:5432/synapass"
telemetry:
  otlp_endpoint: "collector:4318"
auth:
  admin_key_env: SYNAPASS_TEST_LOAD_ADMIN_KEY
logging:
  level: warn
routing:
  default_strategy: lowest_latency
`)

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.App.Name != "from-file" {
		t.Errorf("app name = %q", cfg.App.Name)
	}
	if cfg.HTTP.Addr != ":18080" {
		t.Errorf("http addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Database.DSN != "postgres://file:file@filehost:5432/synapass" {
		t.Errorf("dsn = %q", cfg.Database.DSN)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("log level = %q", cfg.Logging.Level)
	}
	if cfg.Routing.DefaultStrategy != "lowest_latency" {
		t.Errorf("strategy = %q", cfg.Routing.DefaultStrategy)
	}
	// Settings the file did not mention keep their defaults rather than
	// becoming zero values.
	if cfg.Telemetry.PrometheusPath != "/metrics" {
		t.Errorf("prometheus path = %q, want the default preserved", cfg.Telemetry.PrometheusPath)
	}
	if cfg.ConfigFile != path {
		t.Errorf("config file = %q, want %q recorded for diagnostics", cfg.ConfigFile, path)
	}
	if cfg.Auth.AdminKey != "syn_admin_from_the_environment" {
		t.Errorf("admin key = %q, want it resolved through the named variable", cfg.Auth.AdminKey)
	}
}

func TestLoadEnvironmentBeatsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synapass.yaml")
	writeFile(t, path, "app:\n  name: from-file\nhttp:\n  addr: \":18080\"\ntelemetry:\n  otlp_endpoint: \"collector:4318\"\n")

	t.Setenv("SYNAPASS_APP_NAME", "from-env")

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.Name != "from-env" {
		t.Errorf("app name = %q, want the environment to win", cfg.App.Name)
	}
	// The file still supplies what the environment is silent about.
	if cfg.HTTP.Addr != ":18080" {
		t.Errorf("http addr = %q, want the file value", cfg.HTTP.Addr)
	}
}

func TestLoadRejectsAnExplicitlyNamedMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), true)
	if err == nil {
		t.Fatal("an explicitly named missing config file must be reported, not ignored")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synapass.yaml")
	writeFile(t, path, "app: [this is not a mapping]\n")

	if _, err := Load(path, true); err == nil {
		t.Fatal("malformed YAML must be reported")
	}
}

// ---------------------------------------------------------------------------
// Environment classification
// ---------------------------------------------------------------------------

func TestIsProductionIsExplicit(t *testing.T) {
	cases := map[string]bool{
		"production":  true,
		"prod":        true,
		"Production":  true,
		"staging":     false,
		"development": false,
		"":            false,
	}

	// Development affordances (anonymous tenant, CORS wildcard, the published
	// placeholder key) apply to a local environment only. Staging is deployed
	// and must be held to the same standard as production for those checks.
	development := map[string]bool{
		"development": true,
		"dev":         true,
		"test":        true,
		"":            true,
		"staging":     false,
		"stage":       false,
		"production":  false,
		"prod":        false,
		"Production":  false,
	}

	for environment, wantProduction := range cases {
		cfg := Default()
		cfg.App.Environment = environment

		if got := cfg.IsProduction(); got != wantProduction {
			t.Errorf("environment %q: IsProduction = %v, want %v", environment, got, wantProduction)
		}
		if want, ok := development[environment]; ok && cfg.IsDevelopment() != want {
			t.Errorf("environment %q: IsDevelopment = %v, want %v", environment, cfg.IsDevelopment(), want)
		}
	}
}

func TestDefaultSearchPathsIncludeTheUsualLocations(t *testing.T) {
	paths := DefaultSearchPaths()
	if len(paths) == 0 {
		t.Fatal("a config file must be discoverable without an explicit path")
	}
	joined := strings.Join(paths, " ")
	for _, want := range []string{".yaml", ".yml", ".json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("search paths %v should cover %s files", paths, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// redactedToJSON renders the redacted configuration for secret scanning.
func redactedToJSON(t *testing.T, cfg *Config) string {
	t.Helper()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// writeFile writes a test fixture.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
