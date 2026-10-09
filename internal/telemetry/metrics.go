// Package telemetry instruments Synapass with Prometheus metrics, OpenTelemetry
// traces and an asynchronous record pipeline.
//
// # Why both Prometheus and OpenTelemetry
//
// They answer different questions. Prometheus answers "is the service healthy right
// now?", with cheap cardinality-bounded counters that an alert can be written
// against. OpenTelemetry traces answer "what happened to this specific request?",
// with per-request detail that would be ruinous as a metric label. The gateway
// needs both, and exporting metrics through OTLP as well as scraping them means a
// deployment can choose either path without code changes.
//
// # Cardinality discipline
//
// Every label below is bounded by configuration: tenants, providers and models are
// finite and change rarely. Nothing per-request (request id, user id, prompt) is
// ever a label. This is the single most common way an observability stack is
// destroyed, so it is enforced by construction here.
package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Metrics holds every collector Synapass exports.
type Metrics struct {
	registry *prometheus.Registry
	enabled  bool

	// Identity labels for the build_info gauge, captured at construction so the
	// gauge can be set later without re-reading configuration.
	versionLabel     string
	instanceLabel    string
	environmentLabel string

	// RequestsTotal counts completed inference requests.
	RequestsTotal *prometheus.CounterVec
	// RequestDuration observes end-to-end latency in seconds.
	RequestDuration *prometheus.HistogramVec
	// RequestsInFlight tracks concurrency.
	RequestsInFlight *prometheus.GaugeVec
	// RequestSize and ResponseSize observe payload sizes in bytes.
	RequestSize  *prometheus.HistogramVec
	ResponseSize *prometheus.HistogramVec

	// ProviderAttemptsTotal counts provider calls, including retries.
	ProviderAttemptsTotal *prometheus.CounterVec
	// ProviderDuration observes a single provider call's latency.
	ProviderDuration *prometheus.HistogramVec
	// ProviderTimeToFirstToken observes streaming startup latency.
	ProviderTimeToFirstToken *prometheus.HistogramVec
	// TruncationsTotal counts answers that ended because a limit was reached
	// rather than the model finishing: max tokens or a deadline. A request that
	// completes normally must never appear here, so the counter is the honest
	// measure of "answers cut short" across the fleet.
	TruncationsTotal *prometheus.CounterVec
	// DashboardAuthTotal counts console login outcomes: setup, login, logout and
	// their failures.
	//
	// Labelled by outcome only. A username label would let an attacker inflate
	// metric cardinality simply by guessing names, which is how a Prometheus
	// instance is taken down.
	DashboardAuthTotal *prometheus.CounterVec
	// CompletionTokensRatio observes emitted completion tokens against the
	// ceiling that was applied, which is the early-warning signal for a
	// limit that is too tight for real workloads.
	CompletionTokensRatio *prometheus.HistogramVec
	// ProviderHealth reports the current health state as a gauge: 1 for each
	// state label, so an alert can be written against "unhealthy == 1".
	ProviderHealth *prometheus.GaugeVec
	// FallbacksTotal counts failovers.
	FallbacksTotal *prometheus.CounterVec

	// TokensTotal counts tokens by kind.
	TokensTotal *prometheus.CounterVec
	// CostUSDTotal accumulates estimated cost.
	CostUSDTotal *prometheus.CounterVec

	// CacheHitsTotal and CacheMissesTotal report response cache behaviour.
	CacheHitsTotal   *prometheus.CounterVec
	CacheMissesTotal *prometheus.CounterVec
	// Phase 5: cache explainability.
	// CacheBypassTotal counts bypasses by reason (never served, never stored).
	CacheBypassTotal *prometheus.CounterVec
	// CacheLookupDuration observes cache lookup latency in seconds.
	CacheLookupDuration *prometheus.HistogramVec
	// CacheInvalidationsTotal counts flushes by scope and reason.
	CacheInvalidationsTotal *prometheus.CounterVec
	// CacheLatencySaved observes provider latency saved by hits in seconds.
	CacheLatencySaved *prometheus.HistogramVec
	// CacheSemanticSimilarity observes the similarity of semantic hits.
	CacheSemanticSimilarity *prometheus.HistogramVec

	// PolicyDecisionsTotal counts routing decisions.
	PolicyDecisionsTotal *prometheus.CounterVec
	// RouteCandidatesTotal observes how many candidates were considered.
	RouteCandidatesTotal *prometheus.HistogramVec

	// RateLimitedTotal counts rejections by limiter kind.
	RateLimitedTotal *prometheus.CounterVec
	// BudgetBlockedTotal counts requests blocked by a spend ceiling.
	BudgetBlockedTotal *prometheus.CounterVec

	// AuthFailuresTotal counts credential rejections by reason.
	AuthFailuresTotal *prometheus.CounterVec

	// AsyncDroppedTotal counts telemetry records dropped by the async pipeline.
	AsyncDroppedTotal *prometheus.CounterVec
	// AsyncFlushedTotal counts records successfully handed to sinks.
	AsyncFlushedTotal *prometheus.CounterVec
	// AsyncQueueDepth reports the current backlog.
	AsyncQueueDepth prometheus.Gauge

	// Phase 4: tool metrics. Tool names are operator-registered and finite, so a
	// per-tool label is bounded the same way providers and models are.
	// ToolRunsTotal counts bounded multi-step runs.
	ToolRunsTotal *prometheus.CounterVec
	// ToolRunDuration observes a whole run's wall-clock time in seconds.
	ToolRunDuration *prometheus.HistogramVec
	// ToolInvocationsTotal counts individual tool calls, by outcome.
	ToolInvocationsTotal *prometheus.CounterVec
	// ToolPersistErrorsTotal counts run traces that failed to persist, because a
	// run the dashboard cannot show is an incident, not a shrug.
	ToolPersistErrorsTotal *prometheus.CounterVec

	// Tunnel metrics. Targets are operator-chosen from a tiny set, so the
	// target label is bounded the same way providers and models are.
	// TunnelUp reports whether a tunnel is currently exposing anything.
	TunnelUp *prometheus.GaugeVec
	// TunnelSessionsTotal counts sessions by target and terminal outcome.
	TunnelSessionsTotal *prometheus.CounterVec
	// TunnelRestartsTotal counts supervisor-initiated process restarts.
	TunnelRestartsTotal *prometheus.CounterVec

	// Phase 2: intelligence metrics.
	ClassifierTotal    *prometheus.CounterVec
	ShapingTotal       *prometheus.CounterVec
	GuardrailBlocks    *prometheus.CounterVec
	EvalJobsTotal      *prometheus.CounterVec
	ReplayJobsTotal    *prometheus.CounterVec
	FeedbackTotal      *prometheus.CounterVec
	ProviderScoreGauge *prometheus.GaugeVec

	// Streaming metrics. Provider labels are bounded the same way as
	// elsewhere; outcome/stage labels are closed sets.
	// StreamTTFTSeconds observes time to first downstream byte in seconds,
	// measured from request accept. This is the responsiveness signal.
	StreamTTFTSeconds *prometheus.HistogramVec
	// StreamFirstTextSeconds observes time to first visible text in seconds,
	// measured from request accept. Headers/meta frames do not count.
	StreamFirstTextSeconds *prometheus.HistogramVec
	// StreamDurationSeconds observes total stream lifetime in seconds.
	StreamDurationSeconds *prometheus.HistogramVec
	// StreamsActive reports currently open streams.
	StreamsActive prometheus.Gauge
	// StreamErrorsTotal counts streams that ended in failure, by stage.
	StreamErrorsTotal *prometheus.CounterVec
	// StreamCancelsTotal counts client-cancelled streams.
	StreamCancelsTotal *prometheus.CounterVec
	// StreamBackpressureTotal counts downstream writes slower than the
	// slow-write threshold (slow-reading clients).
	StreamBackpressureTotal *prometheus.CounterVec

	// Info exposes build and configuration identity as a labelled gauge.
	Info *prometheus.GaugeVec
}

