'use client';

/**
 * The response viewer.
 *
 * Three readings of the same run are offered because they answer different
 * questions: the rendered answer is what the user would see, the raw JSON is
 * what the gateway actually returned, and the request view is what was sent.
 * A failure is rendered as prominently as a success — a testing console that
 * only looks good when things work is not a testing console.
 */
import { AlertTriangle, Check, Copy, Loader2, RotateCcw, Sparkles } from 'lucide-react';
import * as React from 'react';

import { Markdown } from '@/components/playground/markdown';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { EmptyState } from '@/components/ui/state';
import { formatDurationMs, formatNumber } from '@/lib/format';
import {
  buildChatBody,
  buildCurl,
  intentHeaders,
  type PlaygroundConfig,
  type PlaygroundError,
  type PlaygroundMessage,
  type PlaygroundRun,
  type StreamState,
} from '@/lib/playground';
import { cn } from '@/lib/utils';

type View = 'answer' | 'raw' | 'request';

const VIEWS: Array<{ id: View; label: string }> = [
  { id: 'answer', label: 'Answer' },
  { id: 'raw', label: 'Raw response' },
  { id: 'request', label: 'Request' },
];

/**
 * A next step for a failure, keyed on what the gateway or proxy reported.
 *
 * The point of the Playground is diagnosing routing, so a bare error message is
 * not enough — each one here says what to try next. An unrecognised code still
 * gets the message and status; guessing at a cause would be worse than silence.
 */
export function failureHint(error: PlaygroundError): string | null {
  switch (error.code) {
    case 'not_authenticated':
      return 'The dashboard session expired. Sign in again, then re-run.';
    case 'playground_missing_api_key':
      return 'Paste a tenant API key in the Transport section. Mint one on the API keys page if you have none.';
    case 'playground_invalid_request':
      return 'Fix the highlighted configuration and run again — nothing was sent.';
    case 'playground_cancelled':
      return 'Cancelled before the gateway finished. Run again to get a full answer.';
    case 'dashboard_upstream_unreachable':
      return 'The dashboard could not reach the gateway. Check that it is running and that SYNAPASS_API_URL points at it.';
    case 'invalid_api_key':
      return 'The gateway rejected the key. Check it is complete, active and not expired or revoked.';
    case 'unknown_model':
    case 'model_not_found':
      return 'The gateway does not know this model. Pick one from the registry or check the alias.';
    case 'not_found':
      return 'If you selected an endpoint scope, confirm its slug on the Endpoints page.';
    case 'permission_denied':
    case 'forbidden':
      return 'If you selected an endpoint scope, it may be disabled. Enable it or clear the selection.';
    case 'rate_limited':
      return 'Rate limited upstream. Wait for the Retry-After window or try another provider.';
    case 'quota_exceeded':
    case 'budget_exceeded':
      return 'A budget or quota blocked this request. Raise it on Budgets, or run as a different key.';
    case 'provider_unavailable':
      return 'No healthy provider for this model. Check Providers, or drop the no-fallback constraint.';
    case 'upstream_error':
      return 'The provider answered badly. Re-run to see whether it fails over, or compare another provider.';
    case 'all_providers_failed':
      return 'Every candidate failed. Inspect Providers, then re-run with debug on to read the attempts.';
    default:
      break;
  }
  if (error.status === 401) return 'The credential was rejected. Check the key and its status.';
  if (error.status === 403) return 'The key or scope is not permitted for this request.';
  if (error.status === 404) return 'Confirm the model name and any endpoint scope slug.';
  if (error.status === 429) return 'Rate limited. Wait and re-run, or use another provider.';
  if (error.status === 502 || error.status === 503) {
    return 'A provider failed. Check Providers, then re-run to see the fallback decide.';
  }
  if (error.status >= 500) return 'The gateway reported a server fault. Re-run with debug on for detail.';
  return null;
}

