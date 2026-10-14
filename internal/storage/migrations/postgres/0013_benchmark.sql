-- Autonomous Benchmark Engine schema (0013).
--
-- Disabled by default: these tables are inert until ENABLE_BENCHMARK_ENGINE=true.
-- Every row is tenant scoped; results are append-only and never overwritten.
-- Indexes are chosen for millions of benchmark_results rows: tenant+time first,
-- then provider/model, then run/job lookups.

-- ---------------------------------------------------------------------------
-- Benchmark datasets and prompts
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_datasets (
    id          UUID        PRIMARY KEY,
    tenant_id   UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    is_default  BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_datasets_name_format CHECK (name <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS benchmark_datasets_tenant_name_key
    ON benchmark_datasets (tenant_id, name);
CREATE INDEX IF NOT EXISTS benchmark_datasets_default_idx
    ON benchmark_datasets (is_default) WHERE is_default;

CREATE TABLE IF NOT EXISTS benchmark_prompts (
    id           UUID        PRIMARY KEY,
    dataset_id   UUID        NOT NULL REFERENCES benchmark_datasets (id) ON DELETE CASCADE,
    prompt       TEXT        NOT NULL,
    category     TEXT        NOT NULL DEFAULT 'reasoning',
    expected_json TEXT       NOT NULL DEFAULT '',
    difficulty   TEXT        NOT NULL DEFAULT 'medium',
    weight       DOUBLE PRECISION NOT NULL DEFAULT 1,
    max_tokens   INTEGER     NOT NULL DEFAULT 512,
    temperature  DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_prompts_category_check
        CHECK (category IN ('coding','reasoning','math','translation','vision','tool_calling','json_mode','creative_writing','long_context','function_calling')),
    CONSTRAINT benchmark_prompts_difficulty_check
        CHECK (difficulty IN ('easy','medium','hard')),
    CONSTRAINT benchmark_prompts_weight_check CHECK (weight >= 0)
);

CREATE INDEX IF NOT EXISTS benchmark_prompts_dataset_idx ON benchmark_prompts (dataset_id);
CREATE INDEX IF NOT EXISTS benchmark_prompts_category_idx ON benchmark_prompts (category);

-- ---------------------------------------------------------------------------
-- Benchmark jobs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_jobs (
    id                  UUID        PRIMARY KEY,
    tenant_id           UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    name                TEXT        NOT NULL,
    description         TEXT        NOT NULL DEFAULT '',
    enabled             BOOLEAN     NOT NULL DEFAULT TRUE,
    providers           TEXT[]      NOT NULL DEFAULT '{}',
    models              TEXT[]      NOT NULL DEFAULT '{}',
    dataset_id          UUID        REFERENCES benchmark_datasets (id) ON DELETE SET NULL,
    schedule            TEXT        NOT NULL DEFAULT 'every_hour',
    cron_expression     TEXT        NOT NULL DEFAULT '',
    timeout_ms          BIGINT      NOT NULL DEFAULT 60000,
    retry_count         INTEGER     NOT NULL DEFAULT 1,
    region              TEXT        NOT NULL DEFAULT '',
    concurrency         INTEGER     NOT NULL DEFAULT 5,
    max_daily_budget_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    tags                JSONB       NOT NULL DEFAULT '{}'::jsonb,
    last_run_at         TIMESTAMPTZ,
    next_run_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_jobs_name_format CHECK (name <> ''),
    CONSTRAINT benchmark_jobs_schedule_check
        CHECK (schedule IN ('every_minute','every_5m','every_15m','every_30m','every_hour','every_3h','every_6h','every_12h','every_24h','custom')),
    CONSTRAINT benchmark_jobs_concurrency_check CHECK (concurrency BETWEEN 1 AND 100),
    CONSTRAINT benchmark_jobs_budget_check CHECK (max_daily_budget_usd >= 0)
);

CREATE INDEX IF NOT EXISTS benchmark_jobs_tenant_idx ON benchmark_jobs (tenant_id);
CREATE INDEX IF NOT EXISTS benchmark_jobs_enabled_next_idx
    ON benchmark_jobs (enabled, next_run_at) WHERE enabled;
CREATE INDEX IF NOT EXISTS benchmark_jobs_tenant_name_key_unique
    ON benchmark_jobs (tenant_id, name);

-- ---------------------------------------------------------------------------
-- Benchmark runs (one row per scheduled execution)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_runs (
    id            UUID        PRIMARY KEY,
    job_id        UUID        NOT NULL REFERENCES benchmark_jobs (id) ON DELETE CASCADE,
    tenant_id     UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    status        TEXT        NOT NULL DEFAULT 'queued',
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    duration_ms   BIGINT      NOT NULL DEFAULT 0,
    total_probes  INTEGER     NOT NULL DEFAULT 0,
    success_count INTEGER     NOT NULL DEFAULT 0,
    failure_count INTEGER     NOT NULL DEFAULT 0,
    total_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_tokens  BIGINT      NOT NULL DEFAULT 0,
    error         TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_runs_status_check
        CHECK (status IN ('queued','running','completed','failed','cancelled'))
);

CREATE INDEX IF NOT EXISTS benchmark_runs_job_created_idx ON benchmark_runs (job_id, created_at DESC);
CREATE INDEX IF NOT EXISTS benchmark_runs_tenant_created_idx ON benchmark_runs (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS benchmark_runs_status_idx ON benchmark_runs (status);

-- ---------------------------------------------------------------------------
-- Benchmark results (append-only; millions of rows expected)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_results (
    id                   UUID        PRIMARY KEY,
    run_id               UUID        NOT NULL REFERENCES benchmark_runs (id) ON DELETE CASCADE,
    job_id               UUID        NOT NULL REFERENCES benchmark_jobs (id) ON DELETE CASCADE,
    tenant_id            UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    provider             TEXT        NOT NULL DEFAULT '',
    model                TEXT        NOT NULL DEFAULT '',
    prompt_id            UUID,
    category             TEXT        NOT NULL DEFAULT '',
    ttft_ms              BIGINT      NOT NULL DEFAULT 0,
    latency_ms           BIGINT      NOT NULL DEFAULT 0,
    tokens_per_second    DOUBLE PRECISION NOT NULL DEFAULT 0,
    prompt_tokens        INTEGER     NOT NULL DEFAULT 0,
    completion_tokens    INTEGER     NOT NULL DEFAULT 0,
    total_tokens         INTEGER     NOT NULL DEFAULT 0,
    cost_usd             DOUBLE PRECISION NOT NULL DEFAULT 0,
    success              BOOLEAN     NOT NULL DEFAULT FALSE,
    error                TEXT        NOT NULL DEFAULT '',
    error_type           TEXT        NOT NULL DEFAULT '',
    http_status          INTEGER     NOT NULL DEFAULT 0,
    retry_count          INTEGER     NOT NULL DEFAULT 0,
    finish_reason        TEXT        NOT NULL DEFAULT '',
    streaming_speed_tps  DOUBLE PRECISION NOT NULL DEFAULT 0,
    json_success         BOOLEAN,
    tool_calling_success BOOLEAN,
    context_length       INTEGER     NOT NULL DEFAULT 0,
    provider_metadata    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hot path: tenant + time (dashboard windows, retention pruning).
CREATE INDEX IF NOT EXISTS benchmark_results_tenant_created_idx
    ON benchmark_results (tenant_id, created_at DESC);
-- Provider/model breakdowns and comparisons.
CREATE INDEX IF NOT EXISTS benchmark_results_provider_created_idx
    ON benchmark_results (provider, created_at DESC);
CREATE INDEX IF NOT EXISTS benchmark_results_model_created_idx
    ON benchmark_results (model, created_at DESC);
CREATE INDEX IF NOT EXISTS benchmark_results_provider_model_created_idx
    ON benchmark_results (provider, model, created_at DESC);
-- Run/timeline views.
CREATE INDEX IF NOT EXISTS benchmark_results_run_idx ON benchmark_results (run_id);
CREATE INDEX IF NOT EXISTS benchmark_results_job_created_idx
    ON benchmark_results (job_id, created_at DESC);
-- Category and success filtering without scanning failures twice.
CREATE INDEX IF NOT EXISTS benchmark_results_category_idx ON benchmark_results (category);
CREATE INDEX IF NOT EXISTS benchmark_results_errors_idx
    ON benchmark_results (created_at DESC) WHERE success = FALSE;

-- ---------------------------------------------------------------------------
-- Benchmark metrics rollups (optional pre-aggregates)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_metrics (
    id            UUID        PRIMARY KEY,
    tenant_id     UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    provider      TEXT        NOT NULL DEFAULT '',
    model         TEXT        NOT NULL DEFAULT '',
    window_start  TIMESTAMPTZ NOT NULL,
    window_end    TIMESTAMPTZ NOT NULL,
    probes        BIGINT      NOT NULL DEFAULT 0,
    success_rate  DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_ttft_ms   DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_tps       DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_tokens  BIGINT      NOT NULL DEFAULT 0,
    error_rate    DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS benchmark_metrics_tenant_window_idx
    ON benchmark_metrics (tenant_id, window_start DESC);
CREATE INDEX IF NOT EXISTS benchmark_metrics_provider_window_idx
    ON benchmark_metrics (provider, model, window_start DESC);

-- ---------------------------------------------------------------------------
-- Benchmark reports (daily / weekly / monthly)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_reports (
    id                 UUID        PRIMARY KEY,
    tenant_id          UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    period             TEXT        NOT NULL,
    period_start       TIMESTAMPTZ NOT NULL,
    period_end         TIMESTAMPTZ NOT NULL,
    fastest_provider   TEXT        NOT NULL DEFAULT '',
    cheapest_provider  TEXT        NOT NULL DEFAULT '',
    most_reliable      TEXT        NOT NULL DEFAULT '',
    lowest_latency     TEXT        NOT NULL DEFAULT '',
    highest_throughput TEXT        NOT NULL DEFAULT '',
    highest_success    TEXT        NOT NULL DEFAULT '',
    biggest_regression TEXT        NOT NULL DEFAULT '',
    biggest_improvement TEXT       NOT NULL DEFAULT '',
    overall_winner     TEXT        NOT NULL DEFAULT '',
    summary            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_reports_period_check
        CHECK (period IN ('daily','weekly','monthly'))
);

CREATE INDEX IF NOT EXISTS benchmark_reports_tenant_period_idx
    ON benchmark_reports (tenant_id, period, period_start DESC);

-- ---------------------------------------------------------------------------
-- Benchmark notifications
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_notifications (
    id         UUID        PRIMARY KEY,
    tenant_id  UUID        REFERENCES tenants (id) ON DELETE CASCADE,
    channel    TEXT        NOT NULL,
    target     TEXT        NOT NULL DEFAULT '',
    events     TEXT[]      NOT NULL DEFAULT '{}',
    enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_notifications_channel_check
        CHECK (channel IN ('email','discord','slack','webhook'))
);

CREATE INDEX IF NOT EXISTS benchmark_notifications_tenant_idx
    ON benchmark_notifications (tenant_id);

-- ---------------------------------------------------------------------------
-- Benchmark worker logs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS benchmark_worker_logs (
    id         UUID        PRIMARY KEY,
    worker_id  TEXT        NOT NULL DEFAULT '',
    run_id     UUID        REFERENCES benchmark_runs (id) ON DELETE SET NULL,
    level      TEXT        NOT NULL DEFAULT 'info',
    message    TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS benchmark_worker_logs_created_idx
    ON benchmark_worker_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS benchmark_worker_logs_run_idx
    ON benchmark_worker_logs (run_id);

-- ---------------------------------------------------------------------------
-- Audit surface: benchmark resources participate in the control-plane history.
-- ---------------------------------------------------------------------------

ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_resource_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_resource_check
    CHECK (resource IN ('tenant', 'api_key', 'provider', 'model', 'routing_policy', 'budget', 'settings', 'endpoint', 'override', 'replay_job', 'evaluation', 'cache', 'feedback', 'credential', 'test_result', 'tool', 'tool_policy', 'agent_run', 'tunnel', 'dashboard_user', 'dashboard_session', 'pricing_version', 'cost_anomaly', 'benchmark_job', 'benchmark_run', 'benchmark_dataset', 'benchmark_report', 'benchmark_notification'));