// MetricsConfig configures metric construction.
type MetricsConfig struct {
	// Enabled turns metric collection off entirely. When false, every method on
	// Metrics is a safe no-op, so instrumentation calls need no guards.
	Enabled bool
	// Namespace prefixes every metric name, defaulting to "synapass".
	Namespace string
	// Instance and Version identify the process in the Info metric.
	Instance string
	Version  string
	// Environment distinguishes production from staging in dashboards.
	Environment string
	// GoCollector enables the standard process and Go runtime collectors.
	GoCollector bool
}

// NewMetrics builds the metric collectors.
func NewMetrics(cfg MetricsConfig) *Metrics {
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = "synapass"
	}

	registry := prometheus.NewRegistry()
	m := &Metrics{
		registry:         registry,
		enabled:          cfg.Enabled,
		versionLabel:     orUnknown(cfg.Version),
		instanceLabel:    orUnknown(cfg.Instance),
		environmentLabel: orUnknown(cfg.Environment),
	}

	if !cfg.Enabled {
		// Enabling requires returning a usable registry for the metrics endpoint so
		// an operator sees an empty-but-valid response rather than a 404.
		return m
	}

	latencyBuckets := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 13, 21, 34, 60, 120}
	sizeBuckets := []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304}
	candidateBuckets := []float64{1, 2, 3, 4, 6, 8, 12, 16, 24, 32}

	m.RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "gateway", Name: "requests_total",
		Help: "Total number of inference requests handled, by outcome.",
	}, []string{"tenant", "provider", "model", "request_type", "outcome", "status_class"})

	m.RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "gateway", Name: "request_duration_seconds",
		Help:    "End-to-end request latency in seconds.",
		Buckets: latencyBuckets,
	}, []string{"provider", "model", "streaming"})

	m.RequestsInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "gateway", Name: "requests_in_flight",
		Help: "Number of requests currently being processed.",
	}, []string{"streaming"})

	m.RequestSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "gateway", Name: "request_size_bytes",
		Help:    "Request body size in bytes.",
		Buckets: sizeBuckets,
	}, []string{"request_type"})

	m.ResponseSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "gateway", Name: "response_size_bytes",
		Help:    "Response body size in bytes.",
		Buckets: sizeBuckets,
	}, []string{"request_type", "streaming"})

	m.ProviderAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "provider", Name: "attempts_total",
		Help: "Total provider calls, including retries and fallbacks.",
	}, []string{"provider", "provider_kind", "model", "outcome", "error_code"})

	m.ProviderDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "provider", Name: "duration_seconds",
		Help:    "Latency of a single provider call in seconds.",
		Buckets: latencyBuckets,
	}, []string{"provider", "model"})

	m.ProviderTimeToFirstToken = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "provider", Name: "time_to_first_token_seconds",
		Help:    "Time to the first streamed token in seconds.",
		Buckets: latencyBuckets,
	}, []string{"provider", "model"})

	m.ProviderHealth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "provider", Name: "health",
		Help: "Current provider health; 1 for the active state, 0 for the others.",
	}, []string{"provider", "state"})

	m.FallbacksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "routing", Name: "fallbacks_total",
		Help: "Total number of requests that used a non-primary provider.",
	}, []string{"from_provider", "to_provider", "reason"})

	m.TokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "usage", Name: "tokens_total",
		Help: "Total tokens processed, by kind.",
	}, []string{"tenant", "provider", "model", "kind"})

	m.CostUSDTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "usage", Name: "cost_usd_total",
		Help: "Total estimated cost in US dollars.",
	}, []string{"tenant", "provider", "model"})

	m.CacheHitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "cache", Name: "hits_total",
		Help: "Total response cache hits.",
	}, []string{"tenant", "kind"})

	m.CacheMissesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "cache", Name: "misses_total",
		Help: "Total response cache misses.",
	}, []string{"tenant"})

	m.CacheBypassTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "cache", Name: "bypass_total",
		Help: "Total cache bypasses, by reason.",
	}, []string{"tenant", "reason"})

	m.CacheLookupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "cache", Name: "lookup_duration_seconds",
		Help:    "Cache lookup latency in seconds.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"tenant", "outcome"})

	m.CacheInvalidationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "cache", Name: "invalidations_total",
		Help: "Cache flushes, by scope and reason.",
	}, []string{"scope", "reason"})

	m.CacheLatencySaved = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "cache", Name: "latency_saved_seconds",
		Help:    "Provider latency saved by cache hits in seconds.",
		Buckets: latencyBuckets,
	}, []string{"tenant", "kind"})

	m.CacheSemanticSimilarity = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "cache", Name: "semantic_similarity",
		Help:    "Similarity score of semantic cache hits.",
		Buckets: []float64{0.5, 0.7, 0.8, 0.85, 0.9, 0.92, 0.95, 0.97, 0.99, 1},
	}, []string{"tenant"})

	m.TruncationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "provider", Name: "truncations_total",
		Help: "Answers cut short by a limit, by tenant, provider and reason (max_tokens, timeout).",
	}, []string{"tenant", "provider", "reason"})

	m.DashboardAuthTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "dashboard_auth", Name: "events_total",
		Help: "Console authentication outcomes: setup_success, login_success, login_failed, logout.",
	}, []string{"outcome"})

	m.CompletionTokensRatio = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "provider", Name: "completion_tokens_ratio",
		Help: "Emitted completion tokens divided by the applied ceiling. A pile-up at 1.0 means the ceiling is cutting answers off.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 1},
	}, []string{"tenant", "provider"})

	m.PolicyDecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "routing", Name: "decisions_total",
		Help: "Total routing decisions, by strategy and policy.",
	}, []string{"policy", "strategy", "degraded"})

	m.RouteCandidatesTotal = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "routing", Name: "candidates",
		Help:    "Number of route candidates considered per request.",
		Buckets: candidateBuckets,
	}, []string{"strategy"})

	m.RateLimitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "policy", Name: "rate_limited_total",
		Help: "Total requests rejected by a rate limit.",
	}, []string{"tenant", "limiter"})

	m.BudgetBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "policy", Name: "budget_blocked_total",
		Help: "Total requests rejected by a spend ceiling.",
	}, []string{"tenant", "period"})

	m.AuthFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "auth", Name: "failures_total",
		Help: "Total authentication failures, by reason.",
	}, []string{"reason"})

	m.AsyncDroppedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "async", Name: "dropped_total",
		Help: "Telemetry records dropped before reaching a sink.",
	}, []string{"kind", "reason"})

	m.AsyncFlushedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "async", Name: "flushed_total",
		Help: "Telemetry records successfully written to sinks.",
	}, []string{"kind"})

	m.AsyncQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "async", Name: "queue_depth",
		Help: "Number of telemetry records waiting to be written.",
	})

	m.ClassifierTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "classifier", Name: "requests_total",
		Help: "Requests classified, by task.",
	}, []string{"task"})

	m.ShapingTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "shaping", Name: "requests_total",
		Help: "Requests shaped, by applied step.",
	}, []string{"step"})

	m.GuardrailBlocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "guardrail", Name: "blocks_total",
		Help: "Requests blocked by guardrails, by kind.",
	}, []string{"kind"})

	m.EvalJobsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "eval", Name: "jobs_total",
		Help: "Evaluation jobs, by status.",
	}, []string{"status"})

	m.ReplayJobsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "replay", Name: "jobs_total",
		Help: "Replay jobs, by status.",
	}, []string{"status"})

	m.FeedbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "feedback", Name: "events_total",
		Help: "Feedback events, by tenant.",
	}, []string{"tenant"})

	m.ProviderScoreGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "scoring", Name: "provider_score",
		Help: "Current blended provider score in [0,1].",
	}, []string{"provider"})

	m.ToolRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "tools", Name: "runs_total",
		Help: "Bounded multi-step tool runs, by mode and terminal status.",
	}, []string{"tenant", "mode", "status"})

	m.ToolRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "tools", Name: "run_duration_seconds",
		Help:    "Wall-clock time of a whole tool run in seconds.",
		Buckets: latencyBuckets,
	}, []string{"mode"})

	m.ToolInvocationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "tools", Name: "invocations_total",
		Help: "Individual tool calls, by tool and outcome.",
	}, []string{"tenant", "tool", "status"})

	m.ToolPersistErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "tools", Name: "persist_errors_total",
		Help: "Tool run traces that failed to persist.",
	}, []string{"tenant"})

	m.TunnelUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "tunnel", Name: "up",
		Help: "Whether a temporary tunnel is currently exposing a target; 1 per active target.",
	}, []string{"target"})

	m.TunnelSessionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "tunnel", Name: "sessions_total",
		Help: "Tunnel sessions, by target and terminal outcome (stopped, failed).",
	}, []string{"target", "outcome"})

	m.TunnelRestartsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "tunnel", Name: "restarts_total",
		Help: "Supervisor-initiated tunnel process restarts, by target.",
	}, []string{"target"})

	m.Info = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help: "Build and configuration identity; always 1.",
	}, []string{"version", "instance", "environment", "go_version"})

	ttftBuckets := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

	m.StreamTTFTSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "stream", Name: "ttft_seconds",
		Help:    "Time from request accept to first downstream byte, in seconds.",
		Buckets: ttftBuckets,
	}, []string{"provider"})

	m.StreamFirstTextSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "stream", Name: "first_text_seconds",
		Help:    "Time from request accept to first visible text, in seconds.",
		Buckets: ttftBuckets,
	}, []string{"provider"})

	m.StreamDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "stream", Name: "duration_seconds",
		Help:    "Total stream lifetime in seconds, by terminal outcome.",
		Buckets: latencyBuckets,
	}, []string{"provider", "outcome"})

	m.StreamsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "stream", Name: "active",
		Help: "Number of currently open streams.",
	})

	m.StreamErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "stream", Name: "errors_total",
		Help: "Streams that ended in failure, by stage (upstream, downstream, timeout).",
	}, []string{"provider", "stage"})

	m.StreamCancelsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "stream", Name: "cancels_total",
		Help: "Streams cancelled by the client.",
	}, []string{"provider"})

	m.StreamBackpressureTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "stream", Name: "backpressure_total",
		Help: "Downstream writes slower than the slow-write threshold.",
	}, []string{"provider"})

	registry.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.RequestsInFlight, m.RequestSize,
		m.ResponseSize, m.ProviderAttemptsTotal, m.ProviderDuration,
		m.ProviderTimeToFirstToken, m.ProviderHealth, m.FallbacksTotal, m.TokensTotal,
		m.CostUSDTotal, m.CacheHitsTotal, m.CacheMissesTotal, m.CacheBypassTotal,
		m.CacheLookupDuration, m.CacheInvalidationsTotal, m.CacheLatencySaved,
		m.CacheSemanticSimilarity, m.PolicyDecisionsTotal,
		m.TruncationsTotal, m.CompletionTokensRatio,
		m.DashboardAuthTotal,
		m.RouteCandidatesTotal, m.RateLimitedTotal, m.BudgetBlockedTotal,
		m.AuthFailuresTotal, m.AsyncDroppedTotal, m.AsyncFlushedTotal, m.AsyncQueueDepth,
		m.ClassifierTotal, m.ShapingTotal, m.GuardrailBlocks,
		m.EvalJobsTotal, m.ReplayJobsTotal, m.FeedbackTotal, m.ProviderScoreGauge,
		m.ToolRunsTotal, m.ToolRunDuration, m.ToolInvocationsTotal, m.ToolPersistErrorsTotal,
		m.TunnelUp, m.TunnelSessionsTotal, m.TunnelRestartsTotal,
		m.StreamTTFTSeconds, m.StreamFirstTextSeconds, m.StreamDurationSeconds,
		m.StreamsActive, m.StreamErrorsTotal, m.StreamCancelsTotal,
		m.StreamBackpressureTotal,
		m.Info,
	)

	if cfg.GoCollector {
		// The Go and process collectors are what make a memory leak or a file
		// descriptor exhaustion visible before it becomes an outage.
		registry.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
	}

	return m
}

