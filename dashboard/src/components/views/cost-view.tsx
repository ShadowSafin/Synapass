'use client';

import {
  AlertTriangle,
  BadgeDollarSign,
  Coins,
  Download,
  PiggyBank,
  Scale,
  Siren,
  TrendingUp,
  Wallet,
} from 'lucide-react';
import { usePathname, useSearchParams } from 'next/navigation';
import * as React from 'react';

import { BarList } from '@/components/charts/bar-list';
import { LineChart } from '@/components/charts/time-series';
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
import {
  useCostAnomalies,
  useCostBudgets,
  useCostForecast,
  useCostGrouped,
  useCostOverview,
  useCostRequest,
  useCostSavings,
  useCostSeries,
  useCostTop,
  useCreatePricing,
  useModels,
  usePricing,
  useProviders,
  useResolveAnomaly,
  useTenants,
} from '@/hooks/use-admin';
import {
  formatCurrency,
  formatDateTime,
  formatMoney,
  formatNumber,
  formatPercent,
  formatTime,
} from '@/lib/format';
import { bucketLabels } from '@/lib/metrics';
import type { BudgetStatus, CostDimensionRow, CostRecord } from '@/lib/types';

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
    <label className="inline-flex min-w-0 max-w-full items-center gap-2 text-xs text-neutral-400">
      <span className="sr-only">{label}</span>
      <select
        value={value}
        onChange={(event) => window.location.assign(hrefFor(event.target.value))}
        className="h-8.5 max-w-[calc(100vw-2rem)] truncate rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-xs text-neutral-200 shadow-sm focus:outline-none focus:ring-1 focus:ring-primary/50 sm:max-w-[180px]"
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

function dimensionRows(rows: CostDimensionRow[] | undefined, limit = 8) {
  return (rows ?? []).slice(0, limit).map((row) => ({
    label: row.key || 'unattributed',
    value: row.actual_usd,
    detail: `${formatNumber(row.requests)} req · ${formatPercent(row.share)}`,
  }));
}

function BudgetBar({ status }: { status: BudgetStatus }) {
  const pct = Math.min(100, status.utilization * 100);
  const tone =
    status.exhausted || status.utilization >= 1
      ? 'bg-rose-500'
      : status.utilization >= 0.8
        ? 'bg-amber-400'
        : 'bg-emerald-400';
  return (
    <div>
      <div className="flex items-baseline justify-between gap-2">
        <p className="truncate text-[13px] font-medium">
          {status.scope}
          <span className="ml-2 font-mono text-[10px] text-muted-foreground">{status.period}</span>
        </p>
        <p className="shrink-0 text-xs tabular-nums text-neutral-300">
          {formatMoney(status.spent_usd)} / {formatMoney(status.limit_usd)}
        </p>
      </div>
      <div className="mt-1.5 h-2 overflow-hidden rounded-full bg-white/[0.06]">
        <div className={`h-full rounded-full ${tone}`} style={{ width: `${pct}%` }} />
      </div>
      <p className="mt-1 text-[11px] text-muted-foreground">
        {formatPercent(Math.min(status.utilization, 9.99))} used · burn {status.burn_rate.toFixed(2)}×
        {status.projected_usd > 0 ? ` · projected ${formatMoney(status.projected_usd)}` : ''}
        {status.enforced ? '' : ' · observational'}
      </p>
    </div>
  );
}

function severityTone(severity: string): 'default' | 'warning' | 'danger' {
  if (severity === 'critical') return 'danger';
  if (severity === 'warning') return 'warning';
  return 'default';
}

