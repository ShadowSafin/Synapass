'use client';

import {
  Activity,
  ArrowDownUp,
  ChevronDown,
  Download,
  FlaskConical,
  HeartPulse,
  Play,
  Plus,
  RefreshCw,
  Sparkles,
  Trash2,
} from 'lucide-react';
import * as React from 'react';

import { BarList } from '@/components/charts/bar-list';
import { LineChart } from '@/components/charts/time-series';
import { PageHeader } from '@/components/page-header';
import { StatCard } from '@/components/stat-card';
import { TenantPicker, useTenantParam } from '@/components/tenant-picker';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { EmptyState, ErrorState, TableSkeleton } from '@/components/ui/state';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import {
  useBenchmarkCompare,
  useBenchmarkDatasets,
  useBenchmarkHealth,
  useBenchmarkHistory,
  useBenchmarkInsights,
  useBenchmarkJobs,
  useBenchmarkOverview,
  useBenchmarkReports,
  useBenchmarkRuns,
  useBenchmarkStatus,
  useCreateBenchmarkJob,
  useDeleteBenchmarkJob,
  useGenerateBenchmarkReport,
  useModels,
  useProviders,
  useSetBenchmarkEnabled,
  useTriggerBenchmarkRun,
  useUpdateBenchmarkJob,
} from '@/hooks/use-admin';
import {
  formatCurrency,
  formatDateTime,
  formatDurationMs,
  formatNumber,
  formatPercent,
  formatRelative,
} from '@/lib/format';
import { cn } from '@/lib/utils';
import type {
  BenchmarkJob,
  BenchmarkJobInput,
  BenchmarkProviderHealth,
  BenchmarkResult,
} from '@/lib/types';

const RANGES = [
  { value: '1h', label: 'Last hour' },
  { value: '24h', label: 'Last 24 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
  { value: '90d', label: 'Last 90 days' },
  { value: '1y', label: 'Last year' },
] as const;

const SCHEDULES = [
  { value: 'every_minute', label: 'Every minute' },
  { value: 'every_5m', label: 'Every 5 minutes' },
  { value: 'every_15m', label: 'Every 15 minutes' },
  { value: 'every_30m', label: 'Every 30 minutes' },
  { value: 'every_hour', label: 'Every hour' },
  { value: 'every_3h', label: 'Every 3 hours' },
  { value: 'every_6h', label: 'Every 6 hours' },
  { value: 'every_12h', label: 'Every 12 hours' },
  { value: 'every_24h', label: 'Every 24 hours' },
  { value: 'custom', label: 'Custom cron' },
] as const;

const STATUS_DOT: Record<string, string> = {
  healthy: 'bg-emerald-400',
  slow: 'bg-amber-400',
  degraded: 'bg-orange-400',
  down: 'bg-rose-500',
};

const STATUS_LABEL: Record<string, string> = {
  healthy: 'Healthy',
  slow: 'Slow',
  degraded: 'Degraded',
  down: 'Down',
};

function healthDot(status: string): string {
  return STATUS_DOT[status] ?? 'bg-neutral-500';
}

function healthLabel(status: string): string {
  return STATUS_LABEL[status] ?? status;
}

/** Bucket history rows into N time buckets per provider for one metric. */
function bucketMetric(
  results: BenchmarkResult[],
  metric: (r: BenchmarkResult) => number,
  buckets = 24,
): { labels: string[]; byProvider: Map<string, number[]> } {
  const sorted = [...results].sort(
    (a, b) => new Date(a.created_at).getTime() - new Date(b.created_at).getTime(),
  );
  const byProvider = new Map<string, number[]>();
  const labels: string[] = [];
  if (sorted.length === 0) return { labels, byProvider };
  const first = new Date(sorted[0]?.created_at ?? Date.now()).getTime();
  const last = new Date(sorted[sorted.length - 1]?.created_at ?? Date.now()).getTime();
  const width = Math.max(1, (last - first) / buckets);
  const sums = new Map<string, number[]>();
  const counts = new Map<string, number[]>();
  for (const r of sorted) {
    const idx = Math.min(buckets - 1, Math.floor((new Date(r.created_at).getTime() - first) / width));
    if (!sums.has(r.provider)) {
      sums.set(r.provider, new Array<number>(buckets).fill(0));
      counts.set(r.provider, new Array<number>(buckets).fill(0));
    }
    const s = sums.get(r.provider);
    const c = counts.get(r.provider);
    if (s && c) {
      s[idx] = (s[idx] ?? 0) + metric(r);
      c[idx] = (c[idx] ?? 0) + 1;
    }
  }
  for (const [provider, s] of sums) {
    const c = counts.get(provider) ?? [];
    byProvider.set(
      provider,
      s.map((total, i) => {
        const n = c[i] ?? 0;
        return n > 0 ? total / n : 0;
      }),
    );
  }
  for (let i = 0; i < buckets; i++) {
    const t = new Date(first + width * (i + 1));
    labels.push(t.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }));
  }
  return { labels, byProvider };
}

const CHART_COLORS = ['#60a5fa', '#34d399', '#fbbf24', '#f472b6', '#a78bfa', '#f97316', '#22d3ee', '#e879f9'];

function toSeries(byProvider: Map<string, number[]>) {
  return [...byProvider.entries()].map(([name, values], i) => ({
    name,
    color: CHART_COLORS[i % CHART_COLORS.length] ?? '#60a5fa',
    values,
  }));
}