// Registry exposes the Prometheus registry.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return prometheus.NewRegistry()
	}
	return m.registry
}

// Enabled reports whether metrics are being collected.
func (m *Metrics) Enabled() bool { return m != nil && m.enabled }

// Handler returns the HTTP handler that serves the metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{
		// Errors during collection are surfaced to the scraper rather than
		// silently producing a partial response.
		ErrorHandling: promhttp.HTTPErrorOnError,
		// The default timeout of 0 means no timeout; an explicit one prevents a
		// wedged collector from holding a scrape open.
		Timeout: 15 * 1e9,
	})
}

// SetBuildInfo records the identity gauge.
func (m *Metrics) SetBuildInfo(goVersion string) {
	if !m.Enabled() || m.Info == nil {
		return
	}
	m.Info.WithLabelValues(m.versionLabel, m.instanceLabel, m.environmentLabel, goVersion).Set(1)
}

// orUnknown substitutes a placeholder for an empty label value, because an empty
// Prometheus label is legal but indistinguishable from a missing one in a query.
func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// ObserveRequest records a completed request.
func (m *Metrics) ObserveRequest(
	tenantID, provider, model, requestType string,
	outcome domain.UsageOutcome,
	status int,
	durationSeconds float64,
	streaming bool,
) {
	if !m.Enabled() {
		return
	}
	m.RequestsTotal.WithLabelValues(tenantID, provider, model, requestType,
		string(outcome), statusClass(status)).Inc()
	m.RequestDuration.WithLabelValues(provider, model, boolLabel(streaming)).Observe(durationSeconds)
}

