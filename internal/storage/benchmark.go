package storage

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Benchmark datasets and prompts
// ---------------------------------------------------------------------------

// BenchmarkDatasetRepository stores prompt datasets.
type BenchmarkDatasetRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkDatasetRepository builds the repository.
func NewBenchmarkDatasetRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkDatasetRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkDatasetRepository{pool: pool, logger: logger}
}

const benchmarkDatasetColumns = `id, COALESCE(tenant_id::text, ''), name, description, is_default, created_at, updated_at`

// Upsert creates or updates a dataset by tenant+name.
func (r *BenchmarkDatasetRepository) Upsert(ctx context.Context, d *domain.BenchmarkDataset) (*domain.BenchmarkDataset, error) {
	if d.ID == "" {
		d.ID = domain.NewID()
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO benchmark_datasets (id, tenant_id, name, description, is_default)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING `+benchmarkDatasetColumns,
		d.ID, nullableUUID(d.TenantID), d.Name, d.Description, d.IsDefault)
	out, err := scanBenchmarkDataset(row)
	if err != nil {
		if isNotFound(err) {
			// Conflict path: read the existing row.
			var existingID string
			if d.TenantID == "" {
				_ = r.pool.QueryRow(ctx, `SELECT id::text FROM benchmark_datasets WHERE tenant_id IS NULL AND name = $1`, d.Name).Scan(&existingID)
			} else {
				_ = r.pool.QueryRow(ctx, `SELECT id::text FROM benchmark_datasets WHERE tenant_id = $1 AND name = $2`, d.TenantID, d.Name).Scan(&existingID)
			}
			if existingID != "" {
				return r.GetByID(ctx, existingID)
			}
		}
		return nil, wrapDBError("upsert benchmark dataset", err)
	}
	return out, nil
}

// GetByID returns a dataset.
func (r *BenchmarkDatasetRepository) GetByID(ctx context.Context, id string) (*domain.BenchmarkDataset, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+benchmarkDatasetColumns+` FROM benchmark_datasets WHERE id = $1`, id)
	d, err := scanBenchmarkDataset(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load benchmark dataset", err)
	}
	return d, nil
}

// List returns datasets visible to a tenant (own + global defaults).
func (r *BenchmarkDatasetRepository) List(ctx context.Context, tenantID string) ([]domain.BenchmarkDataset, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+benchmarkDatasetColumns+` FROM benchmark_datasets
		WHERE tenant_id IS NULL OR tenant_id = $1 OR $1 = '' ORDER BY is_default DESC, name`, nullableUUID(tenantID))
	if err != nil {
		return nil, wrapDBError("list benchmark datasets", err)
	}
	defer rows.Close()
	var out []domain.BenchmarkDataset
	for rows.Next() {
		d, err := scanBenchmarkDataset(rows)
		if err != nil {
			return nil, wrapDBError("scan benchmark dataset", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// Delete removes a dataset.
func (r *BenchmarkDatasetRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM benchmark_datasets WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete benchmark dataset", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "benchmark dataset not found")
	}
	return nil
}

func scanBenchmarkDataset(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkDataset, error) {
	var d domain.BenchmarkDataset
	if err := row.Scan(&d.ID, &d.TenantID, &d.Name, &d.Description, &d.IsDefault, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// BenchmarkPromptRepository stores prompts.
type BenchmarkPromptRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkPromptRepository builds the repository.
func NewBenchmarkPromptRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkPromptRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkPromptRepository{pool: pool, logger: logger}
}

// Create inserts a prompt.
func (r *BenchmarkPromptRepository) Create(ctx context.Context, p *domain.BenchmarkPrompt) (*domain.BenchmarkPrompt, error) {
	if p.ID == "" {
		p.ID = domain.NewID()
	}
	if p.Weight <= 0 {
		p.Weight = 1
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO benchmark_prompts (id, dataset_id, prompt, category, expected_json, difficulty, weight, max_tokens, temperature)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, dataset_id::text, prompt, category, expected_json, difficulty, weight, max_tokens, temperature, created_at`,
		p.ID, p.DatasetID, p.Prompt, string(p.Category), p.ExpectedJSON, string(p.Difficulty), p.Weight, p.MaxTokens, p.Temperature)
	out, err := scanBenchmarkPrompt(row)
	if err != nil {
		return nil, wrapDBError("create benchmark prompt", err)
	}
	return out, nil
}

