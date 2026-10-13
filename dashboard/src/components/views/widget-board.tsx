'use client';

import { ArrowDownRight, ArrowUpRight, Minus } from 'lucide-react';
import * as React from 'react';

import { DraggableWidgetGrid, type WidgetItem } from '@/components/ui/draggable-widget-grid';
import { Button } from '@/components/ui/button';
import { EmptyState, ErrorState } from '@/components/ui/state';
import { useTenantParam } from '@/components/tenant-picker';
import {
  useAgentRuns,
  useEvalRuns,
  useOverview,
  useProviderHealth,
  useToolInvocations,
} from '@/hooks/use-admin';
import { useWindowParam, windowToFromParam } from '@/components/range-picker';
import { deltaRatio } from '@/lib/metrics';
import { bucketTotals } from '@/lib/charts';
import { formatCurrency, formatDurationMs, formatNumber, formatPercent } from '@/lib/format';
import { cn } from '@/lib/utils';

/* ------------------------------------------------------------------ *
 * Widget board: the analytics KPI set as rearrangeable widgets.
 *
 * Every figure comes from the same queries the rest of the page uses —
 * nothing here is simulated. The arrangement persists per browser in
 * localStorage; dragging is opt-in behind Customize so a stray drag never
 * scrambles the layout. Figures render in the bundled monospace face, which
 * is what the type system reserves for measured data.
 * ------------------------------------------------------------------ */

type Kind = 'runs' | 'health' | 'cost' | 'failures' | 'traces' | 'evals' | 'tools' | 'models';

interface BoardWidget extends WidgetItem {
  kind: Kind;
}

const WIDGETS: BoardWidget[] = [
  { id: 'runs', kind: 'runs', size: 'wide', label: 'Requests in window' },
  { id: 'health', kind: 'health', size: 'sm', label: 'Provider health' },
  { id: 'cost', kind: 'cost', size: 'sm', label: 'Estimated spend' },
  { id: 'failures', kind: 'failures', size: 'sm', label: 'Failures' },
  { id: 'traces', kind: 'traces', size: 'wide', label: 'Recent agent runs' },
  { id: 'evals', kind: 'evals', size: 'sm', label: 'Evaluations' },
  { id: 'tools', kind: 'tools', size: 'wide', label: 'Tool calls' },
  { id: 'models', kind: 'models', size: 'wide', label: 'Token usage by model' },
];

const STORAGE_KEY = 'synapass-widget-order';

function loadOrder(): BoardWidget[] {
  try {
    const raw = globalThis.localStorage?.getItem(STORAGE_KEY);
    if (!raw) return WIDGETS;
    const ids = JSON.parse(raw) as unknown;
    if (!Array.isArray(ids)) return WIDGETS;
    const known = new Map(WIDGETS.map((w) => [w.id, w]));
    const ordered = (ids as unknown[])
      .filter((id): id is string => typeof id === 'string')
      .map((id) => known.get(id))
      .filter((w): w is BoardWidget => w != null);
    for (const w of WIDGETS) {
      if (!ordered.some((o) => o.id === w.id)) ordered.push(w);
    }
    return ordered;
  } catch {
    return WIDGETS;
  }
}

/* ------------------------------------------------------------------ *
 * Building blocks (typography: mono for measured figures)
 * ------------------------------------------------------------------ */

function Shell({ title, meta, children }: { title: string; meta?: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="@container flex h-full flex-col gap-3 p-4 sm:p-5">
      <header className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 text-[14px] leading-none">
        <h3 className="truncate text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
          {title}
        </h3>
        {meta && <span className="shrink-0 text-xs text-muted-foreground">{meta}</span>}
      </header>
      <div className="flex min-h-0 flex-1 flex-col">{children}</div>
    </section>
  );
}

function Big({ children, unit }: { children: React.ReactNode; unit?: string }) {
  return (
    <p className="break-words font-mono text-[22px] font-medium leading-tight tracking-tight tabular-nums text-foreground min-[400px]:text-[28px] min-[400px]:leading-none">
      {children}
      {unit && <span className="ml-1 font-sans text-[13px] font-normal text-muted-foreground">{unit}</span>}
    </p>
  );
}

function Delta({ ratio, label, higherIsWorse = false }: { ratio: number | null; label: string; higherIsWorse?: boolean }) {
  if (ratio === null || !Number.isFinite(ratio) || Math.abs(ratio) < 0.005) {
    return (
      <span className="inline-flex items-center gap-0.5 text-xs text-muted-foreground">
        <Minus className="size-3.5" /> flat
        <span className="sr-only"> {label}</span>
      </span>
    );
  }
  const up = ratio > 0;
  const bad = higherIsWorse ? up : !up;
  const Icon = up ? ArrowUpRight : ArrowDownRight;
  return (
    <span className={cn('inline-flex items-center gap-0.5 text-xs tabular-nums', bad ? 'text-danger' : 'text-success')}>
      <Icon className="size-3.5" aria-hidden="true" />
      {up ? '+' : ''}{(ratio * 100).toFixed(1)}%
      <span className="sr-only"> {label}</span>
    </span>
  );
}

