'use client';

import { Check, Copy, Download, RefreshCw, Search } from 'lucide-react';
import { usePathname } from 'next/navigation';
import * as React from 'react';

import { PageHeader } from '@/components/page-header';
import { RangePicker, useWindowParam, windowToFromParam } from '@/components/range-picker';
import { TenantPicker, useTenantParam } from '@/components/tenant-picker';
import { Badge, OutcomeBadge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { Input, Select } from '@/components/ui/input';
import { EmptyState, ErrorState, TableSkeleton } from '@/components/ui/state';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useRequests } from '@/hooks/use-admin';
import { toCsv } from '@/lib/api';
import { formatCurrency, formatDateTime, formatDurationMs, formatNumber, truncateMiddle } from '@/lib/format';
import type { RequestLog } from '@/lib/types';

const PAGE_SIZE = 50;

const OUTCOMES = ['', 'success', 'fallback', 'error', 'rejected', 'canceled'] as const;

/**
 * Copies the full request id, not the truncated text shown in the table.
 * The id is what the Replay page accepts, so one click bridges the two views.
 */
function CopyRequestId({ requestId }: { requestId: string }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <button
      type="button"
      onClick={(event) => {
        event.stopPropagation();
        void navigator.clipboard
          .writeText(requestId)
          .then(() => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1500);
          })
          .catch(() => undefined);
      }}
      title={copied ? 'Copied' : 'Copy full request id'}
      aria-label={copied ? 'Copied request id' : 'Copy full request id'}
      className="rounded p-0.5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
    >
      {copied ? <Check className="size-3" /> : <Copy className="size-3" />}
    </button>
  );
}

const EXPORT_COLUMNS: Array<keyof RequestLog> = [
  'created_at',
  'request_id',
  'tenant_id',
  'requested_model',
  'routed_model',
  'provider',
  'strategy',
  'status',
  'outcome',
  'error_code',
  'latency_ms',
  'attempts',
  'fallback_used',
  'prompt_tokens',
  'completion_tokens',
  'cost_usd',
];

