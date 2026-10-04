'use client';

import * as React from 'react';
import { DatabaseZap, RefreshCw, ShieldCheck, Timer, Trash2 } from 'lucide-react';
import { PageHeader } from '@/components/page-header';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { ErrorState, TableSkeleton } from '@/components/ui/state';
import { StatCard } from '@/components/stat-card';
import {
  useCacheInspect,
  useCacheInvalidations,
  useCachePolicies,
  useCacheStats,
  useDeleteCachePolicy,
  useInvalidateCache,
  useUpsertCachePolicy,
} from '@/hooks/use-admin';
import { formatCurrency, formatDurationMs, formatNumber, formatPercent } from '@/lib/format';

export function CacheView() {
  const [tenant, setTenant] = React.useState('');
  const [scope, setScope] = React.useState('tenant');
  const [scopeTarget, setScopeTarget] = React.useState('');
  const [entryModel, setEntryModel] = React.useState('');
  const [entryProvider, setEntryProvider] = React.useState('');
  const { data, isPending, isError, error, refetch, isFetching } = useCacheStats(tenant);
  const policies = useCachePolicies(tenant);
  const inspect = useCacheInspect({ tenantId: tenant || undefined, model: entryModel || undefined, provider: entryProvider || undefined, limit: 15 });
  const invalidations = useCacheInvalidations(tenant);
  const invalidate = useInvalidateCache();
  const upsertPolicy = useUpsertCachePolicy();
  const deletePolicy = useDeleteCachePolicy();

  const [policyName, setPolicyName] = React.useState('');
  const [policyTtl, setPolicyTtl] = React.useState('300');

  const stats = data?.stats;
  const config = (data?.config ?? {}) as Record<string, unknown>;
  const bypassEntries = Object.entries(stats?.bypass_by_reason ?? {}).sort((a, b) => b[1] - a[1]);
  const misses = stats?.misses ?? stats?.exact_misses ?? 0;
  const hits = (stats?.exact_hits ?? 0) + (stats?.prefix_hits ?? 0) + (stats?.semantic_hits ?? 0);
  const totalLookups = hits + misses + (stats?.bypasses ?? 0);
  const missRate = totalLookups > 0 ? misses / totalLookups : 0;
  const bypassRate = totalLookups > 0 ? (stats?.bypasses ?? 0) / totalLookups : 0;
  const tierFlags = [
    `exact ${config.exact_enabled ? 'on' : 'off'}`,
    `prefix ${config.prefix_enabled ? 'on' : 'off'}`,
    `semantic ${config.semantic_enabled ? 'on' : 'off'}`,
  ].join(' · ');
  const health = !data?.enabled
    ? 'disabled'
    : hits + misses === 0 && (stats?.bypasses ?? 0) === 0
      ? 'enabled · no traffic yet'
      : 'enabled · serving';

  const flush = () => {
    if (scope === 'tenant') {
      invalidate.mutate({ tenantId: scopeTarget || tenant, scope: 'tenant' });
    } else if (scope === 'model') {
      invalidate.mutate({ scope: 'model', model: scopeTarget, tenantId: tenant || undefined });
    } else if (scope === 'provider') {
      invalidate.mutate({ scope: 'provider', provider: scopeTarget, tenantId: tenant || undefined });
    } else if (scope === 'key') {
      invalidate.mutate({ scope: 'key', key: scopeTarget, tenantId: tenant || undefined });
    } else {
      invalidate.mutate({ scope: 'all' });
    }
  };

  return (
    <>
      <PageHeader
        title="Cache analytics"
        description="Exact, prefix and semantic hit rates with bypass reasons, reuse counts and latency saved. Tenant-isolated keys; flushes are audited."
        actions={
          <div className="flex items-center gap-2">
            <input
              className="h-8 w-44 rounded-md border border-input bg-background px-2 text-xs"
              placeholder="tenant filter (empty = all)"
              value={tenant}
              onChange={(e) => setTenant(e.target.value)}
            />
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
              <RefreshCw className={isFetching ? 'animate-spin' : undefined} />
              Refresh
            </Button>
          </div>
        }
      />
      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}
      {isPending ? (
        <TableSkeleton rows={3} columns={4} />
      ) : (
        <>
          <div className="grid gap-3 md:grid-cols-4">
            <StatCard label="Hit rate" value={stats ? formatPercent(stats.hit_rate) : '—'} hint={`${formatNumber(hits)} hits / ${formatNumber(misses)} misses`} />
            <StatCard label="Exact hits" value={formatNumber(stats?.exact_hits ?? 0)} hint="byte-identical requests" />
            <StatCard label="Prefix / semantic" value={`${formatNumber(stats?.prefix_hits ?? 0)} / ${formatNumber(stats?.semantic_hits ?? 0)}`} hint="short-prefix vs embedding similarity" />
            <StatCard label="Bypasses" value={formatNumber(stats?.bypasses ?? 0)} hint="sensitive, tools, streaming, policy" />
          </div>
          <div className="mt-3 grid gap-3 md:grid-cols-4">
            <StatCard label="Reuse count" value={formatNumber(stats?.reuse_count ?? 0)} hint="total served-from-cache" />
            <StatCard label="Latency saved" value={formatDurationMs(stats?.latency_saved_ms ?? 0)} hint="provider time avoided" />
            <StatCard label="Cost saved" value={formatCurrency(stats?.cost_saved_usd ?? 0)} hint="billed cost avoided by reuse" />
            <StatCard label="Lookup avg" value={formatDurationMs(stats?.lookup_ms_avg ?? 0)} hint="cache lookup time" />
          </div>
          <div className="mt-3 grid gap-3 md:grid-cols-4">
            <StatCard label="Miss rate" value={formatPercent(missRate)} hint={`${formatNumber(misses)} misses of ${formatNumber(totalLookups)} lookups`} />
            <StatCard label="Bypass rate" value={formatPercent(bypassRate)} hint="streaming, tools, policy, sensitive" />
            <StatCard label="Invalidations" value={formatNumber(stats?.invalidations ?? 0)} hint="flushes this process" />
            <StatCard label="Cache health" value={health} hint={tierFlags} />
          </div>
        </>
      )}

      <div className="mt-4 grid gap-3 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <ShieldCheck className="size-4" />
              Bypass reasons
            </CardTitle>
            <CardDescription>Why requests skipped the cache. A miss without a reason is a bug.</CardDescription>
          </CardHeader>
          <CardContent>
            {bypassEntries.length === 0 ? (
              <p className="text-xs text-muted-foreground">No bypasses recorded yet.</p>
            ) : (
              <ul className="space-y-1.5">
                {bypassEntries.map(([reason, count]) => (
                  <li key={reason} className="flex items-center justify-between text-xs">
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono">{reason}</code>
                    <span className="text-muted-foreground">{formatNumber(count)}</span>
                  </li>
                ))}
              </ul>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Timer className="size-4" />
              Top cached prompts
            </CardTitle>
            <CardDescription>Most-reused entries. Bodies never leave Redis; only hashes and previews show here.</CardDescription>
          </CardHeader>
          <CardContent>
            {(stats?.top_prompts ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">No reuse recorded yet. Repeat a deterministic prompt to populate this.</p>
            ) : (
              <ul className="space-y-2">
                {(stats?.top_prompts ?? []).map((p) => (
                  <li key={p.prompt_hash} className="text-xs">
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate text-foreground">{p.prompt_preview || p.prompt_hash.slice(0, 16)}</span>
                      <span className="shrink-0 text-muted-foreground">{formatNumber(p.hits)} hits</span>
                    </div>
                    <div className="mt-0.5 font-mono text-[10px] text-muted-foreground">
                      {p.model} · {p.prompt_hash.slice(0, 12)}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </CardContent>
        </Card>
      </div>

      <Card className="mt-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <DatabaseZap className="size-4" />
            Invalidate
          </CardTitle>
          <CardDescription>
            Flush by tenant, model, provider or a specific key. Tenant flushes touch only that tenant&apos;s namespace. Recorded in the audit log and published on NATS.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-center gap-2">
          <select
            className="h-9 rounded-md border border-input bg-background px-2 text-sm"
            value={scope}
            onChange={(e) => setScope(e.target.value)}
          >
            <option value="tenant">tenant</option>
            <option value="model">model</option>
            <option value="provider">provider</option>
            <option value="key">key</option>
            <option value="all">all</option>
          </select>
          {scope !== 'all' ? (
            <input
              className="h-9 w-64 rounded-md border border-input bg-background px-3 text-sm"
              placeholder={scope === 'tenant' ? 'tenant id (empty = all)' : scope === 'key' ? 'exact cache key (tenant filter required)' : `${scope} name or key`}
              value={scopeTarget}
              onChange={(e) => setScopeTarget(e.target.value)}
            />
          ) : null}
          <Button variant="destructive" size="sm" disabled={invalidate.isPending} onClick={flush}>
            <Trash2 />
            {invalidate.isPending ? 'Invalidating…' : 'Invalidate'}
          </Button>
          {invalidate.data ? (
            <span className="text-xs text-muted-foreground">
              removed {invalidate.data.invalidated}{invalidate.data.scope ? ` (${invalidate.data.scope})` : null}
            </span>
          ) : null}
          {invalidate.isError ? <span className="text-xs text-danger">failed: {String((invalidate.error as Error)?.message)}</span> : null}
        </CardContent>
      </Card>

      <div className="mt-4 grid gap-3 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Cache policies</CardTitle>
            <CardDescription>Per-scope rules overriding the global config. Key &gt; endpoint &gt; tenant+model &gt; tenant+provider &gt; tenant &gt; global.</CardDescription>
          </CardHeader>
          <CardContent>
            {policies.isPending ? (
              <TableSkeleton rows={2} columns={3} />
            ) : (policies.data ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">No per-scope policies. Global config applies.</p>
            ) : (
              <ul className="space-y-2">
                {(policies.data ?? []).map((p) => (
                  <li key={p.id} className="flex items-center justify-between gap-2 text-xs">
                    <div>
                      <span className="font-medium">{p.name}</span>
                      <span className="ml-2 text-muted-foreground">
                        {p.enabled ? 'enabled' : 'disabled'} · ttl {p.ttl_seconds}s
                        {p.tenant_id ? ` · tenant ${p.tenant_id.slice(0, 8)}` : ''}
                        {p.model ? ` · model ${p.model}` : ''}
                        {p.provider ? ` · provider ${p.provider}` : ''}
                      </span>
                    </div>
                    <Button variant="ghost" size="sm" disabled={deletePolicy.isPending} onClick={() => deletePolicy.mutate(p.id)}>
                      Delete
                    </Button>
                  </li>
                ))}
              </ul>
            )}
            <div className="mt-3 flex items-center gap-2">
              <input
                className="h-8 w-40 rounded-md border border-input bg-background px-2 text-xs"
                placeholder="policy name"
                value={policyName}
                onChange={(e) => setPolicyName(e.target.value)}
              />
              <input
                className="h-8 w-24 rounded-md border border-input bg-background px-2 text-xs"
                placeholder="ttl (s)"
                value={policyTtl}
                onChange={(e) => setPolicyTtl(e.target.value)}
              />
              <Button
                variant="outline"
                size="sm"
                disabled={upsertPolicy.isPending || !policyName}
                onClick={() =>
                  upsertPolicy.mutate({
                    name: policyName,
                    tenant_id: tenant || undefined,
                    ttl_seconds: Number(policyTtl) || 300,
                    enabled: true,
                  })
                }
              >
                {upsertPolicy.isPending ? 'Saving…' : 'Add policy'}
              </Button>
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>TTL &amp; tiers</CardTitle>
            <CardDescription>Global defaults. Exact reuse is the reliable tier; semantic stays behind a high threshold.</CardDescription>
          </CardHeader>
          <CardContent className="text-xs">
            <dl className="grid grid-cols-2 gap-x-4 gap-y-1.5">
              <dt className="text-muted-foreground">response_cache</dt>
              <dd className="font-mono">{String(config.response_cache ?? '—')}</dd>
              <dt className="text-muted-foreground">exact_enabled</dt>
              <dd className="font-mono">{String(config.exact_enabled ?? '—')}</dd>
              <dt className="text-muted-foreground">response_ttl</dt>
              <dd className="font-mono">{String(config.response_ttl ?? '—')}</dd>
              <dt className="text-muted-foreground">semantic_enabled</dt>
              <dd className="font-mono">{String(config.semantic_enabled ?? '—')}</dd>
              <dt className="text-muted-foreground">semantic_threshold</dt>
              <dd className="font-mono">{String(config.semantic_threshold ?? '—')}</dd>
              <dt className="text-muted-foreground">prefix_enabled</dt>
              <dd className="font-mono">{String(config.prefix_enabled ?? '—')}</dd>
              <dt className="text-muted-foreground">prefix_length</dt>
              <dd className="font-mono">{String(config.prefix_length ?? '—')}</dd>
              <dt className="text-muted-foreground">bypass_tool_requests</dt>
              <dd className="font-mono">{String(config.bypass_tool_requests ?? '—')}</dd>
              <dt className="text-muted-foreground">allow_nondeterministic</dt>
              <dd className="font-mono">{String(config.allow_nondeterministic ?? '—')}</dd>
            </dl>
          </CardContent>
        </Card>
      </div>

      <div className="mt-4 grid gap-3 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Recent entries</CardTitle>
            <CardDescription>Metadata only. Bodies stay in Redis. Filter by model or provider to slice by serving context.</CardDescription>
          </CardHeader>
          <CardContent>
            <div className="mb-2 flex items-center gap-2">
              <input
                className="h-8 w-36 rounded-md border border-input bg-background px-2 text-xs"
                placeholder="model filter"
                value={entryModel}
                onChange={(e) => setEntryModel(e.target.value)}
              />
              <input
                className="h-8 w-36 rounded-md border border-input bg-background px-2 text-xs"
                placeholder="provider filter"
                value={entryProvider}
                onChange={(e) => setEntryProvider(e.target.value)}
              />
            </div>
            {inspect.isPending ? (
              <TableSkeleton rows={3} columns={3} />
            ) : (inspect.data ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">No entry metadata recorded yet.</p>
            ) : (
              <ul className="space-y-2">
                {(inspect.data ?? []).map((e) => (
                  <li key={e.cache_key} className="text-xs">
                    <div className="flex items-center justify-between gap-2">
                      <code className="rounded bg-muted px-1 py-0.5 font-mono text-[10px]">{e.kind}</code>
                      <span className="truncate">{e.prompt_preview || e.prompt_hash.slice(0, 16)}</span>
                      <span className="shrink-0 text-muted-foreground">{e.hit_count + (e.reuse_count ?? 0)} hits</span>
                    </div>
                    <div className="mt-0.5 font-mono text-[10px] text-muted-foreground">
                      {e.model}{e.provider ? ` · ${e.provider}` : ''}{e.tenant_id ? ` · tenant ${e.tenant_id.slice(0, 8)}` : ''} · {e.cache_key.slice(-12)}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Invalidation events</CardTitle>
            <CardDescription>Every flush, with scope, reason and actor.</CardDescription>
          </CardHeader>
          <CardContent>
            {invalidations.isPending ? (
              <TableSkeleton rows={3} columns={3} />
            ) : (invalidations.data ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">No flushes recorded yet.</p>
            ) : (
              <ul className="space-y-1.5">
                {(invalidations.data ?? []).map((ev) => (
                  <li key={ev.id} className="flex items-center justify-between gap-2 text-xs">
                    <span>
                      <code className="rounded bg-muted px-1 py-0.5 font-mono text-[10px]">{ev.scope}</code>{' '}
                      {ev.target} · {ev.reason}
                    </span>
                    <span className="shrink-0 text-muted-foreground">removed {ev.removed}</span>
                  </li>
                ))}
              </ul>
            )}
          </CardContent>
        </Card>
      </div>

      {!data?.enabled && !isPending ? (
        <p className="mt-3 text-xs text-muted-foreground">Caching is disabled in the gateway config. Enable response, prefix or semantic caches to populate this view.</p>
      ) : null}
    </>
  );
}
