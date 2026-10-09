'use client';

/**
 * The Playground conversation rendered as a chat.
 *
 * User turns sit right in a solid pill; assistant turns sit left in a dark
 * glass card so the existing Markdown rendering (authored for dark surfaces)
 * stays legible on the light gradient page. Each assistant turn carries the
 * routing chips the console is for — provider, model, tokens, latency — fed
 * from `metaById`, which the view records when a run completes. A run in
 * flight streams into the same card shape, and a failed run renders as an
 * error card with the same next-step hint the response panel shows.
 */
import { AlertTriangle, Check, Copy, Loader2, RotateCcw, Sparkles, X } from 'lucide-react';
import * as React from 'react';

import { Markdown } from '@/components/playground/markdown';
import { failureHint } from '@/components/playground/response-panel';
import { Badge } from '@/components/ui/badge';
import { formatDurationMs, formatNumber } from '@/lib/format';
import type {
  PlaygroundMessage,
  PlaygroundRun,
  PlaygroundToolCall,
} from '@/lib/playground';
import { cn } from '@/lib/utils';

/** Routing facts shown under an assistant turn, captured when its run completed. */
export interface ChatMeta {
  provider: string;
  model: string;
  totalTokens: number | null;
  latencyMs: number;
  cacheHit: boolean;
  fallbackUsed: boolean;
}

function CopyAnswer({ text }: { text: string }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <button
      type="button"
      title={copied ? 'Copied' : 'Copy answer'}
      aria-label={copied ? 'Copied' : 'Copy answer'}
      onClick={() => {
        void navigator.clipboard
          .writeText(text)
          .then(() => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1500);
          })
          .catch(() => undefined);
      }}
      className="flex size-7 items-center justify-center rounded-lg text-neutral-400 transition-colors hover:bg-white/10 hover:text-white"
    >
      {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
    </button>
  );
}

function RetryButton({ onRetry, disabled }: { onRetry: () => void; disabled: boolean }) {
  return (
    <button
      type="button"
      onClick={onRetry}
      disabled={disabled}
      title="Run again with the same configuration"
      className="flex h-7 items-center gap-1.5 rounded-lg bg-white/10 px-2.5 text-[12px] font-medium text-white transition-colors hover:bg-white/20 disabled:opacity-40"
    >
      <RotateCcw className="size-3.5" />
      Retry run
    </button>
  );
}

/** A completed user turn. Memoized: turns never change, so streaming chunks
 * must not re-render the whole thread on every frame. */
const UserTurn = React.memo(function UserTurn({
  message,
  onDelete,
  disabled,
}: {
  message: PlaygroundMessage;
  onDelete: (id: string) => void;
  disabled: boolean;
}) {
  return (
    <div className="group flex items-start justify-end gap-2">
      <button
        type="button"
        onClick={() => onDelete(message.id)}
        disabled={disabled}
        aria-label="Remove this message"
        title="Remove this message"
        className="mt-2 flex size-6 shrink-0 items-center justify-center rounded-full bg-neutral-950/60 text-white/70 opacity-0 backdrop-blur transition-all hover:bg-neutral-950 hover:text-white focus-visible:opacity-100 group-hover:opacity-100 disabled:opacity-0"
      >
        <X className="size-3" />
      </button>
      <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-neutral-950 px-4 py-2.5 text-[13px] leading-relaxed text-white shadow-lg shadow-neutral-950/20">
        {message.content}
      </div>
    </div>
  );
});

