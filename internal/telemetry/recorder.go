package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Sink is a durable destination for telemetry records.
//
// It is an interface so the recorder does not depend on Postgres, ClickHouse or
// NATS directly. The concrete implementation lives in the wiring layer, which is
// also where the decision of "which sink receives which record" belongs.
type Sink interface {
	// WriteUsage persists a usage record to the system of record.
	WriteUsage(ctx context.Context, rec *domain.UsageRecord) error
	// WriteRequestLog persists the per-request debug entry.
	WriteRequestLog(ctx context.Context, entry *domain.RequestLog) error
	// WriteTrace persists the full request trace to the analytical store.
	WriteTrace(ctx context.Context, trace *domain.RequestTrace) error
	// WriteRequestPayload persists the captured prompt behind a request id so
	// it can be re-executed offline. Best-effort: a capture failure is logged
	// by the recorder and never fails the request it describes.
	WriteRequestPayload(ctx context.Context, payload *domain.RequestPayload) error
}

// RequestOutcome is the complete description of a finished request.
//
// It is a flat value type rather than a reference to the routing executor's result
// so the telemetry package does not import the provider or routing packages. That
// direction of dependency is what lets telemetry be tested with a fake sink and no
// network, and it keeps the record shape stable if the executor's internals move.
type RequestOutcome struct {
	RequestID      domain.RequestID
	TraceID        string
	TenantID       string
	APIKeyID       string
	Provider       string
	Model          string
	RequestedModel string
	PolicyID       string
	RequestType    domain.RequestType

	Usage             domain.TokenUsage
	Cost              domain.Cost
	EstimateCost      domain.Cost
	PricingVersionID  string
	PricingSource     string
	Breakdown         domain.CostBreakdown
	EndpointID        string
	LatencyMS         int64
	ProviderLatencyMS int64
	FirstTokenMS      int64
	Attempts          int
	FallbackUsed      bool
	CacheHit          bool
	CacheKind         string
	CacheLookupMS     float64
	CacheSimilarity   float64
	CacheBypassReason string
	CacheReuseCount   int64
	CacheLatencySavedMS int64
	Streaming         bool

	Status       int
	Outcome      domain.UsageOutcome
	ErrorCode    domain.ErrorCode
	ErrorMessage string

	ClientIP  string
	UserAgent string
	EndUser   string

	// Phase 2 intelligence context.
	Task           domain.TaskClassification
	Shaping        domain.PromptShapePlan
	PolicyDecision *domain.PolicyDecision
	ScoreNotes     []string
	CostBeforeUSD  float64
	CostAfterUSD   float64

	// Decision is the routing decision, retained on the request log.
	Decision *domain.RouteDecision
	// Trace is the full attempt history.
	Trace *domain.RequestTrace
	// Messages is the normalized prompt, retained so a pasted request id can
	// be re-executed offline. Empty when the path had no messages to keep.
	Messages []domain.ChatMessage
	// MaxOutputTokens is the effective completion allowance, reused by replay
	// to bound re-execution cost to what the original request allowed.
	MaxOutputTokens int
}

// RecordKind identifies a queued record's type.
type RecordKind string

// Record kinds.
const (
	KindUsage   RecordKind = "usage"
	KindLog     RecordKind = "log"
	KindTrace   RecordKind = "trace"
	KindPayload RecordKind = "payload"
)

// queuedRecord is one buffered telemetry record.
type queuedRecord struct {
	kind    RecordKind
	usage   *domain.UsageRecord
	log     *domain.RequestLog
	payload *domain.RequestPayload
	trace *domain.RequestTrace
}

// RecorderConfig configures the pipeline.
type RecorderConfig struct {
	// QueueSize bounds the in-memory buffer.
	QueueSize int
	// BatchSize is how many records a flush writes at once.
	BatchSize int
	// FlushInterval bounds how long a record can sit in the buffer.
	FlushInterval time.Duration
	// WriteTimeout bounds a single sink write.
	WriteTimeout time.Duration
	// VerboseLogging controls whether each write is logged at debug level.
	VerboseLogging bool
}

// Recorder buffers telemetry records and writes them asynchronously.
//
// The request path must never wait for telemetry, so the queue is bounded and a
// full queue drops records rather than blocking. That is an explicit trade: losing
// a telemetry row during an extreme burst is acceptable, whereas adding latency to
// every inference request is not. Drops are counted and exported so the condition
// is visible instead of silent.
type Recorder struct {
	sink    Sink
	metrics *Metrics
	logger  *slog.Logger
	cfg     RecorderConfig

	queue chan queuedRecord
	stop  chan struct{}
	done  chan struct{}

	closeOnce sync.Once
	closed    atomic.Bool

	dropped atomic.Uint64
	written atomic.Uint64
}