// ListByDataset returns a dataset's prompts.
func (r *BenchmarkPromptRepository) ListByDataset(ctx context.Context, datasetID string) ([]domain.BenchmarkPrompt, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, dataset_id::text, prompt, category, expected_json, difficulty, weight, max_tokens, temperature, created_at
		FROM benchmark_prompts WHERE dataset_id = $1 ORDER BY created_at`, datasetID)
	if err != nil {
		return nil, wrapDBError("list benchmark prompts", err)
	}
	defer rows.Close()
	var out []domain.BenchmarkPrompt
	for rows.Next() {
		p, err := scanBenchmarkPrompt(rows)
		if err != nil {
			return nil, wrapDBError("scan benchmark prompt", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func scanBenchmarkPrompt(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkPrompt, error) {
	var (
		p        domain.BenchmarkPrompt
		category string
		diff     string
	)
	if err := row.Scan(&p.ID, &p.DatasetID, &p.Prompt, &category, &p.ExpectedJSON, &diff, &p.Weight, &p.MaxTokens, &p.Temperature, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.Category = domain.BenchmarkPromptCategory(category)
	p.Difficulty = domain.BenchmarkPromptDifficulty(diff)
	return &p, nil
}

// ---------------------------------------------------------------------------
// Benchmark jobs
// ---------------------------------------------------------------------------

// BenchmarkJobRepository stores continuous benchmarking jobs.
type BenchmarkJobRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkJobRepository builds the repository.
func NewBenchmarkJobRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkJobRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkJobRepository{pool: pool, logger: logger}
}

const benchmarkJobColumns = `id, COALESCE(tenant_id::text, ''), name, description, enabled, providers, models,
	COALESCE(dataset_id::text, ''), schedule, cron_expression, timeout_ms, retry_count, region, concurrency,
	max_daily_budget_usd, tags, last_run_at, next_run_at, created_at, updated_at`

// Create inserts a job.
func (r *BenchmarkJobRepository) Create(ctx context.Context, j *domain.BenchmarkJob) (*domain.BenchmarkJob, error) {
	if j.ID == "" {
		j.ID = domain.NewID()
	}
	tags, err := json.Marshal(orEmptyMap(j.Tags))
	if err != nil {
		return nil, wrapDBError("encode benchmark job tags", err)
	}
	providers := j.Providers
	if providers == nil {
		providers = []string{}
	}
	models := j.Models
	if models == nil {
		models = []string{}
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO benchmark_jobs (id, tenant_id, name, description, enabled, providers, models, dataset_id,
			schedule, cron_expression, timeout_ms, retry_count, region, concurrency, max_daily_budget_usd, tags, next_run_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING `+benchmarkJobColumns,
		j.ID, nullableUUID(j.TenantID), j.Name, j.Description, j.Enabled, providers, models,
		nullableUUID(j.DatasetID), j.Schedule, j.CronExpression, j.TimeoutMS, j.RetryCount,
		j.Region, j.Concurrency, j.MaxDailyBudgetUSD, tags, j.NextRunAt)
	out, err := scanBenchmarkJob(row)
	if err != nil {
		return nil, wrapDBError("create benchmark job", err)
	}
	return out, nil
}

