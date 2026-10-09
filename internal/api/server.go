// Package api implements Synapass's HTTP surface: the OpenAI-compatible
// inference endpoints, the health and metrics endpoints, and the administrative
// API that backs the dashboard.
//
// # Handler discipline
//
// Handlers in this package do four things and nothing else: decode the request,
// enforce transport-level concerns, delegate to a service, and encode the response.
// Every routing, policy, provider and persistence decision lives in the package
// that owns it. That is what keeps the request path readable and the logic
// testable without a running server.
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/cost"
	"github.com/shadowsafin/synapass/internal/dashboardauth"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/logging"
	"github.com/shadowsafin/synapass/internal/platcache"
	"github.com/shadowsafin/synapass/internal/policy"
	"github.com/shadowsafin/synapass/internal/providers"
	"github.com/shadowsafin/synapass/internal/replay"
	"github.com/shadowsafin/synapass/internal/routing"
	"github.com/shadowsafin/synapass/internal/storage"
	"github.com/shadowsafin/synapass/internal/telemetry"
	"github.com/shadowsafin/synapass/internal/version"
)

// pathMetrics is the Prometheus scrape path, referenced by the span namer.
const pathMetrics = "/metrics"

// ModelSource lists registry entries for the model endpoints.
//
// It is an interface defined here, in the consuming package, so the model
// endpoints can be tested against a fixture registry without a database. The
// storage repository satisfies it unmodified.
type ModelSource interface {
	List(ctx context.Context) ([]domain.Model, error)
}

// Deps is everything the HTTP layer needs.
//
// It is a struct rather than a long argument list, and every field is an interface
// or a concrete service the handler actually uses, which makes it obvious from one
// place what the API can reach.
type Deps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Version version.Info

	// Authenticator validates credentials.
	Authenticator *auth.Authenticator
	// Engine produces route decisions.
	Engine *routing.Engine
	// Executor carries a decision through its fallback chain.
	Executor *routing.Executor
	// Health tracks provider health.
	Health *routing.HealthTracker
	// Policies resolves routing policies.
	Policies *policy.Resolver
	// Enforcer applies rate limits and budgets.
	Enforcer *policy.Enforcer
	// Adapters holds the live provider adapters.
	Adapters *providers.Registry

	// Repositories provide durable reads for the admin API.
	Repositories *storage.Repositories
	// Models overrides the model registry source. When nil, the repository's model
	// store is used.
	Models ModelSource
	// Redis backs rate limiting and credential caching; it may be nil.
	Redis *storage.Redis
	// Postgres backs the readiness check; it may be nil when only the admin API
	// is unavailable.
	Postgres *storage.Postgres
	// ClickHouse backs the readiness check; it may be nil.
	ClickHouse *storage.ClickHouse
	// NATS backs the readiness check; it may be nil.
	NATS *storage.NATS

	Metrics  *telemetry.Metrics
	Tracer   *telemetry.Tracer
	Recorder *telemetry.Recorder

	// Phase 2 intelligence services (all optional; nil preserves Phase 1).
	Classifier    ClassifierService
	PolicyEngine  PolicyEngineService
	Shaper        ShapingService
	ResponseCache ResponseCacheService
	// PlatformCache is the multi-layer tenant/catalog/route cache. Nil
	// disables it; every hook is nil-safe so a gateway without it behaves
	// exactly as before.
	PlatformCache PlatformCacheService
	Scorer        ScoringService
	Guardrails    GuardrailService
	Replay        ReplayService

	// Reloader applies catalogue writes immediately. When nil, the background
	// refresh loop converges on its own interval instead.
	Reloader Reloader

	// Tools is the Phase 4 tool plane: registry, policy, invocation history and
	// agent runs. Nil preserves the pre-Phase-4 behaviour exactly.
	Tools *ToolServices

	// Tunnels manages temporary public tunnels. Nil means the surface
	// reports the feature as disabled.
	Tunnels TunnelService

	// DashboardAuth authenticates human operators of the console. Nil means the
	// login surface is absent. It is optional rather than required because a
	// gateway deployed only as an inference proxy has no console to protect.
	DashboardAuth *dashboardauth.Service

	// StartedAt is used to report uptime.
	StartedAt time.Time
}

