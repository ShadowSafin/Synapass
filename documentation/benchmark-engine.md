# Autonomous Benchmark Engine

An enterprise-grade, fully modular service that continuously benchmarks
providers and models, stores every measurement permanently, generates reports,
and powers a real-time analytics dashboard.

**Disabled by default.** When off, the gateway behaves exactly as before: no
Benchmark page, no scheduler, no workers, no benchmark API routes, and zero
impact on inference performance.

**Related:** [Dashboard](dashboard.md) · [API reference](api.md) ·
[Architecture](architecture.md) · [Database](database.md)

## Contents

- [Feature flag](#feature-flag)
- [Architecture](#architecture)
- [Dashboard](#dashboard)
- [Benchmark jobs](#benchmark-jobs)
- [Scheduler](#scheduler)
- [Prompt datasets](#prompt-datasets)
- [Benchmark execution](#benchmark-execution)
- [Metrics](#metrics)
- [Charts and history](#charts-and-history)
- [Provider health](#provider-health)
- [Comparison mode](#comparison-mode)
- [Reports](#reports)
- [AI insights](#ai-insights)
- [Live dashboard](#live-dashboard)
- [Budget protection](#budget-protection)
- [Notifications](#notifications)
- [Export](#export)
- [API](#api)
- [Database](#database)
- [Security](#security)
- [Performance](#performance)

## Feature flag

```sh
ENABLE_BENCHMARK_ENGINE=false
```

| State | Behaviour |
| --- | --- |
| `false` (default) | Benchmark page shows an enable switch; no scheduler; no workers; benchmark data routes answer 404; `GET /admin/v1/benchmark/status` answers `{enabled: false}` |
| `true` | Benchmark page live; scheduler starts; workers start; all benchmark API routes serve |

Enabling from the dashboard: open the Benchmark page and press **Enable
Benchmark Engine**. The toggle persists server-side (settings table, so it
survives restarts) and starts the scheduler and workers immediately — no
restart or config change needed. Press **Disable** in the page header to stop
them the same way. Enabling needs full admin scope. When the engine is on via
configuration, the page notes it is managed by configuration instead.

The namespaced alias `SYNAPASS_BENCHMARK_ENABLED` (and the
`benchmark.enabled` YAML key) set the same switch. The dashboard shows its
sidebar entry only when built with `NEXT_PUBLIC_ENABLE_BENCHMARK_ENGINE=true`.

Further tuning (all optional):

```yaml
benchmark:
  enabled: false
  scheduler_enabled: true
  workers: 2
  queue_size: 1000
  default_timeout: 60s
  default_concurrency: 5
  max_daily_cost_usd: 0      # 0 = unlimited (per-job ceilings still apply)
  retention_days: 0          # 0 = keep everything forever
  redis_queue_key: benchmark:queue
```

## Architecture

The engine is its own independent module (`internal/benchmark`), depending
only on `config`, `domain` and `storage`, so it can later be extracted into
its own service or sold as a premium enterprise feature.

```text
Dashboard
    │
    ▼
Benchmark API          (internal/api/benchmark_admin.go, gated on the flag)
    │
    ▼
Scheduler              (ticks every 15s, enqueues due jobs)
    │
    ▼
Redis Queue            (list; in-memory fallback when Redis is absent)
    │
    ▼
Benchmark Workers      (in-process pool + horizontal replicas)
    │
    ▼
Synapass Gateway       (live provider adapters)
    │
    ▼
Inference Providers
    │
    ▼
Database               (append-only results, reports, logs)
```

Workers scale horizontally: every replica drains the same Redis queue.

## Dashboard

A new `Benchmark` page under the Workspace section, styled in the existing
Synapass design language (responsive, glassmorphism cards, dark mode) with a
Grafana / Vercel-Analytics feel: stat cards, SVG time-series charts, a
latency heatmap, health table, comparison mode, job management, run timeline,
results explorer, reports, insights and export — all updating
live without a refresh.

### Overview

Engine status, scheduler status, active jobs, running workers, queue size,
last and next benchmark, average benchmark duration, total benchmarks,
success rate, failed jobs, total daily cost and total tokens benchmarked.

## Benchmark jobs

Unlimited jobs, each with name, description, enabled flag, providers,
models, prompt dataset, benchmark interval, timeout, retry count, region,
concurrency (1–100), maximum daily budget and tags.

## Scheduler

`every_minute`, `every_5m`, `every_15m`, `every_30m`, `every_hour`,
`every_3h`, `every_6h`, `every_12h`, `every_24h`, or `custom` with a standard
5-field cron expression (`minute hour dom month dow`, supporting `*`,
`*/n`, lists, ranges and steps).

## Prompt datasets

A default corpus (`synapass-default`, one prompt per category) is seeded on
first enable; custom datasets can be created through the API or dashboard.
Each prompt carries the text, category, optional expected JSON, difficulty,
weight, max tokens and temperature. Categories: coding, reasoning, math,
translation, vision, tool calling, JSON mode, creative writing, long context
and function calling.

## Benchmark execution

For every scheduled run: for each provider → for each model → run every
prompt → collect metrics → store results. Everything runs asynchronously in
bounded worker goroutines; a slow provider can never stall the gateway.

## Metrics

TTFT, total latency, tokens/sec, prompt/completion/total tokens, cost,
success, error + error type, HTTP status, retry count, finish reason,
streaming speed, JSON success, tool-calling success, context length and
provider metadata.

## Charts and history

Results are never overwritten. Windows: last hour, 24 hours, 7/30/90 days,
last year, or a custom range. Charts: TTFT, latency, tokens/sec, cost,
error/success rates, availability (uptime), throughput, streaming speed,
response distribution, heatmap, run timeline, and daily/weekly/monthly
trends. A results explorer adds search, provider/category/outcome filtering
and sorting.

## Provider health

Healthy 🟢, slow 🟡, degraded 🟠, down 🔴 — each with uptime, average
latency, average TTFT, average cost, throughput, success rate, last failure,
last benchmark and probe count.

## Comparison mode

Select multiple providers for a side-by-side comparison across TTFT,
latency, tokens/sec, streaming performance, cost, error/success rates and
availability.

## Reports

Daily, weekly and monthly reports naming the fastest, cheapest and most
reliable providers, lowest latency, highest throughput, highest success
rate, biggest regression, biggest improvement and overall winner. Generate
on demand (`GET /benchmark/report?period=daily&generate=true`) or list past
reports.

## AI insights

Window-over-window summaries such as “Claude became 14% faster” or “HopsAPI
experienced outages”, plus fastest-provider and reliability callouts.

## Live dashboard

`GET /benchmark/live` streams run lifecycle events as server-sent events;
the dashboard additionally polls key views every 5s, so panels update
without a refresh either way.

## Budget protection

Per-job `max_daily_budget_usd` plus a global `max_daily_cost_usd` ceiling.
When exceeded, jobs pause automatically and resume the next day; the event
fans out to notifications.

## Notifications

Email, Discord and Slack targets, triggered by benchmark
completion, provider down, high latency, high error rate, budget exceeded
and new report generation. Delivery is best-effort and asynchronous — a
slow chat webhook never stalls a run.

## Export

History downloads as CSV, JSON, Excel (tab-separated, opens natively) or a
printable PDF-ready text report.

## API

All under `/admin/v1`, all workspace scoped by `tenant_id`, all gated on
the feature flag:

```text
GET    /benchmark/status
POST   /benchmark/enabled           (runtime enable/disable toggle, full admin)
GET    /benchmark/overview
GET    /benchmark/jobs
POST   /benchmark/jobs
PUT    /benchmark/jobs/:id
DELETE /benchmark/jobs/:id
POST   /benchmark/run
GET    /benchmark/runs
GET    /benchmark/history[?range=1h|24h|7d|30d|90d|1y][&export=csv|json|excel|pdf]
GET    /benchmark/report[?period=daily|weekly|monthly][&generate=true]
GET    /benchmark/live            (SSE)
GET    /benchmark/providers
GET    /benchmark/models[?provider=]
GET    /benchmark/health
GET    /benchmark/compare?providers=a,b
GET    /benchmark/insights
GET    /benchmark/datasets
POST   /benchmark/datasets
GET    /benchmark/notifications
POST   /benchmark/notifications
```

## Database

Migration `0013_benchmark.sql`: `benchmark_jobs`, `benchmark_runs`,
`benchmark_results` (indexed for millions of rows: tenant+time,
provider+time, model+time, provider+model+time, run, job, category, and a
partial failures index), `benchmark_metrics`, `benchmark_datasets`,
`benchmark_prompts`, `benchmark_reports`, `benchmark_notifications` and
`benchmark_worker_logs`. Audit events accept the new `benchmark_*`
resources.

## Security

Every row carries its workspace (`tenant_id`); every query filters on it, so
a workspace can never read another's benchmarks. Existing authentication
and RBAC apply unchanged (reads on `usage:read`, writes on full admin).

## Performance

Fully asynchronous and non-blocking: the scheduler never applies
backpressure (a full queue drops with an error, loudly), probes are
concurrency-bounded per job, notifications are fire-and-forget, history is
paginated with bounded limits, and aggregation queries read the
purpose-built indexes. The inference hot path never touches the engine.
