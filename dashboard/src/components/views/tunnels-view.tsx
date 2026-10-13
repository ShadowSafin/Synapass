'use client';

import * as React from 'react';
import { Check, Copy, Globe, Play, RefreshCw, RotateCcw, Square } from 'lucide-react';
import { PageHeader } from '@/components/page-header';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { EmptyState, ErrorState, TableSkeleton } from '@/components/ui/state';
import { StatCard } from '@/components/stat-card';
import {
  useCreateTunnel,
  useRestartTunnel,
  useStopTunnel,
  useTunnelHistory,
  useTunnelStatus,
} from '@/hooks/use-admin';
import { formatDateTime, formatNumber } from '@/lib/format';
import type { TunnelSession } from '@/lib/types';

function StatusBadge({ status }: { status: string }) {
  const tone =
    status === 'running'
      ? 'bg-success/15 text-success'
      : status === 'starting'
        ? 'bg-warning/15 text-warning'
        : status === 'failed'
          ? 'bg-danger/15 text-danger'
          : 'bg-muted text-muted-foreground';
  return (
    <span className={`rounded-full px-2 py-0.5 text-[11px] font-medium ${tone}`}>
      {status}
    </span>
  );
}

function CopyURL({ url }: { url: string }) {
  const [copied, setCopied] = React.useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(url);
    } catch {
      const area = document.createElement('textarea');
      area.value = url;
      document.body.appendChild(area);
      area.select();
      document.execCommand('copy');
      document.body.removeChild(area);
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  };
  return (
    <span className="flex min-w-0 items-center gap-2">
      <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs">
        {url}
      </code>
      <Button variant="outline" size="sm" onClick={() => void copy()}>
        {copied ? <Check /> : <Copy />}
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </span>
  );
}