// IncInFlight increments the in-flight gauge.
func (m *Metrics) IncInFlight(streaming bool) {
	if !m.Enabled() {
		return
	}
	m.RequestsInFlight.WithLabelValues(boolLabel(streaming)).Inc()
}

// DecInFlight decrements the in-flight gauge.
func (m *Metrics) DecInFlight(streaming bool) {
	if !m.Enabled() {
		return
	}
	m.RequestsInFlight.WithLabelValues(boolLabel(streaming)).Dec()
}

// ObserveAttempt records one provider call.
func (m *Metrics) ObserveAttempt(provider, providerKind, model string, durationSeconds float64, err error) {
	if !m.Enabled() {
		return
	}
	outcome := "success"
	code := ""
	if err != nil {
		outcome = "error"
		code = string(domain.AsError(err).Code)
	}
	m.ProviderAttemptsTotal.WithLabelValues(provider, providerKind, model, outcome, code).Inc()
	m.ProviderDuration.WithLabelValues(provider, model).Observe(durationSeconds)
}

// ObserveFirstToken records streaming startup latency.
func (m *Metrics) ObserveFirstToken(provider, model string, seconds float64) {
	if !m.Enabled() || seconds <= 0 {
		return
	}
	m.ProviderTimeToFirstToken.WithLabelValues(provider, model).Observe(seconds)
}