// NewRecorder builds a recorder and starts its flush loop.
func NewRecorder(sink Sink, metrics *Metrics, cfg RecorderConfig, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 8192
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 256
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}

	r := &Recorder{
		sink:    sink,
		metrics: metrics,
		logger:  logger,
		cfg:     cfg,
		queue:   make(chan queuedRecord, cfg.QueueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go r.run()
	return r
}

// RecordRequest builds and enqueues every record for a finished request.
//
// All three sinks are fed from one call so a caller cannot accidentally record
// usage without recording the request log, which would leave the dashboard's cost
// view and its request view permanently disagreeing.
func (r *Recorder) RecordRequest(ctx context.Context, out RequestOutcome) {
	if r == nil || r.sink == nil {
		return
	}

	now := domain.Now()
	usage := out.Usage.Normalize()

	r.observeMetrics(out, usage)

	r.enqueue(queuedRecord{
		kind: KindUsage,
		usage: &domain.UsageRecord{
			ID:                domain.NewID(),
			RequestID:         out.RequestID,
			TraceID:           out.TraceID,
			TenantID:          out.TenantID,
			APIKeyID:          out.APIKeyID,
			Provider:          out.Provider,
			Model:             out.Model,
			RequestedModel:    out.RequestedModel,
			PolicyID:          out.PolicyID,
			RequestType:       out.RequestType,
		Usage:             usage,
		Cost:              out.Cost,
		EstimateCost:      out.EstimateCost,
		PricingVersionID:  out.PricingVersionID,
		PricingSource:     out.PricingSource,
		Breakdown:         out.Breakdown,
		EndpointID:        out.EndpointID,
		CostBeforeUSD:     out.CostBeforeUSD,
		CostAfterUSD:      out.CostAfterUSD,
			LatencyMS:         out.LatencyMS,
			ProviderLatencyMS: out.ProviderLatencyMS,
			Attempts:          out.Attempts,
			FallbackUsed:      out.FallbackUsed,
			CacheHit:          out.CacheHit,
			Outcome:           out.Outcome,
			ErrorCode:         out.ErrorCode,
			Streaming:         out.Streaming,
			Status:            out.Status,
			ClientIP:          out.ClientIP,
			UserAgent:         out.UserAgent,
			EndUser:           out.EndUser,
			CreatedAt:         now,
		},
	})

	entry := &domain.RequestLog{
		ID:               domain.NewID(),
		RequestID:        out.RequestID.String(),
		TraceID:          out.TraceID,
		TenantID:         out.TenantID,
		APIKeyID:         out.APIKeyID,
		RequestedModel:   out.RequestedModel,
		RoutedModel:      out.Model,
		Provider:         out.Provider,
		PolicyID:         out.PolicyID,
		Status:           out.Status,
		Outcome:          out.Outcome,
		ErrorCode:        out.ErrorCode,
		ErrorMessage:     out.ErrorMessage,
		LatencyMS:        out.LatencyMS,
		Attempts:         out.Attempts,
		FallbackUsed:     out.FallbackUsed,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CostUSD:          out.Cost.USD,
		Decision:         out.Decision,
		ClientIP:         out.ClientIP,
		UserAgent:        out.UserAgent,
		CreatedAt:        now,
	}
	if out.Decision != nil {
		entry.Strategy = string(out.Decision.Strategy)
	}
	r.enqueue(queuedRecord{kind: KindLog, log: entry})

	// The prompt capture rides the same async path as the log row so it can
	// never slow the request. Requests without messages (rejections before
	// parsing, non-chat surfaces) store nothing.
	if len(out.Messages) > 0 {
		r.enqueue(queuedRecord{kind: KindPayload, payload: &domain.RequestPayload{
			RequestID:       out.RequestID.String(),
			TenantID:        out.TenantID,
			Model:           out.RequestedModel,
			Messages:        out.Messages,
			MaxOutputTokens: out.MaxOutputTokens,
			CreatedAt:       now,
		}})
	}

	if out.Trace != nil {
		r.enqueue(queuedRecord{kind: KindTrace, trace: out.Trace})
	}
}

// observeMetrics feeds the Prometheus collectors.
func (r *Recorder) observeMetrics(out RequestOutcome, usage domain.TokenUsage) {
	if r.metrics == nil {
		return
	}
	r.metrics.ObserveRequest(out.TenantID, out.Provider, out.Model,
		string(out.RequestType), out.Outcome, out.Status,
		float64(out.LatencyMS)/1000, out.Streaming)
	r.metrics.ObserveTokens(out.TenantID, out.Provider, out.Model, usage)
	r.metrics.ObserveCost(out.TenantID, out.Provider, out.Model, out.Cost)

	if out.FallbackUsed {
		r.metrics.ObserveFallback(out.RequestedModel, out.Provider, string(out.ErrorCode))
	}
	if out.CacheHit {
		r.metrics.ObserveCacheKind(out.TenantID, out.CacheKind, true)
		if out.CacheLatencySavedMS > 0 {
			r.metrics.ObserveCacheHitDetail(out.TenantID, out.CacheKind,
				float64(out.CacheLatencySavedMS)/1000, out.CacheSimilarity)
		} else if out.CacheSimilarity > 0 {
			r.metrics.ObserveCacheHitDetail(out.TenantID, out.CacheKind, 0, out.CacheSimilarity)
		}
	} else if out.CacheBypassReason == "" {
		// A bypass is not a miss: it never consulted the cache, so it
		// must not inflate the miss counter the hit-rate math uses.
		r.metrics.ObserveCacheKind(out.TenantID, "", false)
	}
	if out.CacheBypassReason != "" {
		r.metrics.ObserveCacheBypass(out.TenantID, out.CacheBypassReason)
	}
	if out.CacheLookupMS > 0 || out.CacheBypassReason != "" {
		outcome := "miss"
		if out.CacheHit {
			outcome = "hit"
		} else if out.CacheBypassReason != "" {
			outcome = "bypass"
		}
		r.metrics.ObserveCacheLookup(out.TenantID, outcome, out.CacheLookupMS/1000)
	}
	if out.FirstTokenMS > 0 {
		r.metrics.ObserveFirstToken(out.Provider, out.Model, float64(out.FirstTokenMS)/1000)
	}
	// Phase 2 signals.
	if out.Task.Task != "" {
		r.metrics.ObserveClassification(string(out.Task.Task))
	}
	if len(out.Shaping.Steps) > 0 {
		applied := []string{}
		for _, s := range out.Shaping.Steps {
			if s.Applied {
				applied = append(applied, s.Name)
			}
		}
		r.metrics.ObserveShaping(applied)
	}
}

// enqueue adds a record to the buffer, dropping when full.
func (r *Recorder) enqueue(rec queuedRecord) {
	if r.closed.Load() {
		// Records arriving during shutdown would be written to a closed sink. They
		// are counted rather than sent, and the shutdown sequence flushes whatever
		// is already queued.
		r.dropped.Add(1)
		if r.metrics != nil {
			r.metrics.AsyncDropped(string(rec.kind), "shutdown")
		}
		return
	}

	select {
	case r.queue <- rec:
		if r.metrics != nil {
			r.metrics.SetAsyncQueueDepth(len(r.queue))
		}
	default:
		// The buffer is full. Dropping is the correct behaviour: the alternative is
		// blocking the request that is trying to report its own completion.
		r.dropped.Add(1)
		if r.metrics != nil {
			r.metrics.AsyncDropped(string(rec.kind), "queue_full")
		}
		if r.cfg.VerboseLogging {
			r.logger.Warn("telemetry queue is full; dropping a record",
				"kind", rec.kind, "queue_size", r.cfg.QueueSize, "dropped", r.dropped.Load())
		}
	}
}

// run is the flush loop.
func (r *Recorder) run() {
	defer close(r.done)

	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]queuedRecord, 0, r.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.cfg.WriteTimeout)
		r.flushBatch(ctx, batch)
		cancel()
		batch = batch[:0]
	}

	for {
		select {
		case <-r.stop:
			// Drain whatever is already buffered, then flush it, so a graceful
			// shutdown does not discard a completed request's telemetry.
			for {
				select {
				case rec := <-r.queue:
					batch = append(batch, rec)
					if len(batch) >= r.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}

		case rec := <-r.queue:
			batch = append(batch, rec)
			if len(batch) >= r.cfg.BatchSize {
				flush()
			}
			if r.metrics != nil {
				r.metrics.SetAsyncQueueDepth(len(r.queue))
			}

		case <-ticker.C:
			flush()
		}
	}
}