function StatusDot({ tone }: { tone: 'ok' | 'warn' | 'err' | 'idle' }) {
  return (
    <span
      aria-hidden="true"
      className={cn(
        'inline-block size-1.5 shrink-0 rounded-full',
        tone === 'ok' && 'bg-success',
        tone === 'warn' && 'bg-warning',
        tone === 'err' && 'bg-danger',
        tone === 'idle' && 'bg-muted-foreground/60',
      )}
    />
  );
}

function Row({ children, value }: { children: React.ReactNode; value: React.ReactNode }) {
  return (
    <div className="flex items-center gap-2 text-[13px]">
      <span className="flex min-w-0 items-center gap-2 truncate text-foreground">{children}</span>
      <span className="ml-auto shrink-0 font-mono tabular-nums text-muted-foreground">{value}</span>
    </div>
  );
}

/* ------------------------------------------------------------------ *
 * Widgets (all real data)
 * ------------------------------------------------------------------ */

function RunsWidget({ totals, requests, previous }: { totals: number[]; requests: number; previous?: number }) {
  const peak = Math.max(...totals, 1);
  return (
    <Shell title="Requests" meta={<Delta ratio={deltaRatio(requests, previous)} label="vs previous window" />}>
      <Big>{formatNumber(requests)}</Big>
      <div
        role="img"
        aria-label={`${formatNumber(requests)} requests in this window.`}
        className="mt-auto flex h-10 items-end gap-[3px] pt-3"
      >
        {totals.map((v, i) => (
          <span
            key={i}
            className="min-w-0 flex-1 rounded-full bg-primary/80"
            style={{ height: `${Math.max(6, (v / peak) * 100)}%` }}
          />
        ))}
      </div>
    </Shell>
  );
}

function HealthWidget() {
  const { data } = useProviderHealth();
  const rows = data ?? [];
  const bad = rows.filter((h) => h.state === 'unhealthy' || h.state === 'degraded');
  const ok = rows.length - bad.length;
  return (
    <Shell title="Provider health" meta={`${rows.length} configured`}>
      <Big>
        {ok}
        <span className="text-muted-foreground">/{rows.length}</span>
      </Big>
      <p className="mt-2 text-[13px] text-muted-foreground">{bad.length === 0 ? 'All healthy' : `${bad.length} need attention`}</p>
      <div className="mt-auto flex flex-wrap gap-1.5 pt-3" role="img" aria-label={`${ok} of ${rows.length} providers healthy.`}>
        {rows.map((h) => (
          <StatusDot key={h.provider_id} tone={h.state === 'healthy' ? 'ok' : h.state === 'unknown' ? 'idle' : h.state === 'degraded' ? 'warn' : 'err'} />
        ))}
      </div>
    </Shell>
  );
}

function CostWidget({
  total,
  previous,
  byProvider,
}: {
  total: number;
  previous?: number;
  byProvider: Array<{ provider: string; cost_usd: number }>;
}) {
  return (
    <Shell title="Spend" meta={<Delta ratio={deltaRatio(total, previous)} label="vs previous window" higherIsWorse />}>
      <Big>{formatCurrency(total)}</Big>
      <div className="mt-auto space-y-2 pt-3">
        {byProvider.slice(0, 3).map((r) => (
          <Row key={r.provider} value={formatCurrency(r.cost_usd)}>
            <span className="truncate">{r.provider}</span>
          </Row>
        ))}
      </div>
    </Shell>
  );
}

function FailuresWidget({ errors, rejections, fallbacks }: { errors: number; rejections: number; fallbacks: number }) {
  return (
    <Shell title="Failures" meta="window">
      <Big>{formatNumber(errors + rejections)}</Big>
      <div className="mt-auto space-y-2 pt-3">
        <Row value={formatNumber(errors)}>
          <StatusDot tone="err" /> server errors
        </Row>
        <Row value={formatNumber(rejections)}>
          <StatusDot tone="warn" /> rejected
        </Row>
        <Row value={formatNumber(fallbacks)}>
          <StatusDot tone="idle" /> absorbed by fallback
        </Row>
      </div>
    </Shell>
  );
}