export function RequestsView() {
  const pathname = usePathname();
  const window = useWindowParam();
  const tenantId = useTenantParam();
  const from = windowToFromParam(window);

  const [outcome, setOutcome] = React.useState('');
  const [provider, setProvider] = React.useState('');
  const [search, setSearch] = React.useState('');
  const [appliedSearch, setAppliedSearch] = React.useState('');
  const [offset, setOffset] = React.useState(0);

  // Changing any filter must reset pagination, or the operator lands on an empty
  // page five of a result set that only has two pages.
  React.useEffect(() => {
    setOffset(0);
  }, [outcome, provider, appliedSearch, tenantId, from]);

  const { data, isPending, isError, error, refetch, isFetching } = useRequests({
    tenantId,
    from,
    outcome: outcome || undefined,
    provider: provider || undefined,
    search: appliedSearch || undefined,
    limit: PAGE_SIZE,
    offset,
  });

  const rows = data?.requests ?? [];
  const hasNext = rows.length === PAGE_SIZE;
  const hasPrevious = offset > 0;
  const hasActiveFilters =
    tenantId !== '' || outcome !== '' || provider !== '' || appliedSearch !== '' || window !== '24h';

  const onClearFilters = () => {
    setOutcome('');
    setProvider('');
    setSearch('');
    setAppliedSearch('');
    // The tenant lives in the URL (shared links), so clearing it navigates.
    // globalThis: the local `window` binding is the time-window param.
    globalThis.location.assign(`${pathname}?window=${window}`);
  };

  const exportCsv = () => {
    if (rows.length === 0) return;
    const csv = toCsv(rows as unknown as Array<Record<string, unknown>>, EXPORT_COLUMNS as string[]);
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = `synapass-requests-${new Date().toISOString().slice(0, 19).replace(/:/g, '-')}.csv`;
    anchor.click();
    // Revoking immediately is safe: the click has already started the download.
    URL.revokeObjectURL(url);
  };

  return (
    <>
      <PageHeader
        title="Requests"
        description="One row per inference request, as recorded after the response completed. Provider is the upstream that actually served the request, which is not necessarily the one the policy preferred — check the fallback badge."
        actions={
          <>
            <TenantPicker value={tenantId} />
            <RangePicker value={window} />
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
              <RefreshCw className={isFetching ? 'animate-spin' : undefined} />
              Refresh
            </Button>
            <Button variant="outline" size="sm" onClick={exportCsv} disabled={rows.length === 0}>
              <Download />
              Export CSV
            </Button>
          </>
        }
      />

      <Card className="mb-4">
        <CardContent className="flex flex-col gap-3 pt-4 sm:flex-row sm:flex-wrap sm:items-end sm:pt-6">
          <label className="flex w-full flex-col gap-1 text-xs text-muted-foreground sm:w-auto">
            Outcome
            <Select value={outcome} onChange={(event) => setOutcome(event.target.value)} className="w-full sm:w-36">
              {OUTCOMES.map((value) => (
                <option key={value || 'all'} value={value}>
                  {value === '' ? 'All outcomes' : value}
                </option>
              ))}
            </Select>
          </label>

          <label className="flex w-full flex-col gap-1 text-xs text-muted-foreground sm:w-auto">
            Provider
            <Input
              value={provider}
              onChange={(event) => setProvider(event.target.value)}
              placeholder="exact provider name"
              className="w-full sm:w-48"
            />
          </label>

          <form
            className="flex w-full flex-col gap-1 text-xs text-muted-foreground sm:w-auto"
            onSubmit={(event) => {
              event.preventDefault();
              setAppliedSearch(search.trim());
            }}
          >
            Search
            <div className="flex w-full gap-2">
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder="request id, model, error…"
                className="min-w-0 flex-1 sm:w-64 sm:flex-none"
              />
              <Button type="submit" variant="secondary" size="default" className="shrink-0">
                <Search />
                Apply
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}

      {!isError ? (
        <Card>
          {isPending ? (
            <TableSkeleton rows={10} columns={8} />
          ) : rows.length === 0 ? (
            <div className="m-5">
              <EmptyState
                title="No requests match"
                description="Either nothing was served in this window, or the filters are excluding everything. Widen the window or clear a filter."
              />
              {hasActiveFilters ? (
                <div className="mt-3 flex justify-center">
                  <Button variant="outline" size="sm" onClick={onClearFilters}>
                    Clear filters
                  </Button>
                </div>
              ) : null}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Time</TableHead>
                  <TableHead>Request</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Provider</TableHead>
                  <TableHead>Outcome</TableHead>
                  <TableHead className="text-right">Latency</TableHead>
                  <TableHead className="text-right">Tokens</TableHead>
                  <TableHead className="text-right">Cost</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={row.id || row.request_id}>
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      {formatDateTime(row.created_at)}
                    </TableCell>
                    <TableCell className="font-mono text-xs" title={row.request_id}>
                      <span className="inline-flex items-center gap-1">
                        {truncateMiddle(row.request_id, 20)}
                        <CopyRequestId requestId={row.request_id} />
                      </span>
                    </TableCell>
                    <TableCell className="text-xs">
                      <div className="font-medium">{row.routed_model || '—'}</div>
                      {row.requested_model && row.requested_model !== row.routed_model ? (
                        <div className="text-muted-foreground">asked for {row.requested_model}</div>
                      ) : null}
                    </TableCell>
                    <TableCell className="text-xs">{row.provider || '—'}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap items-center gap-1">
                        <OutcomeBadge outcome={row.outcome} />
                        {row.fallback_used ? <Badge tone="warning">fallback</Badge> : null}
                        {row.error_code ? <Badge tone="danger">{row.error_code}</Badge> : null}
                      </div>
                    </TableCell>
                    <TableCell className="text-right text-xs tabular-nums">
                      {formatDurationMs(row.latency_ms)}
                      {row.attempts > 1 ? (
                        <span className="text-muted-foreground"> · {row.attempts} attempts</span>
                      ) : null}
                    </TableCell>
                    <TableCell className="text-right text-xs tabular-nums">
                      {formatNumber(row.prompt_tokens)} / {formatNumber(row.completion_tokens)}
                    </TableCell>
                    <TableCell className="text-right text-xs tabular-nums">
                      {formatCurrency(row.cost_usd)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}

          {rows.length > 0 ? (
            <div className="flex flex-col gap-2 border-t border-border px-4 py-3 text-xs text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
              <span className="break-words">
                Showing {offset + 1}–{offset + rows.length}
                {data?.limit ? ` · page size ${data.limit}` : ''}
              </span>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!hasPrevious}
                  onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                  className="flex-1 sm:flex-none"
                >
                  Previous
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!hasNext}
                  onClick={() => setOffset(offset + PAGE_SIZE)}
                  className="flex-1 sm:flex-none"
                >
                  Next
                </Button>
              </div>
            </div>
          ) : null}
        </Card>
      ) : null}
    </>
  );
}