// ObserveTokens records token consumption.
func (m *Metrics) ObserveTokens(tenantID, provider, model string, usage domain.TokenUsage) {
	if !m.Enabled() {
		return
	}
	if usage.PromptTokens > 0 {
		m.TokensTotal.WithLabelValues(tenantID, provider, model, "prompt").Add(float64(usage.PromptTokens))
	}
	if usage.CompletionTokens > 0 {
		m.TokensTotal.WithLabelValues(tenantID, provider, model, "completion").Add(float64(usage.CompletionTokens))
	}
	if usage.CachedPromptTokens > 0 {
		m.TokensTotal.WithLabelValues(tenantID, provider, model, "cached").Add(float64(usage.CachedPromptTokens))
	}
}

// ObserveCost records estimated cost.
func (m *Metrics) ObserveCost(tenantID, provider, model string, cost domain.Cost) {
	if !m.Enabled() || cost.IsZero() {
		return
	}
	m.CostUSDTotal.WithLabelValues(tenantID, provider, model).Add(cost.USD)
}

// ObserveTruncation records an answer that ended because a limit was reached.
//
// reason is "max_tokens" when the applied ceiling stopped the model, or
// "timeout" when a deadline cancelled generation mid-answer. Both are
// truncation from the caller's point of view: the answer is incomplete either
// way, and the difference matters for deciding which limit to raise.
func (m *Metrics) ObserveTruncation(tenant, provider, reason string) {
	if !m.Enabled() || m.TruncationsTotal == nil {
		return
	}
	m.TruncationsTotal.WithLabelValues(orUnknown(tenant), orUnknown(provider), orUnknown(reason)).Inc()
}