// Update rewrites a job's mutable fields.
func (r *BenchmarkJobRepository) Update(ctx context.Context, j *domain.BenchmarkJob) (*domain.BenchmarkJob, error) {
	tags, err := json.Marshal(orEmptyMap(j.Tags))
	if err != nil {
		return nil, wrapDBError("encode benchmark job tags", err)
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE benchmark_jobs SET name=$2, description=$3, enabled=$4, providers=$5, models=$6,
			dataset_id=$7, schedule=$8, cron_expression=$9, timeout_ms=$10, retry_count=$11,
			region=$12, concurrency=$13, max_daily_budget_usd=$14, tags=$15, updated_at=now()
		WHERE id=$1`,
		j.ID, j.Name, j.Description, j.Enabled, j.Providers, j.Models, nullableUUID(j.DatasetID),
		j.Schedule, j.CronExpression, j.TimeoutMS, j.RetryCount, j.Region, j.Concurrency,
		j.MaxDailyBudgetUSD, tags)
	if err != nil {
		return nil, wrapDBError("update benchmark job", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, domain.NewError(domain.ErrCodeNotFound, "benchmark job not found")
	}
	return r.GetByID(ctx, j.ID)
}

// GetByID returns a job.
func (r *BenchmarkJobRepository) GetByID(ctx context.Context, id string) (*domain.BenchmarkJob, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+benchmarkJobColumns+` FROM benchmark_jobs WHERE id = $1`, id)
	j, err := scanBenchmarkJob(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load benchmark job", err)
	}
	return j, nil
}

// List returns jobs for a tenant (empty tenant lists all for admin views).
func (r *BenchmarkJobRepository) List(ctx context.Context, tenantID string, limit, offset int) ([]domain.BenchmarkJob, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	qrows, err := r.pool.Query(ctx, `SELECT `+benchmarkJobColumns+` FROM benchmark_jobs
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, wrapDBError("list benchmark jobs", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkJob
	for qrows.Next() {
		j, err := scanBenchmarkJob(qrows)
		if err != nil {
			return nil, wrapDBError("scan benchmark job", err)
		}
		out = append(out, *j)
	}
	return out, qrows.Err()
}

// ListDue returns enabled jobs whose next run is due.
func (r *BenchmarkJobRepository) ListDue(ctx context.Context, now time.Time, limit int) ([]domain.BenchmarkJob, error) {
	if limit <= 0 {
		limit = 100
	}
	qrows, err := r.pool.Query(ctx, `SELECT `+benchmarkJobColumns+` FROM benchmark_jobs
		WHERE enabled AND (next_run_at IS NULL OR next_run_at <= $1)
		ORDER BY next_run_at NULLS FIRST LIMIT $2`, now, limit)
	if err != nil {
		return nil, wrapDBError("list due benchmark jobs", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkJob
	for qrows.Next() {
		j, err := scanBenchmarkJob(qrows)
		if err != nil {
			return nil, wrapDBError("scan benchmark job", err)
		}
		out = append(out, *j)
	}
	return out, qrows.Err()
}

// MarkRan records last/next run timestamps.
func (r *BenchmarkJobRepository) MarkRan(ctx context.Context, id string, lastRun, nextRun time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE benchmark_jobs SET last_run_at=$2, next_run_at=$3, updated_at=now() WHERE id=$1`,
		id, lastRun, nextRun)
	return err
}

// Delete removes a job (runs and results cascade).
func (r *BenchmarkJobRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM benchmark_jobs WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete benchmark job", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "benchmark job not found")
	}
	return nil
}

// CountActive counts enabled jobs.
func (r *BenchmarkJobRepository) CountActive(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM benchmark_jobs
		WHERE enabled AND ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)`, tenantID).Scan(&n)
	return n, err
}

func scanBenchmarkJob(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkJob, error) {
	var (
		j    domain.BenchmarkJob
		tags []byte
	)
	if err := row.Scan(&j.ID, &j.TenantID, &j.Name, &j.Description, &j.Enabled, &j.Providers,
		&j.Models, &j.DatasetID, &j.Schedule, &j.CronExpression, &j.TimeoutMS, &j.RetryCount,
		&j.Region, &j.Concurrency, &j.MaxDailyBudgetUSD, &tags, &j.LastRunAt, &j.NextRunAt,
		&j.CreatedAt, &j.UpdatedAt); err != nil {
		return nil, err
	}
	if len(tags) > 0 {
		_ = json.Unmarshal(tags, &j.Tags)
	}
	return &j, nil
}

// ---------------------------------------------------------------------------
// Benchmark runs
// ---------------------------------------------------------------------------

// BenchmarkRunRepository stores runs.
type BenchmarkRunRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkRunRepository builds the repository.
func NewBenchmarkRunRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkRunRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkRunRepository{pool: pool, logger: logger}
}

const benchmarkRunColumns = `id, job_id::text, COALESCE(tenant_id::text, ''), status, started_at, finished_at,
	duration_ms, total_probes, success_count, failure_count, total_cost_usd, total_tokens, error, created_at`

// Create inserts a run.
func (r *BenchmarkRunRepository) Create(ctx context.Context, run *domain.BenchmarkRun) (*domain.BenchmarkRun, error) {
	if run.ID == "" {
		run.ID = domain.NewID()
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO benchmark_runs (id, job_id, tenant_id, status, started_at)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+benchmarkRunColumns,
		run.ID, run.JobID, nullableUUID(run.TenantID), run.Status, run.StartedAt)
	out, err := scanBenchmarkRun(row)
	if err != nil {
		return nil, wrapDBError("create benchmark run", err)
	}
	return out, nil
}

// Finish completes a run.
func (r *BenchmarkRunRepository) Finish(ctx context.Context, id, status string, probes, successes, failures int, cost float64, tokens int64, runErr string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE benchmark_runs SET status=$2, finished_at=now(),
			duration_ms = EXTRACT(EPOCH FROM (now() - started_at))::bigint * 1000,
			total_probes=$3, success_count=$4, failure_count=$5, total_cost_usd=$6, total_tokens=$7, error=$8
		WHERE id=$1`, id, status, probes, successes, failures, cost, tokens, runErr)
	return err
}

// SetStatus moves a run to a new status without finalizing counters.
func (r *BenchmarkRunRepository) SetStatus(ctx context.Context, id, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE benchmark_runs SET status=$2 WHERE id=$1`, id, status)
	return err
}

// GetByID returns a run.
func (r *BenchmarkRunRepository) GetByID(ctx context.Context, id string) (*domain.BenchmarkRun, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+benchmarkRunColumns+` FROM benchmark_runs WHERE id=$1`, id)
	run, err := scanBenchmarkRun(row)
	if isNotFound(err) {
		return nil, nil
	}
	return run, err
}