/** A completed assistant turn. Memoized for the same reason as UserTurn. */
const AssistantTurn = React.memo(function AssistantTurn({
  message,
  meta,
  onDelete,
  disabled,
}: {
  message: PlaygroundMessage;
  meta: ChatMeta | undefined;
  onDelete: (id: string) => void;
  disabled: boolean;
}) {
  return (
    <div className="group flex items-start gap-2">
      <div className="min-w-0 flex-1 rounded-2xl rounded-bl-md border border-white/15 bg-neutral-950/70 shadow-xl shadow-neutral-950/20 backdrop-blur-md">
        <div className="px-4 pt-3">
          <Markdown source={message.content} />
        </div>
        <div className="flex flex-wrap items-center gap-2 px-4 pb-3 pt-1">
          <div className="min-w-0 flex-1">{meta ? <MetaChips meta={meta} /> : null}</div>
          <CopyAnswer text={message.content} />
          <button
            type="button"
            onClick={() => onDelete(message.id)}
            disabled={disabled}
            aria-label="Remove this answer"
            title="Remove this answer"
            className="flex size-7 items-center justify-center rounded-lg text-neutral-400 opacity-0 transition-all hover:bg-white/10 hover:text-white focus-visible:opacity-100 group-hover:opacity-100 disabled:opacity-0"
          >
            <X className="size-3.5" />
          </button>
        </div>
      </div>
    </div>
  );
});
function MetaChips({ meta }: { meta: ChatMeta }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {meta.provider ? <Badge tone="info">{meta.provider}</Badge> : null}
      {meta.model ? <Badge tone="neutral">{meta.model}</Badge> : null}
      {meta.totalTokens != null ? (
        <Badge tone="outline">{formatNumber(meta.totalTokens)} tok</Badge>
      ) : null}
      {meta.latencyMs > 0 ? <Badge tone="neutral">{formatDurationMs(meta.latencyMs)}</Badge> : null}
      {meta.cacheHit ? <Badge tone="success">cache hit</Badge> : null}
      {meta.fallbackUsed ? <Badge tone="warning">fallback</Badge> : null}
    </div>
  );
}