export function CostView() {
  const window = useWindowParam();
  const tenantId = useTenantParam();
  const searchParams = useSearchParams();
  const provider = searchParams.get('provider') ?? '';
  const model = searchParams.get('model') ?? '';
  const from = windowToFromParam(window);
  const filters = {
    tenantId,
    from,
    provider: provider || undefined,
    model: model || undefined,
  };

  const overview = useCostOverview(filters);
  const series = useCostSeries(filters);
  const byProvider = useCostGrouped(filters, 'provider');
  const byModel = useCostGrouped(filters, 'model');
  const byTenant = useCostGrouped(filters, 'tenant');
  const byEndpoint = useCostGrouped(filters, 'endpoint');
  const top = useCostTop(filters, 10);
  const savings = useCostSavings(filters);
  const forecast = useCostForecast(filters);
  const budgets = useCostBudgets(tenantId);
  const anomalies = useCostAnomalies(tenantId);
  const pricing = usePricing();
  const { data: providerList } = useProviders();
  const { data: modelList } = useModels();
  const { data: tenants } = useTenants();
  const resolveAnomaly = useResolveAnomaly();
  const createPricing = useCreatePricing();

  const [selectedRequest, setSelectedRequest] = React.useState<string | null>(null);
  const requestDetail = useCostRequest(selectedRequest);

  const [sheet, setSheet] = React.useState({
    scope: 'global',
    scopeId: '',
    input: '0.15',
    output: '0.60',
    cached: '',
    baseFee: '',
  });

  const tenantNames = React.useMemo(() => {
    const map = new Map<string, string>();
    for (const tenant of tenants ?? []) map.set(tenant.id, tenant.name);
    return map;
  }, [tenants]);

  // Cost points carry no interval: the gateway truncates by hour for a day and
  // by day for a week or a month. bucketLabels infers the width from the point
  // spacing, so a 7d/30d axis is labelled by date instead of a repeated time.
  const labels = React.useMemo(
    () => bucketLabels((series.data?.series ?? []).map((p) => ({ start: p.bucket }))),
    [series.data],
  );
  const actualSeries = React.useMemo(
    () => (series.data?.series ?? []).map((p) => p.actual_usd),
    [series.data],
  );
  const estimatedSeries = React.useMemo(
    () => (series.data?.series ?? []).map((p) => p.estimated_usd),
    [series.data],
  );

  const ov = overview.data?.overview;
  const sv = savings.data;
  const fc = forecast.data?.forecast;

  const exportQuery = new URLSearchParams({
    ...(tenantId ? { tenant_id: tenantId } : {}),
    ...(provider ? { provider } : {}),
    ...(model ? { model } : {}),
    from,
  }).toString();

  const failed = [overview, series, top, savings, forecast, budgets, anomalies].find((q) => q.isError);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Cost intelligence"
        description="Exact, explainable spend for every request: what it cost, why, under which price sheet, and where the money goes. Figures aggregate from the usage store for the range and filters below."
        actions={
          <>
            <TenantPicker value={tenantId} />
            <UrlFilterSelect
              param="provider"
              value={provider}
              label="Provider"
              allLabel="All providers"
              options={(providerList ?? []).map((p) => ({ value: p.name, label: p.name }))}
            />
            <UrlFilterSelect
              param="model"
              value={model}
              label="Model"
              allLabel="All models"
              options={(modelList ?? []).map((m) => ({ value: m.name, label: m.name }))}
            />
            <RangePicker value={window} />
            <Button variant="outline" size="sm" asChild>
              <a href={`/api/gateway/cost/export?format=csv&${exportQuery}`}>
                <Download className="mr-1.5 h-3.5 w-3.5" /> CSV
              </a>
            </Button>
            <Button variant="outline" size="sm" asChild>
              <a href={`/api/gateway/cost/export?format=json&${exportQuery}`}>
                <Download className="mr-1.5 h-3.5 w-3.5" /> JSON
              </a>
            </Button>
          </>
        }
      />

      {failed ? (
        <ErrorState
          error={failed.error instanceof Error ? failed.error : new Error('The cost endpoints did not answer.')}
          onRetry={() => {
            void overview.refetch();
          }}
        />
      ) : null}

      {overview.isPending ? (
        <CardsSkeleton count={4} />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          <StatCard
            label="Actual spend"
            icon={Wallet}
            value={
              ov && Number.isFinite(ov.actual_usd) ? (
                <span>
                  $<NumberTicker value={ov.actual_usd} decimalPlaces={2} />
                </span>
              ) : (
                formatMoney(ov?.actual_usd)
              )
            }
            hint={
              ov
                ? `${formatNumber(ov.requests)} requests · ${formatNumber(ov.tokens)} tokens`
                : undefined
            }
          />
          <StatCard
            label="Estimate accuracy"
            icon={Scale}
            value={formatPercent(ov?.estimate_accuracy)}
            hint={ov ? `projected ${formatMoney(ov.estimated_usd)}` : undefined}
            tone={ov && ov.estimate_accuracy < 0.8 ? 'warning' : 'default'}
          />
          <StatCard
            label="Unit rate"
            icon={Coins}
            value={formatCurrency(ov?.cost_per_kilo_token_usd)}
            hint="actual spend per 1k tokens"
          />
          <StatCard
            label="Measured savings"
            icon={PiggyBank}
            value={formatMoney(sv?.total_usd)}
            hint={
              sv
                ? `cache ${formatMoney(sv.savings.cache_usd)} · routing ${formatMoney(sv.savings.routing_usd)} · fallback ${formatMoney(sv.savings.fallback_usd)}`
                : undefined
            }
          />
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Spend over time</CardTitle>
          <CardDescription>
            Final billed cost against the routing-time projection. A widening gap means estimates are drifting.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {series.isPending ? (
            <ChartSkeleton />
          ) : actualSeries.length === 0 ? (
            <EmptyState title="No spend in range" description="No billed requests matched the filters." />
          ) : (
            <LineChart
              labels={labels}
              series={[
                { name: 'Actual', color: 'hsl(var(--primary))', values: actualSeries },
                { name: 'Estimated', color: '#94a3b8', values: estimatedSeries },
              ]}
              formatValue={formatCurrency}
            />
          )}
        </CardContent>
      </Card>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Spend by provider</CardTitle>
            <CardDescription>Where the money goes upstream.</CardDescription>
          </CardHeader>
          <CardContent>
            {byProvider.isPending ? (
              <ChartSkeleton />
            ) : (
              <BarList rows={dimensionRows(byProvider.data?.rows)} formatValue={formatCurrency} />
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Spend by model</CardTitle>
            <CardDescription>Which models drive the bill.</CardDescription>
          </CardHeader>
          <CardContent>
            {byModel.isPending ? (
              <ChartSkeleton />
            ) : (
              <BarList rows={dimensionRows(byModel.data?.rows)} formatValue={formatCurrency} />
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Spend by tenant</CardTitle>
            <CardDescription>Who spends, and their share of the window.</CardDescription>
          </CardHeader>
          <CardContent>
            {byTenant.isPending ? (
              <ChartSkeleton />
            ) : (
              <BarList
                rows={(byTenant.data?.rows ?? []).slice(0, 8).map((row) => ({
                  label: tenantNames.get(row.key) ?? row.key ?? 'unattributed',
                  value: row.actual_usd,
                  detail: `${formatNumber(row.requests)} req · ${formatPercent(row.share)}`,
                }))}
                formatValue={formatCurrency}
              />
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Spend by endpoint</CardTitle>
            <CardDescription>Cost under admin-managed endpoint scopes.</CardDescription>
          </CardHeader>
          <CardContent>
            {byEndpoint.isPending ? (
              <ChartSkeleton />
            ) : (
              <BarList rows={dimensionRows(byEndpoint.data?.rows)} formatValue={formatCurrency} />
            )}
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 xl:grid-cols-5">
        <Card className="xl:col-span-3">
          <CardHeader>
            <CardTitle>Most expensive requests</CardTitle>
            <CardDescription>
              Select a row to read its exact cost trace: line items, tokens, and the pricing source.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {top.isPending ? (
              <TableSkeleton />
            ) : (top.data?.requests ?? []).length === 0 ? (
              <EmptyState title="No requests" description="Nothing billable in this window." />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Request</TableHead>
                    <TableHead>Provider / model</TableHead>
                    <TableHead className="text-right">Actual</TableHead>
                    <TableHead className="text-right">Estimate</TableHead>
                    <TableHead className="text-right">Sheet</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(top.data?.requests ?? []).map((row: CostRecord) => (
                    <TableRow
                      key={row.id}
                      className={selectedRequest === row.request_id ? 'bg-white/[0.04]' : 'cursor-pointer'}
                      onClick={() => setSelectedRequest(row.request_id)}
                    >
                      <TableCell>
                        <p className="font-mono text-[11px]">{row.request_id.slice(0, 8)}…</p>
                        <p className="text-[10px] text-muted-foreground">{formatDateTime(row.created_at)}</p>
                      </TableCell>
                      <TableCell>
                        <p className="text-[13px]">{row.provider || '—'}</p>
                        <p className="font-mono text-[10px] text-muted-foreground">{row.model}</p>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{formatCurrency(row.cost.usd)}</TableCell>
                      <TableCell className="text-right tabular-nums text-neutral-400">
                        {formatCurrency(row.estimate_cost.usd)}
                      </TableCell>
                      <TableCell className="text-right font-mono text-[11px] text-neutral-400">
                        {row.pricing_source || 'registry'}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
        <Card className="xl:col-span-2">
          <CardHeader>
            <CardTitle>Cost trace</CardTitle>
            <CardDescription>
              {selectedRequest ? `Request ${selectedRequest.slice(0, 8)}…` : 'Select a request to inspect it.'}
            </CardDescription>
          </CardHeader>
          <CardContent>
            {requestDetail.isPending && selectedRequest ? (
              <TableSkeleton />
            ) : !requestDetail.data ? (
              <EmptyState title="No trace selected" description="Pick a row on the left." />
            ) : (
              <div className="space-y-3">
                <div className="grid grid-cols-1 gap-2 text-xs sm:grid-cols-2">
                  <div>
                    <p className="text-muted-foreground">Actual</p>
                    <p className="text-sm font-semibold tabular-nums">
                      {formatCurrency(requestDetail.data.record.cost.usd)}
                    </p>
                  </div>
                  <div>
                    <p className="text-muted-foreground">Estimate (accuracy {formatPercent(requestDetail.data.estimate_accuracy)})</p>
                    <p className="text-sm tabular-nums">
                      {formatCurrency(requestDetail.data.record.estimate_cost.usd)}
                    </p>
                  </div>
                  <div>
                    <p className="text-muted-foreground">Tokens</p>
                    <p className="tabular-nums">
                      {formatNumber(requestDetail.data.record.usage.prompt_tokens)} in /{' '}
                      {formatNumber(requestDetail.data.record.usage.completion_tokens)} out
                      {requestDetail.data.record.usage.cached_prompt_tokens > 0
                        ? ` (${formatNumber(requestDetail.data.record.usage.cached_prompt_tokens)} cached)`
                        : ''}
                    </p>
                  </div>
                  <div>
                    <p className="text-muted-foreground">Pricing</p>
                    <p className="font-mono text-[11px]">
                      {requestDetail.data.record.pricing_source || 'registry'}
                      {requestDetail.data.record.pricing_version_id
                        ? ` · ${requestDetail.data.record.pricing_version_id.slice(0, 8)}`
                        : ''}
                    </p>
                  </div>
                </div>
                {(requestDetail.data.record.breakdown?.lines ?? []).length > 0 ? (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Line</TableHead>
                        <TableHead className="text-right">Qty</TableHead>
                        <TableHead className="text-right">Amount</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {requestDetail.data.record.breakdown!.lines.map((line, i) => (
                        <TableRow key={i}>
                          <TableCell className="text-xs">{line.label}</TableCell>
                          <TableCell className="text-right text-xs tabular-nums">
                            {formatNumber(line.quantity)}
                          </TableCell>
                          <TableCell className="text-right text-xs tabular-nums">
                            {formatCurrency(line.amount_usd)}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    No line items: this request billed nothing (a cache serve or a pre-billing failure).
                  </p>
                )}
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Wallet className="h-4 w-4" /> Budgets
            </CardTitle>
            <CardDescription>
              Evaluated against authoritative usage spend when this page loads; new threshold crossings fire once.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {budgets.isPending ? (
              <ChartSkeleton />
            ) : (budgets.data?.budgets ?? []).length === 0 ? (
              <EmptyState title="No budgets" description="Budgets are managed under PUT /admin/v1/budgets." />
            ) : (
              <div className="space-y-4">
                {(budgets.data?.budgets ?? []).map((b: BudgetStatus) => (
                  <BudgetBar key={b.budget_id} status={b} />
                ))}
                {(budgets.data?.alerts ?? []).length > 0 ? (
                  <div className="border-t border-white/[0.06] pt-3">
                    <p className="mb-2 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                      Recent alerts
                    </p>
                    {(budgets.data?.alerts ?? []).slice(0, 5).map((a) => (
                      <p key={a.id} className="flex items-center gap-1.5 text-xs text-amber-300">
                        <AlertTriangle className="h-3 w-3 shrink-0" />
                        {formatMoney(a.threshold_usd)} crossed at {formatMoney(a.spent_usd)} ·{' '}
                        {formatTime(a.fired_at)}
                      </p>
                    ))}
                  </div>
                ) : null}
              </div>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <TrendingUp className="h-4 w-4" /> Forecast
            </CardTitle>
            <CardDescription>Month-end projection from the trailing week of daily spend.</CardDescription>
          </CardHeader>
          <CardContent>
            {forecast.isPending ? (
              <ChartSkeleton />
            ) : !fc || fc.days_observed === 0 ? (
              <EmptyState title="No history" description="Forecasting needs at least a day of spend." />
            ) : (
              <div className="space-y-3">
                <div>
                  <p className="text-[11px] text-muted-foreground">Projected month-end</p>
                  <p className="text-2xl font-semibold tabular-nums">{formatMoney(fc.projected_month_usd)}</p>
                  <p className="text-xs text-muted-foreground">
                    range {formatMoney(fc.projected_month_low_usd)} – {formatMoney(fc.projected_month_high_usd)}
                  </p>
                </div>
                <div className="grid grid-cols-2 gap-2 text-xs">
                  <div>
                    <p className="text-muted-foreground">Daily average</p>
                    <p className="tabular-nums">{formatMoney(fc.daily_average_usd)}</p>
                  </div>
                  <div>
                    <p className="text-muted-foreground">Trend</p>
                    <p className={`tabular-nums ${fc.trend_usd_per_day > 0 ? 'text-amber-300' : 'text-emerald-300'}`}>
                      {fc.trend_usd_per_day > 0 ? '+' : ''}
                      {formatMoney(fc.trend_usd_per_day)}/day
                    </p>
                  </div>
                </div>
                <p className="text-[11px] text-muted-foreground">
                  From {fc.days_observed} observed days. A range, not a promise: the band is one residual standard
                  deviation.
                </p>
              </div>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Siren className="h-4 w-4" /> Anomalies
            </CardTitle>
            <CardDescription>Daily spend spikes against a 14-day median baseline.</CardDescription>
          </CardHeader>
          <CardContent>
            {anomalies.isPending ? (
              <ChartSkeleton />
            ) : (anomalies.data?.anomalies ?? []).length === 0 ? (
              <EmptyState title="No open anomalies" description="Spend is within baseline across dimensions." />
            ) : (
              <div className="space-y-2">
                {(anomalies.data?.anomalies ?? []).slice(0, 8).map((a) => (
                  <div
                    key={a.id}
                    className="flex items-center justify-between gap-2 rounded-lg border border-white/[0.06] px-2.5 py-2"
                  >
                    <div className="min-w-0">
                      <p className="truncate text-xs font-medium">
                        {a.dimension}:{a.key}
                      </p>
                      <p className="text-[11px] text-muted-foreground">
                        {formatMoney(a.observed_usd)} vs {formatMoney(a.expected_usd)} expected · {a.ratio.toFixed(1)}×
                      </p>
                    </div>
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={resolveAnomaly.isPending}
                      onClick={() => resolveAnomaly.mutate(a.id)}
                    >
                      Resolve
                    </Button>
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <BadgeDollarSign className="h-4 w-4" /> Pricing registry
            </CardTitle>
            <CardDescription>
              Immutable dated sheets. Tenant beats model beats provider beats global; a price change is a new row,
              never an edit.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {pricing.isPending ? (
              <TableSkeleton />
            ) : (pricing.data ?? []).length === 0 ? (
              <p className="py-4 text-center text-xs text-muted-foreground">
                No versioned sheets: every request prices from the static registry. Mint one below to override.
              </p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Scope</TableHead>
                    <TableHead className="text-right">In / 1M</TableHead>
                    <TableHead className="text-right">Out / 1M</TableHead>
                    <TableHead className="text-right">Base fee</TableHead>
                    <TableHead>Effective</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(pricing.data ?? []).slice(0, 12).map((v) => (
                    <TableRow key={v.id}>
                      <TableCell>
                        <p className="text-xs font-medium">{v.scope}</p>
                        {v.scope_id ? (
                          <p className="font-mono text-[10px] text-muted-foreground">{v.scope_id.slice(0, 8)}…</p>
                        ) : null}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {formatCurrency(v.input_cost_per_million)}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {formatCurrency(v.output_cost_per_million)}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {v.base_fee_usd ? formatCurrency(v.base_fee_usd) : '—'}
                      </TableCell>
                      <TableCell className="text-[11px] text-muted-foreground">
                        {formatDateTime(v.effective_from)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
            <form
              className="mt-4 grid grid-cols-2 gap-2 border-t border-white/[0.06] pt-4 sm:grid-cols-6"
              onSubmit={(event) => {
                event.preventDefault();
                createPricing.mutate({
                  scope: sheet.scope,
                  scopeId: sheet.scopeId || undefined,
                  inputCostPerMillion: Number(sheet.input) || 0,
                  outputCostPerMillion: Number(sheet.output) || 0,
                  cachedInputCostPerMillion: sheet.cached ? Number(sheet.cached) : undefined,
                  baseFeeUsd: sheet.baseFee ? Number(sheet.baseFee) : undefined,
                });
              }}
            >
              <select
                value={sheet.scope}
                onChange={(e) => setSheet({ ...sheet, scope: e.target.value })}
                className="h-8 rounded-lg border border-white/[0.08] bg-[#121217] px-2 text-xs"
                aria-label="Scope"
              >
                <option value="global">global</option>
                <option value="provider">provider</option>
                <option value="model">model</option>
                <option value="tenant">tenant</option>
              </select>
              <input
                value={sheet.scopeId}
                onChange={(e) => setSheet({ ...sheet, scopeId: e.target.value })}
                placeholder="scope id"
                className="h-8 rounded-lg border border-white/[0.08] bg-[#121217] px-2 font-mono text-xs"
              />
              <input
                value={sheet.input}
                onChange={(e) => setSheet({ ...sheet, input: e.target.value })}
                placeholder="$/1M in"
                inputMode="decimal"
                className="h-8 rounded-lg border border-white/[0.08] bg-[#121217] px-2 text-xs tabular-nums"
                aria-label="Input price per million"
              />
              <input
                value={sheet.output}
                onChange={(e) => setSheet({ ...sheet, output: e.target.value })}
                placeholder="$/1M out"
                inputMode="decimal"
                className="h-8 rounded-lg border border-white/[0.08] bg-[#121217] px-2 text-xs tabular-nums"
                aria-label="Output price per million"
              />
              <input
                value={sheet.cached}
                onChange={(e) => setSheet({ ...sheet, cached: e.target.value })}
                placeholder="$/1M cached"
                inputMode="decimal"
                className="h-8 rounded-lg border border-white/[0.08] bg-[#121217] px-2 text-xs tabular-nums"
                aria-label="Cached input price per million"
              />
              <Button type="submit" size="sm" disabled={createPricing.isPending} className="col-span-2 sm:col-span-1">
                Mint sheet
              </Button>
            </form>
            {createPricing.isError ? (
              <p className="mt-2 text-xs text-rose-400">
                {createPricing.error instanceof Error ? createPricing.error.message : 'Minting failed.'}
              </p>
            ) : null}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Savings ledger</CardTitle>
            <CardDescription>
              Avoided spend measured from recorded rows, not modeled. Cache hits bill zero while their estimates
              stand; routing deltas come from the shaping bracket.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {savings.isPending ? (
              <ChartSkeleton />
            ) : (
              <BarList
                rows={[
                  { label: 'Response cache', value: sv?.savings.cache_usd ?? 0 },
                  { label: 'Route optimization', value: sv?.savings.routing_usd ?? 0 },
                  { label: 'Cheaper fallbacks', value: sv?.savings.fallback_usd ?? 0 },
                ]}
                formatValue={formatCurrency}
              />
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
