package storage

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/cost"
	"github.com/shadowsafin/synapass/internal/domain"
)

// Column orders are declared once and paired with their INSERT statement, because
// ClickHouse's batch API is positional: a mismatch between the column list and the
// Append arguments would silently write values into the wrong columns.
const (
	insertRequestTrace = `INSERT INTO synapass.request_traces (
		trace_id, request_id, tenant_id, api_key_id, policy_id, strategy, requested_model,
		routed_provider, routed_model, outcome, error_code, attempts, fallback_used, degraded,
		client_streamed, estimated_cost_usd, total_ms, first_token_ms, prompt_tokens,
		completion_tokens, decision_json, started_at)`

	insertTraceAttempt = `INSERT INTO synapass.trace_attempts (
		trace_id, request_id, tenant_id, attempt_number, provider_id, provider_name,
		provider_kind, model, status, error_code, error_message, duration_ms,
		started_offset_ms, backoff_ms, first_token_ms, retry_triggered, fallback_triggered,
		prompt_tokens, completion_tokens, cost_usd, started_at)`

	insertUsageEvent = `INSERT INTO synapass.usage_events (
		request_id, tenant_id, api_key_id, provider, model, requested_model, outcome,
		error_code, fallback_used, cache_hit, estimated, streaming, prompt_tokens,
		completion_tokens, total_tokens, cost_usd, estimate_cost_usd, pricing_source,
		endpoint_id, latency_ms, created_at)`

	insertProviderSnapshot = `INSERT INTO synapass.provider_status_snapshots (
		provider_id, provider_name, provider_kind, state, window_ms, request_count,
		success_count, error_count, success_rate, error_rate, latency_p50_ms,
		latency_p95_ms, latency_p99_ms, total_tokens, total_cost_usd, message, captured_at)`

	insertRouteDecision = `INSERT INTO synapass.route_decisions (
		request_id, tenant_id, policy_id, policy_name, strategy, requested_model,
		chosen_provider, chosen_model, degraded, candidate_count, eligible_count,
		skipped_count, estimated_cost_usd, reason, created_at)`
)

// ClickHouse is the analytical telemetry store.
//
// Writes are batched and asynchronous. That is not an optimisation but a
// requirement: ClickHouse creates one part per INSERT, and inserting one row at a
// time produces thousands of tiny parts that the merge background job cannot keep
// up with, which degrades the whole cluster. Batching solves it, and doing the
// batching in-process avoids a per-request network hop on the request path.
type ClickHouse struct {
	conn   driver.Conn
	cfg    config.ClickHouseConfig
	logger *slog.Logger

	mu      sync.Mutex
	batches map[string]*pendingBatch

	// pricer resolves per-attempt cost through versioned sheets. Nil in tests
	// and lightweight deployments; per-attempt cost then falls back to the
	// attempt target's registry rates (see attemptCost).
	pricer AttemptPricer

	stop   chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool

	// Counters for the metrics endpoint, so a dropped telemetry write is visible
	// rather than silent.
	written atomic.Uint64
	dropped atomic.Uint64
	failed  atomic.Uint64
}

// pendingBatch is an open batch for one table.
type pendingBatch struct {
	batch   driver.Batch
	count   int
	started time.Time
}

// NewClickHouse connects to ClickHouse and starts the flush loop.
func NewClickHouse(ctx context.Context, cfg config.ClickHouseConfig, logger *slog.Logger) (*ClickHouse, error) {
	if logger == nil {
		logger = slog.Default()
	}

	options := &clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: 10 * time.Second,
		// Compression trades a little CPU for a large reduction in bytes over the
		// wire, which matters when shipping telemetry to a managed service.
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		Settings: clickhouse.Settings{
			// Async inserts off: the client batches explicitly, and letting the
			// server also batch would make flush timing unobservable.
			"async_insert": 0,
		},
	}
	if cfg.TLS {
		options.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse connection: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("connect to clickhouse at %s: %w", cfg.Addr, err)
	}

	ch := &ClickHouse{
		conn:    conn,
		cfg:     cfg,
		logger:  logger,
		batches: map[string]*pendingBatch{},
		stop:    make(chan struct{}),
	}
	ch.startFlushLoop()

	logger.Info("connected to clickhouse",
		"addr", cfg.Addr, "database", cfg.Database,
		"batch_size", cfg.BatchSize, "flush_interval", cfg.FlushInterval.Std())

	return ch, nil
}

