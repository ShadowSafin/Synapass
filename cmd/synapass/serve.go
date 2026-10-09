package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/shadowsafin/synapass/internal/api"
	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/bootstrap"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/platcache"
	"github.com/shadowsafin/synapass/internal/policy"
	"github.com/shadowsafin/synapass/internal/providers"
	"github.com/shadowsafin/synapass/internal/routing"
	"github.com/shadowsafin/synapass/internal/storage"
	"github.com/shadowsafin/synapass/internal/telemetry"
	"github.com/shadowsafin/synapass/internal/tunnel"
	"github.com/shadowsafin/synapass/internal/version"
)

// App holds every constructed service so shutdown can release them deterministically.
//
// Construction is separated from serving so a wiring failure is reported before any
// port is opened, which means a broken deployment never briefly accepts traffic it
// cannot serve.
type App struct {
	cfg    *config.Config
	logger *slog.Logger

	postgres   *storage.Postgres
	redis      *storage.Redis
	clickhouse *storage.ClickHouse
	nats       *storage.NATS

	repos    *storage.Repositories
	adapters *providers.Registry
	health   *routing.HealthTracker
	policies *policy.Resolver

	metrics  *telemetry.Metrics
	tracer   *telemetry.Tracer
	recorder *telemetry.Recorder

	// tunnels owns the temporary-tunnel child process, if any.
	tunnels *tunnel.Manager

	server *api.Server

	// registry owns the adapter registry so a catalogue refresh can swap it in
	// atomically while requests are in flight.
	registry *providerRegistryHolder

	// Background loops. The context is retained so shutdown can cancel them and
	// wait for the in-flight iteration to finish.
	cancelLoops context.CancelFunc
	loopsCtx    context.Context
	loops       sync.WaitGroup
}

// runServe constructs and runs the gateway.
func runServe(cfg *config.Config, logger *slog.Logger) error {
	ctx, cancel := signalContext()
	defer cancel()

	app, err := buildApp(ctx, cfg, logger)
	if err != nil {
		code := exitUnavailable
		if errors.Is(err, errConfiguration) {
			code = exitConfig
		}
		return &exitError{code: code, err: err}
	}
	defer app.Close()

	return app.Run(ctx)
}

// errConfiguration marks an error caused by configuration rather than availability.
var errConfiguration = errors.New("configuration error")

