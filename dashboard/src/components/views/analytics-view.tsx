'use client';

import {
  Activity,
  AlertTriangle,
  ChartColumn,
  Clock,
  Coins,
  DatabaseZap,
  Layers,
  Route,
  ShieldAlert,
  Wrench,
} from 'lucide-react';
import { usePathname, useSearchParams } from 'next/navigation';
import * as React from 'react';

import { BarList } from '@/components/charts/bar-list';
import { LineChart } from '@/components/charts/time-series';
import { ChartDebug } from '@/components/chart-debug';
import { PageHeader } from '@/components/page-header';
import { RangePicker, useWindowParam, windowToFromParam } from '@/components/range-picker';
import { StatCard } from '@/components/stat-card';
import { TenantPicker, useTenantParam } from '@/components/tenant-picker';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { NumberTicker } from '@/components/ui/number-ticker';
import { ChartSkeleton, CardsSkeleton, EmptyState, ErrorState, TableSkeleton } from '@/components/ui/state';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { WidgetBoard } from '@/components/views/widget-board';
import { RequestRhythmCard } from '@/components/views/request-rhythm-card';
import {
  useAnalyticsReport,
  useCacheStats,
  useModels,
  useProviders,
  useTenants,
  useToolInvocations,
} from '@/hooks/use-admin';
import { bucketSeries, latencySplit, outcomeSplit } from '@/lib/charts';
import {
  formatCompact,
  formatCurrency,
  formatDurationMs,
  formatNumber,
  formatPercent,
} from '@/lib/format';
import { bucketLabels, deltaRatio, successRate } from '@/lib/metrics';
import type { DimensionRow, MetricDelta, TimeBucket } from '@/lib/types';

/** Ranking table sort keys the API accepts. */
const SORTS = [
  { value: 'requests', label: 'Requests' },
  { value: 'cost', label: 'Cost' },
  { value: 'errors', label: 'Errors' },
  { value: 'latency', label: 'Latency' },
  { value: 'tokens', label: 'Tokens' },
] as const;

/**
 * A select that writes one query parameter, preserving the rest.
 *
 * Filter state lives in the URL for the same reason the range does: an operator
 * pasting an incident link must land on the same view, and a refresh must not
 * silently reset the filters.
 */
function UrlFilterSelect({
  param,
  value,
  options,
  label,
  allLabel,
}: {
  param: string;
  value: string;
  options: Array<{ value: string; label: string }>;
  label: string;
  allLabel: string;
}) {
  const pathname = usePathname();
  const params = useSearchParams();

  const hrefFor = (next: string): string => {
    const search = new URLSearchParams(params.toString());
    if (next) search.set(param, next);
    else search.delete(param);
    return `${pathname}?${search.toString()}`;
  };

  return (
    <label className="inline-flex items-center gap-2 text-xs text-neutral-400">
      <span className="sr-only">{label}</span>
      <select
        value={value}
        onChange={(event) => window.location.assign(hrefFor(event.target.value))}
        className="h-8.5 max-w-[180px] rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50"
      >
        <option value="">{allLabel}</option>
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  );
}

interface Column<T> {
  header: string;
  cell: (row: T) => React.ReactNode;
  /** Right-align numeric columns so figures line up. */
  numeric?: boolean;
}

/**
 * A compact ranked table.
 *
 * Tables, not charts, for the per-entity breakdowns: operators compare several
 * numbers across rows, and a table is the only form that lets them read down a
 * column. Charts are reserved for the things that have a shape over time.
 */