function Chip({
  label,
  value,
  tone = 'neutral',
}: {
  label: string;
  value: React.ReactNode;
  tone?: 'neutral' | 'success' | 'warning' | 'danger' | 'info';
}) {
  return (
    <span
      className={cn(
        'inline-flex items-baseline gap-1.5 rounded-full border px-2 py-0.5 text-[11px]',
        tone === 'success'
          ? 'border-emerald-500/25 bg-emerald-500/10'
          : tone === 'warning'
            ? 'border-amber-500/25 bg-amber-500/10'
            : tone === 'danger'
              ? 'border-rose-500/25 bg-rose-500/10'
              : tone === 'info'
                ? 'border-sky-500/25 bg-sky-500/10'
                : 'border-white/[0.08] bg-white/[0.03]',
      )}
    >
      <span className="text-muted-foreground">{label}</span>
      <span className="font-mono text-neutral-100">{value}</span>
    </span>
  );
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = React.useState(false);
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      // Clipboard access can be denied; the text stays selectable on screen.
      setCopied(false);
    }
  };
  return (
    <Button variant="ghost" size="sm" onClick={() => void onCopy()}>
      {copied ? <Check /> : <Copy />}
      {copied ? 'Copied' : label}
    </Button>
  );
}

function ErrorCard({ error, onRetry }: { error: PlaygroundError; onRetry?: () => void }) {
  const hint = failureHint(error);
  return (
    <div className="rounded-xl border border-danger/30 bg-danger/[0.06] p-4">
      <div className="flex items-center gap-2">
        <AlertTriangle className="size-4 shrink-0 text-danger" />
        <p className="text-sm font-medium text-white">The run failed</p>
        <div className="ml-auto flex flex-wrap items-center gap-1.5">
          {error.status > 0 ? <Badge tone="danger">HTTP {error.status}</Badge> : null}
          {error.code ? <Badge tone="outline">{error.code}</Badge> : null}
          {error.type ? <Badge tone="neutral">{error.type}</Badge> : null}
          {onRetry ? (
            <button
              type="button"
              onClick={onRetry}
              title="Run again with the same configuration"
              className="flex items-center gap-1 rounded-md bg-white/10 px-2 py-1 text-[11px] font-medium text-white transition-colors hover:bg-white/20"
            >
              <RotateCcw className="size-3" />
              Retry
            </button>
          ) : null}
        </div>
      </div>
      <p className="mt-2 break-words text-[13px] leading-relaxed text-neutral-200">{error.message}</p>
      {hint ? <p className="mt-1.5 text-[12px] leading-relaxed text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

export function ResponsePanel({
  config,
  messages,
  baseUrl,
  partial,
  running,
  run,
  elapsedMs,
  firstTokenMs,
  onRetry,
}: {
  config: PlaygroundConfig;
  messages: PlaygroundMessage[];
  baseUrl: string;
  /** Live state for the primary lane while a run is in flight. */
  partial: StreamState;
  running: boolean;
  /** The most recent completed primary-lane run, or null before the first. */
  run: PlaygroundRun | null;
  elapsedMs: number;
  /** First-token timing for the in-flight run, 0 until the first token. */
  firstTokenMs?: number;
  /** Re-run the failed run with its recorded configuration. */
  onRetry?: () => void;
}) {
  const [view, setView] = React.useState<View>('answer');

  const body = React.useMemo(() => buildChatBody(config, messages).body, [config, messages]);
  const intent = React.useMemo(() => intentHeaders(config), [config]);
  const curl = React.useMemo(() => buildCurl(config, messages, baseUrl), [config, messages, baseUrl]);

  const answer = running ? partial.content : (run?.content ?? '');
  const toolCalls = React.useMemo(
    () => (running ? partial.toolCalls : (run?.toolCalls ?? [])),
    [running, partial.toolCalls, run],
  );

  const usage = running ? partial.usage : (run?.usage ?? null);
  const route = running ? partial.route : (run?.route ?? partial.route);
  const error = !running ? (run?.error ?? null) : null;

  const rawText = run?.raw ? JSON.stringify(run.raw, null, 2) : '';
  const requestText = JSON.stringify(
    { target: `${baseUrl.replace(/\/+$/, '')}/v1/chat/completions`, headers: intent, body },
    null,
    2,
  );

  const copyText = view === 'raw' ? rawText : view === 'request' ? requestText : answer;

  return (
    <div className="flex min-h-[420px] flex-col overflow-hidden rounded-2xl border border-white/[0.07] bg-card/95">
      <div className="flex flex-wrap items-center gap-2 border-b border-white/[0.06] px-4 py-3">
        <div className="flex gap-1 rounded-lg border border-white/[0.07] bg-white/[0.02] p-0.5">
          {VIEWS.map((item) => (
            <button
              key={item.id}
              type="button"
              onClick={() => setView(item.id)}
              className={cn(
                'rounded-md px-2.5 py-1 text-[11px] font-medium transition-colors',
                view === item.id
                  ? 'bg-white/[0.08] text-foreground'
                  : 'text-muted-foreground hover:text-foreground',
              )}
            >
              {item.label}
            </button>
          ))}
        </div>

        {running ? (
          <span className="inline-flex items-center gap-1.5 text-[11px] text-primary">
            <Loader2 className="size-3 animate-spin" />
            streaming · {formatDurationMs(elapsedMs)}
            {firstTokenMs != null && firstTokenMs > 0
              ? ` · first token ${formatDurationMs(firstTokenMs)}`
              : ''}
          </span>
        ) : run ? (
          <span className="text-[11px] text-muted-foreground">
            {formatDurationMs(run.durationMs)}
            {run.firstTokenMs > 0 ? ` · first token ${formatDurationMs(run.firstTokenMs)}` : ''}
          </span>
        ) : null}

        <div className="ml-auto flex items-center gap-1">
          {route.provider ? <Chip label="provider" value={route.provider} tone="info" /> : null}
          {route.model ? <Chip label="model" value={route.model} /> : null}
          {route.cache_hit ? <Chip label="cache" value="hit" tone="success" /> : null}
          {route.fallback_used ? <Chip label="fallback" value="used" tone="warning" /> : null}
          {usage ? <Chip label="tokens" value={formatNumber(usage.total_tokens)} /> : null}
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto p-4 sm:p-5">
        {error ? (
          <ErrorCard error={error} onRetry={onRetry} />
        ) : view === 'answer' ? (
          answer ? (
            <Markdown source={answer} />
          ) : running ? (
            <p className="flex items-center gap-2 text-[13px] text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" />
              Waiting for the first token…
            </p>
          ) : (
            <EmptyState
              title="No run yet"
              description="Choose a model, write a prompt and run it. The response, its routing decision and its token usage appear here."
              className="border-0 bg-transparent p-6"
            />
          )
        ) : (
          <pre className="overflow-x-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-neutral-300">
            {view === 'raw' ? rawText || 'No response body — the run has not completed.' : requestText}
          </pre>
        )}

        {toolCalls.length > 0 && view === 'answer' ? (
          <div className="mt-4 rounded-xl border border-sky-500/25 bg-sky-500/[0.05] p-3">
            <p className="flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wide text-sky-300">
              <Sparkles className="size-3.5" />
              Tool calls the model asked for
            </p>
            <ul className="mt-2 space-y-2">
              {toolCalls.map((call, index) => (
                <li key={`${call.name}-${index}`} className="font-mono text-[11px] text-neutral-200">
                  <span className="text-sky-300">{call.name}</span>
                  {call.arguments ? (
                    <span className="text-muted-foreground">({call.arguments})</span>
                  ) : (
                    <span className="text-muted-foreground"> …</span>
                  )}
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center gap-1 border-t border-white/[0.06] px-3 py-2">
        <CopyButton text={copyText} label={view === 'answer' ? 'Copy answer' : 'Copy JSON'} />
        <CopyButton text={curl} label="Copy as curl" />
        <p className="ml-auto truncate px-2 text-[11px] text-muted-foreground">
          {intent['X-Synapass-Endpoint']
            ? `scope ${intent['X-Synapass-Endpoint']}`
            : 'no endpoint scope'}
        </p>
      </div>
    </div>
  );
}