// ObserveCompletionRatio records emitted completion tokens against the applied
// ceiling, which is how a too-tight limit becomes visible before anyone reports
// truncated answers.
func (m *Metrics) ObserveCompletionRatio(tenant, provider string, completionTokens, appliedTokens int) {
	if !m.Enabled() || m.CompletionTokensRatio == nil || appliedTokens <= 0 || completionTokens < 0 {
		return
	}
	ratio := float64(completionTokens) / float64(appliedTokens)
	if ratio > 1 {
		ratio = 1
	}
	m.CompletionTokensRatio.WithLabelValues(orUnknown(tenant), orUnknown(provider)).Observe(ratio)
}

// ObserveDashboardAuth records a console authentication outcome.
//
// outcome is a closed set of flow states (setup_success, login_success,
// login_failed, setup_rejected, logout), never a username or an error string.
func (m *Metrics) ObserveDashboardAuth(outcome string) {
	if !m.Enabled() || m.DashboardAuthTotal == nil {
		return
	}
	m.DashboardAuthTotal.WithLabelValues(orUnknown(outcome)).Inc()
}

// ObserveFallback records a failover.
func (m *Metrics) ObserveFallback(fromProvider, toProvider, reason string) {
	if !m.Enabled() {
		return
	}
	m.FallbacksTotal.WithLabelValues(fromProvider, toProvider, reason).Inc()
}

// ObserveDecision records a routing decision.
func (m *Metrics) ObserveDecision(policyName string, strategy domain.RoutingStrategy, degraded bool, candidates int) {
	if !m.Enabled() {
		return
	}
	m.PolicyDecisionsTotal.WithLabelValues(policyName, string(strategy), boolLabel(degraded)).Inc()
	m.RouteCandidatesTotal.WithLabelValues(string(strategy)).Observe(float64(candidates))
}

// SetProviderHealth publishes a provider's health state.
func (m *Metrics) SetProviderHealth(providerName, state string) {
	if !m.Enabled() {
		return
	}
	for _, candidate := range []string{"healthy", "degraded", "unhealthy", "unknown"} {
		value := 0.0
		if candidate == state {
			value = 1.0
		}
		m.ProviderHealth.WithLabelValues(providerName, candidate).Set(value)
	}
}

// ObserveAuthFailure records a credential rejection.
func (m *Metrics) ObserveAuthFailure(reason string) {
	if !m.Enabled() {
		return
	}
	m.AuthFailuresTotal.WithLabelValues(reason).Inc()
}

// ObserveRateLimited records a rate limit rejection.
func (m *Metrics) ObserveRateLimited(tenantID, limiter string) {
	if !m.Enabled() {
		return
	}
	m.RateLimitedTotal.WithLabelValues(tenantID, limiter).Inc()
}