function RankingTable<T>({
  rows,
  columns,
  rowKey,
  empty,
}: {
  rows: T[];
  columns: Array<Column<T>>;
  rowKey: (row: T) => string;
  empty: string;
}) {
  if (rows.length === 0) {
    return <p className="py-6 text-center text-xs text-muted-foreground">{empty}</p>;
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          {columns.map((column) => (
            <TableHead key={column.header} className={column.numeric ? 'text-right' : undefined}>
              {column.header}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={rowKey(row)}>
            {columns.map((column) => (
              <TableCell
                key={column.header}
                className={column.numeric ? 'text-right tabular-nums' : undefined}
              >
                {column.cell(row)}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function EntityCell({ name, sub }: { name: string; sub?: string }) {
  return (
    <div className="min-w-0">
      <p className="truncate text-[13px] font-medium" title={name}>
        {name || 'unattributed'}
      </p>
      {sub ? <p className="truncate font-mono text-[10px] text-muted-foreground">{sub}</p> : null}
    </div>
  );
}

/** Map metric name -> delta, for the KPI cards. */
function useDeltaIndex(comparison: MetricDelta[] | null | undefined) {
  return React.useMemo(() => {
    const map = new Map<string, MetricDelta>();
    for (const delta of comparison ?? []) map.set(delta.metric, delta);
    return map;
  }, [comparison]);
}

function kpiDelta(map: Map<string, MetricDelta>, metric: string) {
  const delta = map.get(metric);
  if (!delta) return undefined;
  return {
    // A ratio against an empty previous window is not a statement we can make,
    // so it renders as no delta rather than as a fabricated percentage.
    ratio: deltaRatio(delta.current, delta.previous),
    label: 'prev',
    higherIsWorse: delta.higher_is_worse,
  };
}

export function AnalyticsView() {
  const window = useWindowParam();
  const tenantId = useTenantParam();
  const searchParams = useSearchParams();
  const provider = searchParams.get('provider') ?? '';
  const model = searchParams.get('model') ?? '';
  const [sort, setSort] = React.useState<string>('requests');
  const from = windowToFromParam(window);

  const { data, isPending, isError, error, refetch, isFetching } = useAnalyticsReport({
    tenantId,
    from,
    provider: provider || undefined,
    model: model || undefined,
    top: 10,
    sort,
  });
  const { data: providerList } = useProviders();
  const { data: modelList } = useModels();
  const { data: tenants } = useTenants();
  const { data: cache } = useCacheStats(tenantId);
  const { data: toolHistory } = useToolInvocations(200);

  const deltas = useDeltaIndex(data?.comparison);
  const summary = data?.summary;
  const buckets = React.useMemo<TimeBucket[]>(() => data?.buckets ?? [], [data?.buckets]);
  const labels = React.useMemo(() => bucketLabels(buckets), [buckets]);
  const latencies = React.useMemo(() => latencySplit(buckets), [buckets]);
  const outcomes = React.useMemo(() => outcomeSplit(buckets), [buckets]);
  const requestsSeries = React.useMemo(() => bucketSeries(buckets, 'requests').values, [buckets]);
  const costSeries = React.useMemo(() => bucketSeries(buckets, 'cost_usd').values, [buckets]);
  const errorSeries = React.useMemo(() => bucketSeries(buckets, 'errors').values, [buckets]);
  const seriesTotal = requestsSeries.reduce((total, value) => total + value, 0);

  const tenantNames = React.useMemo(() => {
    const map = new Map<string, string>();
    for (const tenant of tenants ?? []) map.set(tenant.id, tenant.name);
    return map;
  }, [tenants]);

  const cacheReport = data?.cache;
  const routing = data?.routing;

  const toolStats = React.useMemo(() => {
    const invocations = toolHistory?.invocations ?? [];
    let failed = 0;
    let latency = 0;
    const byTool = new Map<string, number>();
    for (const invocation of invocations) {
      // Tool invocations are executed, denied, failed, skipped or superseded;
      // only the first of those is a success.
      if (invocation.status !== 'executed') failed += 1;
      latency += invocation.latency_ms;
      byTool.set(invocation.tool_name, (byTool.get(invocation.tool_name) ?? 0) + 1);
    }
    const top = [...byTool.entries()]
      .sort((a, b) => b[1] - a[1])
      .slice(0, 8)
      .map(([label, value]) => ({ label, value }));
    return {
      sampled: invocations.length,
      total: toolHistory?.total ?? 0,
      failed,
      avgLatency: invocations.length > 0 ? latency / invocations.length : 0,
      top,
    };
  }, [toolHistory]);

  const dimensionColumns: Array<Column<DimensionRow>> = [
    {
      header: 'Entity',
      cell: (row) => <EntityCell name={row.key} />,
    },
    { header: 'Requests', numeric: true, cell: (row) => formatNumber(row.requests) },
    { header: 'Success', numeric: true, cell: (row) => formatPercent(row.success_rate) },
    {
      header: 'Errors',
      numeric: true,
      cell: (row) =>
        row.errors > 0 ? (
          <span className="text-danger">{formatPercent(row.error_rate)}</span>
        ) : (
          <span className="text-muted-foreground">—</span>
        ),
    },
    {
      header: 'Fallbacks',
      numeric: true,
      cell: (row) =>
        row.fallbacks > 0 ? (
          <span className="text-warning">{formatPercent(row.fallback_rate)}</span>
        ) : (
          <span className="text-muted-foreground">—</span>
        ),
    },
    { header: 'p95', numeric: true, cell: (row) => formatDurationMs(row.latency_p95_ms) },
    { header: 'Cost', numeric: true, cell: (row) => formatCurrency(row.cost_usd) },
    {
      header: '$ / success',
      numeric: true,
      cell: (row) => formatCurrency(row.cost_per_success_usd),
    },
  ];

  const tenantColumns: Array<Column<DimensionRow>> = [
    {
      header: 'Tenant',
      cell: (row) => <EntityCell name={tenantNames.get(row.key) ?? row.key} sub={row.key} />,
    },
    { header: 'Requests', numeric: true, cell: (row) => formatNumber(row.requests) },
    { header: 'Cost', numeric: true, cell: (row) => formatCurrency(row.cost_usd) },
    { header: 'p95', numeric: true, cell: (row) => formatDurationMs(row.latency_p95_ms) },
    { header: 'Cache hit', numeric: true, cell: (row) => formatPercent(row.cache_hit_rate) },
    {
      header: 'Errors',
      numeric: true,
      cell: (row) =>
        row.errors > 0 ? (
          <span className="text-danger">{formatNumber(row.errors)}</span>
        ) : (
          <span className="text-muted-foreground">—</span>
        ),
    },
  ];

  const activeFilters = [
    tenantId ? { key: 'tenant', label: tenantNames.get(tenantId) ?? 'Tenant' } : null,
    provider ? { key: 'provider', label: provider } : null,
    model ? { key: 'model', label: model } : null,
  ].filter((item): item is { key: string; label: string } => item !== null);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Analytics"
        description="Traffic, latency, cost, cache, routing and failure analytics over one window. Every figure is aggregated from the usage store for the range and filters below."
        actions={
          <>
            <TenantPicker value={tenantId} />
            <UrlFilterSelect
              param="provider"
              value={provider}
              label="Provider"
              allLabel="All providers"
              options={(providerList ?? []).map((item) => ({ value: item.name, label: item.name }))}
            />
            <UrlFilterSelect
              param="model"
              value={model}
              label="Model"
              allLabel="All models"
              options={(modelList ?? []).map((item) => ({ value: item.name, label: item.name }))}
            />
            <RangePicker value={window} />
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
              {isFetching ? 'Refreshing…' : 'Refresh'}
            </Button>
          </>
        }
      />

      {/* Active filters, so a narrowed view is never mistaken for a quiet window. */}
      {activeFilters.length > 0 ? (
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <span>Filtered by</span>
          {activeFilters.map((filter) => (
            <span
              key={filter.key}
              className="inline-flex items-center gap-1.5 rounded-lg border border-white/[0.08] bg-white/[0.03] px-2 py-0.5 text-[11px] text-neutral-300"
            >
              {filter.key}: <span className="font-medium text-white">{filter.label}</span>
            </span>
          ))}
        </div>
      ) : null}

      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}

      <ChartDebug
        window={window}
        tenantId={tenantId}
        buckets={buckets}
        summaryRequests={summary?.requests}
        seriesTotal={seriesTotal}
        warnings={[]}
      />

      {/* KPI row. Deltas are against the immediately preceding window. */}
      {isPending && !summary ? (
        <CardsSkeleton count={6} />
      ) : summary ? (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-6">
          <StatCard
            label="Requests"
            icon={Activity}
            value={
              Number.isFinite(summary.requests) ? (
                // The headline count is the one figure a change in range moves
                // most visibly, so it is the one worth animating. The others are
                // rates, durations and currency, where a counting spring would
                // fight the formatting rather than help.
                <NumberTicker value={summary.requests} />
              ) : (
                formatNumber(summary.requests)
              )
            }
            delta={kpiDelta(deltas, 'requests')}
            hint={`${formatNumber(summary.successes)} succeeded`}
          />
          <StatCard
            label="Success rate"
            icon={ChartColumn}
            value={formatPercent(successRate(summary))}
            delta={kpiDelta(deltas, 'success_rate')}
            tone={successRate(summary) >= 0.95 ? 'success' : 'warning'}
            hint={`${formatNumber(summary.rejections)} rejected`}
          />
          <StatCard
            label="p95 latency"
            icon={Clock}
            value={formatDurationMs(summary.latency_p95_ms)}
            delta={kpiDelta(deltas, 'p95_latency_ms')}
            hint={`p50 ${formatDurationMs(summary.latency_p50_ms)} · p99 ${formatDurationMs(summary.latency_p99_ms)}`}
          />
          <StatCard
            label="Estimated cost"
            icon={Coins}
            value={formatCurrency(summary.total_cost_usd)}
            delta={kpiDelta(deltas, 'total_cost_usd')}
            hint={`${formatCurrency(summary.avg_cost_usd)} per request`}
          />
          <StatCard
            label="Cache hit rate"
            icon={DatabaseZap}
            value={cacheReport ? formatPercent(cacheReport.hit_rate) : '—'}
            tone={cacheReport && cacheReport.hit_rate >= 0.2 ? 'success' : 'default'}
            hint={
              cacheReport
                ? `${formatNumber(cacheReport.hits)} hits · ${formatNumber(cacheReport.misses)} misses`
                : 'response cache telemetry unavailable'
            }
          />
          <StatCard
            label="Fallback rate"
            icon={Route}
            value={formatPercent(summary.fallback_rate)}
            delta={kpiDelta(deltas, 'fallback_rate')}
            tone={summary.fallback_rate > 0.1 ? 'warning' : 'default'}
            hint={`${formatNumber(summary.fallbacks)} requests reassigned`}
          />
        </div>
      ) : null}

      {/* Traffic */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base">Traffic volume</CardTitle>
          <CardDescription>
            Requests per {data?.interval ?? 'bucket'} across the selected range. Times in UTC.
          </CardDescription>
        </CardHeader>
        <CardContent className="pt-2">
          {isPending ? (
            <ChartSkeleton height={260} />
          ) : buckets.length === 0 ? (
            <EmptyState
              title="No requests in this window"
              description="Traffic appears here as soon as the gateway serves a request for the current filters."
            />
          ) : (
            <LineChart
              labels={labels}
              height={260}
              formatValue={(value) => formatCompact(Math.round(value))}
              area
              areaGradient
              series={[
                { name: 'Requests', color: 'hsl(222 100% 68%)', values: requestsSeries },
                { name: 'Errors', color: 'hsl(var(--danger))', values: errorSeries },
              ]}
            />
          )}
        </CardContent>
      </Card>

      {/* Daily rhythm: streaks and quiet days as a calendar. */}
      <RequestRhythmCard tenantId={tenantId} />

      {/* Latency + outcomes */}
      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-semibold">
              <Clock className="size-4 text-muted-foreground" />
              Latency percentiles
            </CardTitle>
            <CardDescription>End-to-end response time, p50 through p99.</CardDescription>
          </CardHeader>
          <CardContent>
            {isPending ? (
              <ChartSkeleton />
            ) : buckets.length === 0 ? (
              <EmptyState title="No latency samples" />
            ) : (
              <LineChart
                labels={labels}
                formatValue={(value) => formatDurationMs(value)}
                series={[
                  { name: 'p50', color: 'hsl(var(--success))', values: latencies.p50 },
                  { name: 'p90', color: 'hsl(var(--info))', values: latencies.p90 },
                  { name: 'p95', color: 'hsl(var(--warning))', values: latencies.p95 },
                  { name: 'p99', color: 'hsl(var(--danger))', values: latencies.p99 },
                ]}
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-semibold">
              <AlertTriangle className="size-4 text-muted-foreground" />
              Outcomes over time
            </CardTitle>
            <CardDescription>
              Successes, automatic fallbacks and client-facing errors.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {isPending ? (
              <ChartSkeleton />
            ) : buckets.length === 0 ? (
              <EmptyState title="No outcome data" />
            ) : (
              <LineChart
                labels={labels}
                formatValue={(value) => formatCompact(value)}
                series={[
                  { name: 'Success', color: 'hsl(var(--success))', values: outcomes.successes },
                  { name: 'Fallback', color: 'hsl(var(--warning))', values: outcomes.fallbacks },
                  { name: 'Errors', color: 'hsl(var(--danger))', values: outcomes.errors },
                ]}
              />
            )}
          </CardContent>
        </Card>
      </div>

      {/* Cost */}
      <div className="grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-semibold">
              <Coins className="size-4 text-muted-foreground" />
              Cost over time
            </CardTitle>
            <CardDescription>
              Estimated spend per bucket from registry pricing — the same numbers budgets enforce.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {isPending ? (
              <ChartSkeleton />
            ) : buckets.length === 0 ? (
              <EmptyState title="No spend in this window" />
            ) : (
              <LineChart
                labels={labels}
                area
                areaGradient
                formatValue={(value) => formatCurrency(value)}
                series={[{ name: 'Cost', color: 'hsl(160 84% 45%)', values: costSeries }]}
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-semibold">Top cost drivers</CardTitle>
            <CardDescription>Models ranked by estimated spend.</CardDescription>
          </CardHeader>
          <CardContent>
            {isPending ? (
              <TableSkeleton rows={5} columns={1} />
            ) : (
              <BarList
                rows={[...(data?.models ?? [])]
                  .sort((a, b) => b.cost_usd - a.cost_usd)
                  .slice(0, 6)
                  .map((row) => ({
                    label: row.key,
                    value: row.cost_usd,
                    detail: `${formatNumber(row.requests)} req`,
                  }))}
                formatValue={(value) => formatCurrency(value)}
                emptyMessage="No model spend recorded"
              />
            )}
          </CardContent>
        </Card>
      </div>

      {/* Cache */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <DatabaseZap className="size-4 text-muted-foreground" />
            Cache effectiveness
          </CardTitle>
          <CardDescription>
            Whether caching is actually helping. Savings are estimated from the provider-served
            population, since a cache hit has no counterfactual to measure.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {cacheReport && !cacheReport.active ? (
            <EmptyState
              title="No cache activity in this window"
              description="Either the response cache is disabled or nothing in this range was eligible for it."
            />
          ) : (
            <>
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
                {[
                  { label: 'Hit rate', value: formatPercent(cacheReport?.hit_rate ?? 0) },
                  { label: 'Hits', value: formatNumber(cacheReport?.hits ?? 0) },
                  { label: 'Misses', value: formatNumber(cacheReport?.misses ?? 0) },
                  {
                    label: 'Latency saved',
                    value: formatDurationMs(cacheReport?.latency_saved_total_ms ?? 0),
                  },
                  { label: 'Cost saved', value: formatCurrency(cacheReport?.cost_saved_usd ?? 0) },
                  {
                    label: 'Prompt cache share',
                    value: formatPercent(cacheReport?.prompt_cache_share ?? 0),
                  },
                ].map((item) => (
                  <div
                    key={item.label}
                    className="rounded-xl border border-white/[0.07] bg-white/[0.02] p-3 text-center"
                  >
                    <p className="text-[10px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">
                      {item.label}
                    </p>
                    <p className="mt-1 text-base font-semibold tabular-nums text-foreground">
                      {item.value}
                    </p>
                  </div>
                ))}
              </div>
              <div className="grid gap-4 lg:grid-cols-2">
                <div>
                  <p className="mb-2 text-[11px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">
                    Hits by tier
                  </p>
                  <BarList
                    rows={[
                      { label: 'Exact', value: cache?.stats.exact_hits ?? 0 },
                      { label: 'Prefix', value: cache?.stats.prefix_hits ?? 0 },
                      { label: 'Semantic', value: cache?.stats.semantic_hits ?? 0 },
                    ]}
                    formatValue={(value) => formatNumber(value)}
                    emptyMessage="No tier-level cache telemetry"
                  />
                </div>
                <div>
                  <p className="mb-2 text-[11px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">
                    Bypass reasons
                  </p>
                  <BarList
                    rows={Object.entries(cache?.stats.bypass_by_reason ?? {}).map(([label, value]) => ({
                      label,
                      value,
                      tone: 'warning' as const,
                    }))}
                    formatValue={(value) => formatNumber(value)}
                    emptyMessage="No bypasses recorded"
                  />
                </div>
              </div>
            </>
          )}
        </CardContent>
      </Card>

      {/* Provider + model rankings, with a shared sort control. */}
      <div className="flex items-center justify-end gap-2 text-xs text-muted-foreground">
        <span>Rank by</span>
        <div className="inline-flex items-center rounded-xl border border-white/[0.08] bg-white/[0.02] p-1">
          {SORTS.map((option) => (
            <button
              key={option.value}
              type="button"
              onClick={() => setSort(option.value)}
              aria-pressed={sort === option.value}
              className={
                sort === option.value
                  ? 'rounded-lg bg-white/[0.1] px-2.5 py-1 font-medium text-white'
                  : 'rounded-lg px-2.5 py-1 text-neutral-400 transition-colors hover:text-white'
              }
            >
              {option.label}
            </button>
          ))}
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <Layers className="size-4 text-muted-foreground" />
            Provider performance
          </CardTitle>
          <CardDescription>
            Success, error and fallback rates with tail latency and cost per successful request.
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {isPending ? (
            <TableSkeleton rows={5} columns={7} />
          ) : (
            <RankingTable
              rows={data?.providers ?? []}
              columns={dimensionColumns}
              rowKey={(row) => row.key}
              empty="No provider traffic in this window"
            />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm font-semibold">
            <Layers className="size-4 text-muted-foreground" />
            Model performance
          </CardTitle>
          <CardDescription>
            Which models are worth keeping active: volume, failures, tail latency and cost.
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {isPending ? (
            <TableSkeleton rows={5} columns={7} />
          ) : (
            <RankingTable
              rows={data?.models ?? []}
              columns={dimensionColumns}
              rowKey={(row) => row.key}
              empty="No model traffic in this window"
            />
          )}
        </CardContent>
      </Card>

      {/* Tenants */}
      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-semibold">Tenant activity</CardTitle>
          <CardDescription>
            Traffic, spend, tail latency and cache effectiveness per tenant — the view billing and
            isolation questions are answered from.
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {isPending ? (
            <TableSkeleton rows={5} columns={6} />
          ) : (
            <RankingTable
              rows={data?.tenants ?? []}
              columns={tenantColumns}
              rowKey={(row) => row.key}
              empty="No tenant traffic in this window"
            />
          )}
        </CardContent>
      </Card>

      {/* Routing */}
      <div className="grid gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-semibold">
              <Route className="size-4 text-muted-foreground" />
              Routing behaviour
            </CardTitle>
            <CardDescription>
              Which strategy served the traffic, and how often the router left its first choice.
            </CardDescription>
          </CardHeader>
          <CardContent className="p-0">
            {isPending ? (
              <TableSkeleton rows={4} columns={4} />
            ) : (routing?.strategies ?? []).length === 0 ? (
              <EmptyState
                title="No routing decisions recorded"
                description="Strategy attribution comes from the request log; it is empty when logging is disabled."
                className="m-5"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Strategy</TableHead>
                    <TableHead className="text-right">Requests</TableHead>
                    <TableHead className="text-right">Fallbacks</TableHead>
                    <TableHead className="text-right">Avg attempts</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(routing?.strategies ?? []).map((row) => (
                    <TableRow key={row.strategy}>
                      <TableCell>
                        <span className="font-mono text-xs">{row.strategy}</span>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {formatNumber(row.requests)}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {row.fallbacks > 0 ? (
                          <span className="text-warning">
                            {formatNumber(row.fallbacks)} · {formatPercent(row.fallback_rate)}
                          </span>
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {row.avg_attempts.toFixed(2)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-semibold">Routing summary</CardTitle>
            <CardDescription>Across every strategy in the window.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3 text-xs">
            {[
              {
                label: 'Provider switch rate',
                value: formatPercent(routing?.provider_switch_rate ?? 0),
                hint: 'requests that left the primary choice',
              },
              {
                label: 'Average attempts',
                value: (routing?.avg_attempts ?? 0).toFixed(2),
                hint: 'provider calls per request',
              },
              {
                label: 'Fallbacks',
                value: formatNumber(routing?.fallbacks ?? 0),
                hint: 'of ' + formatNumber(routing?.requests ?? 0) + ' decisions',
              },
              {
                label: 'Routing errors',
                value: formatNumber(routing?.errors ?? 0),
                hint: 'requests that ended in an error',
              },
            ].map((item) => (
              <div
                key={item.label}
                className="flex items-baseline justify-between gap-3 rounded-xl border border-white/[0.07] bg-white/[0.02] px-3 py-2"
              >
                <span className="text-muted-foreground">{item.label}</span>
                <span className="text-right">
                  <span className="block font-semibold tabular-nums text-foreground">{item.value}</span>
                  <span className="block text-[10px] text-muted-foreground">{item.hint}</span>
                </span>
              </div>
            ))}
          </CardContent>
        </Card>
      </div>

      {/* Failures */}
      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-semibold">
              <ShieldAlert className="size-4 text-muted-foreground" />
              Failures by error code
            </CardTitle>
            <CardDescription>
              Normalized error codes, ranked by volume — the starting point for triage.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {isPending ? (
              <TableSkeleton rows={5} columns={1} />
            ) : (
              <BarList
                rows={(data?.errors ?? [])
                  .filter((row) => row.key)
                  .map((row) => ({
                    label: row.key,
                    value: row.requests,
                    detail: formatDurationMs(row.latency_p95_ms),
                    tone:
                      row.error_rate > 0.5 ? ('danger' as const) : ('warning' as const),
                  }))}
                formatValue={(value) => formatNumber(value)}
                emptyMessage="No failures in this window"
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-semibold">Outcome mix</CardTitle>
            <CardDescription>Every request in the window, by how it ended.</CardDescription>
          </CardHeader>
          <CardContent>
            {isPending ? (
              <TableSkeleton rows={5} columns={1} />
            ) : (
              <BarList
                rows={(data?.outcomes ?? []).map((row) => ({
                  label: row.key || 'unknown',
                  value: row.requests,
                  detail: formatPercent(row.requests > 0 ? row.requests / (summary?.requests || 1) : 0),
                  tone:
                    row.key === 'error' || row.key === 'rejected'
                      ? ('danger' as const)
                      : row.key === 'fallback'
                        ? ('warning' as const)
                        : ('default' as const),
                }))}
                formatValue={(value) => formatNumber(value)}
                emptyMessage="No outcome data"
              />
            )}
          </CardContent>
        </Card>
      </div>

      {/* Request types + policies */}
      <div className="grid gap-4 xl:grid-cols-3">
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-semibold">Traffic by request type</CardTitle>
            <CardDescription>Chat, tool use, structured output and the rest.</CardDescription>
          </CardHeader>
          <CardContent>
            <BarList
              rows={(data?.request_types ?? [])
                .filter((row) => row.key)
                .map((row) => ({
                  label: row.key,
                  value: row.requests,
                  detail: formatCurrency(row.cost_usd),
                }))}
              formatValue={(value) => formatNumber(value)}
              emptyMessage="No request types recorded"
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-semibold">Policy impact</CardTitle>
            <CardDescription>
              Volume, fallback rate and spend per routing policy.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-2">
            {(data?.policies ?? []).length === 0 ? (
              <p className="py-6 text-center text-xs text-muted-foreground">
                No policy attribution in this window
              </p>
            ) : (
              (data?.policies ?? []).map((row) => (
                <div
                  key={row.key}
                  className="flex items-center justify-between gap-3 rounded-xl border border-white/[0.07] bg-white/[0.02] px-3 py-2 text-xs"
                >
                  <span className="truncate font-mono text-[11px] text-neutral-300" title={row.key}>
                    {row.key || 'unattributed'}
                  </span>
                  <span className="shrink-0 text-right text-muted-foreground">
                    <span className="text-foreground">{formatNumber(row.requests)}</span> req ·{' '}
                    {formatPercent(row.fallback_rate)} fb · {formatCurrency(row.cost_usd)}
                  </span>
                </div>
              ))
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm font-semibold">
              <Wrench className="size-4 text-muted-foreground" />
              Tool usage
            </CardTitle>
            <CardDescription>
              Tool-call volume and reliability. Counts come from the tool-invocation log.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-3 gap-3 text-center">
              {[
                { label: 'Calls (all time)', value: formatNumber(toolStats.total) },
                { label: 'Failures (recent)', value: formatNumber(toolStats.failed) },
                { label: 'Avg latency', value: formatDurationMs(toolStats.avgLatency) },
              ].map((item) => (
                <div key={item.label} className="rounded-xl border border-white/[0.07] bg-white/[0.02] p-2.5">
                  <p className="text-[10px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">
                    {item.label}
                  </p>
                  <p className="mt-1 text-sm font-semibold tabular-nums text-foreground">
                    {item.value}
                  </p>
                </div>
              ))}
            </div>
            <BarList
              rows={toolStats.top}
              formatValue={(value) => formatNumber(value)}
              emptyMessage="No tool invocations recorded"
            />
          </CardContent>
        </Card>
      </div>

      {isFetching && !isPending ? (
        <p className="text-center text-xs text-muted-foreground">
          Refreshing analytics · last generated{' '}
          {data?.generated_at ? new Date(data.generated_at).toLocaleTimeString() : '—'}
        </p>
      ) : null}

      <div className="mt-6">
        <WidgetBoard />
      </div>
    </div>
  );
}