// Server is the HTTP application.
type Server struct {
	config   *config.Config
	logger   *slog.Logger
	version  version.Info
	redactor *logging.Redactor

	auth     *auth.Authenticator
	engine   *routing.Engine
	executor *routing.Executor
	health   *routing.HealthTracker
	policies *policy.Resolver
	enforcer *policy.Enforcer
	adapters *providers.Registry

	repos      *storage.Repositories
	models     ModelSource
	redis      *storage.Redis
	postgres   *storage.Postgres
	clickhouse *storage.ClickHouse
	nats       *storage.NATS

	metrics  *telemetry.Metrics
	tracer   *telemetry.Tracer
	recorder *telemetry.Recorder

	classifier ClassifierService
	policyEng  PolicyEngineService
	shaper     ShapingService
	cache      ResponseCacheService
	// plat is the platform cache (tenant/catalog/route/flags). Nil-safe.
	plat       *platcache.Cache
	scorer     ScoringService
	guard      GuardrailService
	replaySvc  ReplayService
	// replayRunner executes replay jobs in a background goroutine through
	// the live provider adapters. Nil when the stores are absent; creation
	// then persists a queued job that never runs, as before.
	replayRunner *replay.Runner

	// reloader applies catalogue writes immediately (Phase 3).
	reloader Reloader

	// tools is the Phase 4 tool plane.
	tools *ToolServices

	// tunnels manages temporary public tunnels.
	tunnels TunnelService

	// dashboardAuth authenticates console operators. Nil disables the surface.
	dashboardAuth *dashboardauth.Service

	// priceCache memoizes resolved versioned price sheets on the request
	// path so exact costing costs a map lookup, not a query.
	priceCache *cost.PriceCache

	startedAt time.Time
	// trustedProxies are the networks whose forwarding headers are honoured.
	trustedProxies []*net.IPNet

	// handler is the fully assembled router.
	handler http.Handler
}

// NewServer builds the HTTP application and its router.
func NewServer(deps Deps) (*Server, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		config:     deps.Config,
		logger:     logger,
		version:    deps.Version,
		redactor:   logging.NewRedactor(deps.Config.Logging.RedactHeaders),
		auth:       deps.Authenticator,
		engine:     deps.Engine,
		executor:   deps.Executor,
		health:     deps.Health,
		policies:   deps.Policies,
		enforcer:   deps.Enforcer,
		adapters:   deps.Adapters,
		repos:      deps.Repositories,
		models:     deps.Models,
		redis:      deps.Redis,
		postgres:   deps.Postgres,
		clickhouse: deps.ClickHouse,
		nats:       deps.NATS,
		metrics:    deps.Metrics,
		tracer:     deps.Tracer,
		recorder:   deps.Recorder,
		classifier: deps.Classifier,
		policyEng:  deps.PolicyEngine,
		shaper:     deps.Shaper,
		cache:      deps.ResponseCache,
		plat:       resolvePlatformCache(deps.PlatformCache),
		scorer:     deps.Scorer,
		guard:      deps.Guardrails,
		replaySvc:  deps.Replay,
		reloader:   deps.Reloader,
		tools:      deps.Tools,
		tunnels:    deps.Tunnels,

		dashboardAuth: deps.DashboardAuth,
		startedAt:  deps.StartedAt,
		priceCache: cost.NewPriceCache(priceCacheTTL),
	}

	// The replay runner needs the request logs, the captured prompts, the
	// job store and the live adapters together; this constructor is the only
	// place that holds all four. Pricing resolves through the same
	// versioned-sheet path live traffic uses, so replay costs foot with the
	// cost views instead of a second price book.
	if deps.Repositories != nil {
		s.replayRunner = replay.NewRunner(replay.RunnerDeps{
			Payloads: deps.Repositories.Payloads,
			Logs:     deps.Repositories.Logs,
			Store:    deps.Repositories.Replay,
			Adapters: deps.Adapters,
			Pricer:   s.priceReplayUsage,
			Logger:   logger,
		})
	}

	// Parse the trusted proxy list once. A malformed entry is reported at startup
	// rather than silently ignored, because silently ignoring it would mean the
	// gateway trusts a forwarding header it should not.
	for _, cidr := range deps.Config.HTTP.TrustedProxies {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if !strings.Contains(cidr, "/") {
			// A bare address is accepted as a single host.
			if ip := net.ParseIP(cidr); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				cidr = cidr + "/" + itoa(bits)
			}
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, domain.Errorf(domain.ErrCodeInternal,
				"http.trusted_proxies entry %q is not a valid CIDR", cidr)
		}
		s.trustedProxies = append(s.trustedProxies, network)
	}

	s.handler = s.buildRouter()
	return s, nil
}