// AttemptPricer prices one trace attempt's usage in USD.
//
// It mirrors the replay Runner's Pricer (internal/replay/runner.go): the
// gateway wires the same versioned-sheet resolution live traffic uses
// (tenant > model > provider > global, registry fallback), tests wire a stub.
// The full attempt target is passed rather than bare names so the registry
// fallback can price from the rates copied onto the target at decision time.
type AttemptPricer func(ctx context.Context, tenantID string, target domain.RouteTarget, usage domain.TokenUsage) float64

// SetPricer wires versioned-sheet pricing for per-attempt cost. It is called
// once at construction before traffic flows; re-wiring later is serialized
// with the write lock so a concurrent WriteTrace cannot observe a half-write.
func (c *ClickHouse) SetPricer(p AttemptPricer) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pricer = p
}

// Ping verifies connectivity.
func (c *ClickHouse) Ping(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return fmt.Errorf("clickhouse is not initialized")
	}
	return c.conn.Ping(ctx)
}

// Close flushes pending batches and closes the connection.
func (c *ClickHouse) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.closed.Store(true)
	close(c.stop)
	c.wg.Wait()

	if err := c.Flush(context.Background()); err != nil {
		c.logger.Warn("final clickhouse flush failed", "error", err)
	}
	return c.conn.Close()
}

// Stats reports writer counters.
func (c *ClickHouse) Stats() map[string]uint64 {
	if c == nil {
		return nil
	}
	return map[string]uint64{
		"rows_written": c.written.Load(),
		"rows_dropped": c.dropped.Load(),
		"flush_failed": c.failed.Load(),
	}
}

// startFlushLoop flushes batches on the configured interval.
func (c *ClickHouse) startFlushLoop() {
	interval := c.cfg.FlushInterval.Std()
	if interval <= 0 {
		interval = 2 * time.Second
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := c.Flush(ctx); err != nil {
					c.logger.Warn("clickhouse flush failed", "error", err)
					c.failed.Add(1)
				}
				cancel()
			}
		}
	}()
}

// append adds a row to a table's batch, flushing when the batch is full.
//
// Telemetry must never block or fail a request. A batch creation error is logged
// and counted, and the row is dropped: losing a trace row is strictly better than
// turning a ClickHouse hiccup into a user-visible failure.
func (c *ClickHouse) append(ctx context.Context, table, statement string, values ...any) {
	if c == nil || c.conn == nil || c.closed.Load() {
		if c != nil {
			c.dropped.Add(1)
		}
		return
	}

	batchSize := c.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}

	c.mu.Lock()
	pending, ok := c.batches[table]
	if !ok || pending.batch == nil {
		batch, err := c.conn.PrepareBatch(ctx, statement)
		if err != nil {
			c.mu.Unlock()
			c.dropped.Add(1)
			c.logger.Warn("failed to prepare a clickhouse batch", "table", table, "error", err)
			return
		}
		pending = &pendingBatch{batch: batch, started: time.Now()}
		c.batches[table] = pending
	}

	if err := pending.batch.Append(values...); err != nil {
		// A failed append leaves the batch's internal state questionable, so it is
		// discarded rather than retried.
		_ = pending.batch.Abort()
		delete(c.batches, table)
		c.mu.Unlock()
		c.dropped.Add(1)
		c.logger.Warn("failed to append to a clickhouse batch", "table", table, "error", err)
		return
	}
	pending.count++
	shouldFlush := pending.count >= batchSize
	c.mu.Unlock()

	if shouldFlush {
		if err := c.flushTable(ctx, table); err != nil {
			c.logger.Warn("clickhouse batch send failed", "table", table, "error", err)
			c.failed.Add(1)
		}
	}
}