// buildApp constructs every service in dependency order.
//
// The order is not arbitrary: migrations must run before repositories can be used,
// the catalogue must be seeded before adapters can be built, and adapters must exist
// before the routing engine can check provider availability.
func buildApp(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error) {
	app := &App{cfg: cfg, logger: logger}

	logger.Info("starting Synapass",
		"version", version.Short(),
		"environment", cfg.App.Environment,
		"instance", cfg.App.InstanceID,
		"config_file", cfg.ConfigFile,
	)

	// ---- PostgreSQL: required ----
	postgres, err := storage.NewPostgres(ctx, cfg.Database, logger)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errConfiguration, err)
	}
	app.postgres = postgres

	if cfg.Database.AutoMigrate {
		result, err := postgres.Migrate(ctx)
		if err != nil {
			return nil, fmt.Errorf("apply database migrations: %w", err)
		}
		if len(result.Applied) > 0 {
			logger.Info("database schema updated",
				"applied", result.Applied, "skipped", len(result.Skipped))
		}
	}

	app.repos = storage.NewRepositories(postgres.Pool(), logger)

	// ---- Optional subsystems ----
	// Each is attempted, and a failure is tolerated unless the configuration marks
	// the dependency as required. Degrading rather than refusing to start is what
	// lets a single-host install run without a full observability stack.
	if err := app.connectRedis(ctx); err != nil {
		return nil, err
	}
	if err := app.connectClickHouse(ctx); err != nil {
		return nil, err
	}
	if err := app.connectNATS(ctx); err != nil {
		return nil, err
	}

	// ---- Telemetry ----
	app.metrics = telemetry.NewMetrics(telemetry.MetricsConfig{
		Enabled:     true,
		Namespace:   "synapass",
		Instance:    cfg.App.InstanceID,
		Version:     version.Version,
		Environment: cfg.App.Environment,
		GoCollector: true,
	})
	app.metrics.SetBuildInfo(version.Current().GoVersion)

	tracer, err := telemetry.NewTracer(ctx, cfg.Telemetry, cfg.App, logger)
	if err != nil {
		// A broken collector configuration must not prevent serving traffic.
		logger.Warn("failed to configure OpenTelemetry; continuing without it", "error", err)
		tracer = nil
	}
	app.tracer = tracer

	// ---- Catalogue seeding ----
	if cfg.App.BootstrapFromConfig {
		seeder := bootstrap.NewSeeder(cfg, app.repos, logger)
		if _, err := seeder.Apply(ctx); err != nil {
			return nil, fmt.Errorf("apply the configured provider catalogue: %w", err)
		}
	}

	// ---- Phase 4: tool registry ----
	// Built-in handlers are seeded only when gateway execution is enabled. With
	// client-executed tools (the default) there is nothing for the gateway to
	// run, so seeding now/echo would only advertise tools no request will ever
	// use. Upsert-by-name makes seeding idempotent, and it never overwrites an
	// operator edit because the seed writes only the fields it owns and
	// preserves a disabled state.
	if cfg.Tools.GatewayExecution {
		if err := bootstrap.SeedBuiltinTools(ctx, app.repos.Tools, logger); err != nil {
			logger.Warn("could not seed the built-in tool registry; tool calling will advertise nothing",
				"error", err)
		}
	} else {
		logger.Info("gateway-side tool execution is disabled; clients run their own tools",
			"hint", "set tools.gateway_execution: true or SYNAPASS_TOOLS_GATEWAY_EXECUTION=true to opt in")
	}

	// ---- Provider adapters ----
	registry, failures, err := bootstrap.BuildAdapters(ctx, cfg, app.repos, logger)
	if err != nil {
		return nil, fmt.Errorf("build provider adapters: %w", err)
	}
	app.adapters = registry
	app.registry = &providerRegistryHolder{cfg: cfg, registry: registry, app: app}
	if registry.Len() == 0 {
		logger.Warn("no provider adapters are available; every inference request will fail",
			"failed", len(failures))
	}

	// ---- Routing ----
	app.health = routing.NewHealthTracker(routing.HealthConfig{
		DegradeThreshold:   domain.DegradeThreshold,
		UnhealthyThreshold: domain.DegradeThreshold * 3,
		Window:             cfg.Routing.HealthWindow.Std(),
	})

	app.policies = policy.NewResolver(app.repos.Policies, cfg.Cache.PolicyCacheTTL.Std(),
		policy.WithLogger(logger))
	if err := app.policies.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("load routing policies: %w", err)
	}

	// The registry is read through an adapter so routing never imports storage, and
	// through a snapshot cache because it is read on every request and changes a few
	// times a week.
	catalogue := storage.NewCatalogue(app.repos.Providers, app.repos.Models)
	cacheCatalogue := routing.NewCachedCatalogue(catalogue)
	if err := cacheCatalogue.Load(ctx); err != nil {
		return nil, fmt.Errorf("load the model registry: %w", err)
	}

	// ---- Phase 2 intelligence services ----
	phase2 := bootstrap.BuildPhase2(cfg, app.redis)
	// Attach score/guardrail hooks to the engine so routing is score-informed
	// and kill-switch aware. Nil-safe: hooks no-op when services are disabled.
	engine := routing.NewEngine(cacheCatalogue, app.policies, app.health,
		routing.WithDefaults(routingDefaults(cfg)),
		routing.WithAvailability(bootstrap.NewRegistryAvailability(registry)),
		routing.WithLogger(logger),
		routing.WithIntelligent(routing.IntelligentOptions{
			Scores:     phase2.ScoreAdapter,
			Guardrails: phase2.Guardrails,
		}),
	)

	sink := bootstrap.NewSink(bootstrap.SinkOptions{
		Repos:      app.repos,
		ClickHouse: app.clickhouse,
		NATS:       app.nats,
		Logger:     logger,
	})
	app.recorder = telemetry.NewRecorder(sink, app.metrics, telemetry.RecorderConfig{
		QueueSize:     8192,
		BatchSize:     256,
		FlushInterval: 2 * time.Second,
		WriteTimeout:  10 * time.Second,
	}, logger)

	executor := routing.NewExecutor(registry, app.health,
		routing.WithObserver(app.recorder),
		routing.WithExecutorLogger(logger),
		routing.WithJitter(cfg.Routing.DefaultRetry.Jitter == nil || *cfg.Routing.DefaultRetry.Jitter),
	)

	enforcer := policy.NewEnforcer(app.redis, policy.WithEnforcerLogger(logger))

	// ---- Authentication ----
	keyStore := app.repos.APIKeys
	authenticator := auth.New(auth.Options{
		Store:                keyStore,
		Cache:                app.redis,
		CacheTTL:             cfg.Auth.CacheTTL.Std(),
		AdminKey:             bootstrap.ResolveAdminKey(cfg),
		MinKeyLength:         cfg.Auth.MinKeyLength,
		KeyPrefix:            cfg.Auth.KeyPrefix,
		HeaderName:           cfg.Auth.HeaderName,
		RequireScope:         cfg.Admin.RequireScope,
		AllowAnonymousTenant: cfg.Auth.AllowAnonymousTenant,
		Logger:               logger,
	})

	// ---- Temporary tunnels ----
	// The manager exists regardless of the enabled flag so the admin surface
	// can report "disabled" uniformly; creation itself is gated on the flag.
	tunnels := tunnel.NewManager(tunnel.Config{
		Binary:             cfg.Tunnel.Binary,
		DefaultTarget:      cfg.Tunnel.DefaultTarget,
		DashboardTarget:    cfg.Tunnel.DashboardTarget,
		AllowCustomTargets: cfg.Tunnel.AllowCustomTargets,
		AutoRestart:        cfg.Tunnel.AutoRestart,
		StartupTimeout:     cfg.Tunnel.StartupTimeout.Std(),
	}, gatewayLocalAddr(cfg.HTTP.Addr), app.repos.Tunnels, logger)
	tunnels.SetOnEvent(func(e tunnel.Event) {
		observeTunnelEvent(app.metrics, app.nats, e)
	})
	if reconciled, err := app.repos.Tunnels.MarkStaleStopped(ctx, "gateway restarted"); err != nil {
		logger.Warn("could not reconcile tunnel sessions from a previous lifetime; "+
			"a dead tunnel may be reported until it is stopped", "error", err)
	} else if reconciled > 0 {
		logger.Info("reconciled tunnel sessions left behind by a previous lifetime",
			"stopped", reconciled)
	}
	app.tunnels = tunnels

	// ---- Platform cache (tenant/catalog/route/flags) ----
	// L2 is Redis when available, absent otherwise: without Redis the cache
	// runs L1 + DB fallback only and every hook stays safe. Reads never
	// fail on a Redis outage; they degrade to the loader (DB).
	var platStore platcache.Store
	if app.redis != nil && app.redis.Client() != nil {
		platStore = platcache.NewRedisAdapter(app.redis.Client(), app.redis.Prefix())
	}
	platMetrics := &platcache.MetricsHook{
		OnHit: func(tenant, kind string) {
			app.metrics.ObserveCacheKind(tenant, "platform:"+kind, true)
		},
		OnMiss: func(tenant string) {
			app.metrics.ObserveCacheKind(tenant, "platform:miss", false)
		},
		OnStaleHit: func(tenant, kind string) {
			app.metrics.ObserveCacheKind(tenant, "platform-stale:"+kind, true)
		},
		OnInvalidate: func(scope, reason string) {
			app.metrics.ObserveCacheInvalidation("platform:"+scope, reason)
		},
		OnResolve: func(cached bool, seconds float64) {
			outcome := "miss"
			if cached {
				outcome = "hit"
			}
			app.metrics.ObserveCacheLookup("platform", outcome, seconds)
		},
	}
	platformCache := platcache.New(platStore, platcache.Options{
		KeyPrefix: "synapass",
		Logger:    logger,
		Metrics:   platMetrics,
	})

	// ---- HTTP ----
	server, err := api.NewServer(api.Deps{
		Config:        cfg,
		Logger:        logger,
		Version:       version.Current(),
		Authenticator: authenticator,
		Engine:        engine,
		Executor:      executor,
		Health:        app.health,
		Policies:      app.policies,
		Enforcer:      enforcer,
		Adapters:      registry,
		Repositories:  app.repos,
		Redis:         app.redis,
		PlatformCache: platformCache,
		Postgres:      app.postgres,
		ClickHouse:    app.clickhouse,
		NATS:          app.nats,
		Metrics:       app.metrics,
		Tracer:        tracer,
		Recorder:      app.recorder,
		Tools: &api.ToolServices{
			Registry:    app.repos.Tools,
			Policies:    app.repos.ToolPolicies,
			Invocations: &toolInvocationSink{repo: app.repos.Invocations},
			Runs:        &agentRunSink{runs: app.repos.AgentRuns},
		},
		Tunnels: tunnels,

		// Console operator login. Built here because this is the only place that
		// holds the user store, the session store and the settings latch together.
		DashboardAuth: bootstrap.BuildDashboardAuth(cfg, app.repos, logger),
		Classifier:    &bootstrap.ClassifierAdapter{Inner: phase2.Classifier},
		PolicyEngine:  &bootstrap.PolicyEngineAdapter{Inner: phase2.PolicyEngine},
		Shaper:        &bootstrap.ShapingAdapter{Inner: phase2.Shaper},
		ResponseCache: &bootstrap.CacheAdapter{Inner: phase2.Cache},
		Scorer:        phase2.ScoreAdapter,
		Guardrails:    phase2.Guardrails,
		Replay:        &bootstrap.ReplayAdapter{Replay: app.repos.Replay, Feedback: app.repos.Feedback},
		StartedAt:     time.Now(),
		// Catalogue writes through the admin API apply immediately through
		// this hook; the background refresh loop remains the convergence
		// backstop when a reload fails.
		Reloader: api.ReloaderFunc(func(ctx context.Context) error {
			if err := cacheCatalogue.Load(ctx); err != nil {
				return err
			}
			if err := app.registry.reload(ctx); err != nil {
				return err
			}
			return app.policies.Refresh(ctx)
		}),
	})
	if err != nil {
		return nil, err
	}
	app.server = server

	// The catalogue is refreshed on an interval so a provider added through the
	// dashboard becomes routable without a restart. The refresh also rebuilds the
	// adapter registry, which is what picks up a changed credential.
	app.startCatalogueRefresh(cacheCatalogue, app.registry)

	if cfg.Routing.HealthCheckEnabled {
		app.startHealthProbes()
	}
	app.startUsageReconciliation()

	return app, nil
}