// List returns recent runs for a tenant with pagination.
func (r *BenchmarkRunRepository) List(ctx context.Context, tenantID, jobID string, limit, offset int) ([]domain.BenchmarkRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	qrows, err := r.pool.Query(ctx, `SELECT `+benchmarkRunColumns+` FROM benchmark_runs
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		  AND ($2 = '' OR job_id = $2::uuid)
		ORDER BY created_at DESC LIMIT $3 OFFSET $4`, tenantID, jobID, limit, offset)
	if err != nil {
		return nil, wrapDBError("list benchmark runs", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkRun
	for qrows.Next() {
		run, err := scanBenchmarkRun(qrows)
		if err != nil {
			return nil, wrapDBError("scan benchmark run", err)
		}
		out = append(out, *run)
	}
	return out, qrows.Err()
}

// LastRun returns the most recent run for a tenant.
func (r *BenchmarkRunRepository) LastRun(ctx context.Context, tenantID string) (*domain.BenchmarkRun, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+benchmarkRunColumns+` FROM benchmark_runs
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		ORDER BY created_at DESC LIMIT 1`, tenantID)
	run, err := scanBenchmarkRun(row)
	if isNotFound(err) {
		return nil, nil
	}
	return run, err
}

// Stats aggregates run totals for the overview.
func (r *BenchmarkRunRepository) Stats(ctx context.Context, tenantID string) (total int64, failed int64, avgDuration float64, err error) {
	err = r.pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE status='failed'),
		COALESCE(AVG(duration_ms),0) FROM benchmark_runs
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)`, tenantID).Scan(&total, &failed, &avgDuration)
	return total, failed, avgDuration, err
}

func scanBenchmarkRun(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkRun, error) {
	var run domain.BenchmarkRun
	if err := row.Scan(&run.ID, &run.JobID, &run.TenantID, &run.Status, &run.StartedAt,
		&run.FinishedAt, &run.DurationMS, &run.TotalProbes, &run.SuccessCount, &run.FailureCount,
		&run.TotalCostUSD, &run.TotalTokens, &run.Error, &run.CreatedAt); err != nil {
		return nil, err
	}
	return &run, nil
}

// ---------------------------------------------------------------------------
// Benchmark results
// ---------------------------------------------------------------------------

// BenchmarkResultRepository stores append-only measurements.
type BenchmarkResultRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkResultRepository builds the repository.
func NewBenchmarkResultRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkResultRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkResultRepository{pool: pool, logger: logger}
}

const benchmarkResultColumns = `id, run_id::text, job_id::text, COALESCE(tenant_id::text, ''),
	provider, model, COALESCE(prompt_id::text, ''), category, ttft_ms, latency_ms, tokens_per_second,
	prompt_tokens, completion_tokens, total_tokens, cost_usd, success, error, error_type, http_status,
	retry_count, finish_reason, streaming_speed_tps, json_success, tool_calling_success, context_length,
	provider_metadata, created_at`

// Insert stores one result.
func (r *BenchmarkResultRepository) Insert(ctx context.Context, res *domain.BenchmarkResult) error {
	if res.ID == "" {
		res.ID = domain.NewID()
	}
	meta, err := json.Marshal(orEmptyMap(res.ProviderMetadata))
	if err != nil {
		return wrapDBError("encode benchmark provider metadata", err)
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO benchmark_results (id, run_id, job_id, tenant_id, provider, model, prompt_id,
			category, ttft_ms, latency_ms, tokens_per_second, prompt_tokens, completion_tokens,
			total_tokens, cost_usd, success, error, error_type, http_status, retry_count,
			finish_reason, streaming_speed_tps, json_success, tool_calling_success, context_length,
			provider_metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		res.ID, res.RunID, res.JobID, nullableUUID(res.TenantID), res.Provider, res.Model,
		nullableUUID(res.PromptID), res.Category, res.TTFTMS, res.LatencyMS, res.TokensPerSecond,
		res.PromptTokens, res.CompletionTokens, res.TotalTokens, res.CostUSD, res.Success,
		res.Error, res.ErrorType, res.HTTPStatus, res.RetryCount, res.FinishReason,
		res.StreamingSpeed, res.JSONSuccess, res.ToolCallingSuccess, res.ContextLength, meta)
	if err != nil {
		return wrapDBError("insert benchmark result", err)
	}
	return nil
}

// BenchmarkHistoryFilter scopes history queries.
type BenchmarkHistoryFilter struct {
	TenantID string
	JobID    string
	Provider string
	Model    string
	Category string
	From     time.Time
	To       time.Time
	Success  *bool
	Limit    int
	Offset   int
}

// List returns paginated history (newest first).
func (r *BenchmarkResultRepository) List(ctx context.Context, f BenchmarkHistoryFilter) ([]domain.BenchmarkResult, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	qrows, err := r.pool.Query(ctx, `SELECT `+benchmarkResultColumns+` FROM benchmark_results
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		  AND ($2 = '' OR job_id = $2::uuid)
		  AND ($3 = '' OR provider = $3)
		  AND ($4 = '' OR model = $4)
		  AND ($5 = '' OR category = $5)
		  AND ($6::timestamptz IS NULL OR created_at >= $6)
		  AND ($7::timestamptz IS NULL OR created_at <= $7)
		  AND ($8::boolean IS NULL OR success = $8)
		ORDER BY created_at DESC LIMIT $9 OFFSET $10`,
		f.TenantID, f.JobID, f.Provider, f.Model, f.Category,
		benchmarkNullTime(f.From), benchmarkNullTime(f.To), f.Success, f.Limit, f.Offset)
	if err != nil {
		return nil, wrapDBError("list benchmark results", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkResult
	for qrows.Next() {
		res, err := scanBenchmarkResult(qrows)
		if err != nil {
			return nil, wrapDBError("scan benchmark result", err)
		}
		out = append(out, *res)
	}
	return out, qrows.Err()
}

// Totals aggregates cost/tokens/success for the overview and budget guards.
func (r *BenchmarkResultRepository) Totals(ctx context.Context, tenantID string, from, to time.Time) (probes int64, successRate float64, cost float64, tokens int64, err error) {
	err = r.pool.QueryRow(ctx, `SELECT COUNT(*),
		COALESCE(AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END),0),
		COALESCE(SUM(cost_usd),0), COALESCE(SUM(total_tokens),0)
		FROM benchmark_results
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		  AND ($2::timestamptz IS NULL OR created_at >= $2)
		  AND ($3::timestamptz IS NULL OR created_at <= $3)`,
		tenantID, benchmarkNullTime(from), benchmarkNullTime(to)).Scan(&probes, &successRate, &cost, &tokens)
	return probes, successRate, cost, tokens, err
}

// ProviderStats aggregates per-provider metrics for health, charts and reports.
func (r *BenchmarkResultRepository) ProviderStats(ctx context.Context, tenantID string, from, to time.Time) ([]domain.BenchmarkProviderHealth, error) {
	qrows, err := r.pool.Query(ctx, `SELECT provider,
		COUNT(*), COALESCE(AVG(CASE WHEN success THEN 1.0 ELSE 0.0 END),0),
		COALESCE(AVG(latency_ms),0), COALESCE(AVG(ttft_ms),0),
		COALESCE(AVG(cost_usd),0), COALESCE(AVG(tokens_per_second),0), COALESCE(AVG(streaming_speed_tps),0),
		MAX(CASE WHEN NOT success THEN created_at END), MAX(created_at)
		FROM benchmark_results
		WHERE provider <> '' AND ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		  AND ($2::timestamptz IS NULL OR created_at >= $2)
		  AND ($3::timestamptz IS NULL OR created_at <= $3)
		GROUP BY provider ORDER BY provider`, tenantID, benchmarkNullTime(from), benchmarkNullTime(to))
	if err != nil {
		return nil, wrapDBError("benchmark provider stats", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkProviderHealth
	for qrows.Next() {
		var h domain.BenchmarkProviderHealth
		if err := qrows.Scan(&h.Provider, &h.TotalProbes, &h.SuccessRate, &h.AvgLatencyMS, &h.AvgTTFTMS,
			&h.AvgCostUSD, &h.AvgTPS, &h.AvgStreamingTPS, &h.LastFailure, &h.LastBenchmark); err != nil {
			return nil, wrapDBError("scan benchmark provider stats", err)
		}
		h.Uptime = h.SuccessRate
		h.Status = benchmarkHealthStatus(h.SuccessRate, h.AvgLatencyMS)
		out = append(out, h)
	}
	return out, qrows.Err()
}

func benchmarkHealthStatus(successRate, avgLatencyMS float64) string {
	switch {
	case successRate < 0.5:
		return "down"
	case successRate < 0.9:
		return "degraded"
	case avgLatencyMS > 10000:
		return "slow"
	case successRate < 0.99:
		return "degraded"
	default:
		return "healthy"
	}
}

func scanBenchmarkResult(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkResult, error) {
	var (
		res  domain.BenchmarkResult
		meta []byte
	)
	if err := row.Scan(&res.ID, &res.RunID, &res.JobID, &res.TenantID, &res.Provider,
		&res.Model, &res.PromptID, &res.Category, &res.TTFTMS, &res.LatencyMS, &res.TokensPerSecond,
		&res.PromptTokens, &res.CompletionTokens, &res.TotalTokens, &res.CostUSD, &res.Success,
		&res.Error, &res.ErrorType, &res.HTTPStatus, &res.RetryCount, &res.FinishReason,
		&res.StreamingSpeed, &res.JSONSuccess, &res.ToolCallingSuccess, &res.ContextLength,
		&meta, &res.CreatedAt); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &res.ProviderMetadata)
	}
	return &res, nil
}

func benchmarkNullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ---------------------------------------------------------------------------
// Benchmark reports
// ---------------------------------------------------------------------------

// BenchmarkReportRepository stores generated reports.
type BenchmarkReportRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkReportRepository builds the repository.
func NewBenchmarkReportRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkReportRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkReportRepository{pool: pool, logger: logger}
}

const benchmarkReportColumns = `id, COALESCE(tenant_id::text, ''), period, period_start, period_end,
	fastest_provider, cheapest_provider, most_reliable, lowest_latency, highest_throughput,
	highest_success, biggest_regression, biggest_improvement, overall_winner, summary, created_at`

// Create inserts a report.
func (r *BenchmarkReportRepository) Create(ctx context.Context, rep *domain.BenchmarkReport) (*domain.BenchmarkReport, error) {
	if rep.ID == "" {
		rep.ID = domain.NewID()
	}
	summary, err := json.Marshal(orEmptyAny(rep.Summary))
	if err != nil {
		return nil, wrapDBError("encode benchmark report summary", err)
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO benchmark_reports (id, tenant_id, period, period_start, period_end,
			fastest_provider, cheapest_provider, most_reliable, lowest_latency, highest_throughput,
			highest_success, biggest_regression, biggest_improvement, overall_winner, summary)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING `+benchmarkReportColumns,
		rep.ID, nullableUUID(rep.TenantID), rep.Period, rep.PeriodStart, rep.PeriodEnd,
		rep.FastestProvider, rep.CheapestProvider, rep.MostReliable, rep.LowestLatency,
		rep.HighestThroughput, rep.HighestSuccess, rep.BiggestRegression, rep.BiggestImprovement,
		rep.OverallWinner, summary)
	out, err := scanBenchmarkReport(row)
	if err != nil {
		return nil, wrapDBError("create benchmark report", err)
	}
	return out, nil
}