function TracesWidget() {
  const { data } = useAgentRuns(20);
  const runs = data?.runs ?? [];
  const latencies = runs.map((r) => r.latency_ms).filter((v) => v > 0).sort((a, b) => a - b);
  const p50 = latencies.length > 0 ? latencies[Math.floor(latencies.length / 2)] : 0;
  return (
    <Shell title="Recent agent runs" meta={runs.length > 0 ? `${runs.length} runs` : undefined}>
      <Big unit="p50">{formatDurationMs(p50)}</Big>
      <ol aria-label="Most recent agent runs" className="mt-auto space-y-2 pt-3 text-[13px]">
        {runs.slice(0, 4).map((r, i) => (
          <li key={r.id} className={cn('flex items-center gap-3', i > 0 && 'text-muted-foreground')}>
            <StatusDot tone={r.status === 'completed' ? 'ok' : r.status === 'failed' || r.status === 'denied' ? 'err' : 'warn'} />
            <span className="truncate font-mono text-xs" title={r.id}>
              {r.id.slice(0, 8)}
              <span className="sr-only">, {r.status}, {r.model ?? 'unknown model'}</span>
            </span>
            <span aria-hidden="true" className="hidden truncate @[440px]:block">
              {r.model ?? '—'}
            </span>
            <span className="ml-auto shrink-0 font-mono tabular-nums">{formatDurationMs(r.latency_ms)}</span>
          </li>
        ))}
        {runs.length === 0 && <li className="text-xs text-muted-foreground">No agent runs yet.</li>}
      </ol>
    </Shell>
  );
}

function EvalsWidget({ tenantId }: { tenantId: string }) {
  const { data } = useEvalRuns(tenantId);
  const runs = data ?? [];
  const done = runs.filter((r) => r.status === 'completed').length;
  return (
    <Shell title="Evaluations" meta={runs.length > 0 ? `${runs.length} runs` : undefined}>
      <Big>{formatNumber(done)}</Big>
      <p className="mt-2 text-[13px] text-muted-foreground">completed</p>
      <div className="mt-auto space-y-2 pt-3">
        {runs.slice(0, 3).map((r) => (
          <Row key={r.id} value={r.status}>
            <span className="truncate">{r.dataset ?? r.id.slice(0, 8)}</span>
          </Row>
        ))}
        {runs.length === 0 && <p className="text-xs text-muted-foreground">No evaluation runs yet.</p>}
      </div>
    </Shell>
  );
}

function ToolsWidget() {
  const { data } = useToolInvocations(100);
  const invocations = data?.invocations ?? [];
  const total = data?.total ?? invocations.length;
  const byTool = new Map<string, number>();
  for (const inv of invocations) byTool.set(inv.tool_name, (byTool.get(inv.tool_name) ?? 0) + 1);
  const rows = [...byTool.entries()].sort((a, b) => b[1] - a[1]).slice(0, 4);
  const max = Math.max(...rows.map(([, c]) => c), 1);
  return (
    <Shell title="Tool calls" meta="top tools">
      <Big>{formatNumber(total)}</Big>
      <div className="mt-auto space-y-2 pt-3">
        {rows.map(([name, count], i) => (
          <div key={name} className={cn('flex min-w-0 items-center gap-2 text-[13px] min-[400px]:gap-3', i > 0 && 'text-muted-foreground')}>
            <span className="w-[100px] shrink-0 truncate font-mono text-xs min-[400px]:w-[140px]">{name}</span>
            <span aria-hidden="true" className="block h-[3px] min-w-0 flex-1 rounded-full bg-foreground/10">
              <span
                className={cn('block h-full rounded-full', i === 0 ? 'bg-primary' : 'bg-foreground/25')}
                style={{ width: `${(count / max) * 100}%` }}
              />
            </span>
            <span className="w-[48px] shrink-0 text-right font-mono tabular-nums min-[400px]:w-[56px]">{formatNumber(count)}</span>
          </div>
        ))}
        {rows.length === 0 && <p className="text-xs text-muted-foreground">No tool calls recorded.</p>}
      </div>
    </Shell>
  );
}