// gatewayLocalAddr resolves the HTTP listen address to the loopback URL the
// "gateway" tunnel target proxies to. A bare ":port" listen address means all
// interfaces, but the tunnel must point at this process, so the host is
// rewritten to 127.0.0.1 while the port is preserved.
func gatewayLocalAddr(listenAddr string) string {
	host, port := "127.0.0.1", ""
	if h, p, ok := splitListenAddr(listenAddr); ok {
		if h != "" {
			host = h
		}
		port = p
	}
	if port == "" {
		port = "8080"
	}
	return "http://" + host + ":" + port
}

// splitListenAddr splits a Go listen address ("host:port" or ":port").
func splitListenAddr(addr string) (host, port string, ok bool) {
	if addr == "" {
		return "", "", false
	}
	idx := -1
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", "", false
	}
	return addr[:idx], addr[idx+1:], addr[idx+1:] != ""
}

// observeTunnelEvent feeds tunnel lifecycle transitions into metrics and the
// event stream. It runs synchronously from the tunnel supervisor, so it does
// CPU-bound and best-effort work only — never database writes.
func observeTunnelEvent(metrics *telemetry.Metrics, nats *storage.NATS, e tunnel.Event) {
	if e.Session == nil {
		return
	}
	target := e.Session.Target
	switch e.Kind {
	case "url":
		if metrics != nil {
			metrics.SetTunnelUp(target, true)
		}
	case "restarted":
		if metrics != nil {
			metrics.ObserveTunnelRestart(target)
			metrics.SetTunnelUp(target, true)
		}
	case "stopped", "failed":
		if metrics != nil {
			metrics.SetTunnelUp(target, false)
			metrics.ObserveTunnelSession(target, e.Kind)
		}
	}
	if nats != nil && (e.Kind == "url" || e.Kind == "failed" || e.Kind == "restarted") {
		nats.PublishTunnelStatus(&domain.TunnelEvent{
			Event: e.Kind, Session: e.Session, CreatedAt: domain.Now(),
		})
	}
}