// Latest returns the newest report of a period for a tenant.
func (r *BenchmarkReportRepository) Latest(ctx context.Context, tenantID, period string) (*domain.BenchmarkReport, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+benchmarkReportColumns+` FROM benchmark_reports
		WHERE period=$2 AND ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		ORDER BY period_start DESC LIMIT 1`, tenantID, period)
	rep, err := scanBenchmarkReport(row)
	if isNotFound(err) {
		return nil, nil
	}
	return rep, err
}

// List returns recent reports.
func (r *BenchmarkReportRepository) List(ctx context.Context, tenantID string, limit int) ([]domain.BenchmarkReport, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	qrows, err := r.pool.Query(ctx, `SELECT `+benchmarkReportColumns+` FROM benchmark_reports
		WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL)
		ORDER BY period_start DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, wrapDBError("list benchmark reports", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkReport
	for qrows.Next() {
		rep, err := scanBenchmarkReport(qrows)
		if err != nil {
			return nil, wrapDBError("scan benchmark report", err)
		}
		out = append(out, *rep)
	}
	return out, qrows.Err()
}

func scanBenchmarkReport(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkReport, error) {
	var (
		rep     domain.BenchmarkReport
		summary []byte
	)
	if err := row.Scan(&rep.ID, &rep.TenantID, &rep.Period, &rep.PeriodStart, &rep.PeriodEnd,
		&rep.FastestProvider, &rep.CheapestProvider, &rep.MostReliable, &rep.LowestLatency,
		&rep.HighestThroughput, &rep.HighestSuccess, &rep.BiggestRegression, &rep.BiggestImprovement,
		&rep.OverallWinner, &summary, &rep.CreatedAt); err != nil {
		return nil, err
	}
	if len(summary) > 0 {
		_ = json.Unmarshal(summary, &rep.Summary)
	}
	return &rep, nil
}

func orEmptyAny(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// ---------------------------------------------------------------------------
// Benchmark notifications
// ---------------------------------------------------------------------------

// BenchmarkNotificationRepository stores delivery rules.
type BenchmarkNotificationRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkNotificationRepository builds the repository.
func NewBenchmarkNotificationRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkNotificationRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkNotificationRepository{pool: pool, logger: logger}
}

// Upsert creates or updates a notification rule.
func (r *BenchmarkNotificationRepository) Upsert(ctx context.Context, n *domain.BenchmarkNotification) (*domain.BenchmarkNotification, error) {
	if n.ID == "" {
		n.ID = domain.NewID()
	}
	events := n.Events
	if events == nil {
		events = []string{}
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO benchmark_notifications (id, tenant_id, channel, target, events, enabled)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO UPDATE SET channel=EXCLUDED.channel, target=EXCLUDED.target,
			events=EXCLUDED.events, enabled=EXCLUDED.enabled, updated_at=now()
		RETURNING id, COALESCE(tenant_id::text,''), channel, target, events, enabled, created_at, updated_at`,
		n.ID, nullableUUID(n.TenantID), string(n.Channel), n.Target, events, n.Enabled)
	out, err := scanBenchmarkNotification(row)
	if err != nil {
		return nil, wrapDBError("upsert benchmark notification", err)
	}
	return out, nil
}