export function BenchmarkView() {
  const tenant = useTenantParam();
  const [range, setRange] = React.useState<string>('24h');
  const [compare, setCompare] = React.useState<string[]>([]);
  const [showJobForm, setShowJobForm] = React.useState(false);
  const [editing, setEditing] = React.useState<BenchmarkJob | null>(null);

  const status = useBenchmarkStatus();
  const overview = useBenchmarkOverview(tenant);
  const jobs = useBenchmarkJobs(tenant);
  const runs = useBenchmarkRuns({ tenantId: tenant, limit: 20 });
  const history = useBenchmarkHistory({ tenantId: tenant, range, limit: 500 });
  const health = useBenchmarkHealth(tenant, range);
  const comparison = useBenchmarkCompare(tenant, compare, range);
  const insights = useBenchmarkInsights(tenant);
  const reports = useBenchmarkReports(tenant);
  const datasets = useBenchmarkDatasets(tenant);
  const providers = useProviders();
  const models = useModels();
  const triggerRun = useTriggerBenchmarkRun();
  const createJob = useCreateBenchmarkJob();
  const updateJob = useUpdateBenchmarkJob();
  const deleteJob = useDeleteBenchmarkJob();
  const generateReport = useGenerateBenchmarkReport();

  const enabled = status.data?.enabled ?? false;

  const results = React.useMemo(() => history.data ?? [], [history.data]);
  const latency = React.useMemo(() => bucketMetric(results, (r) => r.latency_ms), [results]);
  const ttft = React.useMemo(() => bucketMetric(results, (r) => r.ttft_ms), [results]);
  const tps = React.useMemo(() => bucketMetric(results, (r) => r.tokens_per_second), [results]);
  const cost = React.useMemo(() => bucketMetric(results, (r) => r.cost_usd, 24), [results]);

  const heatmap = React.useMemo(() => {
    const cats = [...new Set(results.map((r) => r.category || 'unknown'))].sort();
    const provs = [...new Set(results.map((r) => r.provider))].sort();
    const avg = new Map<string, number>();
    for (const p of provs) {
      for (const c of cats) {
        const rows = results.filter((r) => r.provider === p && (r.category || 'unknown') === c);
        avg.set(`${p}::${c}`, rows.length > 0 ? rows.reduce((a, r) => a + r.latency_ms, 0) / rows.length : 0);
      }
    }
    return { cats, provs, avg };
  }, [results]);

  function toggleCompare(provider: string) {
    setCompare((prev) => (prev.includes(provider) ? prev.filter((p) => p !== provider) : [...prev, provider]));
  }

  function download(format: string) {
    const params = new URLSearchParams({ range, limit: '1000' });
    if (tenant) params.set('tenant_id', tenant);
    window.open(`/api/gateway/benchmark/history?${params.toString()}&export=${format}`, '_blank');
  }

  if (status.isPending) {
    return (
      <>
        <PageHeader title="Benchmark" description="Continuous provider benchmarking and analytics." />
        <TableSkeleton rows={6} columns={4} />
      </>
    );
  }

  if (status.isError) {
    return (
      <>
        <PageHeader title="Benchmark" description="Continuous provider benchmarking and analytics." />
        <ErrorState error={status.error} onRetry={() => void status.refetch()} />
      </>
    );
  }

  if (!enabled) {
    return (
      <>
        <PageHeader
          title="Benchmark"
          description="Continuous provider benchmarking, historical metrics, reports and a real-time analytics dashboard."
        />
        <BenchmarkEnableCard
          onEnabled={() => {
            void status.refetch();
            void overview.refetch();
          }}
        />
      </>
    );
  }

  const ov = overview.data;

  return (
    <>
      <PageHeader
        title="Benchmark"
        description="Continuous provider benchmarking, historical metrics, reports and live analytics. Results are append-only and workspace scoped."
        actions={
          <>
            <span className="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/25 bg-emerald-500/10 px-2.5 py-1 text-[11px] font-medium text-emerald-400">
              <span className="relative flex size-1.5">
                <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75" />
                <span className="relative inline-flex size-1.5 rounded-full bg-emerald-400" />
              </span>
              Live · 5s
            </span>
            <TenantPicker value={tenant} />
            <select
              value={range}
              onChange={(e) => setRange(e.target.value)}
              aria-label="Date range"
              className="h-8.5 rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
            >
              {RANGES.map((r) => (
                <option key={r.value} value={r.value}>
                  {r.label}
                </option>
              ))}
            </select>
            <Button variant="outline" size="sm" onClick={() => void overview.refetch()} disabled={overview.isFetching}>
              <RefreshCw className={overview.isFetching ? 'animate-spin' : undefined} />
              Refresh
            </Button>
            {!status.data?.config_enabled && (
              <DisableEngineButton
                onDisabled={() => {
                  void status.refetch();
                }}
              />
            )}
          </>
        }
      />

      {/* Overview */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-4">
        <StatCard label="Engine" value={ov?.engine_status ?? '—'} hint={`Scheduler: ${ov?.scheduler_status ?? '—'}`} icon={Activity} />
        <StatCard label="Active jobs" value={formatNumber(ov?.active_jobs)} hint={`${ov?.running_workers ?? 0} workers · queue ${ov?.queue_size ?? 0}`} icon={FlaskConical} />
        <StatCard label="Total benchmarks" value={formatNumber(ov?.total_benchmarks)} hint={`Success ${formatPercent(ov?.success_rate)}`} icon={ArrowDownUp} />
        <StatCard label="Daily cost" value={formatCurrency(ov?.total_daily_cost_usd)} hint={`${formatNumber(ov?.total_tokens_benchmarked)} tokens`} icon={Activity} />
        <StatCard label="Avg duration" value={formatDurationMs(ov?.avg_benchmark_duration_ms)} hint={ov?.last_benchmark ? `Last ${formatRelative(ov.last_benchmark)}` : 'No runs yet'} />
        <StatCard label="Failed jobs" value={formatNumber(ov?.failed_jobs)} hint={ov?.next_benchmark ? `Next ${formatRelative(ov.next_benchmark)}` : 'Nothing scheduled'} tone="warning" />
        <StatCard label="Queue size" value={formatNumber(ov?.queue_size)} hint="Pending runs across replicas" />
        <StatCard label="Workers" value={formatNumber(ov?.running_workers)} hint="Horizontally scalable" />
      </div>

      {/* Charts */}
      <div className="mt-4 grid grid-cols-1 gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Latency</CardTitle>
            <CardDescription>Total response latency per provider over time.</CardDescription>
          </CardHeader>
          <CardContent>
            <LineChart series={toSeries(latency.byProvider)} labels={latency.labels} formatValue={(v) => `${Math.round(v)}ms`} area areaGradient />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Time to first token</CardTitle>
            <CardDescription>Responsiveness per provider over time.</CardDescription>
          </CardHeader>
          <CardContent>
            <LineChart series={toSeries(ttft.byProvider)} labels={ttft.labels} formatValue={(v) => `${Math.round(v)}ms`} area />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Tokens per second</CardTitle>
            <CardDescription>Throughput per provider over time.</CardDescription>
          </CardHeader>
          <CardContent>
            <LineChart series={toSeries(tps.byProvider)} labels={tps.labels} formatValue={(v) => `${v.toFixed(1)} tok/s`} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Cost trend</CardTitle>
            <CardDescription>Benchmark spend per provider: daily, weekly and monthly shape.</CardDescription>
          </CardHeader>
          <CardContent>
            <LineChart series={toSeries(cost.byProvider)} labels={cost.labels} formatValue={(v) => `$${v.toFixed(4)}`} area areaGradient />
          </CardContent>
        </Card>
      </div>

      {/* Distribution + heatmap */}
      <div className="mt-4 grid grid-cols-1 gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Response distribution</CardTitle>
            <CardDescription>Average latency by provider in this window.</CardDescription>
          </CardHeader>
          <CardContent>
            <BarList
              rows={(health.data ?? []).map((h) => ({
                label: h.provider,
                value: h.avg_latency_ms,
                detail: `${Math.round(h.success_rate * 100)}% ok`,
                tone: h.status === 'down' ? 'danger' : h.status === 'healthy' ? 'default' : 'warning',
              }))}
              formatValue={(v) => `${Math.round(v)}ms`}
              emptyMessage="No benchmark data in this window yet."
            />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Latency heatmap</CardTitle>
            <CardDescription>Average latency by provider and prompt category.</CardDescription>
          </CardHeader>
          <CardContent className="overflow-x-auto">
            {heatmap.provs.length === 0 ? (
              <EmptyState title="No data" description="Run a benchmark to fill the heatmap." />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Provider</TableHead>
                    {heatmap.cats.map((c) => (
                      <TableHead key={c} className="text-right">
                        {c}
                      </TableHead>
                    ))}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {heatmap.provs.map((p) => (
                    <TableRow key={p}>
                      <TableCell className="font-medium">{p}</TableCell>
                      {heatmap.cats.map((c) => {
                        const v = heatmap.avg.get(`${p}::${c}`) ?? 0;
                        return (
                          <TableCell key={c} className="text-right tabular-nums">
                            {v > 0 ? `${Math.round(v)}ms` : '—'}
                          </TableCell>
                        );
                      })}
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Provider health */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <HeartPulse className="size-4" /> Provider health
          </CardTitle>
          <CardDescription>Uptime, latency, TTFT, success rate and current status per provider.</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {health.isPending ? (
            <TableSkeleton rows={4} columns={6} />
          ) : (health.data ?? []).length === 0 ? (
            <EmptyState title="No health data" description="Health appears after the first benchmark run." className="m-5" />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Provider</TableHead>
                  <TableHead className="text-right">Uptime</TableHead>
                  <TableHead className="text-right">Avg latency</TableHead>
                  <TableHead className="text-right">Avg TTFT</TableHead>
                  <TableHead className="text-right">Success</TableHead>
                  <TableHead>Last failure</TableHead>
                  <TableHead>Last benchmark</TableHead>
                  <TableHead className="text-right">Probes</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(health.data ?? []).map((h) => (
                  <TableRow key={h.provider}>
                    <TableCell>
                      <span className="inline-flex items-center gap-1.5 text-xs font-medium">
                        <span className={cn('size-2 rounded-full', healthDot(h.status))} />
                        {healthLabel(h.status)}
                      </span>
                    </TableCell>
                    <TableCell className="font-medium">{h.provider}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPercent(h.uptime)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatDurationMs(h.avg_latency_ms)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatDurationMs(h.avg_ttft_ms)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPercent(h.success_rate)}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{h.last_failure ? formatRelative(h.last_failure) : '—'}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{h.last_benchmark ? formatRelative(h.last_benchmark) : '—'}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatNumber(h.total_probes)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Comparison mode */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle>Comparison mode</CardTitle>
          <CardDescription>Select providers for a side-by-side comparison of TTFT, latency, cost, throughput, error and success rates.</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="flex flex-wrap gap-2">
            {(health.data ?? []).map((h) => (
              <label key={h.provider} className="inline-flex cursor-pointer items-center gap-1.5 rounded-full border border-white/[0.08] px-3 py-1 text-xs">
                <input type="checkbox" checked={compare.includes(h.provider)} onChange={() => toggleCompare(h.provider)} className="accent-sky-500" />
                {h.provider}
              </label>
            ))}
          </div>
          {compare.length > 0 && (
            <div className="mt-4 overflow-x-auto">
              <CompareTable rows={comparison.data ?? []} />
            </div>
          )}
        </CardContent>
      </Card>

      {/* AI insights */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Sparkles className="size-4" /> AI insights
          </CardTitle>
          <CardDescription>Automatically generated summaries of regressions, improvements and outages.</CardDescription>
        </CardHeader>
        <CardContent>
          {(insights.data ?? []).length === 0 ? (
            <p className="text-xs text-muted-foreground">No insights yet.</p>
          ) : (
            <ul className="space-y-2">
              {(insights.data ?? []).map((insight, i) => (
                <li key={i} className="rounded-xl border border-white/[0.06] bg-white/[0.02] px-3 py-2 text-xs text-neutral-200">
                  {insight.text}
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      {/* Jobs */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <FlaskConical className="size-4" /> Benchmark jobs
          </CardTitle>
          <CardDescription>Create unlimited jobs with providers, models, datasets, schedules, budgets and tags.</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="mb-3 flex flex-wrap gap-2">
            <Button
              size="sm"
              onClick={() => {
                setEditing(null);
                setShowJobForm((v) => !v);
              }}
            >
              <Plus /> New job
            </Button>
          </div>
          {showJobForm && (
            <JobForm
              initial={editing}
              providers={providers.data ?? []}
              models={models.data ?? []}
              datasets={datasets.data ?? []}
              saving={createJob.isPending || updateJob.isPending}
              onCancel={() => {
                setShowJobForm(false);
                setEditing(null);
              }}
              onSubmit={(input) => {
                if (editing) {
                  updateJob.mutate(
                    { id: editing.id, patch: input },
                    {
                      onSuccess: () => {
                        setShowJobForm(false);
                        setEditing(null);
                      },
                    },
                  );
                } else {
                  createJob.mutate(input, {
                    onSuccess: () => {
                      setShowJobForm(false);
                      setEditing(null);
                    },
                  });
                }
              }}
            />
          )}
          {jobs.isPending ? (
            <TableSkeleton rows={3} columns={5} />
          ) : (jobs.data ?? []).length === 0 ? (
            <EmptyState title="No benchmark jobs" description="Create a job to start continuous benchmarking." />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Schedule</TableHead>
                  <TableHead>Targets</TableHead>
                  <TableHead className="text-right">Budget</TableHead>
                  <TableHead>Next run</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(jobs.data ?? []).map((job) => (
                  <TableRow key={job.id}>
                    <TableCell>
                      <span className="font-medium">{job.name}</span>
                      {!job.enabled && <Badge tone="outline">paused</Badge>}
                      {job.description && <p className="text-[11px] text-muted-foreground">{job.description}</p>}
                    </TableCell>
                    <TableCell className="text-xs">{job.schedule}</TableCell>
                    <TableCell className="max-w-xs truncate text-xs text-muted-foreground">
                      {job.providers.join(', ')} · {job.models.join(', ')}
                    </TableCell>
                    <TableCell className="text-right tabular-nums text-xs">
                      {job.max_daily_budget_usd > 0 ? `$${job.max_daily_budget_usd}/day` : '—'}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {job.next_run_at ? formatRelative(job.next_run_at) : '—'}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="outline"
                          size="sm"
                          title="Run now"
                          onClick={() => void triggerRun.mutate({ job_id: job.id })}
                          disabled={triggerRun.isPending}
                        >
                          <Play />
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          title={job.enabled ? 'Pause' : 'Resume'}
                          onClick={() => void updateJob.mutate({ id: job.id, patch: { enabled: !job.enabled } })}
                        >
                          {job.enabled ? 'Pause' : 'Resume'}
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          title="Edit"
                          onClick={() => {
                            setEditing(job);
                            setShowJobForm(true);
                          }}
                        >
                          Edit
                        </Button>
                        <Button variant="outline" size="sm" title="Delete" onClick={() => void deleteJob.mutate(job.id)}>
                          <Trash2 />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Timeline */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle>Timeline</CardTitle>
          <CardDescription>Recent benchmark runs across all jobs.</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {(runs.data ?? []).length === 0 ? (
            <EmptyState title="No runs yet" description="Runs appear here once jobs execute." className="m-5" />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Run</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Probes</TableHead>
                  <TableHead className="text-right">Cost</TableHead>
                  <TableHead>Started</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(runs.data ?? []).map((run) => (
                  <TableRow key={run.id}>
                    <TableCell className="font-mono text-[11px]">{run.id.slice(0, 8)}</TableCell>
                    <TableCell>
                      <Badge tone={run.status === 'completed' ? 'success' : run.status === 'failed' ? 'danger' : 'outline'}>
                        {run.status}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {run.success_count}/{run.total_probes}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{formatCurrency(run.total_cost_usd)}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateTime(run.started_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Results explorer */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle>Results explorer</CardTitle>
          <CardDescription>Search, filter and sort every stored measurement. History is append-only — nothing is overwritten.</CardDescription>
        </CardHeader>
        <CardContent>
          <ResultsExplorer results={results} loading={history.isPending} onRetry={() => void history.refetch()} error={history.error} />
        </CardContent>
      </Card>

      {/* Reports */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle>Reports</CardTitle>
          <CardDescription>Daily, weekly and monthly rollups with winners, regressions and improvements.</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="mb-3 flex flex-wrap gap-2">
            {(['daily', 'weekly', 'monthly'] as const).map((period) => (
              <Button
                key={period}
                variant="outline"
                size="sm"
                onClick={() => void generateReport.mutate(period)}
                disabled={generateReport.isPending}
              >
                Generate {period}
              </Button>
            ))}
          </div>
          {(reports.data ?? []).length === 0 ? (
            <p className="text-xs text-muted-foreground">No reports yet. Generate one to see fastest, cheapest and most reliable providers.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Period</TableHead>
                  <TableHead>Winner</TableHead>
                  <TableHead>Fastest</TableHead>
                  <TableHead>Cheapest</TableHead>
                  <TableHead>Most reliable</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(reports.data ?? []).map((rep) => (
                  <TableRow key={rep.id}>
                    <TableCell className="font-medium">{rep.period}</TableCell>
                    <TableCell>{rep.overall_winner || '—'}</TableCell>
                    <TableCell>{rep.fastest_provider || '—'}</TableCell>
                    <TableCell>{rep.cheapest_provider || '—'}</TableCell>
                    <TableCell>{rep.most_reliable_provider || '—'}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateTime(rep.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Export */}
      <Card className="mt-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Download className="size-4" /> Export
          </CardTitle>
          <CardDescription>Download history in CSV, JSON, Excel or PDF form.</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="flex flex-wrap gap-2">
            {(['csv', 'json', 'excel', 'pdf'] as const).map((format) => (
              <Button key={format} variant="outline" size="sm" onClick={() => download(format)}>
                {format.toUpperCase()}
              </Button>
            ))}
          </div>
        </CardContent>
      </Card>
    </>
  );
}

function DisableEngineButton({ onDisabled }: { onDisabled: () => void }) {
  const setEnabled = useSetBenchmarkEnabled();
  return (
    <Button
      variant="outline"
      size="sm"
      title="Stop the scheduler and workers"
      onClick={() => setEnabled.mutate(false, { onSuccess: onDisabled })}
      disabled={setEnabled.isPending}
    >
      {setEnabled.isPending ? 'Stopping…' : 'Disable'}
    </Button>
  );
}

/**
 * The disabled-state card with the runtime enable switch.
 *
 * Enabling persists server-side and starts the scheduler and workers
 * immediately — no restart, no config change. Disabling stops them the same
 * way. The switch needs full admin scope.
 */
function BenchmarkEnableCard({ onEnabled }: { onEnabled: () => void }) {
  const setEnabled = useSetBenchmarkEnabled();

  return (
    <Card>
      <CardContent className="py-10">
        <EmptyState
          title="The Benchmark Engine is disabled"
          description="The gateway runs unchanged while the engine is off: no scheduler, no workers, no background spend. Enable it here to start continuous benchmarking immediately — no restart or config change needed."
          action={
            <Button
              size="sm"
              onClick={() => setEnabled.mutate(true, { onSuccess: onEnabled })}
              disabled={setEnabled.isPending}
            >
              <Play />
              {setEnabled.isPending ? 'Enabling…' : 'Enable Benchmark Engine'}
            </Button>
          }
        />
        {setEnabled.isError ? (
          <p className="mx-auto mt-3 max-w-md text-center text-xs text-rose-400">
            {(setEnabled.error as Error)?.message ?? 'Enabling failed. Full admin scope is required.'}
          </p>
        ) : null}
        <p className="mx-auto mt-3 max-w-md text-center text-[11px] text-neutral-500">
          Alternatively set ENABLE_BENCHMARK_ENGINE=true on the gateway. The choice persists across restarts.
        </p>
      </CardContent>
    </Card>
  );
}

function CompareTable({ rows }: { rows: BenchmarkProviderHealth[] }) {  if (rows.length === 0) return <p className="text-xs text-muted-foreground">No data for the selected providers.</p>;
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Provider</TableHead>
          <TableHead className="text-right">TTFT</TableHead>
          <TableHead className="text-right">Latency</TableHead>
          <TableHead className="text-right">Tokens/sec</TableHead>
          <TableHead className="text-right">Streaming</TableHead>
          <TableHead className="text-right">Avg cost</TableHead>
          <TableHead className="text-right">Success</TableHead>
          <TableHead className="text-right">Error rate</TableHead>
          <TableHead className="text-right">Availability</TableHead>
          <TableHead className="text-right">Probes</TableHead>
          <TableHead>Status</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((h) => (
          <TableRow key={h.provider}>
            <TableCell className="font-medium">{h.provider}</TableCell>
            <TableCell className="text-right tabular-nums">{formatDurationMs(h.avg_ttft_ms)}</TableCell>
            <TableCell className="text-right tabular-nums">{formatDurationMs(h.avg_latency_ms)}</TableCell>
            <TableCell className="text-right tabular-nums">{h.avg_tokens_per_second.toFixed(1)}</TableCell>
            <TableCell className="text-right tabular-nums">{h.avg_streaming_tps.toFixed(1)}</TableCell>
            <TableCell className="text-right tabular-nums">{formatCurrency(h.avg_cost_usd)}</TableCell>
            <TableCell className="text-right tabular-nums">{formatPercent(h.success_rate)}</TableCell>
            <TableCell className="text-right tabular-nums">{formatPercent(1 - h.success_rate)}</TableCell>
            <TableCell className="text-right tabular-nums">{formatPercent(h.uptime)}</TableCell>
            <TableCell className="text-right tabular-nums">{formatNumber(h.total_probes)}</TableCell>
            <TableCell>
              <span className="inline-flex items-center gap-1.5 text-xs">
                <span className={cn('size-2 rounded-full', healthDot(h.status))} />
                {healthLabel(h.status)}
              </span>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

/**
 * A multi-select dropdown with search, select-all and clear.
 *
 * Native `<select multiple>` is a usability trap (ctrl-click, invisible
 * selections), so options render as checkboxes in a popup panel instead.
 */
function MultiSelect({
  label,
  options,
  selected,
  onChange,
  placeholder,
  emptyHint,
}: {
  label: string;
  options: Array<{ value: string; hint?: string }>;
  selected: string[];
  onChange: (next: string[]) => void;
  placeholder: string;
  emptyHint?: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState('');
  const ref = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    function onDocClick(event: MouseEvent) {
      if (ref.current && !ref.current.contains(event.target as Node)) setOpen(false);
    }
    document.addEventListener('mousedown', onDocClick);
    return () => document.removeEventListener('mousedown', onDocClick);
  }, []);

  const filtered = options.filter((o) => o.value.toLowerCase().includes(query.trim().toLowerCase()));

  function toggle(value: string) {
    onChange(selected.includes(value) ? selected.filter((v) => v !== value) : [...selected, value]);
  }

  const summary =
    selected.length === 0 ? placeholder : `${selected.length} selected · ${selected.join(', ')}`;

  return (
    <div className="min-w-0 text-xs text-neutral-400">
      {label}
      <div ref={ref} className="relative mt-1">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          className="flex h-9 w-full items-center justify-between gap-2 rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-left text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
        >
          <span className={cn('min-w-0 flex-1 truncate', selected.length === 0 && 'text-neutral-500')}>
            {summary}
          </span>
          <ChevronDown className={cn('size-3.5 shrink-0 text-neutral-500 transition-transform', open && 'rotate-180')} />
        </button>
        {open ? (
          <div className="absolute z-30 mt-1 max-h-56 w-full overflow-auto rounded-xl border border-white/[0.1] bg-[#17171d] p-1.5 shadow-xl">
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Filter…"
              aria-label={`Filter ${label}`}
              className="mb-1 h-8 w-full rounded-lg border border-white/[0.08] bg-[#121217] px-2.5 text-xs text-neutral-200 focus:outline-none focus:ring-1 focus:ring-primary/50"
            />
            <div className="mb-1 flex gap-1 px-1">
              <button
                type="button"
                onClick={() => onChange(filtered.map((o) => o.value))}
                className="rounded-md px-2 py-1 text-[11px] text-neutral-400 hover:bg-white/[0.06] hover:text-white"
              >
                Select all
              </button>
              <button
                type="button"
                onClick={() => onChange([])}
                className="rounded-md px-2 py-1 text-[11px] text-neutral-400 hover:bg-white/[0.06] hover:text-white"
              >
                Clear
              </button>
            </div>
            {filtered.length === 0 ? (
              <p className="px-2 py-3 text-center text-[11px] text-neutral-500">No matches</p>
            ) : (
              filtered.map((o) => (
                <label
                  key={o.value}
                  className="flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 hover:bg-white/[0.05]"
                >
                  <input
                    type="checkbox"
                    checked={selected.includes(o.value)}
                    onChange={() => toggle(o.value)}
                    className="accent-sky-500"
                  />
                  <span className="min-w-0 flex-1 truncate text-neutral-200">{o.value}</span>
                  {o.hint ? <span className="shrink-0 text-[11px] text-neutral-500">{o.hint}</span> : null}
                </label>
              ))
            )}
          </div>
        ) : null}
      </div>
      {options.length === 0 && emptyHint ? (
        <span className="mt-1 block text-[11px] text-neutral-500">{emptyHint}</span>
      ) : null}
    </div>
  );
}

function JobForm({
  initial,
  providers,
  models,
  datasets,
  saving,
  onCancel,
  onSubmit,
}: {
  initial: BenchmarkJob | null;
  providers: Array<{ name: string }>;
  models: Array<{ name: string; provider_name?: string }>;
  datasets: Array<{ id: string; name: string }>;
  saving: boolean;
  onCancel: () => void;
  onSubmit: (input: BenchmarkJobInput) => void;
}) {
  const [name, setName] = React.useState(initial?.name ?? '');
  const [description, setDescription] = React.useState(initial?.description ?? '');
  const [selectedProviders, setSelectedProviders] = React.useState<string[]>(initial?.providers ?? []);
  const [selectedModels, setSelectedModels] = React.useState<string[]>(initial?.models ?? []);
  const [datasetId, setDatasetId] = React.useState(initial?.dataset_id ?? '');
  const [schedule, setSchedule] = React.useState(initial?.schedule ?? 'every_hour');
  const [cron, setCron] = React.useState(initial?.cron_expression ?? '');
  const [concurrency, setConcurrency] = React.useState(String(initial?.concurrency ?? 5));
  const [budget, setBudget] = React.useState(String(initial?.max_daily_budget_usd ?? 0));
  const [timeoutMs, setTimeoutMs] = React.useState(String(initial?.timeout_ms ?? 60000));
  const [formError, setFormError] = React.useState('');

  // Model options follow the selected providers so the list stays relevant;
  // with no provider picked, the whole registry is offered.
  const modelOptions = React.useMemo(
    () =>
      models
        .filter(
          (m) =>
            selectedProviders.length === 0 ||
            !m.provider_name ||
            selectedProviders.includes(m.provider_name),
        )
        .map((m) => ({ value: m.name, hint: m.provider_name })),
    [models, selectedProviders],
  );

  const inputClass =
    'h-9 w-full rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50';

  return (
    <form
      className="mb-4 grid grid-cols-1 gap-3 rounded-2xl border border-white/[0.07] bg-white/[0.02] p-4 sm:grid-cols-2"
      onSubmit={(e) => {
        e.preventDefault();
        if (selectedProviders.length === 0 || selectedModels.length === 0) {
          setFormError('Select at least one provider and one model.');
          return;
        }
        setFormError('');
        onSubmit({
          name: name.trim(),
          description: description.trim() || undefined,
          providers: selectedProviders,
          models: selectedModels,
          dataset_id: datasetId || undefined,
          schedule,
          cron_expression: schedule === 'custom' ? cron : undefined,
          concurrency: Math.max(1, Math.min(100, Number(concurrency) || 5)),
          max_daily_budget_usd: Math.max(0, Number(budget) || 0),
          timeout_ms: Math.max(1000, Number(timeoutMs) || 60000),
        });
      }}
    >
      <label className="text-xs text-neutral-400">
        Name
        <input value={name} onChange={(e) => setName(e.target.value)} required className={cn(inputClass, 'mt-1')} placeholder="Nightly provider sweep" />
      </label>
      <label className="text-xs text-neutral-400">
        Description
        <input value={description} onChange={(e) => setDescription(e.target.value)} className={cn(inputClass, 'mt-1')} placeholder="Optional" />
      </label>
      <MultiSelect
        label="Providers"
        options={providers.map((p) => ({ value: p.name }))}
        selected={selectedProviders}
        onChange={setSelectedProviders}
        placeholder="Select providers…"
        emptyHint="No providers configured yet — add one on the Providers page."
      />
      <MultiSelect
        label="Models"
        options={modelOptions}
        selected={selectedModels}
        onChange={setSelectedModels}
        placeholder="Select models…"
        emptyHint="No models in the registry yet."
      />
      <label className="text-xs text-neutral-400">
        Prompt dataset
        <select value={datasetId} onChange={(e) => setDatasetId(e.target.value)} className={cn(inputClass, 'mt-1')}>
          <option value="">Default corpus</option>
          {datasets.map((d) => (
            <option key={d.id} value={d.id}>
              {d.name}
            </option>
          ))}
        </select>
      </label>
      <label className="text-xs text-neutral-400">
        Benchmark interval
        <select value={schedule} onChange={(e) => setSchedule(e.target.value)} className={cn(inputClass, 'mt-1')}>
          {SCHEDULES.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </select>
      </label>
      {schedule === 'custom' && (
        <label className="text-xs text-neutral-400">
          Custom cron expression
          <input value={cron} onChange={(e) => setCron(e.target.value)} required className={cn(inputClass, 'mt-1')} placeholder="*/15 * * * *" />
        </label>
      )}
      <label className="text-xs text-neutral-400">
        Concurrency (1–100)
        <input value={concurrency} onChange={(e) => setConcurrency(e.target.value)} inputMode="numeric" className={cn(inputClass, 'mt-1')} />
      </label>
      <label className="text-xs text-neutral-400">
        Max daily budget USD (0 = unlimited)
        <input value={budget} onChange={(e) => setBudget(e.target.value)} inputMode="decimal" className={cn(inputClass, 'mt-1')} />
      </label>
      <label className="text-xs text-neutral-400">
        Timeout ms
        <input value={timeoutMs} onChange={(e) => setTimeoutMs(e.target.value)} inputMode="numeric" className={cn(inputClass, 'mt-1')} />
      </label>
      <div className="flex items-end gap-2 sm:col-span-2">
        <Button
          type="submit"
          size="sm"
          disabled={saving || !name.trim() || selectedProviders.length === 0 || selectedModels.length === 0}
        >
          {initial ? 'Save job' : 'Create job'}
        </Button>
        <Button type="button" variant="outline" size="sm" onClick={onCancel}>
          Cancel
        </Button>
      </div>
      {formError ? <p className="text-xs text-rose-400 sm:col-span-2">{formError}</p> : null}
    </form>
  );
}

type ResultSortKey = 'created_at' | 'ttft_ms' | 'latency_ms' | 'tokens_per_second' | 'cost_usd';

function ResultsExplorer({
  results,
  loading,
  onRetry,
  error,
}: {
  results: BenchmarkResult[];
  loading: boolean;
  onRetry: () => void;
  error: unknown;
}) {
  const [search, setSearch] = React.useState('');
  const [provider, setProvider] = React.useState('');
  const [category, setCategory] = React.useState('');
  const [outcome, setOutcome] = React.useState('');
  const [sortKey, setSortKey] = React.useState<ResultSortKey>('created_at');
  const [sortDir, setSortDir] = React.useState<1 | -1>(-1);

  const providers = React.useMemo(() => [...new Set(results.map((r) => r.provider))].sort(), [results]);
  const categories = React.useMemo(
    () => [...new Set(results.map((r) => r.category || 'unknown'))].sort(),
    [results],
  );

  const filtered = React.useMemo(() => {
    const q = search.trim().toLowerCase();
    const rows = results.filter((r) => {
      if (provider && r.provider !== provider) return false;
      if (category && (r.category || 'unknown') !== category) return false;
      if (outcome === 'success' && !r.success) return false;
      if (outcome === 'failure' && r.success) return false;
      if (q && ![r.provider, r.model, r.category ?? '', r.error ?? ''].join(' ').toLowerCase().includes(q)) return false;
      return true;
    });
    const valueOf = (r: BenchmarkResult): number => {
      switch (sortKey) {
        case 'ttft_ms':
          return r.ttft_ms;
        case 'latency_ms':
          return r.latency_ms;
        case 'tokens_per_second':
          return r.tokens_per_second;
        case 'cost_usd':
          return r.cost_usd;
        default:
          return new Date(r.created_at).getTime();
      }
    };
    return [...rows].sort((a, b) => (valueOf(a) - valueOf(b)) * sortDir);
  }, [results, search, provider, category, outcome, sortKey, sortDir]);

  function sortHeader(label: string, key: ResultSortKey) {
    return (
      <button
        type="button"
        onClick={() => {
          if (sortKey === key) setSortDir((d) => (d === 1 ? -1 : 1));
          else {
            setSortKey(key);
            setSortDir(-1);
          }
        }}
        className="inline-flex items-center gap-1 hover:text-foreground"
        title={`Sort by ${label}`}
      >
        {label} {sortKey === key ? (sortDir === 1 ? '↑' : '↓') : ''}
      </button>
    );
  }

  const selectClass =
    'h-8.5 rounded-xl border border-white/[0.08] bg-[#121217] px-2.5 text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50';

  if (loading) return <TableSkeleton rows={5} columns={6} />;
  if (error) return <ErrorState error={error} onRetry={onRetry} />;

  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search provider, model, category…"
          aria-label="Search results"
          className="h-8.5 min-w-0 flex-1 rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50 sm:max-w-xs"
        />
        <select value={provider} onChange={(e) => setProvider(e.target.value)} aria-label="Filter by provider" className={selectClass}>
          <option value="">All providers</option>
          {providers.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        <select value={category} onChange={(e) => setCategory(e.target.value)} aria-label="Filter by category" className={selectClass}>
          <option value="">All categories</option>
          {categories.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
        <select value={outcome} onChange={(e) => setOutcome(e.target.value)} aria-label="Filter by outcome" className={selectClass}>
          <option value="">All outcomes</option>
          <option value="success">Success</option>
          <option value="failure">Failure</option>
        </select>
        <span className="text-[11px] tabular-nums text-neutral-500">{filtered.length} rows</span>
      </div>
      {filtered.length === 0 ? (
        <EmptyState title="No matching results" description="Adjust the search or filters." />
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Provider / model</TableHead>
                <TableHead className="text-right">{sortHeader('TTFT', 'ttft_ms')}</TableHead>
                <TableHead className="text-right">{sortHeader('Latency', 'latency_ms')}</TableHead>
                <TableHead className="text-right">{sortHeader('Tok/s', 'tokens_per_second')}</TableHead>
                <TableHead className="text-right">{sortHeader('Cost', 'cost_usd')}</TableHead>
                <TableHead>Outcome</TableHead>
                <TableHead className="text-right">{sortHeader('Time', 'created_at')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.slice(0, 50).map((r) => (
                <TableRow key={r.id}>
                  <TableCell>
                    <span className="text-xs font-medium">{r.provider}</span>
                    <p className="text-[11px] text-muted-foreground">{r.model} · {r.category || 'unknown'}</p>
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{formatDurationMs(r.ttft_ms)}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatDurationMs(r.latency_ms)}</TableCell>
                  <TableCell className="text-right tabular-nums">{r.tokens_per_second.toFixed(1)}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatCurrency(r.cost_usd)}</TableCell>
                  <TableCell>
                    <Badge tone={r.success ? 'success' : 'danger'}>{r.success ? 'ok' : 'fail'}</Badge>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-right text-[11px] text-muted-foreground">
                    {formatDateTime(r.created_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}