// routingDefaults converts configuration into engine defaults.
func routingDefaults(cfg *config.Config) routing.Defaults {
	defaults := routing.DefaultDefaults()
	defaults.Strategy = domain.RoutingStrategy(cfg.Routing.DefaultStrategy)
	if defaults.Strategy == "" {
		defaults.Strategy = domain.StrategyPriority
	}
	if cfg.Routing.DefaultMaxAttempts > 0 {
		defaults.MaxAttempts = cfg.Routing.DefaultMaxAttempts
	}
	defaults.Timeout = domain.TimeoutPolicy{
		Total:      cfg.Routing.DefaultTimeout.Total.Std(),
		PerAttempt: cfg.Routing.DefaultTimeout.PerAttempt.Std(),
		Connect:    cfg.Routing.DefaultTimeout.Connect.Std(),
		StreamIdle: cfg.Routing.DefaultTimeout.StreamIdle.Std(),
		FirstToken: cfg.Routing.DefaultTimeout.FirstToken.Std(),
	}
	defaults.Retry = domain.RetryPolicy{
		MaxAttempts:    cfg.Routing.DefaultRetry.MaxAttempts,
		InitialBackoff: cfg.Routing.DefaultRetry.InitialBackoff.Std(),
		MaxBackoff:     cfg.Routing.DefaultRetry.MaxBackoff.Std(),
		Multiplier:     cfg.Routing.DefaultRetry.Multiplier,
	}
	if cfg.Routing.DefaultRetry.Jitter != nil {
		defaults.Retry.Jitter = *cfg.Routing.DefaultRetry.Jitter
	}
	if cfg.Routing.DefaultRetry.HonorRetryAfter != nil {
		defaults.Retry.HonorRetryAfter = *cfg.Routing.DefaultRetry.HonorRetryAfter
	}
	for _, code := range cfg.Routing.DefaultRetry.RetryOn {
		defaults.Retry.RetryOn = append(defaults.Retry.RetryOn, domain.ErrorCode(code))
	}
	defaults.Limits = domain.PolicyLimits{
		MaxCostPerRequestUSD: cfg.Routing.DefaultLimits.MaxCostPerRequestUSD,
		MaxPromptTokens:      cfg.Routing.DefaultLimits.MaxPromptTokens,
		MaxOutputTokens:      cfg.Routing.DefaultLimits.MaxOutputTokens,
		LatencyTargetMS:      cfg.Routing.DefaultLimits.LatencyTargetMS,
		RequestsPerMinute:    cfg.Routing.DefaultLimits.RequestsPerMinute,
		TokensPerMinute:      cfg.Routing.DefaultLimits.TokensPerMinute,
		DailyBudgetUSD:       cfg.Routing.DefaultLimits.DailyBudgetUSD,
		MonthlyBudgetUSD:     cfg.Routing.DefaultLimits.MonthlyBudgetUSD,
	}
	defaults.Fallback = domain.DefaultFallbackPolicy()
	defaults.Fallback.MaxAttempts = cfg.Routing.DefaultMaxAttempts
	if defaults.Fallback.MaxAttempts <= 0 {
		defaults.Fallback.MaxAttempts = 2
	}
	if cfg.Routing.LatencyPriors != nil {
		defaults.LatencyPriors = make(map[string]int64, len(cfg.Routing.LatencyPriors))
		for model, ms := range cfg.Routing.LatencyPriors {
			defaults.LatencyPriors[model] = int64(ms)
		}
	}
	return defaults
}