// Handler returns the assembled router.
func (s *Server) Handler() http.Handler { return s.handler }

// modelSource returns the registry source, preferring the injected override.
func (s *Server) modelSource() ModelSource {
	if s.models != nil {
		return s.models
	}
	if s.repos != nil && s.repos.Models != nil {
		return s.repos.Models
	}
	return nil
}

// buildRouter assembles the middleware chain and routes.
//
// The chain order matters and is deliberate:
//
//	recover      outermost, so a panic in any later middleware is caught
//	identity     assigns the request id and opens the span
//	cors         answers preflight before any authentication work
//	access log   writes one line per request, including rejected ones
//	auth         per-route, so public endpoints stay public
func (s *Server) buildRouter() http.Handler {
	r := chi.NewRouter()

	r.Use(s.recoverMiddleware)
	r.Use(s.identityMiddleware)
	r.Use(s.corsMiddleware)
	r.Use(chimw.RealIP)
	r.Use(chimw.Compress(5))

	// Probe endpoints are unauthenticated by design: a load balancer cannot present
	// a credential, and a probe that requires one would fail closed on a
	// misconfigured secret and remove a healthy instance from rotation.
	//
	// The root is the same contract: a gateway reached from a browser (a
	// temporary tunnel, a mis-typed URL) gets a status page naming its real
	// endpoints instead of a bare 404 that looks like a broken deployment.
	r.Get("/", s.handleRoot)
	r.Get("/health", s.handleHealth)
	r.Get("/healthz", s.handleHealth)
	r.Get("/ready", s.handleReady)
	r.Get("/readiness", s.handleReady)
	r.Get("/version", s.handleVersion)

	if s.config.Telemetry.PrometheusEnabled {
		path := s.config.Telemetry.PrometheusPath
		if path == "" {
			path = pathMetrics
		}
		r.Handle(path, s.metrics.Handler())
	}

	// The OpenAI-compatible surface.
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware(domain.ScopeInference))

		r.Post("/v1/chat/completions", s.handleChatCompletions)
		r.Post("/v1/completions", s.handleCompletionsNotImplemented)
		r.Post("/v1/embeddings", s.handleEmbeddingsNotImplemented)
		r.Post("/v1/responses", s.handleResponsesNotImplemented)
		r.Post("/v1/feedback", s.handleSubmitFeedback)

		// Listing models requires inference scope rather than a read scope because
		// it is part of the standard inference client handshake.
		r.Get("/v1/models", s.handleListModels)
		r.Get("/v1/models/{model}", s.handleGetModel)
	})

	// Dashboard operator authentication.
	//
	// Registered before the admin group and outside adminMiddleware, because it is
	// how an operator obtains a credential in the first place. Gating it would be
	// circular. The routes are not unauthenticated in the sense that matters: they
	// can only *create* a session, setup is latched shut after the first operator,
	// and login is rate limited and locked out.
	//
	// The admin key still authenticates the admin API itself, so programmatic
	// callers and the smoke suite are unaffected. This is a gate on the console.
	if s.config.Admin.Enabled && s.dashboardAuth != nil {
		r.Route("/admin/v1/auth", func(r chi.Router) {
			r.Get("/state", s.handleAuthState)
			r.Post("/setup", s.handleAuthSetup)
			r.Post("/login", s.handleAuthLogin)
			r.Post("/logout", s.handleAuthLogout)
			r.Get("/me", s.handleAuthMe)
		})
	}

	// Administrative surface, mounted only when enabled.
	if s.config.Admin.Enabled {
		r.Route("/admin/v1", func(r chi.Router) {
			// Every admin route requires at least a read scope; write routes narrow
			// it further. A single group-level requirement would either over-grant
			// read access or force a redundant check on each write route.
			r.Use(s.adminMiddleware)

			r.Get("/overview", s.handleAdminOverview)
			r.Get("/system", s.handleAdminSystem)

			r.Get("/providers", s.handleAdminListProviders)
			r.Get("/providers/health", s.handleAdminProviderHealth)
			r.Post("/providers", s.handleAdminCreateProvider)
			r.Get("/providers/{id}", s.handleAdminGetProvider)
			r.Put("/providers/{id}", s.handleAdminUpdateProvider)
			r.Patch("/providers/{id}", s.handleAdminPatchProvider)
			r.Delete("/providers/{id}", s.handleAdminDeleteProvider)
			r.Post("/providers/{id}/probe", s.handleAdminProbeProvider)
			r.Put("/providers/{id}/credential", s.handleAdminSetProviderCredential)
			r.Get("/providers/{id}/credential", s.handleAdminGetProviderCredential)
			r.Delete("/providers/{id}/credential", s.handleAdminDeleteProviderCredential)
			r.Post("/providers/{id}/test", s.handleAdminTestProvider)
			r.Get("/providers/{id}/tests", s.handleAdminListProviderTests)
			r.Post("/providers/{id}/sync-models", s.handleAdminSyncProviderModels)
			// Probes a provider's models to learn their capabilities. Explicit
			// billable traffic, so this is an operator action, never automatic
			// unless detection is enabled in configuration.
			r.Post("/providers/{id}/detect-capabilities", s.handleAdminDetectCapabilities)

			r.Get("/models", s.handleAdminListModels)
			r.Post("/models", s.handleAdminCreateModel)
			r.Get("/models/{id}", s.handleAdminGetModel)
			r.Put("/models/{id}", s.handleAdminUpdateModel)
			r.Patch("/models/{id}", s.handleAdminPatchModel)
			r.Delete("/models/{id}", s.handleAdminDeleteModel)

			r.Get("/policies", s.handleAdminListPolicies)
			r.Get("/policies/{id}", s.handleAdminGetPolicy)
			r.Post("/policies", s.handleAdminCreatePolicy)
			r.Put("/policies", s.handleAdminUpsertPolicy)
			r.Delete("/policies/{id}", s.handleAdminDeletePolicy)
			r.Post("/policies/reload", s.handleAdminReloadPolicies)

			r.Get("/tenants", s.handleAdminListTenants)
			r.Post("/tenants", s.handleAdminCreateTenant)
			r.Get("/tenants/{id}", s.handleAdminGetTenant)
			r.Put("/tenants/{id}", s.handleAdminUpdateTenant)
			r.Delete("/tenants/{id}", s.handleAdminDeleteTenant)
			r.Get("/keys", s.handleAdminListKeys)
			r.Post("/keys", s.handleAdminCreateKey)
			r.Patch("/keys/{id}", s.handleAdminUpdateKey)
			r.Post("/keys/{id}/rotate", s.handleAdminRotateKey)
			r.Delete("/keys/{id}", s.handleAdminRevokeKey)

			r.Get("/overrides", s.handleAdminListOverrides)
			r.Post("/overrides", s.handleAdminCreateOverride)

			// Phase 4: tool registry, tool policy, tool history and agent runs.
			r.Get("/tools", s.handleAdminListTools)
			r.Post("/tools", s.handleAdminCreateTool)
			r.Put("/tools/{id}", s.handleAdminUpdateTool)
			r.Delete("/tools/{id}", s.handleAdminDeleteTool)
			r.Post("/tools/{id}/enabled", s.handleAdminSetToolEnabled)

			r.Get("/tool-policies", s.handleAdminListToolPolicies)
			r.Put("/tool-policies", s.handleAdminUpsertToolPolicy)
			r.Delete("/tool-policies/{id}", s.handleAdminDeleteToolPolicy)

			r.Get("/tool-invocations", s.handleAdminListToolInvocations)
			r.Get("/tool-invocations/{id}", s.handleAdminGetToolInvocation)
			r.Get("/agent-runs", s.handleAdminListAgentRuns)
			r.Get("/agent-runs/{id}", s.handleAdminGetAgentRun)

			r.Get("/usage/summary", s.handleAdminUsageSummary)
			r.Get("/usage/series", s.handleAdminUsageSeries)
			// The composed analytics report the console is built on: one request,
			// every breakdown, all describing the same window.
			r.Get("/analytics/report", s.handleAdminAnalyticsReport)
			r.Get("/requests", s.handleAdminListRequests)
			r.Get("/requests/{requestID}", s.handleAdminGetRequest)
			r.Get("/errors", s.handleAdminListErrors)

			r.Get("/audit", s.handleAdminListAudit)
			r.Get("/budgets", s.handleAdminListBudgets)
			r.Put("/budgets", s.handleAdminUpdateTenantBudget)

			// Cost intelligence: exact per-request accounting, versioned
			// pricing, budgets with burn rates, anomalies, forecasting,
			// savings and finance exports over the usage records.
			r.Get("/cost/overview", s.handleAdminCostOverview)
			r.Get("/cost/by", s.handleAdminCostGrouped)
			r.Get("/cost/series", s.handleAdminCostSeries)
			r.Get("/cost/requests/top", s.handleAdminCostTop)
			r.Get("/cost/requests/{requestID}", s.handleAdminCostRequest)
			r.Get("/cost/pricing", s.handleAdminPricingList)
			r.Post("/cost/pricing", s.handleAdminPricingCreate)
			r.Get("/cost/budgets", s.handleAdminBudgetsStatus)
			r.Get("/cost/anomalies", s.handleAdminCostAnomalies)
			r.Post("/cost/anomalies/{id}/resolve", s.handleAdminCostAnomalyResolve)
			r.Get("/cost/forecast", s.handleAdminCostForecast)
			r.Get("/cost/savings", s.handleAdminCostSavings)
			r.Get("/cost/export", s.handleAdminCostExport)

			// Phase 2: intelligence and control plane.
			r.Get("/requests/{requestID}/explain", s.handleAdminExplain)
			r.Get("/scores", s.handleAdminProviderScores)
			r.Get("/cache/stats", s.handleAdminCacheStats)
			r.Post("/cache/invalidate", s.handleAdminCacheInvalidate)
			r.Get("/cache/inspect", s.handleAdminCacheInspect)
			r.Get("/cache/policies", s.handleAdminListCachePolicies)
			r.Put("/cache/policies", s.handleAdminUpsertCachePolicy)
			r.Delete("/cache/policies/{id}", s.handleAdminDeleteCachePolicy)
			r.Get("/cache/invalidations", s.handleAdminListCacheInvalidations)
			r.Post("/replay", s.handleAdminCreateReplay)
			r.Get("/replay", s.handleAdminListReplay)
			r.Get("/replay/{id}", s.handleAdminGetReplay)
			r.Get("/evaluations", s.handleAdminListEvals)
			r.Get("/evaluations/{id}", s.handleAdminGetEval)
			r.Post("/providers/{id}/kill", s.handleAdminKillProvider)
			r.Get("/guardrails", s.handleAdminGuardrails)
			r.Get("/endpoints", s.handleAdminListEndpoints)
			r.Put("/endpoints", s.handleAdminUpsertEndpoint)
			r.Delete("/endpoints/{id}", s.handleAdminDeleteEndpoint)

			// Temporary public tunnels (Cloudflare quick tunnels).
			r.Post("/tunnels/create", s.handleTunnelCreate)
			r.Post("/tunnels/stop", s.handleTunnelStop)
			r.Post("/tunnels/restart", s.handleTunnelRestart)
			r.Get("/tunnels/status", s.handleTunnelStatus)
			r.Get("/tunnels/current-url", s.handleTunnelCurrentURL)
			r.Get("/tunnels/history", s.handleTunnelHistory)
		})
	}

	// Anything unmatched gets a JSON 404 rather than chi's plain-text default, so a
	// client parsing responses never has to handle two content types.
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, domain.NewError(domain.ErrCodeNotFound,
			"the requested endpoint does not exist"), nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		err := domain.NewError(domain.ErrCodeInvalidRequest, "method not allowed")
		err.Status = http.StatusMethodNotAllowed
		writeError(w, err, nil)
	})

	return r
}