// List returns rules for a tenant.
func (r *BenchmarkNotificationRepository) List(ctx context.Context, tenantID string) ([]domain.BenchmarkNotification, error) {
	qrows, err := r.pool.Query(ctx, `SELECT id, COALESCE(tenant_id::text,''), channel, target, events, enabled, created_at, updated_at
		FROM benchmark_notifications WHERE ($1 = '' OR tenant_id = $1::uuid OR tenant_id IS NULL) ORDER BY created_at`,
		tenantID)
	if err != nil {
		return nil, wrapDBError("list benchmark notifications", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkNotification
	for qrows.Next() {
		n, err := scanBenchmarkNotification(qrows)
		if err != nil {
			return nil, wrapDBError("scan benchmark notification", err)
		}
		out = append(out, *n)
	}
	return out, qrows.Err()
}

// Delete removes a rule.
func (r *BenchmarkNotificationRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM benchmark_notifications WHERE id=$1`, id)
	if err != nil {
		return wrapDBError("delete benchmark notification", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "benchmark notification not found")
	}
	return nil
}

func scanBenchmarkNotification(row interface {
	Scan(dest ...any) error
}) (*domain.BenchmarkNotification, error) {
	var (
		n       domain.BenchmarkNotification
		channel string
	)
	if err := row.Scan(&n.ID, &n.TenantID, &channel, &n.Target, &n.Events, &n.Enabled, &n.CreatedAt, &n.UpdatedAt); err != nil {
		return nil, err
	}
	n.Channel = domain.BenchmarkNotificationChannel(channel)
	return &n, nil
}

// ---------------------------------------------------------------------------
// Benchmark worker logs
// ---------------------------------------------------------------------------

// BenchmarkWorkerLogRepository stores worker events.
type BenchmarkWorkerLogRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBenchmarkWorkerLogRepository builds the repository.
func NewBenchmarkWorkerLogRepository(pool *pgxpool.Pool, logger *slog.Logger) *BenchmarkWorkerLogRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &BenchmarkWorkerLogRepository{pool: pool, logger: logger}
}

// Append writes one log line (best-effort; never fails a run).
func (r *BenchmarkWorkerLogRepository) Append(ctx context.Context, workerID, runID, level, message string) {
	id := domain.NewID()
	_, _ = r.pool.Exec(ctx, `INSERT INTO benchmark_worker_logs (id, worker_id, run_id, level, message)
		VALUES ($1,$2,$3,$4,$5)`, id, workerID, nullableUUID(runID), level, message)
}

// List returns recent logs.
func (r *BenchmarkWorkerLogRepository) List(ctx context.Context, limit int) ([]domain.BenchmarkWorkerLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	qrows, err := r.pool.Query(ctx, `SELECT id, worker_id, COALESCE(run_id::text,''), level, message, created_at
		FROM benchmark_worker_logs ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, wrapDBError("list benchmark worker logs", err)
	}
	defer qrows.Close()
	var out []domain.BenchmarkWorkerLog
	for qrows.Next() {
		var l domain.BenchmarkWorkerLog
		if err := qrows.Scan(&l.ID, &l.WorkerID, &l.RunID, &l.Level, &l.Message, &l.CreatedAt); err != nil {
			return nil, wrapDBError("scan benchmark worker log", err)
		}
		out = append(out, l)
	}
	return out, qrows.Err()
}