// Run serves traffic until the context is cancelled.
func (a *App) Run(ctx context.Context) error {
	if a.server == nil {
		return fmt.Errorf("the HTTP server was not constructed")
	}

	httpServer := &http.Server{
		Addr:              a.cfg.HTTP.Addr,
		Handler:           a.server.Handler(),
		ReadHeaderTimeout: a.cfg.HTTP.ReadHeaderTimeout.Std(),
		ReadTimeout:       a.cfg.HTTP.ReadTimeout.Std(),
		// WriteTimeout must exceed the longest possible streaming response. It is
		// deliberately long because a chat completion can legitimately take minutes
		// and a shorter deadline would truncate a healthy stream.
		WriteTimeout: a.cfg.HTTP.WriteTimeout.Std(),
		IdleTimeout:  a.cfg.HTTP.IdleTimeout.Std(),
		ErrorLog:     slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		a.logger.Info("http server listening", "addr", a.cfg.HTTP.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server failed: %w", err)
		}
		return nil

	case <-ctx.Done():
		a.logger.Info("shutdown signal received; draining")
	}

	// Shutdown order matters: stop accepting new requests first, then flush
	// telemetry, then close datastores. Closing a datastore before the telemetry
	// pipeline drains would lose the final batch of usage records.
	timeout := shutdownTimeout(a.cfg)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		a.logger.Warn("graceful shutdown timed out; forcing close", "error", err)
		_ = httpServer.Close()
	}
	a.logger.Info("http server stopped")

	if a.cancelLoops != nil {
		a.cancelLoops()
		a.loops.Wait()
	}

	if a.recorder != nil {
		if err := a.recorder.Close(shutdownCtx); err != nil {
			a.logger.Warn("telemetry pipeline did not flush completely", "error", err)
		}
	}

	// A public URL must never outlive the process that served it. The tunnel
	// is stopped after the HTTP server drains so in-flight tunneled requests
	// still complete.
	if a.tunnels != nil && a.cfg.Tunnel.StopOnShutdown {
		if _, err := a.tunnels.Stop(shutdownCtx, "", "gateway shutdown", "system"); err != nil {
			a.logger.Warn("tunnel did not stop cleanly on shutdown", "error", err)
		}
	}

	a.logger.Info("shutdown complete")
	return nil
}