// authMiddleware authenticates a request and enforces a scope.
func (s *Server) authMiddleware(scope domain.Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			rc := requestContext(ctx)

			token := auth.ExtractToken(r.Header.Get(s.auth.HeaderName()))
			p, err := s.auth.Authenticate(ctx, token)
			if err != nil {
				// Anonymous access is a development affordance only, and it is
				// rejected outright in production by configuration validation.
				if p2, aerr := s.auth.AuthenticateAnonymous(ctx); aerr == nil {
					p = p2
					err = nil
				}
			}
			if err != nil {
				s.observeAuthFailure(err)
				writeError(w, err, publicMeta(r, rc, nil))
				return
			}

			if scope != "" && !p.Has(scope) {
				s.observeAuthFailure(auth.ErrInsufficientScope(scope))
				writeError(w, auth.ErrInsufficientScope(scope), publicMeta(r, rc, nil))
				return
			}

			// Populate the request context so downstream code never re-reads the
			// credential.
			if rc != nil {
				rc.Tenant = p.Tenant
				rc.APIKey = p.APIKey
				if p.APIKey != nil {
					rc.PolicyID = p.APIKey.RoutingPolicyID
				}
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyPrincipal, p)))
		})
	}
}

// adminMiddleware enforces authorization on administrative routes.
func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rc := requestContext(ctx)

		token := auth.ExtractToken(r.Header.Get(s.auth.HeaderName()))
		p, err := s.auth.Authenticate(ctx, token)
		if err != nil {
			// Administrative endpoints never fall back to anonymous access, even in
			// development: an unauthenticated read of every tenant's usage would be
			// a serious exposure.
			s.observeAuthFailure(err)
			writeError(w, err, metaFromContext(rc, nil))
			return
		}

		if s.auth.RequireScope() && !p.Has(domain.ScopeAdminAll) {
			// Read endpoints are reachable with a read scope; writes require the
			// specific admin scope. Determining that from the method and path keeps
			// the router readable while still enforcing least privilege.
			required := adminScopeFor(r.Method, r.URL.Path)
			if !p.Has(required) {
				err := auth.ErrInsufficientScope(required)
				s.observeAuthFailure(err)
				writeError(w, err, metaFromContext(rc, nil))
				return
			}
		}

		if rc != nil {
			rc.Tenant = p.Tenant
			rc.APIKey = p.APIKey
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyPrincipal, p)))
	})
}

