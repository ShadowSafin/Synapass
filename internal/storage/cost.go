package storage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Pricing registry
// ---------------------------------------------------------------------------

// PricingRepository stores immutable dated price sheets.
type PricingRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewPricingRepository builds the repository.
func NewPricingRepository(pool *pgxpool.Pool, logger *slog.Logger) *PricingRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &PricingRepository{pool: pool, logger: logger}
}

const pricingColumns = `id, scope, COALESCE(scope_id::text,''), currency,
	input_cost_per_million, output_cost_per_million, cached_input_cost_per_million,
	base_fee_usd, effective_from, effective_to, created_by, created_at`

// Create stores a new pricing version. Versions are never updated; a price
// change is a new row.
func (r *PricingRepository) Create(ctx context.Context, v *domain.PricingVersion) error {
	if v.ID == "" {
		v.ID = domain.NewID()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = domain.Now()
	}
	if v.Currency == "" {
		v.Currency = "USD"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO pricing_versions (
			id, scope, scope_id, currency, input_cost_per_million,
			output_cost_per_million, cached_input_cost_per_million, base_fee_usd,
			effective_from, effective_to, created_by, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		v.ID, string(v.Scope), nullableUUID(v.ScopeID), v.Currency,
		v.InputCostPerMillion, v.OutputCostPerMillion, v.CachedInputCostPerMillion,
		v.BaseFeeUSD, v.EffectiveFrom, nullableTime(v.EffectiveTo), v.CreatedBy, v.CreatedAt)
	if err != nil {
		return wrapDBError("create pricing version", err)
	}
	return nil
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

// List returns versions, optionally narrowed to one scope, newest first.
func (r *PricingRepository) List(ctx context.Context, scope domain.PricingScope, limit int) ([]domain.PricingVersion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := fmt.Sprintf(`SELECT %s FROM pricing_versions`, pricingColumns)
	var args []any
	if scope != "" {
		query += ` WHERE scope = $1`
		args = append(args, string(scope))
	}
	query += fmt.Sprintf(` ORDER BY effective_from DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("list pricing versions", err)
	}
	defer rows.Close()
	return collectPricing(rows)
}

// EffectiveFor returns every version that could price a request for the given
// tenant/model/provider: the tenant sheet, the model sheet, the provider
// sheet and the global default. Resolution precedence is applied by the cost
// package, not by SQL, so the ordering rule lives in exactly one place.
func (r *PricingRepository) EffectiveFor(ctx context.Context, tenantID, modelID, providerID string) ([]domain.PricingVersion, error) {
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT %s FROM pricing_versions
		 WHERE (scope = 'global')
		    OR (scope = 'tenant' AND scope_id = $1)
		    OR (scope = 'model' AND scope_id = $2)
		    OR (scope = 'provider' AND scope_id = $3)
		 ORDER BY effective_from DESC`, pricingColumns),
		nullableUUID(tenantID), nullableUUID(modelID), nullableUUID(providerID))
	if err != nil {
		return nil, wrapDBError("load effective pricing", err)
	}
	defer rows.Close()
	return collectPricing(rows)
}

func collectPricing(rows pgx.Rows) ([]domain.PricingVersion, error) {
	var out []domain.PricingVersion
	for rows.Next() {
		var (
			v     domain.PricingVersion
			scope string
			effTo *time.Time
		)
		if err := rows.Scan(&v.ID, &scope, &v.ScopeID, &v.Currency,
			&v.InputCostPerMillion, &v.OutputCostPerMillion, &v.CachedInputCostPerMillion,
			&v.BaseFeeUSD, &v.EffectiveFrom, &effTo, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Scope = domain.PricingScope(scope)
		v.EffectiveTo = effTo
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Budget alerts
// ---------------------------------------------------------------------------

// BudgetAlertRepository persists exactly-once threshold crossings.
type BudgetAlertRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBudgetAlertRepository builds the repository.
func NewBudgetAlertRepository(pool *pgxpool.Pool, logger *slog.Logger) *BudgetAlertRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BudgetAlertRepository{pool: pool, logger: logger}
}

// Fire records a crossing, silently keeping the existing row when the same
// budget/threshold/period already fired: the unique constraint is the
// exactly-once mechanism, so concurrent recorders cannot double-page.
func (r *BudgetAlertRepository) Fire(ctx context.Context, a *domain.BudgetAlert) error {
	if a.ID == "" {
		a.ID = domain.NewID()
	}
	if a.FiredAt.IsZero() {
		a.FiredAt = domain.Now()
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO budget_alerts (
			id, budget_id, tenant_id, threshold_usd, spent_usd, limit_usd,
			period, period_start, fired_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (budget_id, threshold_usd, period_start) DO NOTHING`,
		a.ID, nullableUUID(a.BudgetID), nullableUUID(a.TenantID), a.ThresholdUSD,
		a.SpentUSD, a.LimitUSD, string(a.Period), a.PeriodStart, a.FiredAt)
	if err != nil {
		return wrapDBError("fire budget alert", err)
	}
	return nil
}

// FiredThresholds returns the thresholds already recorded for a budget in the
// period starting at periodStart, so the evaluator only fires new crossings.
func (r *BudgetAlertRepository) FiredThresholds(ctx context.Context, budgetID string, periodStart time.Time) (map[float64]bool, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT threshold_usd FROM budget_alerts
		 WHERE budget_id = $1 AND period_start = $2`,
		nullableUUID(budgetID), periodStart)
	if err != nil {
		return nil, wrapDBError("load fired thresholds", err)
	}
	defer rows.Close()
	fired := map[float64]bool{}
	for rows.Next() {
		var t float64
		if err := rows.Scan(&t); err != nil {
			return nil, wrapDBError("scan fired threshold", err)
		}
		fired[t] = true
	}
	return fired, rows.Err()
}

// ListRecent returns recent alerts for a tenant (or all tenants when empty).
func (r *BudgetAlertRepository) ListRecent(ctx context.Context, tenantID string, limit int) ([]domain.BudgetAlert, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, COALESCE(budget_id::text,''), COALESCE(tenant_id::text,''),
		threshold_usd, spent_usd, limit_usd, period, period_start, fired_at
		FROM budget_alerts`
	var args []any
	if tenantID != "" {
		query += ` WHERE tenant_id = $1`
		args = append(args, nullableUUID(tenantID))
	}
	query += fmt.Sprintf(` ORDER BY fired_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("list budget alerts", err)
	}
	defer rows.Close()
	var out []domain.BudgetAlert
	for rows.Next() {
		var (
			a      domain.BudgetAlert
			period string
		)
		if err := rows.Scan(&a.ID, &a.BudgetID, &a.TenantID, &a.ThresholdUSD,
			&a.SpentUSD, &a.LimitUSD, &period, &a.PeriodStart, &a.FiredAt); err != nil {
			return nil, wrapDBError("scan budget alert", err)
		}
		a.Period = domain.BudgetPeriod(period)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Cost anomalies
// ---------------------------------------------------------------------------

// AnomalyRepository persists flagged spend deviations.
type AnomalyRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewAnomalyRepository builds the repository.
func NewAnomalyRepository(pool *pgxpool.Pool, logger *slog.Logger) *AnomalyRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &AnomalyRepository{pool: pool, logger: logger}
}

// Record stores a detected anomaly.
func (r *AnomalyRepository) Record(ctx context.Context, a *domain.CostAnomaly) error {
	if a.ID == "" {
		a.ID = domain.NewID()
	}
	if a.DetectedAt.IsZero() {
		a.DetectedAt = domain.Now()
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO cost_anomalies (
			id, dimension, key, window_from, window_to, observed_usd,
			expected_usd, ratio, severity, resolved_at, detected_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		a.ID, a.Dimension, a.Key, a.WindowFrom, a.WindowTo, a.ObservedUSD,
		a.ExpectedUSD, a.Ratio, string(a.Severity), nullableTime(a.ResolvedAt), a.DetectedAt)
	if err != nil {
		return wrapDBError("record cost anomaly", err)
	}
	return nil
}

// ExistsOpen reports whether an unresolved anomaly already covers the same
// dimension/key/day, so detect-on-read never files the same spike twice.
func (r *AnomalyRepository) ExistsOpen(ctx context.Context, dimension, key string, windowFrom time.Time) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM cost_anomalies
			 WHERE dimension = $1 AND key = $2 AND window_from = $3
			   AND resolved_at IS NULL)`, dimension, key, windowFrom).Scan(&exists)
	if err != nil {
		return false, wrapDBError("check anomaly exists", err)
	}
	return exists, nil
}

// ListOpen returns unresolved anomalies, newest first.
func (r *AnomalyRepository) ListOpen(ctx context.Context, limit int) ([]domain.CostAnomaly, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, dimension, key, window_from, window_to, observed_usd,
			expected_usd, ratio, severity, resolved_at, detected_at
		  FROM cost_anomalies
		 WHERE resolved_at IS NULL
		 ORDER BY detected_at DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, wrapDBError("list cost anomalies", err)
	}
	defer rows.Close()
	return collectAnomalies(rows)
}

// Resolve marks an anomaly acknowledged by an operator.
func (r *AnomalyRepository) Resolve(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE cost_anomalies SET resolved_at = now()
		 WHERE id = $1 AND resolved_at IS NULL`, nullableUUID(id))
	if err != nil {
		return wrapDBError("resolve cost anomaly", err)
	}
	return nil
}

func collectAnomalies(rows pgx.Rows) ([]domain.CostAnomaly, error) {
	var out []domain.CostAnomaly
	for rows.Next() {
		var (
			a        domain.CostAnomaly
			severity string
		)
		if err := rows.Scan(&a.ID, &a.Dimension, &a.Key, &a.WindowFrom, &a.WindowTo,
			&a.ObservedUSD, &a.ExpectedUSD, &a.Ratio, &severity, &a.ResolvedAt,
			&a.DetectedAt); err != nil {
			return nil, err
		}
		a.Severity = domain.AnomalySeverity(severity)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Cost analytics over usage_records
// ---------------------------------------------------------------------------

// costDimensionColumns whitelists the GROUP BY dimensions: the dimension name
// is structural, never user input, so a fixed map keeps SQL injection
// unrepresentable.
var costDimensionColumns = map[string]string{
	"provider":     "provider",
	"model":        "model",
	"tenant":       "COALESCE(tenant_id::text,'')",
	"endpoint":     "endpoint_id",
	"request_type": "request_type",
}

// GetByRequestID returns the billing row for one request, newest first when
// a retried write left duplicates: idempotency is on the row id, not the
// request id, so the latest row is the authoritative one.
func (r *UsageRepository) GetByRequestID(ctx context.Context, requestID string) (*domain.UsageRecord, error) {
	query := fmt.Sprintf(`SELECT %s FROM usage_records
		WHERE request_id = $1 ORDER BY created_at DESC LIMIT 1`, usageColumns)
	row := r.pool.QueryRow(ctx, query, requestID)
	rec, err := scanUsage(row)
	if err != nil {
		// A missing row is an ordinary answer ("no such request"), not an
		// infrastructure failure: callers such as feedback submission need to
		// distinguish the two without importing the driver.
		if isNotFound(err) {
			return nil, nil
		}
		return nil, wrapDBError("get usage record", err)
	}
	return rec, nil
}

// CostGrouped aggregates spend by one dimension over the filter's window.
func (r *UsageRepository) CostGrouped(ctx context.Context, filter QueryFilter, dimension string) ([]domain.CostDimensionRow, error) {
	filter.normalize()
	column, ok := costDimensionColumns[strings.ToLower(dimension)]
	if !ok {
		column = costDimensionColumns["provider"]
	}
	where, args := filter.where("usage_records")
	query := fmt.Sprintf(`
		SELECT %s AS key,
			count(*)                              AS requests,
			COALESCE(sum(total_tokens), 0)        AS tokens,
			COALESCE(sum(cost_usd), 0)            AS actual,
			COALESCE(sum(estimate_cost_usd), 0)   AS estimated
		  FROM usage_records
		 WHERE %s
		 GROUP BY 1
		 ORDER BY actual DESC
		 LIMIT 50`, column, where)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("group cost", err)
	}
	defer rows.Close()

	var out []domain.CostDimensionRow
	var total float64
	type raw struct {
		row domain.CostDimensionRow
	}
	var raws []raw
	for rows.Next() {
		var row domain.CostDimensionRow
		if err := rows.Scan(&row.Key, &row.Requests, &row.Tokens, &row.ActualUSD, &row.EstimatedUSD); err != nil {
			return nil, wrapDBError("scan cost group", err)
		}
		total += row.ActualUSD
		raws = append(raws, raw{row: row})
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("group cost", err)
	}
	for _, x := range raws {
		if total > 0 {
			x.row.Share = x.row.ActualUSD / total
		}
		out = append(out, x.row)
	}
	return out, nil
}

// CostTotals aggregates the window totals behind the cost overview: actual
// and estimated spend, tokens, cache savings (estimates on zero-cost cache
// serves) and routing savings (before/after shaping brackets).
func (r *UsageRepository) CostTotals(ctx context.Context, filter QueryFilter) (*domain.CostOverview, error) {
	filter.normalize()
	where, args := filter.where("usage_records")
	query := fmt.Sprintf(`
		SELECT count(*),
			COALESCE(sum(cost_usd), 0),
			COALESCE(sum(estimate_cost_usd), 0),
			COALESCE(sum(total_tokens), 0),
			COALESCE(sum(estimate_cost_usd) FILTER (WHERE cache_hit AND cost_usd = 0), 0),
			COALESCE(sum(cost_before_usd - cost_after_usd) FILTER (WHERE cost_after_usd < cost_before_usd), 0)
		  FROM usage_records
		 WHERE %s`, where)

	ov := &domain.CostOverview{WindowFrom: filter.From, WindowTo: filter.To, GeneratedAt: domain.Now()}
	err := r.pool.QueryRow(ctx, query, args...).Scan(
		&ov.Requests, &ov.ActualUSD, &ov.EstimatedUSD, &ov.Tokens,
		&ov.CacheSavingsUSD, &ov.RoutingSavingsUSD)
	if err != nil {
		return nil, wrapDBError("cost totals", err)
	}
	return ov, nil
}

// CostSeries buckets actual and estimated spend over time with gap filling,
// so charts show flat zeros rather than missing days.
func (r *UsageRepository) CostSeries(ctx context.Context, filter QueryFilter, bucket string) ([]domain.CostPoint, error) {
	filter.normalize()
	trunc, step := costBucket(bucket, filter)
	where, args := filter.where("usage_records")
	query := fmt.Sprintf(`
		SELECT date_trunc('%s', created_at) AS bucket,
			count(*),
			COALESCE(sum(cost_usd), 0),
			COALESCE(sum(estimate_cost_usd), 0)
		  FROM usage_records
		 WHERE %s
		 GROUP BY 1
		 ORDER BY 1`, trunc, where)
	_ = step

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("cost series", err)
	}
	defer rows.Close()

	byBucket := map[time.Time]*domain.CostPoint{}
	var order []time.Time
	for rows.Next() {
		var p domain.CostPoint
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.ActualUSD, &p.EstimatedUSD); err != nil {
			return nil, wrapDBError("scan cost series", err)
		}
		cp := p
		byBucket[p.Bucket.UTC()] = &cp
		order = append(order, p.Bucket.UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("cost series", err)
	}

	// Gap-fill across the window so the chart line never breaks.
	out := []domain.CostPoint{}
	for b := filter.From.UTC().Truncate(time.Hour); !b.After(filter.To); {
		key := b
		switch trunc {
		case "day":
			key = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
		case "week":
			key = b.UTC().Truncate(24 * time.Hour)
		}
		if p, ok := byBucket[key]; ok {
			out = append(out, *p)
		} else {
			out = append(out, domain.CostPoint{Bucket: key})
		}
		switch trunc {
		case "hour":
			b = b.Add(time.Hour)
		case "day":
			b = key.AddDate(0, 0, 1)
		default:
			b = key.AddDate(0, 0, 7)
		}
		if len(out) > 400 {
			break
		}
	}
	_ = order
	return out, nil
}

// costBucket picks the truncation from the window width: hours for a day,
// days for a month, weeks beyond that.
func costBucket(bucket string, filter QueryFilter) (trunc string, step time.Duration) {
	switch strings.ToLower(bucket) {
	case "hour":
		return "hour", time.Hour
	case "day":
		return "day", 24 * time.Hour
	case "week":
		return "week", 7 * 24 * time.Hour
	}
	width := filter.To.Sub(filter.From)
	switch {
	case width <= 36*time.Hour:
		return "hour", time.Hour
	case width <= 45*24*time.Hour:
		return "day", 24 * time.Hour
	default:
		return "week", 7 * 24 * time.Hour
	}
}

// TopCostRequests returns the most expensive requests in the window for the
// "what cost that much" investigation.
func (r *UsageRepository) TopCostRequests(ctx context.Context, filter QueryFilter, limit int) ([]domain.UsageRecord, error) {
	filter.normalize()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	where, args := filter.where("usage_records")
	args = append(args, limit)
	query := fmt.Sprintf(`
		SELECT %s
		  FROM usage_records
		 WHERE %s
		 ORDER BY cost_usd DESC
		 LIMIT $%d`, usageColumns, where, len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("top cost requests", err)
	}
	defer rows.Close()
	var out []domain.UsageRecord
	for rows.Next() {
		rec, err := scanUsage(rows)
		if err != nil {
			return nil, wrapDBError("scan top cost request", err)
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// SpendByDay returns daily spend for forecasting and anomaly baselines.
func (r *UsageRepository) SpendByDay(ctx context.Context, filter QueryFilter, dimension, key string) ([]domain.CostPoint, error) {
	filter.normalize()
	where, args := filter.where("usage_records")
	if dimension != "" {
		column, ok := costDimensionColumns[strings.ToLower(dimension)]
		if !ok {
			column = costDimensionColumns["provider"]
		}
		args = append(args, key)
		where += fmt.Sprintf(" AND %s = $%d", column, len(args))
	}
	query := fmt.Sprintf(`
		SELECT date_trunc('day', created_at) AS bucket,
			count(*),
			COALESCE(sum(cost_usd), 0),
			COALESCE(sum(estimate_cost_usd), 0)
		  FROM usage_records
		 WHERE %s
		 GROUP BY 1
		 ORDER BY 1`, where)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("spend by day", err)
	}
	defer rows.Close()
	var out []domain.CostPoint
	for rows.Next() {
		var p domain.CostPoint
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.ActualUSD, &p.EstimatedUSD); err != nil {
			return nil, wrapDBError("scan spend by day", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SpendSeriesPoint is one day of spend for one dimension value, the input
// anomaly detection works from.
type SpendSeriesPoint struct {
	Key  string
	Day  time.Time
	Cost float64
}

// SpendSeriesByDimension returns daily spend per dimension value over the
// trailing days, capped to the top keys by spend so detection work stays
// bounded on high-cardinality dimensions.
func (r *UsageRepository) SpendSeriesByDimension(ctx context.Context, filter QueryFilter, dimension string, days, maxKeys int) ([]SpendSeriesPoint, error) {
	filter.normalize()
	column, ok := costDimensionColumns[strings.ToLower(dimension)]
	if !ok {
		column = costDimensionColumns["provider"]
	}
	if days <= 0 || days > 90 {
		days = 15
	}
	if maxKeys <= 0 || maxKeys > 100 {
		maxKeys = 30
	}
	where, args := filter.where("usage_records")
	// Unattributed rows (empty key) carry spend that no per-dimension action
	// can address; excluding them keeps anomalies pointing at something real.
	where += fmt.Sprintf(" AND %s <> ''", column)
	since := domain.Now().AddDate(0, 0, -days)
	args = append(args, since)
	query := fmt.Sprintf(`
		WITH ranked AS (
			SELECT %s AS key, COALESCE(sum(cost_usd), 0) AS total
			  FROM usage_records
			 WHERE %s AND created_at >= $%d
			 GROUP BY 1
			 ORDER BY total DESC
			 LIMIT %d
		)
		SELECT r.key, date_trunc('day', u.created_at) AS day,
			COALESCE(sum(u.cost_usd), 0) AS cost
		  FROM usage_records u
		  JOIN ranked r ON r.key = %s
		 WHERE %s AND u.created_at >= $%d
		 GROUP BY 1, 2
		 ORDER BY 1, 2`,
		column, where, len(args), maxKeys, column,
		strings.ReplaceAll(where, "usage_records.", "u."), len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError("spend series by dimension", err)
	}
	defer rows.Close()
	var out []SpendSeriesPoint
	for rows.Next() {
		var p SpendSeriesPoint
		if err := rows.Scan(&p.Key, &p.Day, &p.Cost); err != nil {
			return nil, wrapDBError("scan spend series", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FallbackSavingsTotal sums measured fallback savings over the window: per
// fallback request, the routing estimate (which priced the failed primary)
// minus the actual chain cost, floored at zero. The flooring mirrors
// cost.FallbackSaving row-for-row, so the SQL total and the Go definition
// cannot drift apart.
func (r *UsageRepository) FallbackSavingsTotal(ctx context.Context, filter QueryFilter) (float64, error) {
	filter.normalize()
	where, args := filter.where("usage_records")
	var total float64
	err := r.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT COALESCE(sum(GREATEST(estimate_cost_usd - cost_usd, 0))
			FILTER (WHERE fallback_used), 0)
		  FROM usage_records
		 WHERE %s`, where), args...).Scan(&total)
	if err != nil {
		return 0, wrapDBError("fallback savings", err)
	}
	return total, nil
}

// SpendForBudget sums actual spend in the window: the numerator behind
// budget evaluation. Scope narrowing (tenant/key/policy) is applied by the
// caller via the filter.
func (r *UsageRepository) SpendForBudget(ctx context.Context, filter QueryFilter) (float64, error) {
	filter.normalize()
	where, args := filter.where("usage_records")
	var spent float64
	err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(sum(cost_usd), 0) FROM usage_records WHERE %s`, where),
		args...).Scan(&spent)
	if err != nil {
		return 0, wrapDBError("spend for budget", err)
	}
	return spent, nil
}