function ToolCalls({ calls }: { calls: PlaygroundToolCall[] }) {
  if (calls.length === 0) return null;
  return (
    <div className="mt-3 rounded-xl border border-sky-400/25 bg-sky-400/[0.07] p-3">
      <p className="flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wide text-sky-300">
        <Sparkles className="size-3.5" />
        Tool calls the model asked for
      </p>
      <ul className="mt-2 space-y-1.5">
        {calls.map((call, index) => (
          <li key={`${call.name}-${index}`} className="font-mono text-[11px] text-neutral-200">
            <span className="text-sky-300">{call.name}</span>
            {call.arguments ? (
              <span className="break-words text-neutral-400">({call.arguments})</span>
            ) : (
              <span className="text-neutral-500"> …</span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

export function ChatTranscript({
  messages,
  streaming,
  streamContent,
  streamTools,
  streamFirstTokenMs,
  failedRun,
  metaById,
  onDelete,
  onRetry,
  disabled,
  retryDisabled,
}: {
  messages: PlaygroundMessage[];
  /** A primary-lane run is in flight; its text streams into the trailing card. */
  streaming: boolean;
  streamContent: string;
  streamTools: PlaygroundToolCall[];
  /** First-token timing for the in-flight run, 0 until the first token. */
  streamFirstTokenMs?: number;
  /** The latest primary run when it failed — rendered as an error card. */
  failedRun: PlaygroundRun | null;
  metaById: Record<string, ChatMeta>;
  onDelete: (id: string) => void;
  /** Re-run the failed run with its recorded configuration. */
  onRetry?: () => void;
  disabled: boolean;
  retryDisabled?: boolean;
}) {
  const bottomRef = React.useRef<HTMLDivElement>(null);
  // Whether the viewport is pinned near the page bottom. While a stream is
  // arriving the transcript grows under the reader; yanking scroll on every
  // chunk would trap anyone trying to re-read an earlier turn.
  const pinned = React.useRef(true);

  React.useEffect(() => {
    const onScroll = () => {
      pinned.current =
        window.innerHeight + window.scrollY >
        document.documentElement.scrollHeight - 320;
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    return () => window.removeEventListener('scroll', onScroll);
  }, []);

  const messageCount = messages.length;
  React.useEffect(() => {
    if (pinned.current) bottomRef.current?.scrollIntoView({ block: 'end' });
  }, [messageCount, streaming, streamContent, failedRun]);

  return (
    <div className="space-y-4 py-6">
      {messages.map((message) => {
        if (message.role === 'user') {
          return <UserTurn key={message.id} message={message} onDelete={onDelete} disabled={disabled} />;
        }

        if (message.role === 'assistant') {
          return (
            <AssistantTurn
              key={message.id}
              message={message}
              meta={metaById[message.id]}
              onDelete={onDelete}
              disabled={disabled}
            />
          );
        }

        // System and tool turns only arrive via history loads or manual edits
        // elsewhere; render them as quiet full-width rows rather than dropping
        // content the next run will actually send.
        return (
          <div
            key={message.id}
            className="flex items-start gap-2 rounded-xl border border-white/10 bg-neutral-950/45 px-3 py-2 backdrop-blur-md"
          >
            <span className="shrink-0 rounded-full border border-white/15 px-2 py-px font-mono text-[10px] uppercase tracking-wide text-neutral-300">
              {message.role}
            </span>
            <p className="min-w-0 flex-1 whitespace-pre-wrap break-words text-[12px] leading-relaxed text-neutral-200">
              {message.content || <em className="text-neutral-400">empty — this will block the run</em>}
            </p>
            <button
              type="button"
              onClick={() => onDelete(message.id)}
              disabled={disabled}
              aria-label="Remove this message"
              title="Remove this message"
              className="flex size-6 shrink-0 items-center justify-center rounded-full text-neutral-400 transition-colors hover:bg-white/10 hover:text-white disabled:opacity-40"
            >
              <X className="size-3" />
            </button>
          </div>
        );
      })}

      {streaming ? (
        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1 rounded-2xl rounded-bl-md border border-white/15 bg-neutral-950/70 shadow-xl shadow-neutral-950/20 backdrop-blur-md">
            <div className="px-4 pt-3">
              {streamContent ? (
                <Markdown source={streamContent} />
              ) : (
                <p className="flex items-center gap-2 py-1 text-[13px] text-neutral-300">
                  <Loader2 className="size-3.5 animate-spin" />
                  Waiting for the first token…
                </p>
              )}
              <ToolCalls calls={streamTools} />
            </div>
            <div className="flex items-center gap-1.5 px-4 pb-3 pt-2">
              <span className="size-1.5 animate-bounce rounded-full bg-neutral-300 [animation-delay:-0.2s]" />
              <span className="size-1.5 animate-bounce rounded-full bg-neutral-300 [animation-delay:-0.1s]" />
              <span className="size-1.5 animate-bounce rounded-full bg-neutral-300" />
              {streamFirstTokenMs != null && streamFirstTokenMs > 0 ? (
                <span className="ml-1 text-[11px] text-neutral-400">
                  first token {formatDurationMs(streamFirstTokenMs)}
                </span>
              ) : null}
              {streamContent ? (
                <span className="ml-auto">
                  <CopyAnswer text={streamContent} />
                </span>
              ) : null}
            </div>
          </div>
        </div>
      ) : null}

      {!streaming && failedRun?.error?.code === 'playground_cancelled' ? (
        <div className="rounded-2xl border border-amber-300/25 bg-neutral-950/70 p-4 shadow-xl backdrop-blur-md">
          <div className="flex items-center gap-2">
            <p className="text-sm font-medium text-white">Run stopped</p>
            <div className="ml-auto flex flex-wrap items-center gap-1.5">
              {failedRun.error.code ? <Badge tone="outline">{failedRun.error.code}</Badge> : null}
              {onRetry ? <RetryButton onRetry={onRetry} disabled={retryDisabled ?? false} /> : null}
            </div>
          </div>
          <p className="mt-1.5 break-words text-[12px] leading-relaxed text-neutral-300">
            {failedRun.error.message} — the text below is what arrived before the stop.
          </p>
          {failedRun.content ? (
            <div className="mt-2 rounded-xl border border-white/10 bg-black/30 px-3 py-2">
              <Markdown source={failedRun.content} />
            </div>
          ) : null}
        </div>
      ) : null}

      {!streaming && failedRun?.error && failedRun.error.code !== 'playground_cancelled' ? (
        <div className="rounded-2xl border border-rose-300/25 bg-rose-950/70 p-4 shadow-xl backdrop-blur-md">
          <div className="flex items-center gap-2">
            <AlertTriangle className="size-4 shrink-0 text-rose-300" />
            <p className="text-sm font-medium text-white">The run failed</p>
            <div className="ml-auto flex flex-wrap items-center gap-1.5">
              {failedRun.error.status > 0 ? (
                <Badge tone="danger">HTTP {failedRun.error.status}</Badge>
              ) : null}
              {failedRun.error.code ? <Badge tone="outline">{failedRun.error.code}</Badge> : null}
              {onRetry ? <RetryButton onRetry={onRetry} disabled={retryDisabled ?? false} /> : null}
            </div>
          </div>
          <p className="mt-2 break-words text-[13px] leading-relaxed text-rose-100">
            {failedRun.error.message}
          </p>
          {failureHint(failedRun.error) ? (
            <p className={cn('mt-1.5 text-[12px] leading-relaxed text-rose-200/70')}>
              {failureHint(failedRun.error)}
            </p>
          ) : null}
        </div>
      ) : null}

      <div ref={bottomRef} aria-hidden />
    </div>
  );
}