function sessionDuration(s: TunnelSession): string {
  if (!s.started_at) return '—';
  const start = new Date(s.started_at).getTime();
  const end = s.stopped_at ? new Date(s.stopped_at).getTime() : Date.now();
  if (Number.isNaN(start) || Number.isNaN(end)) return '—';
  const secs = Math.max(0, Math.round((end - start) / 1000));
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ${secs % 60}s`;
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
}

export function TunnelsView() {
  const { data, isPending, isError, error, refetch, isFetching } = useTunnelStatus();
  const history = useTunnelHistory(20);
  const create = useCreateTunnel();
  const stop = useStopTunnel();
  const restart = useRestartTunnel();

  const [target, setTarget] = React.useState('gateway');
  const [custom, setCustom] = React.useState('127.0.0.1:8080');

  const enabled = data?.enabled ?? false;
  const active = data?.active ?? null;
  const sessions = history.data ?? [];
  const busy = create.isPending || stop.isPending || restart.isPending;
  const requestedTarget = target === 'custom' ? custom.trim() : target;
  const exposesAdmin = requestedTarget !== '' && requestedTarget !== 'gateway';

  const onCreate = () => {
    if (!requestedTarget) return;
    create.mutate({ target: requestedTarget });
  };

  return (
    <>
      <PageHeader
        title="Public tunnels"
        description="Disposable Cloudflare URLs that expose one local service to the internet. No port forwarding; every request through the tunnel still passes Synapass auth, policy and rate limits."
        actions={
          <Button variant="outline" size="sm" onClick={() => { void refetch(); void history.refetch(); }} disabled={isFetching}>
            <RefreshCw className={isFetching ? 'animate-spin' : undefined} />
            Refresh
          </Button>
        }
      />
      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}

      {!isError && !enabled && !isPending ? (
        <Card className="mt-4">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Globe className="size-4" />
              Tunnels are disabled
            </CardTitle>
            <CardDescription>
              Set <code className="font-mono">tunnel.enabled: true</code> (or{' '}
              <code className="font-mono">SYNAPASS_TUNNEL_ENABLED=true</code>), then restart
              the gateway. Nothing is exposed until you create a tunnel below. The
              Docker gateway image already includes{' '}
              <code className="font-mono">cloudflared</code>; native installs need it
              on <code className="font-mono">PATH</code> (or set{' '}
              <code className="font-mono">tunnel.binary</code> to its absolute path).
            </CardDescription>
          </CardHeader>
        </Card>
      ) : null}

      {enabled && data && !data.binary_available && !isPending ? (
        <Card className="mt-4">
          <CardHeader>
            <CardTitle>cloudflared is missing</CardTitle>
            <CardDescription>
              {data.binary_error ?? 'The gateway cannot find the cloudflared executable.'}{' '}
              Install it from https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/
              or point <code className="font-mono">tunnel.binary</code> at its absolute path.
            </CardDescription>
          </CardHeader>
        </Card>
      ) : null}

      {isPending ? (
        <TableSkeleton rows={3} columns={4} />
      ) : enabled ? (
        <>
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <StatCard label="Status" value={active ? active.status : 'idle'} hint={active ? `target ${active.target}` : 'no tunnel running'} />
            <StatCard label="Public URL" value={active?.public_url ? 'live' : '—'} hint={active?.public_url ?? 'create a tunnel to mint one'} />
            <StatCard label="Reconnects" value={formatNumber(active?.reconnects ?? 0)} hint="supervisor restarts this session" />
            <StatCard label="Session age" value={active ? sessionDuration(active) : '—'} hint={active ? `since ${formatDateTime(active.started_at)}` : 'no active session'} />
          </div>

          {active ? (
            <Card className="mt-4">
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <Globe className="size-4" />
                  Active tunnel
                  <StatusBadge status={active.status} />
                </CardTitle>
                <CardDescription>
                  Exposing <code className="font-mono">{active.target_addr}</code> as{' '}
                  <code className="font-mono">{active.target}</code>. Anyone with the URL can
                  reach Synapass; auth and policy still apply.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-3">
                {active.public_url ? <CopyURL url={active.public_url} /> : (
                  <p className="text-xs text-muted-foreground">Waiting for cloudflared to print the public URL…</p>
                )}
                {active.last_error ? (
                  <p className="text-xs text-danger">{active.last_error}</p>
                ) : null}
                <div className="flex flex-wrap items-center gap-2">
                  <Button variant="outline" size="sm" disabled={busy} onClick={() => restart.mutate({})}>
                    <RotateCcw />
                    {restart.isPending ? 'Restarting…' : 'Restart (new URL)'}
                  </Button>
                  <Button variant="destructive" size="sm" disabled={busy} onClick={() => stop.mutate({})}>
                    <Square />
                    {stop.isPending ? 'Stopping…' : 'Stop'}
                  </Button>
                  {stop.isError ? <span className="text-xs text-danger">failed: {String((stop.error as Error)?.message)}</span> : null}
                  {restart.isError ? <span className="text-xs text-danger">failed: {String((restart.error as Error)?.message)}</span> : null}
                </div>
              </CardContent>
            </Card>
          ) : (
            <Card className="mt-4">
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <Play className="size-4" />
                  Create a tunnel
                </CardTitle>
                <CardDescription>
                  Mints a disposable <code className="font-mono">*.trycloudflare.com</code> URL.
                  A restart always mints a new URL; the old one stops working.
                </CardDescription>
              </CardHeader>
              <CardContent className="space-y-3">
                <div className="flex flex-wrap items-center gap-2">
                  <select
                    className="h-9 rounded-md border border-input bg-background px-2 text-sm"
                    value={target}
                    onChange={(e) => setTarget(e.target.value)}
                  >
                    <option value="gateway">gateway — inference API</option>
                    <option value="dashboard">dashboard — this UI</option>
                    <option value="custom">custom loopback host:port</option>
                  </select>
                  {target === 'custom' ? (
                    <input
                      className="h-9 min-w-0 w-full flex-1 rounded-md border border-input bg-background px-3 text-sm sm:w-56 sm:flex-none"
                      placeholder="127.0.0.1:8080"
                      value={custom}
                      onChange={(e) => setCustom(e.target.value)}
                    />
                  ) : null}
                  <Button size="sm" disabled={busy || !requestedTarget} onClick={onCreate}>
                    <Play />
                    {create.isPending ? 'Creating…' : 'Create tunnel'}
                  </Button>
                  {create.isError ? <span className="text-xs text-danger">failed: {String((create.error as Error)?.message)}</span> : null}
                </div>
                {exposesAdmin ? (
                  <p className="text-xs text-warning">
                    Warning: this target exposes more than the inference API. Anyone with the URL
                    still needs a valid API key or admin credential — nothing is anonymously
                    accessible — but prefer the gateway target unless you need the UI remotely.
                  </p>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    The gateway target exposes the inference API (keys required) plus health
                    probes, which are unauthenticated by design.
                  </p>
                )}
              </CardContent>
            </Card>
          )}

          <Card className="mt-4">
            <CardHeader>
              <CardTitle>Session history</CardTitle>
              <CardDescription>What was exposed, when, and how each session ended.</CardDescription>
            </CardHeader>
            <CardContent>
              {history.isPending ? (
                <TableSkeleton rows={3} columns={4} />
              ) : sessions.length === 0 ? (
                <EmptyState title="No tunnel sessions yet" description="Created tunnels are recorded here with their URL, target and teardown reason." />
              ) : (
                <ul className="space-y-2">
                  {sessions.map((s) => (
                    <li key={s.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                      <StatusBadge status={s.status} />
                      <code className="font-mono">{s.target}</code>
                      <span className="min-w-0 flex-1 truncate font-mono text-muted-foreground">
                        {s.public_url || 'no URL captured'}
                      </span>
                      <span className="text-muted-foreground">{formatDateTime(s.created_at)}</span>
                      {s.reconnects > 0 ? <span className="text-muted-foreground">{s.reconnects} reconnects</span> : null}
                      {s.last_error ? <span className="w-full text-danger">{s.last_error}</span> : null}
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        </>
      ) : null}
    </>
  );
}