// adminScopeFor picks the scope an administrative route requires.
//
// Model writes are governed by the providers scope: models cannot exist
// without a provider, and splitting them into a separate grant would force
// every operator to hold two scopes for one workflow. Model reads keep the
// historical usage:read grant so existing read-only integrations do not break.
func adminScopeFor(method, path string) domain.Scope {
	switch {
	case strings.Contains(path, "/policies"):
		return domain.ScopeAdminPolicies
	case strings.Contains(path, "/providers"):
		return domain.ScopeAdminProviders
	case strings.Contains(path, "/models"):
		if method == http.MethodGet {
			return domain.ScopeReadUsage
		}
		return domain.ScopeAdminProviders
	case strings.Contains(path, "/keys"):
		return domain.ScopeAdminKeys
	case strings.Contains(path, "/tenants"):
		return domain.ScopeAdminTenants
	case strings.Contains(path, "/tunnels"):
		// Tunnel creation can expose admin surfaces to the public internet,
		// so reads stay on the read scope but every mutation requires full
		// admin. There is no narrower grant for "may expose us publicly".
		if method == http.MethodGet {
			return domain.ScopeReadUsage
		}
		return domain.ScopeAdminAll
	case strings.Contains(path, "/tools"), strings.Contains(path, "/tool-policies"):
		// Tool registry and tool policy share one scope: configuring what a tool
		// is and who may call it are the same decision from a security view.
		if method == http.MethodGet {
			return domain.ScopeReadUsage
		}
		return domain.ScopeAdminProviders
	case strings.Contains(path, "/tool-invocations"),
		strings.Contains(path, "/agent-runs"),
		strings.Contains(path, "/overrides"):
		if method == http.MethodGet {
			return domain.ScopeReadUsage
		}
		return domain.ScopeAdminProviders
	case method == http.MethodGet:
		return domain.ScopeReadUsage
	default:
		return domain.ScopeAdminAll
	}
}

// observeAuthFailure records a credential rejection.
func (s *Server) observeAuthFailure(err error) {
	if s.metrics == nil {
		return
	}
	reason := "invalid"
	switch {
	case err == auth.ErrMissingCredentials:
		reason = "missing"
	case err == auth.ErrKeyRevoked:
		reason = "revoked"
	case err == auth.ErrKeyExpired:
		reason = "expired"
	case err == auth.ErrTenantInactive:
		reason = "tenant_inactive"
	default:
		if normalized := domain.AsError(err); normalized != nil &&
			normalized.Code == domain.ErrCodePermission {
			reason = "insufficient_scope"
		}
	}
	s.metrics.ObserveAuthFailure(reason)
}

// stackTrace captures the current goroutine stack.
func stackTrace() string {
	return string(debug.Stack())
}

// itoa renders a small integer. It exists to avoid pulling strconv into the
// CIDR construction above, which handles values that are always one to three
// digits.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// uptime returns how long the process has been serving.
func (s *Server) uptime() time.Duration {
	if s.startedAt.IsZero() {
		return 0
	}
	return time.Since(s.startedAt)
}