function ModelsWidget({
  tokens,
  models,
}: {
  tokens: number;
  models: Array<{ model: string; requests: number }>;
}) {
  const total = models.reduce((a, m) => a + m.requests, 0);
  const rows = [...models].sort((a, b) => b.requests - a.requests).slice(0, 4);
  return (
    <Shell title="Usage by model" meta="share of requests">
      <Big unit="tokens">{formatCompact(tokens)}</Big>
      <div className="mt-auto space-y-2 pt-3">
        {rows.map((m) => (
          <Row key={m.model} value={total > 0 ? `${Math.round((m.requests / total) * 100)}%` : '—'}>
            <span className="truncate">{m.model}</span>
          </Row>
        ))}
        {rows.length === 0 && <p className="text-xs text-muted-foreground">No model traffic in this window.</p>}
      </div>
      {rows.length > 0 && (
        <div
          role="img"
          aria-label={`Share of requests: ${rows.map((m) => `${m.model} ${total > 0 ? Math.round((m.requests / total) * 100) : 0}%`).join(', ')}.`}
          className="mt-3 flex h-[3px] gap-[3px]"
        >
          {rows.map((m, i) => (
            <span
              key={m.model}
              className={cn('h-full rounded-full', i === 0 ? 'bg-primary' : 'bg-foreground/20')}
              style={{ width: `${total > 0 ? (m.requests / total) * 100 : 0}%` }}
            />
          ))}
        </div>
      )}
    </Shell>
  );
}

function formatCompact(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}k`;
  return String(Math.round(value));
}

/* ------------------------------------------------------------------ *
 * Board
 * ------------------------------------------------------------------ */

const VIEWS: Record<Kind, (ctx: BoardContext) => React.ReactNode> = {
  runs: (ctx) => (
    <RunsWidget totals={ctx.requestTotals} requests={ctx.summary?.requests ?? 0} previous={ctx.previous?.requests} />
  ),
  health: () => <HealthWidget />,
  cost: (ctx) => (
    <CostWidget
      total={ctx.summary?.total_cost_usd ?? 0}
      previous={ctx.previous?.total_cost_usd}
      byProvider={(ctx.providers ?? []).map((r) => ({ provider: r.provider, cost_usd: r.cost_usd }))}
    />
  ),
  failures: (ctx) => (
    <FailuresWidget
      errors={ctx.summary?.errors ?? 0}
      rejections={ctx.summary?.rejections ?? 0}
      fallbacks={ctx.summary?.fallbacks ?? 0}
    />
  ),
  traces: () => <TracesWidget />,
  evals: (ctx) => <EvalsWidget tenantId={ctx.tenantId} />,
  tools: () => <ToolsWidget />,
  models: (ctx) => (
    <ModelsWidget
      tokens={ctx.summary?.total_tokens ?? 0}
      models={(ctx.models ?? []).map((m) => ({ model: m.model, requests: m.requests }))}
    />
  ),
};

interface BoardContext {
  tenantId: string;
  summary?: {
    requests: number;
    errors: number;
    rejections: number;
    fallbacks: number;
    total_cost_usd: number;
    total_tokens: number;
  };
  previous?: { requests: number; total_cost_usd: number };
  requestTotals: number[];
  providers?: Array<{ provider: string; cost_usd: number }>;
  models?: Array<{ model: string; requests: number }>;
}

export function WidgetBoard() {
  const window = useWindowParam();
  const tenantId = useTenantParam();
  const from = windowToFromParam(window);
  const { data, isPending, isError, error, refetch } = useOverview(tenantId, from);

  const [customizing, setCustomizing] = React.useState(false);
  const [order, setOrder] = React.useState<BoardWidget[]>(() =>
    typeof window === 'undefined' ? WIDGETS : loadOrder(),
  );

  const buckets = data?.series ?? [];
  const ctx: BoardContext = {
    tenantId,
    summary: data?.summary,
    previous: data?.previous_summary,
    requestTotals: bucketTotals(buckets),
    providers: data?.providers ?? undefined,
    models: data?.models ?? undefined,
  };

  return (
    <section aria-label="Widget board">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="text-[15px] font-semibold tracking-tight">Widget board</h2>
          <p className="text-[13px] text-muted-foreground">
            {customizing
              ? 'Drag widgets to rearrange, or hold Alt and use the arrow keys. The order is saved in this browser.'
              : 'The same figures as above, rearrangeable.'}
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => setCustomizing((v) => !v)} aria-pressed={customizing}>
          {customizing ? 'Done' : 'Customize'}
        </Button>
      </div>

      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}

      {!isError && (isPending && !data ? (
        <p className="rounded-xl border border-dashed border-border p-8 text-center text-xs text-muted-foreground">
          Loading widgets…
        </p>
      ) : (
        <DraggableWidgetGrid
          items={order}
          editable={customizing}
          maxColumns={4}
          onChange={(next) => {
            setOrder(next as BoardWidget[]);
            try {
              globalThis.localStorage?.setItem(STORAGE_KEY, JSON.stringify(next.map((w) => w.id)));
            } catch {
              /* A full or blocked store must not break the board. */
            }
          }}
          renderItem={(item) => {
            const view = VIEWS[(item as BoardWidget).kind];
            return view ? view(ctx) : null;
          }}
        />
      ))}
    </section>
  );
}

export default WidgetBoard;