// ObserveBudgetBlocked records a budget rejection.
func (m *Metrics) ObserveBudgetBlocked(tenantID, period string) {
	if !m.Enabled() {
		return
	}
	m.BudgetBlockedTotal.WithLabelValues(tenantID, period).Inc()
}

// AsyncDropped records a dropped telemetry record.
func (m *Metrics) AsyncDropped(kind, reason string) {
	if !m.Enabled() {
		return
	}
	m.AsyncDroppedTotal.WithLabelValues(kind, reason).Inc()
}

// AsyncFlushed records a written telemetry record.
func (m *Metrics) AsyncFlushed(kind string, count int) {
	if !m.Enabled() || count <= 0 {
		return
	}
	m.AsyncFlushedTotal.WithLabelValues(kind).Add(float64(count))
}

// SetAsyncQueueDepth publishes the current backlog.
func (m *Metrics) SetAsyncQueueDepth(depth int) {
	if !m.Enabled() {
		return
	}
	m.AsyncQueueDepth.Set(float64(depth))
}

// ObserveClassification records a classifier result.
func (m *Metrics) ObserveClassification(task string) {
	if !m.Enabled() || m.ClassifierTotal == nil {
		return
	}
	m.ClassifierTotal.WithLabelValues(task).Inc()
}

// ObserveShaping records applied shaping steps.
func (m *Metrics) ObserveShaping(steps []string) {
	if !m.Enabled() || m.ShapingTotal == nil {
		return
	}
	for _, s := range steps {
		m.ShapingTotal.WithLabelValues(s).Inc()
	}
}

// ObserveGuardrailBlock records a guardrail denial.
func (m *Metrics) ObserveGuardrailBlock(kind string) {
	if !m.Enabled() || m.GuardrailBlocks == nil {
		return
	}
	m.GuardrailBlocks.WithLabelValues(kind).Inc()
}

// ObserveCacheKind records a cache hit by kind.
func (m *Metrics) ObserveCacheKind(tenant, kind string, hit bool) {
	if !m.Enabled() {
		return
	}
	if hit {
		m.CacheHitsTotal.WithLabelValues(orUnknown(tenant), orUnknown(kind)).Inc()
	} else {
		m.CacheMissesTotal.WithLabelValues(orUnknown(tenant)).Inc()
	}
}

// ObserveCacheBypass records a bypass with its reason.
func (m *Metrics) ObserveCacheBypass(tenant, reason string) {
	if !m.Enabled() || m.CacheBypassTotal == nil {
		return
	}
	m.CacheBypassTotal.WithLabelValues(orUnknown(tenant), orUnknown(reason)).Inc()
}

// ObserveCacheLookup records lookup latency by outcome (hit/miss/bypass).
func (m *Metrics) ObserveCacheLookup(tenant, outcome string, seconds float64) {
	if !m.Enabled() || m.CacheLookupDuration == nil {
		return
	}
	m.CacheLookupDuration.WithLabelValues(orUnknown(tenant), orUnknown(outcome)).Observe(seconds)
}

// ObserveCacheInvalidation records a flush.
func (m *Metrics) ObserveCacheInvalidation(scope, reason string) {
	if !m.Enabled() || m.CacheInvalidationsTotal == nil {
		return
	}
	m.CacheInvalidationsTotal.WithLabelValues(orUnknown(scope), orUnknown(reason)).Inc()
}

// ObserveCacheHitDetail records latency saved and semantic similarity.
func (m *Metrics) ObserveCacheHitDetail(tenant, kind string, latencySavedSeconds, similarity float64) {
	if !m.Enabled() {
		return
	}
	if latencySavedSeconds > 0 && m.CacheLatencySaved != nil {
		m.CacheLatencySaved.WithLabelValues(orUnknown(tenant), orUnknown(kind)).Observe(latencySavedSeconds)
	}
	if kind == "semantic" && similarity > 0 && m.CacheSemanticSimilarity != nil {
		m.CacheSemanticSimilarity.WithLabelValues(orUnknown(tenant)).Observe(similarity)
	}
}

// SetProviderScore publishes a provider score.
func (m *Metrics) SetProviderScore(provider string, score float64) {
	if !m.Enabled() || m.ProviderScoreGauge == nil {
		return
	}
	m.ProviderScoreGauge.WithLabelValues(provider).Set(score)
}

// StreamActiveInc marks one more open stream.
func (m *Metrics) StreamActiveInc() {
	if !m.Enabled() {
		return
	}
	m.StreamsActive.Inc()
}

// StreamActiveDec marks one fewer open stream.
func (m *Metrics) StreamActiveDec() {
	if !m.Enabled() {
		return
	}
	m.StreamsActive.Dec()
}