// flushBatch writes a batch to the sink.
//
// Each record is written independently: one malformed trace must not prevent the
// usage records in the same batch from reaching the billing table.
func (r *Recorder) flushBatch(ctx context.Context, batch []queuedRecord) {
	var (
		usageCount, logCount, traceCount, payloadCount int
		failed                                        int
	)

	for _, rec := range batch {
		var err error
		switch rec.kind {
		case KindUsage:
			err = r.sink.WriteUsage(ctx, rec.usage)
			usageCount++
		case KindLog:
			err = r.sink.WriteRequestLog(ctx, rec.log)
			logCount++
		case KindTrace:
			err = r.sink.WriteTrace(ctx, rec.trace)
			traceCount++
		case KindPayload:
			err = r.sink.WriteRequestPayload(ctx, rec.payload)
			payloadCount++
		}
		if err != nil {
			failed++
			r.logger.Warn("failed to write a telemetry record", "kind", rec.kind, "error", err)
			if r.metrics != nil {
				r.metrics.AsyncDropped(string(rec.kind), "sink_error")
			}
		}
	}

	succeeded := len(batch) - failed
	r.written.Add(uint64(succeeded))
	if r.metrics != nil {
		r.metrics.AsyncFlushed(string(KindUsage), usageCount)
		r.metrics.AsyncFlushed(string(KindLog), logCount)
		r.metrics.AsyncFlushed(string(KindTrace), traceCount)
		r.metrics.AsyncFlushed(string(KindPayload), payloadCount)
		r.metrics.SetAsyncQueueDepth(len(r.queue))
	}
}