// flushTable sends one table's batch.
func (c *ClickHouse) flushTable(ctx context.Context, table string) error {
	c.mu.Lock()
	pending, ok := c.batches[table]
	if !ok || pending.batch == nil {
		c.mu.Unlock()
		return nil
	}
	delete(c.batches, table)
	c.mu.Unlock()

	if err := pending.batch.Send(); err != nil {
		_ = pending.batch.Abort()
		c.dropped.Add(uint64(pending.count))
		return err
	}
	c.written.Add(uint64(pending.count))
	return nil
}

// Flush sends every open batch.
func (c *ClickHouse) Flush(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return nil
	}

	c.mu.Lock()
	tables := make([]string, 0, len(c.batches))
	for table := range c.batches {
		tables = append(tables, table)
	}
	c.mu.Unlock()

	var firstErr error
	for _, table := range tables {
		if err := c.flushTable(ctx, table); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WriteTrace records a complete request trace and its attempts.
func (c *ClickHouse) WriteTrace(ctx context.Context, trace *domain.RequestTrace) {
	if c == nil || trace == nil {
		return
	}

	var (
		decisionJSON string
		policyID     string
		strategy     string
		routedProv   string
		routedModel  string
		clientStream uint8
		degraded     uint8
	)
	if trace.Decision != nil {
		if encoded, err := json.Marshal(trace.Decision); err == nil {
			decisionJSON = string(encoded)
		}
		policyID = trace.Decision.PolicyID
		strategy = string(trace.Decision.Strategy)
		routedProv = trace.Decision.Chosen.ProviderName
		routedModel = trace.Decision.Chosen.Model
		if trace.Decision.Degraded {
			degraded = 1
		}
	}
	if trace.ClientStreamed {
		clientStream = 1
	}

	var promptTokens, completionTokens uint32
	for _, attempt := range trace.Attempts {
		promptTokens += uint32(clampInt(attempt.Usage.PromptTokens))
		completionTokens += uint32(clampInt(attempt.Usage.CompletionTokens))
	}

	c.append(ctx, "request_traces", insertRequestTrace,
		trace.TraceID,
		trace.RequestID.String(),
		trace.TenantID,
		trace.APIKeyID,
		policyID,
		strategy,
		requestedModelOf(trace),
		routedProv,
		routedModel,
		string(trace.Outcome),
		string(trace.ErrorCode),
		uint8(clampInt(len(trace.Attempts))),
		boolToUint8(trace.Outcome == domain.OutcomeFallback),
		degraded,
		clientStream,
		estimatedCostOf(trace),
		uint32(clampInt64(trace.TotalMS)),
		uint32(clampInt64(firstTokenOf(trace))),
		promptTokens,
		completionTokens,
		decisionJSON,
		trace.StartedAt,
	)

	for _, attempt := range trace.Attempts {
		c.append(ctx, "trace_attempts", insertTraceAttempt,
			trace.TraceID,
			trace.RequestID.String(),
			trace.TenantID,
			uint8(clampInt(attempt.Number)),
			attempt.Target.ProviderID,
			attempt.Target.ProviderName,
			string(attempt.Target.Kind),
			attempt.Target.Model,
			uint16(clampInt(attempt.Status)),
			string(attempt.ErrorCode),
			truncate(attempt.Error, 2000),
			uint32(clampInt64(attempt.DurationMS)),
			attempt.StartedOffsetMS,
			uint32(clampInt64(attempt.BackoffMS)),
			uint32(clampInt64(attempt.FirstTokenMS)),
			boolToUint8(attempt.RetryTriggered),
			boolToUint8(attempt.FallbackTriggered),
			uint32(clampInt(attempt.Usage.PromptTokens)),
			uint32(clampInt(attempt.Usage.CompletionTokens)),
			c.attemptCost(ctx, trace.TenantID, attempt),
			trace.StartedAt.Add(time.Duration(attempt.StartedOffsetMS)*time.Millisecond),
		)
	}
}

// WriteUsage records a usage event.
func (c *ClickHouse) WriteUsage(ctx context.Context, rec *domain.UsageRecord) {
	if c == nil || rec == nil {
		return
	}
	c.append(ctx, "usage_events", insertUsageEvent,
		rec.RequestID.String(),
		rec.TenantID,
		rec.APIKeyID,
		rec.Provider,
		rec.Model,
		rec.RequestedModel,
		string(rec.Outcome),
		string(rec.ErrorCode),
		boolToUint8(rec.FallbackUsed),
		boolToUint8(rec.CacheHit),
		boolToUint8(rec.Usage.Estimated),
		boolToUint8(rec.Streaming),
		uint32(clampInt(rec.Usage.PromptTokens)),
		uint32(clampInt(rec.Usage.CompletionTokens)),
		uint32(clampInt(rec.Usage.TotalTokens)),
		rec.Cost.USD,
		rec.EstimateCost.USD,
		rec.PricingSource,
		rec.EndpointID,
		uint32(clampInt64(rec.LatencyMS)),
		rec.CreatedAt,
	)
}

// WriteRouteDecision records a routing decision for analytics.
func (c *ClickHouse) WriteRouteDecision(ctx context.Context, rc *domain.RequestContext, decision *domain.RouteDecision) {
	if c == nil || decision == nil {
		return
	}
	tenantID := ""
	if rc != nil {
		tenantID = rc.TenantID()
	}

	eligible := 0
	for _, candidate := range decision.Candidates {
		if candidate.Eligible {
			eligible++
		}
	}

	c.append(ctx, "route_decisions", insertRouteDecision,
		decision.RequestID.String(),
		tenantID,
		decision.PolicyID,
		decision.PolicyName,
		string(decision.Strategy),
		requestedModelFromDecision(decision),
		decision.Chosen.ProviderName,
		decision.Chosen.Model,
		boolToUint8(decision.Degraded),
		uint8(clampInt(len(decision.Candidates))),
		uint8(clampInt(eligible)),
		uint8(clampInt(len(decision.Chain.Skipped))),
		decision.EstimatedCost.USD,
		truncate(decision.Reason, 2000),
		decision.DecidedAt,
	)
}

// WriteProviderSnapshot records a provider health rollup.
func (c *ClickHouse) WriteProviderSnapshot(ctx context.Context, snap *domain.ProviderStatusSnapshot) {
	if c == nil || snap == nil {
		return
	}
	c.append(ctx, "provider_status_snapshots", insertProviderSnapshot,
		snap.ProviderID,
		snap.ProviderName,
		"",
		string(snap.State),
		uint64(snap.WindowMS),
		uint32(clampInt(snap.RequestCount)),
		uint32(clampInt(snap.SuccessCount)),
		uint32(clampInt(snap.ErrorCount)),
		snap.SuccessRate,
		snap.ErrorRate,
		uint32(clampInt64(snap.LatencyP50MS)),
		uint32(clampInt64(snap.LatencyP95MS)),
		uint32(clampInt64(snap.LatencyP99MS)),
		uint64(snap.TotalTokens),
		snap.TotalCostUSD,
		snap.Message,
		snap.CapturedAt,
	)
}

// Migrate applies the embedded ClickHouse schema.
//
// ClickHouse DDL is not transactional, so each statement is applied independently
// and every statement is written to be idempotent (CREATE TABLE IF NOT EXISTS).
// That is what makes a re-run after a partial failure safe.
func (c *ClickHouse) Migrate(ctx context.Context) (*MigrationResult, error) {
	start := time.Now()

	migrations, err := ResolveMigrations(c.cfg.DSN, "clickhouse", ClickHouseMigrations)
	if err != nil {
		return nil, err
	}
	if err := requireMigrations(migrations, "clickhouse"); err != nil {
		return nil, err
	}

	result := &MigrationResult{}
	for _, migration := range migrations {
		for _, statement := range SplitStatements(migration.SQL) {
			if err := c.conn.Exec(ctx, statement); err != nil {
				return nil, &MigrationError{Version: migration.Version, Statement: statement, Err: err}
			}
		}
		result.Applied = append(result.Applied, migration.Version)
		c.logger.Info("applied clickhouse migration", "version", migration.Version)
	}
	result.Duration = time.Since(start)
	return result, nil
}

// ---------------------------------------------------------------------------
// Small conversion helpers
// ---------------------------------------------------------------------------

// clampInt bounds a signed value to a non-negative int.
func clampInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// clampInt64 bounds a signed value to a non-negative int64.
func clampInt64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// boolToUint8 renders a bool as ClickHouse's UInt8 boolean.
func boolToUint8(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

// requestedModelOf extracts the client-requested model from a trace decision.
func requestedModelOf(trace *domain.RequestTrace) string {
	if trace.Decision == nil {
		return ""
	}
	if trace.Decision.Chosen.Alias != "" {
		return trace.Decision.Chosen.Alias
	}
	return trace.Decision.Chosen.Model
}

// requestedModelFromDecision extracts the requested model from a decision.
func requestedModelFromDecision(decision *domain.RouteDecision) string {
	if decision.Chosen.Alias != "" {
		return decision.Chosen.Alias
	}
	return decision.Chosen.Model
}

// estimatedCostOf sums the decision's estimated cost.
func estimatedCostOf(trace *domain.RequestTrace) float64 {
	if trace.Decision == nil {
		return 0
	}
	return trace.Decision.EstimatedCost.USD
}

// firstTokenOf returns the first token latency from the trace attempts.
func firstTokenOf(trace *domain.RequestTrace) int64 {
	for _, attempt := range trace.Attempts {
		if attempt.FirstTokenMS > 0 {
			return attempt.FirstTokenMS
		}
	}
	return 0
}

// attemptCost prices one trace attempt in USD for the trace_attempts.cost_usd
// column.
//
// When a pricer is wired it wins: the pricer resolves the same versioned
// sheet live traffic bills against (tenant > model > provider > global,
// registry fallback), so retries and fallbacks accumulate what each provider
// metered. Without a pricer the attempt is priced from the registry rates
// copied onto its target at decision time — real USD in the common case, and
// exactly 0 for genuinely unpriced targets (local runtimes, unknown models),
// which is the only case that records 0 here.
//
// Either way the figure is informational: the request-level usage_events row
// remains the billing authority. It zeroes unbilled work, carries the
// base-fee/supersession policy, and is the number the cost views aggregate.
func (c *ClickHouse) attemptCost(ctx context.Context, tenantID string, attempt domain.TraceAttempt) float64 {
	var pricer AttemptPricer
	if c != nil {
		c.mu.Lock()
		pricer = c.pricer
		c.mu.Unlock()
	}
	if pricer != nil {
		return pricer(ctx, tenantID, attempt.Target, attempt.Usage.Normalize())
	}
	return attemptRegistryCostUSD(attempt)
}

// attemptRegistryCostUSD is the pricer-less fallback: one cost-engine step at
// the attempt target's own registry rates, base fee included to match the
// per-step treatment in the request-level breakdown. It is pure computation
// over domain types — no I/O — so unit tests pin it without a live
// ClickHouse.
func attemptRegistryCostUSD(attempt domain.TraceAttempt) float64 {
	usage := attempt.Usage.Normalize()
	price := cost.PriceFromTarget(attempt.Target)
	bd := cost.Compute(usage, price, []cost.AttemptUsage{{
		Label:       "attempt",
		Usage:       usage,
		BillBaseFee: true,
	}})
	return bd.TotalUSD
}