// Close releases every resource.
func (a *App) Close() {
	if a.nats != nil {
		_ = a.nats.Close()
	}
	if a.clickhouse != nil {
		_ = a.clickhouse.Close()
	}
	if a.redis != nil {
		_ = a.redis.Close()
	}
	if a.tracer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.tracer.Shutdown(ctx)
		cancel()
	}
	if a.postgres != nil {
		a.postgres.Close()
	}
}

// ---------------------------------------------------------------------------
// Optional subsystem connections
// ---------------------------------------------------------------------------

// connectRedis attaches Redis, or degrades when it is not required.
func (a *App) connectRedis(ctx context.Context) error {
	if a.cfg.Redis.Addr == "" && len(a.cfg.Redis.SentinelAddrs) == 0 {
		a.logger.Warn("redis is not configured; rate limiting will use the in-process limiter " +
			"and credentials will not be cached")
		return nil
	}

	client, err := storage.NewRedis(ctx, a.cfg.Redis, a.logger)
	if err != nil {
		if a.cfg.Redis.Required {
			return fmt.Errorf("%w: %v", errConfiguration, err)
		}
		// Degrading is a deliberate choice: losing Redis costs caching and
		// distributed rate limiting, but the gateway can still serve traffic and
		// that is more valuable than refusing to start.
		a.logger.Warn("redis is unavailable; continuing without caching and distributed rate limiting",
			"error", err)
		return nil
	}
	a.redis = client
	return nil
}

// connectClickHouse attaches the analytics store.
func (a *App) connectClickHouse(ctx context.Context) error {
	if a.cfg.ClickHouse.Addr == "" && a.cfg.ClickHouse.DSN == "" {
		a.logger.Warn("clickhouse is not configured; traces will be exported over OTLP only")
		return nil
	}

	client, err := storage.NewClickHouse(ctx, a.cfg.ClickHouse, a.logger)
	if err != nil {
		if a.cfg.ClickHouse.Required {
			return fmt.Errorf("%w: %v", errConfiguration, err)
		}
		a.logger.Warn("clickhouse is unavailable; traces will not be persisted locally",
			"error", err)
		return nil
	}

	if _, err := client.Migrate(ctx); err != nil {
		if a.cfg.ClickHouse.Required {
			return fmt.Errorf("apply clickhouse migrations: %w", err)
		}
		a.logger.Warn("clickhouse migrations failed; telemetry writes may be incomplete",
			"error", err)
	}

	a.clickhouse = client
	return nil
}

// connectNATS attaches the messaging layer.
func (a *App) connectNATS(ctx context.Context) error {
	if a.cfg.NATS.URL == "" && len(a.cfg.NATS.URLs) == 0 {
		a.logger.Warn("nats is not configured; asynchronous fan-out is disabled")
		return nil
	}

	client, err := storage.NewNATS(ctx, a.cfg.NATS, a.logger)
	if err != nil {
		if a.cfg.NATS.Required {
			return fmt.Errorf("%w: %v", errConfiguration, err)
		}
		a.logger.Warn("nats is unavailable; asynchronous fan-out is disabled", "error", err)
		return nil
	}
	a.nats = client
	return nil
}