// Close stops the pipeline after flushing.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.closeOnce.Do(func() {
		r.closed.Store(true)
		close(r.stop)
	})

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		// Reporting an incomplete flush is more useful than hanging, because the
		// process is trying to exit.
		return ctx.Err()
	}
}

// Stats reports pipeline counters for the metrics and admin endpoints.
func (r *Recorder) Stats() map[string]any {
	if r == nil {
		return nil
	}
	return map[string]any{
		"queued":  len(r.queue),
		"written": r.written.Load(),
		"dropped": r.dropped.Load(),
	}
}

// ---------------------------------------------------------------------------
// routing.Observer implementation
// ---------------------------------------------------------------------------

// RecordDecision implements routing.Observer.
func (r *Recorder) RecordDecision(ctx context.Context, rc *domain.RequestContext, decision *domain.RouteDecision) {
	if r == nil || decision == nil || r.metrics == nil {
		return
	}
	r.metrics.ObserveDecision(decision.PolicyName, decision.Strategy, decision.Degraded,
		len(decision.Candidates))
}

// RecordAttempt implements routing.Observer.
//
// Attempts are already recorded in the trace, which the sink writes in one batch.
// This hook exists so a provider attempt is visible in Prometheus immediately,
// without waiting for the trace to be flushed.
func (r *Recorder) RecordAttempt(ctx context.Context, rc *domain.RequestContext, attempt domain.TraceAttempt) {
	if r == nil || r.metrics == nil {
		return
	}
	var err error
	if attempt.ErrorCode != "" {
		err = domain.NewError(attempt.ErrorCode, attempt.Error)
	}
	r.metrics.ObserveAttempt(attempt.Target.ProviderName, string(attempt.Target.Kind),
		attempt.Target.Model, float64(attempt.DurationMS)/1000, err)
	if attempt.FirstTokenMS > 0 {
		r.metrics.ObserveFirstToken(attempt.Target.ProviderName, attempt.Target.Model,
			float64(attempt.FirstTokenMS)/1000)
	}
}

// RecordOutcome implements routing.Observer.
//
// The durable write happens in RecordRequest, which the HTTP handler calls with
// the fully assembled outcome. This hook only maintains the in-flight gauge and
// the health gauges, so the request path does not build records twice.
func (r *Recorder) RecordOutcome(ctx context.Context, rc *domain.RequestContext, trace *domain.RequestTrace) {
	if r == nil || trace == nil || r.metrics == nil {
		return
	}
	if provider := traceProvider(trace); provider != "" {
		state := "unknown"
		if trace.Decision != nil && trace.Decision.Chosen.Health != "" {
			state = string(trace.Decision.Chosen.Health)
		}
		r.metrics.SetProviderHealth(provider, state)
	}
}

// traceProvider returns the provider that served a trace.
func traceProvider(trace *domain.RequestTrace) string {
	if trace == nil || trace.Decision == nil {
		return ""
	}
	return trace.Decision.Chosen.ProviderName
}

// NopSink discards every record. It is the default when telemetry persistence is
// disabled, so the recorder needs no nil checks.
type NopSink struct{}

// WriteUsage implements Sink.
func (NopSink) WriteUsage(context.Context, *domain.UsageRecord) error { return nil }

// WriteRequestLog implements Sink.
func (NopSink) WriteRequestLog(context.Context, *domain.RequestLog) error { return nil }

// WriteTrace implements Sink.
func (NopSink) WriteTrace(context.Context, *domain.RequestTrace) error { return nil }

// WriteRequestPayload implements Sink.
func (NopSink) WriteRequestPayload(context.Context, *domain.RequestPayload) error { return nil }