// ObserveStreamTTFT records time to first downstream byte in seconds.
func (m *Metrics) ObserveStreamTTFT(provider string, seconds float64) {
	if !m.Enabled() || m.StreamTTFTSeconds == nil || seconds < 0 {
		return
	}
	m.StreamTTFTSeconds.WithLabelValues(orUnknown(provider)).Observe(seconds)
}

// ObserveStreamFirstText records time to first visible text in seconds.
func (m *Metrics) ObserveStreamFirstText(provider string, seconds float64) {
	if !m.Enabled() || m.StreamFirstTextSeconds == nil || seconds < 0 {
		return
	}
	m.StreamFirstTextSeconds.WithLabelValues(orUnknown(provider)).Observe(seconds)
}

// ObserveStreamEnd records total stream lifetime by terminal outcome.
// outcome is one of completed, error, cancelled, truncated.
func (m *Metrics) ObserveStreamEnd(provider, outcome string, seconds float64) {
	if !m.Enabled() || m.StreamDurationSeconds == nil || seconds < 0 {
		return
	}
	m.StreamDurationSeconds.WithLabelValues(orUnknown(provider), orUnknown(outcome)).Observe(seconds)
}

// ObserveStreamError records a failed stream by stage: upstream (provider
// fault or timeout) or downstream (client write failure).
func (m *Metrics) ObserveStreamError(provider, stage string) {
	if !m.Enabled() || m.StreamErrorsTotal == nil {
		return
	}
	if stage != "upstream" && stage != "downstream" && stage != "timeout" {
		stage = "upstream"
	}
	m.StreamErrorsTotal.WithLabelValues(orUnknown(provider), stage).Inc()
}

// ObserveStreamCancel records a client-cancelled stream.
func (m *Metrics) ObserveStreamCancel(provider string) {
	if !m.Enabled() || m.StreamCancelsTotal == nil {
		return
	}
	m.StreamCancelsTotal.WithLabelValues(orUnknown(provider)).Inc()
}

// ObserveStreamBackpressure records one slow downstream write.
func (m *Metrics) ObserveStreamBackpressure(provider string) {
	if !m.Enabled() || m.StreamBackpressureTotal == nil {
		return
	}
	m.StreamBackpressureTotal.WithLabelValues(orUnknown(provider)).Inc()
}

// ObserveToolRun records a completed bounded tool run and every call it made.
//
// The per-invocation statuses come from the run's own record, so the metrics
// and the stored history cannot disagree about what happened.
func (m *Metrics) ObserveToolRun(
	tenant, mode string,
	status domain.AgentRunStatus,
	latencySeconds float64,
	invocations []domain.ToolInvocation,
	persistErr error,
) {
	if !m.Enabled() {
		return
	}
	m.ToolRunsTotal.WithLabelValues(orUnknown(tenant), mode, string(status)).Inc()
	m.ToolRunDuration.WithLabelValues(mode).Observe(latencySeconds)
	for _, invocation := range invocations {
		m.ToolInvocationsTotal.WithLabelValues(
			orUnknown(tenant), orUnknown(invocation.ToolName), string(invocation.Status)).Inc()
	}
	if persistErr != nil {
		m.ToolPersistErrorsTotal.WithLabelValues(orUnknown(tenant)).Inc()
	}
}

// SetTunnelUp publishes whether a target is currently exposed. Only the
// active target reads 1; every other previously-seen target must be reset to
// 0 by the caller when the tunnel stops, otherwise the gauge lies.
func (m *Metrics) SetTunnelUp(target string, up bool) {
	if !m.Enabled() || m.TunnelUp == nil {
		return
	}
	value := 0.0
	if up {
		value = 1.0
	}
	m.TunnelUp.WithLabelValues(orUnknown(target)).Set(value)
}

// ObserveTunnelSession counts a terminal tunnel session by outcome.
func (m *Metrics) ObserveTunnelSession(target, outcome string) {
	if !m.Enabled() || m.TunnelSessionsTotal == nil {
		return
	}
	m.TunnelSessionsTotal.WithLabelValues(orUnknown(target), orUnknown(outcome)).Inc()
}

// ObserveTunnelRestart counts a supervisor-initiated process restart.
func (m *Metrics) ObserveTunnelRestart(target string) {
	if !m.Enabled() || m.TunnelRestartsTotal == nil {
		return
	}
	m.TunnelRestartsTotal.WithLabelValues(orUnknown(target)).Inc()
}

// statusClass reduces a status code to a class label, which keeps cardinality
// bounded while still distinguishing client from server errors.
func statusClass(status int) string {
	switch {
	case status == 0:
		return "none"
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// boolLabel renders a boolean as a label value.
func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
