'use client';

/**
 * The Playground: a chat-first testing console.
 *
 * The page shows only a chat — a centered composer over a gradient, with the
 * turns above it. Everything else (credential and transport, target, sampling,
 * tools, routing constraints, compare lane, route and debug metadata, curl,
 * session history) lives in the Advanced slide-over opened from the top bar.
 *
 * It still runs real traffic. There is no second inference path and no fixture
 * layer: a run is a `POST /v1/chat/completions` through a session-gated proxy,
 * so it exercises the same policy engine, cache, retries and provider adapters
 * as a production client. Each completed answer is appended to the
 * conversation, so follow-up turns carry the same history a real client would
 * send — and each answer shows the routing chips behind it.
 *
 * Compare mode varies only the target, because comparing two answers that
 * differ in both prompt and target tells you nothing about either. One submit
 * sends the same prompt to both lanes concurrently; the answers stay in the
 * lane panels below so the shared thread is not biased toward either lane,
 * while single-lane answers are appended to the thread as before.
 */
import {
  ImageOff,
  KeyRound,
  Play,
  SlidersHorizontal,
  Square,
} from 'lucide-react';
import * as React from 'react';

import { AdvancedDrawer } from '@/components/playground/advanced-drawer';
import { advancedSummary } from '@/components/playground/advanced-popover';
import { ChatTranscript, type ChatMeta } from '@/components/playground/chat-transcript';
import { newMessage } from '@/components/playground/conversation';
import { ResponsePanel } from '@/components/playground/response-panel';
import { PromptInput } from '@/components/ui/ai-chat-input';
import { useEndpoints, useModels, usePolicies } from '@/hooks/use-admin';
import { usePlayground } from '@/hooks/use-playground';
import { formatDurationMs } from '@/lib/format';
import {
  DEFAULT_CONFIG,
  buildChatBody,
  type PlaygroundConfig,
  type PlaygroundMessage,
  type PlaygroundRun,
} from '@/lib/playground';
import { cn } from '@/lib/utils';

/**
 * The sunset behind the chat. Pure CSS, no image assets: a cool periwinkle top
 * falling through lavender and pink into peach, with a hot orange glow blooming
 * from the bottom edge.
 */
const PAGE_BACKGROUND: React.CSSProperties = {
  backgroundImage:
    'radial-gradient(70% 50% at 50% 108%, rgba(255,92,0,1) 0%, rgba(255,122,10,0.65) 38%, rgba(255,122,10,0) 70%), linear-gradient(180deg, #a8c4f4 0%, #bcaef0 26%, #d9a9e6 44%, #f0a6d6 60%, #f7b3c2 74%, #f9bd8c 88%, #f69c52 100%)',
};

/**
 * The gateway address a browser can reach, for the "copy as curl" command.
 *
 * Compose inlines the public URL into the bundle; the server's address is only
 * used as a fallback, because inside Docker it is a service name no browser
 * (and no pasted curl) can resolve.
 */
function useGatewayUrl(): string {
  const baked = process.env.NEXT_PUBLIC_SYNAPASS_API_URL;
  const [url, setUrl] = React.useState(baked && baked.length > 0 ? baked : '');
  React.useEffect(() => {
    let cancelled = false;
    fetch('/api/client-config')
      .then((response) => (response.ok ? response.json() : null))
      .then((data: { gateway_url?: string } | null) => {
        if (cancelled || !data?.gateway_url) return;
        setUrl((current) => (current ? current : data.gateway_url as string));
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);
  return url || 'http://localhost:8080';
}

/** Fields the compare lane is allowed to change; everything else is shared. */
export interface CompareTarget {
  model: string;
  endpoint: string;
  policy: string;
  temperature: number;
  maxTokens: number;
}

const EMPTY_TARGET: CompareTarget = {
  model: '',
  endpoint: '',
  policy: '',
  temperature: -1,
  maxTokens: 0,
};

/** Metrics worth comparing side by side, in the order an operator reads them. */
function ComparisonTable({ a, b }: { a: PlaygroundRun | null; b: PlaygroundRun | null }) {
  const rows: Array<{ label: string; a: string; b: string; winner?: 'a' | 'b' }> = [
    {
      label: 'Provider',
      a: a?.route.provider || '—',
      b: b?.route.provider || '—',
    },
    { label: 'Model', a: a?.route.model || '—', b: b?.route.model || '—' },
    {
      label: 'Duration',
      a: a ? formatDurationMs(a.durationMs) : '—',
      b: b ? formatDurationMs(b.durationMs) : '—',
      winner: a && b ? (a.durationMs <= b.durationMs ? 'a' : 'b') : undefined,
    },
    {
      label: 'First token',
      a: a && a.firstTokenMs > 0 ? formatDurationMs(a.firstTokenMs) : '—',
      b: b && b.firstTokenMs > 0 ? formatDurationMs(b.firstTokenMs) : '—',
      winner:
        a && b && a.firstTokenMs > 0 && b.firstTokenMs > 0
          ? a.firstTokenMs <= b.firstTokenMs
            ? 'a'
            : 'b'
          : undefined,
    },
    {
      label: 'Tokens (total)',
      a: a?.usage ? String(a.usage.total_tokens) : '—',
      b: b?.usage ? String(b.usage.total_tokens) : '—',
    },
    {
      label: 'Cache',
      a: a ? (a.route.cache_hit ? 'hit' : 'miss') : '—',
      b: b ? (b.route.cache_hit ? 'hit' : 'miss') : '—',
    },
    {
      label: 'Attempts',
      a: a && a.route.attempts > 0 ? String(a.route.attempts) : '—',
      b: b && b.route.attempts > 0 ? String(b.route.attempts) : '—',
    },
    {
      label: 'Result',
      a: a?.error ? (a.error.status > 0 ? `HTTP ${a.error.status}` : a.error.code) : a ? 'ok' : '—',
      b: b?.error ? (b.error.status > 0 ? `HTTP ${b.error.status}` : b.error.code) : b ? 'ok' : '—',
    },
  ];

  return (
    <div className="overflow-x-auto rounded-2xl border border-white/15 bg-neutral-950/70 shadow-xl backdrop-blur-md">
      <table className="w-full text-[12px]">
        <thead>
          <tr className="border-b border-white/10 text-left text-[11px] uppercase tracking-wide text-neutral-400">
            <th className="px-3 py-2 font-medium">Metric</th>
            <th className="px-3 py-2 font-medium">Lane A</th>
            <th className="px-3 py-2 font-medium">Lane B</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-white/[0.06]">
          {rows.map((row) => (
            <tr key={row.label}>
              <td className="px-3 py-1.5 text-neutral-400">{row.label}</td>
              <td
                className={cn(
                  'px-3 py-1.5 font-mono text-neutral-200',
                  row.winner === 'a' && 'text-emerald-300',
                )}
              >
                {row.a}
              </td>
              <td
                className={cn(
                  'px-3 py-1.5 font-mono text-neutral-200',
                  row.winner === 'b' && 'text-emerald-300',
                )}
              >
                {row.b}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function PlaygroundView() {
  const models = useModels();
  const endpoints = useEndpoints();
  const policies = usePolicies();

  const [config, setConfig] = React.useState<PlaygroundConfig>(DEFAULT_CONFIG);
  const [compareTarget, setCompareTarget] = React.useState<CompareTarget>(EMPTY_TARGET);
  const [messages, setMessages] = React.useState<PlaygroundMessage[]>(() => []);
  const [metaById, setMetaById] = React.useState<Record<string, ChatMeta>>({});
  const [apiKey, setApiKey] = React.useState('');
  const [providerFilter, setProviderFilter] = React.useState('');
  const [compareOn, setCompareOn] = React.useState(false);
  const [draft, setDraft] = React.useState('');
  const [drawerOpen, setDrawerOpen] = React.useState(false);
  const [notice, setNotice] = React.useState<string | null>(null);

  const { running, partial, runs, run, cancel, clearHistory, liveFirstTokenMs } = usePlayground();

  // Guards late completions: starting a new chat (or loading history) while a
  // run is in flight must not append that run's answer to the fresh thread.
  const epoch = React.useRef(0);

  const [elapsed, setElapsed] = React.useState(0);
  React.useEffect(() => {
    if (!(running.primary || running.compare)) {
      setElapsed(0);
      return undefined;
    }
    const started = Date.now();
    setElapsed(0);
    const id = window.setInterval(() => setElapsed(Date.now() - started), 120);
    return () => window.clearInterval(id);
  }, [running.primary, running.compare]);

  // Escape stops generation like the Cancel button does. Ignored when
  // nothing is running or when focus is in a select/menu.
  React.useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      if (!(running.primary || running.compare)) return;
      const target = event.target as HTMLElement | null;
      if (target && (target.tagName === 'SELECT' || target.tagName === 'OPTION')) return;
      event.preventDefault();
      cancel('primary');
      cancel('compare');
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [running.primary, running.compare, cancel]);

  const gatewayUrl = useGatewayUrl();
  const latest = React.useMemo(() => {
    const primary = runs.find((item) => item.lane === 'primary') ?? null;
    const compare = runs.find((item) => item.lane === 'compare') ?? null;
    return { primary, compare };
  }, [runs]);

  const patch = React.useCallback((next: Partial<PlaygroundConfig>) => {
    setConfig((current) => ({ ...current, ...next }));
  }, []);

  const patchCompareTarget = React.useCallback((next: Partial<CompareTarget>) => {
    setCompareTarget((current) => ({ ...current, ...next }));
  }, []);

  /**
   * Lane B inherits the whole lane A config and overrides only the target.
   *
   * A bare spread of an empty target would wipe the model (and the endpoint /
   * policy scopes) with empty strings, failing the build and silently skipping
   * the lane — so every field falls back to lane A when unset.
   */
  const withTarget = React.useCallback(
    (laneA: PlaygroundConfig): PlaygroundConfig => ({
      ...laneA,
      model: compareTarget.model.trim() || laneA.model,
      endpoint: compareTarget.endpoint || laneA.endpoint,
      policy: compareTarget.policy || laneA.policy,
    }),
    [compareTarget],
  );

  // The composer's model menu keeps its selection in local state and reads the
  // first entry on mount, so the config is seeded from the registry. Starting
  // empty would show a selected model in the menu while blocking the run for a
  // model the UI believes it already picked.
  React.useEffect(() => {
    if (config.model.trim()) return;
    const list = models.data ?? [];
    const preferred = list.find((item) => item.status === 'active') ?? list[0];
    if (preferred) patch({ model: preferred.name });
  }, [models.data, config.model, patch]);

  // A compare lane with no model runs nothing, so seed lane B from the
  // registry once it (or compare mode) arrives — a different model than lane A
  // when there is one, so the first compare is a real comparison.
  React.useEffect(() => {
    if (!compareOn || compareTarget.model.trim()) return;
    const list = models.data ?? [];
    if (list.length === 0) return;
    const laneA = config.model.trim();
    const seed = list.find((item) => item.name !== laneA) ?? list[0];
    if (seed) setCompareTarget((current) => ({ ...current, model: seed.name }));
  }, [compareOn, compareTarget.model, models.data, config.model]);

  /** Enabling compare mode with an empty lane B seeds it, for the same reason. */
  const onCompareChange = React.useCallback(
    (value: boolean) => {
      setCompareOn(value);
      if (!value) return;
      setCompareTarget((current) => {
        if (current.model.trim()) return current;
        const list = models.data ?? [];
        if (list.length === 0) return current;
        const laneA = config.model.trim();
        const seed = list.find((item) => item.name !== laneA) ?? list[0];
        return seed ? { ...current, model: seed.name } : current;
      });
    },
    [models.data, config.model],
  );

  /**
   * Models offered in the composer's menu, current target first.
   *
   * The ordering matters because the component reads `models[0]` once, when it
   * mounts, and is keyed on the model so it re-reads whenever the target changes
   * externally (a history load, for example).
   */
  const composerModels = React.useMemo(() => {
    const list = models.data ?? [];
    const visible = providerFilter
      ? list.filter((item) => item.provider_id === providerFilter)
      : list;
    const current = config.model.trim();
    const rest = visible.map((item) => item.name).filter((name) => name !== current);
    return current ? [current, ...rest] : rest;
  }, [models.data, providerFilter, config.model]);

  /**
   * Lane B options for the composer-row picker: the same registry list (and
   * provider filter) as lane A, current lane B target first so a filtered-out
   * or custom value is never lost from the menu.
   */
  const compareModels = React.useMemo(() => {
    const list = models.data ?? [];
    const visible = providerFilter
      ? list.filter((item) => item.provider_id === providerFilter)
      : list;
    const current = compareTarget.model.trim();
    const rest = visible.map((item) => item.name).filter((name) => name !== current);
    return current ? [current, ...rest] : rest;
  }, [models.data, providerFilter, compareTarget.model]);

  // Declared here because the callbacks below both read it and list it as a
  // dependency; its value depends only on the key field.
  const missingKey = apiKey.trim().length === 0;

  /** Run the conversation exactly as it stands — the fix-up path when a first
   * attempt was blocked (no key, no model) and the operator has corrected it.
   * In compare mode both lanes run; neither answer joins the thread. */
  const onRunNow = React.useCallback(() => {
    if (missingKey || running.primary || running.compare) return;
    const result = buildChatBody(config, messages);
    if (result.error) {
      setNotice(result.error);
      return;
    }
    if (compareOn) {
      const laneB = withTarget(config);
      const other = buildChatBody(laneB, messages);
      if (other.error) {
        setNotice(`Lane B cannot run: ${other.error}`);
        return;
      }
      setNotice(null);
      void run({ lane: 'primary', config, messages, apiKey });
      void run({ lane: 'compare', config: laneB, messages, apiKey });
      return;
    }
    setNotice(null);
    const seen = epoch.current;
    void run({ lane: 'primary', config, messages, apiKey }).then((entry) => {
      if (seen !== epoch.current) return;
      if (entry.error || !entry.content) return;
      const assistant = newMessage('assistant', entry.content);
      setMessages((current) => [...current, assistant]);
      setMetaById((current) => ({ ...current, [assistant.id]: metaFor(entry) }));
    });
  }, [apiKey, compareOn, config, messages, missingKey, run, running.compare, running.primary, withTarget]);

  /**
   * Submit from the composer.
   *
   * The model comes from the composer's own menu rather than from state, so the
   * menu the operator is looking at is the one that decides — and the config is
   * updated from it, keeping the drawer and the next run in agreement. A
   * completed single-lane answer is appended to the thread with its routing
   * facts, so the next turn carries the full history a real client would send.
   * In compare mode the same prompt is sent to both lanes concurrently and both
   * answers stay in the lane panels, so the thread never favours either lane.
   */
  const onComposerSubmit = React.useCallback(
    (text: string, meta: { model: string; effort: string; attachments: File[] }) => {
      setNotice(null);
      // A run is already in flight: submitting again would start a duplicate
      // generation on the same lane and orphan the first controller.
      if (running.primary || running.compare) return;
      // In compare mode the lane A picker above the composer owns the model —
      // the composer's hidden menu keeps a stale copy that must not win.
      const chosen = meta.model.trim();
      const base = compareOn ? config : chosen ? { ...config, model: chosen } : config;
      if (!compareOn && chosen && chosen !== config.model) setConfig(base);

      const nextMessages = [...messages, newMessage('user', text)];
      setMessages(nextMessages);

      if (meta.attachments.length > 0) {
        setNotice(
          `${meta.attachments.length} image${meta.attachments.length === 1 ? '' : 's'} attached but not sent — this composer sends text only.`,
        );
      }

      if (missingKey) {
        setNotice(
          'Add an API key in Advanced → Connection. The inference API authenticates with a tenant key, so no run can be sent without one.',
        );
        return;
      }
      const result = buildChatBody(base, nextMessages);
      if (result.error) {
        setNotice(result.error);
        return;
      }
      if (compareOn) {
        const laneB = withTarget(base);
        const other = buildChatBody(laneB, nextMessages);
        if (other.error) {
          setNotice(
            `Lane B cannot run: ${other.error} Pick a model for lane B above the composer or in Advanced → Lane B.`,
          );
          return;
        }
        void run({ lane: 'primary', config: base, messages: nextMessages, apiKey });
        void run({ lane: 'compare', config: laneB, messages: nextMessages, apiKey });
        return;
      }
      const seen = epoch.current;
      void run({ lane: 'primary', config: base, messages: nextMessages, apiKey }).then((entry) => {
        if (seen !== epoch.current) return;
        if (entry.error || !entry.content) return;
        const assistant = newMessage('assistant', entry.content);
        setMessages((current) => [...current, assistant]);
        setMetaById((current) => ({ ...current, [assistant.id]: metaFor(entry) }));
      });
    },
    [apiKey, compareOn, config, messages, missingKey, run, running.compare, running.primary, withTarget],
  );

  const compareConfig = React.useMemo(() => withTarget(config), [config, withTarget]);

  const build = buildChatBody(config, messages);
  // Shown only once the thread exists: an empty composer needs no lecture, and
  // the key hint arrives as a notice on the first submit instead.
  const blockedReason =
    messages.length === 0
      ? null
      : missingKey
        ? 'Add an API key in Advanced → Connection — the inference API authenticates with a tenant key.'
        : build.error;

  const onRunAll = React.useCallback(() => {
    if (!apiKey.trim()) return;
    const laneBError = buildChatBody(compareConfig, messages).error;
    if (laneBError) {
      setNotice(
        `Lane B cannot run: ${laneBError} Pick a model for lane B above the composer or in Advanced → Lane B.`,
      );
      return;
    }
    if (!buildChatBody(config, messages).error) {
      void run({ lane: 'primary', config, messages, apiKey });
    }
    void run({ lane: 'compare', config: compareConfig, messages, apiKey });
  }, [apiKey, config, compareConfig, messages, run]);

  /** Put a past run's configuration and messages back in the composer. */
  const onLoad = React.useCallback((item: PlaygroundRun) => {
    epoch.current += 1;
    setMessages(item.messages);
    setMetaById({});
    setNotice(null);
    setDrawerOpen(false);
    if (item.lane === 'compare') {
      // A compare run's config is lane B's target, not lane A's: restore it
      // there so re-running compares the same two targets.
      setCompareTarget({
        model: item.config.model,
        endpoint: item.config.endpoint,
        policy: item.config.policy,
        temperature: -1,
        maxTokens: 0,
      });
      setCompareOn(true);
      return;
    }
    setConfig(item.config);
  }, []);

  const onRerun = React.useCallback(
    (item: PlaygroundRun) => {
      if (!apiKey.trim()) return;
      if (running.primary || running.compare) return;
      void run({ lane: item.lane, config: item.config, messages: item.messages, apiKey });
    },
    [apiKey, run, running.primary, running.compare],
  );

  const onNewChat = React.useCallback(() => {
    epoch.current += 1;
    setMessages([]);
    setMetaById({});
    setDraft('');
    setNotice(null);
  }, []);

  const onDeleteMessage = React.useCallback((id: string) => {
    setMessages((current) => current.filter((message) => message.id !== id));
    setMetaById((current) => {
      if (!(id in current)) return current;
      const next = { ...current };
      delete next[id];
      return next;
    });
  }, []);

  const isBusy = running.primary || running.compare;
  const hasConversation = messages.length > 0 || running.primary || latest.primary != null;
  const advancedActive = advancedSummary(config) !== 'all defaults';

  const failedRun =
    !running.primary && latest.primary?.error ? latest.primary : null;

  return (
    <div
      className="flex min-h-[calc(100dvh-6rem)] flex-col lg:min-h-[calc(100dvh-2.5rem)]"
      style={PAGE_BACKGROUND}
    >
      {/* Top bar ----------------------------------------------------------- */}
      <div className="mx-auto flex w-full max-w-3xl flex-wrap items-center gap-2 px-4 pt-5 sm:px-6">
        <div className="min-w-0 flex-1">
          <h1 className="text-[15px] font-semibold tracking-tight text-neutral-900">
            Playground
          </h1>
          <p className="truncate text-[11px] text-neutral-800/70">
            Live gateway traffic · history stays in this session
          </p>
        </div>

        <span
          title={missingKey ? 'No API key — open Advanced to add one' : 'API key set for this session'}
          className="flex items-center gap-1.5 rounded-full bg-neutral-950/70 px-2.5 py-1 text-[11px] text-white backdrop-blur"
        >
          <span
            className={cn('size-1.5 rounded-full', missingKey ? 'bg-amber-400' : 'bg-emerald-400')}
            aria-hidden
          />
          {missingKey ? 'No key' : 'Key set'}
        </span>

        <button
          type="button"
          onClick={() => setDrawerOpen(true)}
          aria-haspopup="dialog"
          title="Credential, target, sampling, tools, routing, history"
          className="relative flex h-8 items-center gap-1.5 rounded-full bg-neutral-950 px-3 text-xs font-medium text-white shadow-lg transition-colors hover:bg-neutral-800"
        >
          <SlidersHorizontal className="size-3.5" />
          Advanced
          {advancedActive ? (
            <span className="absolute -right-0.5 -top-0.5 size-2 rounded-full bg-amber-400 ring-2 ring-white/60" aria-hidden />
          ) : null}
        </button>
      </div>

      {/* Chat column ------------------------------------------------------- */}
      <div
        className={cn(
          'mx-auto flex w-full max-w-3xl flex-1 flex-col px-4 sm:px-6',
          !hasConversation && 'justify-center',
        )}
      >
        {hasConversation ? (
          <ChatTranscript
            messages={messages}
            // In compare mode both answers stream in the lane panels below;
            // echoing lane A into the thread would duplicate it there.
            streaming={compareOn ? false : running.primary}
            streamContent={compareOn ? '' : partial.primary.content}
            streamTools={compareOn ? [] : partial.primary.toolCalls}
            streamFirstTokenMs={compareOn ? 0 : liveFirstTokenMs.primary}
            failedRun={compareOn ? null : failedRun}
            metaById={metaById}
            onDelete={onDeleteMessage}
            onRetry={
              failedRun && !isBusy ? () => onRerun(failedRun) : undefined
            }
            disabled={isBusy}
            retryDisabled={isBusy || missingKey}
          />
        ) : null}

        <div
          className={cn(
            'flex flex-col items-center gap-2.5',
            hasConversation ? 'sticky bottom-4 z-20 pb-2' : 'py-2',
          )}
        >
          {compareOn ? (
            <div className="flex w-full max-w-[480px] items-center gap-1.5">
              <label className="flex min-w-0 flex-1 items-center gap-1.5 rounded-full bg-neutral-950/80 py-1 pl-2.5 pr-1 text-white shadow backdrop-blur">
                <span className="shrink-0 text-[10px] font-bold uppercase tracking-wide text-white/60">
                  A
                </span>
                <select
                  value={config.model}
                  onChange={(event) => patch({ model: event.target.value })}
                  disabled={isBusy}
                  title="Lane A model"
                  className="min-w-0 flex-1 cursor-pointer truncate bg-transparent text-[11px] font-medium outline-none disabled:opacity-50 [&>option]:text-neutral-900"
                >
                  {composerModels.map((name) => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </select>
              </label>
              <span aria-hidden className="shrink-0 text-[10px] font-bold uppercase tracking-wide text-neutral-800/60">
                vs
              </span>
              <label className="flex min-w-0 flex-1 items-center gap-1.5 rounded-full bg-neutral-950/80 py-1 pl-2.5 pr-1 text-white shadow backdrop-blur">
                <span className="shrink-0 text-[10px] font-bold uppercase tracking-wide text-white/60">
                  B
                </span>
                <select
                  value={compareTarget.model}
                  onChange={(event) => patchCompareTarget({ model: event.target.value })}
                  disabled={isBusy}
                  title="Lane B model — the same prompt runs against both at once"
                  className="min-w-0 flex-1 cursor-pointer truncate bg-transparent text-[11px] font-medium outline-none disabled:opacity-50 [&>option]:text-neutral-900"
                >
                  {compareModels.map((name) => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </select>
              </label>
            </div>
          ) : null}
          <PromptInput
            key={config.model || 'no-model'}
            value={draft}
            onChange={setDraft}
            onSubmit={onComposerSubmit}
            models={composerModels}
            placeholder="Ask anything..."
            className="w-full"
            hideModelSelect={compareOn}
          />

          {running.primary || running.compare ? (
            <button
              type="button"
              onClick={() => {
                cancel('primary');
                cancel('compare');
              }}
              className="flex h-7 items-center gap-1.5 rounded-full bg-rose-950/90 px-3 text-[11px] font-medium text-white shadow backdrop-blur transition-colors hover:bg-rose-900"
            >
              <Square className="size-3" />
              {compareOn ? 'Cancel runs' : 'Cancel run'}
            </button>
          ) : messages.length > 0 && !blockedReason ? (
            <button
              type="button"
              onClick={onRunNow}
              title="Send the conversation as it stands, without adding a turn"
              className="flex h-7 items-center gap-1.5 rounded-full bg-neutral-950/80 px-3 text-[11px] font-medium text-white shadow backdrop-blur transition-colors hover:bg-neutral-950"
            >
              <Play className="size-3" />
              Run
            </button>
          ) : null}

          {notice ? (
            <p className="flex max-w-full items-start gap-1.5 rounded-2xl bg-neutral-950/85 px-3.5 py-2 text-[11px] leading-relaxed text-amber-200 shadow backdrop-blur">
              <ImageOff className="mt-px size-3.5 shrink-0" />
              <span>{notice}</span>
            </p>
          ) : blockedReason ? (
            <p className="flex max-w-full items-start gap-1.5 rounded-2xl bg-neutral-950/70 px-3.5 py-2 text-[11px] leading-relaxed text-neutral-200 shadow backdrop-blur">
              <KeyRound className="mt-px size-3.5 shrink-0" />
              <span>{blockedReason}</span>
            </p>
          ) : null}
        </div>
      </div>

      {/* Compare results (compare mode only) -------------------------------- */}
      {compareOn ? (
        <div className="mx-auto w-full max-w-5xl space-y-4 px-4 pb-10 sm:px-6">
          <div className="grid gap-4">
            <div className="space-y-1.5">
              <div className="flex items-center gap-2">
                <p className="text-[11px] font-semibold uppercase tracking-wide text-neutral-800/70">
                  Lane A · {config.model || 'no model'}
                </p>
                {running.primary ? (
                  <button
                    type="button"
                    onClick={() => cancel('primary')}
                    title="Stop lane A"
                    className="flex h-6 items-center gap-1 rounded-full bg-rose-950/90 px-2 text-[10px] font-medium text-white transition-colors hover:bg-rose-900"
                  >
                    <Square className="size-2.5" />
                    Stop A
                  </button>
                ) : null}
              </div>
              <ResponsePanel
                config={config}
                messages={messages}
                baseUrl={gatewayUrl}
                partial={partial.primary}
                running={running.primary}
                run={latest.primary}
                elapsedMs={elapsed}
                firstTokenMs={liveFirstTokenMs.primary}
                onRetry={
                  latest.primary?.error && !isBusy ? () => onRerun(latest.primary as PlaygroundRun) : undefined
                }
              />
            </div>
            <div className="space-y-1.5">
              <div className="flex items-center gap-2">
                <p className="text-[11px] font-semibold uppercase tracking-wide text-neutral-800/70">
                  Lane B · {compareConfig.model || 'no model'}
                </p>
                {running.compare ? (
                  <button
                    type="button"
                    onClick={() => cancel('compare')}
                    title="Stop lane B"
                    className="flex h-6 items-center gap-1 rounded-full bg-rose-950/90 px-2 text-[10px] font-medium text-white transition-colors hover:bg-rose-900"
                  >
                    <Square className="size-2.5" />
                    Stop B
                  </button>
                ) : null}
              </div>
              <ResponsePanel
                config={compareConfig}
                messages={messages}
                baseUrl={gatewayUrl}
                partial={partial.compare}
                running={running.compare}
                run={latest.compare}
                elapsedMs={elapsed}
                firstTokenMs={liveFirstTokenMs.compare}
                onRetry={
                  latest.compare?.error && !isBusy ? () => onRerun(latest.compare as PlaygroundRun) : undefined
                }
              />
            </div>
          </div>

          <section className="space-y-2">
            <h2 className="text-[13px] font-semibold text-neutral-900">Lane comparison</h2>
            <ComparisonTable a={latest.primary} b={latest.compare} />
            <p className="text-[11px] text-neutral-800/70">
              Green marks the better value on latency. Tokens are reported as totals, so a
              shorter answer is not automatically the cheaper one.
            </p>
          </section>
        </div>
      ) : null}

      <AdvancedDrawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        config={config}
        onChange={patch}
        apiKey={apiKey}
        onApiKeyChange={setApiKey}
        models={models.data ?? []}
        modelsPending={models.isPending}
        endpoints={endpoints.data ?? []}
        endpointsPending={endpoints.isPending}
        policies={policies.data ?? []}
        policiesPending={policies.isPending}
        providerFilter={providerFilter}
        onProviderFilterChange={setProviderFilter}
        disabled={isBusy}
        compareOn={compareOn}
        onCompareChange={onCompareChange}
        onNewChat={onNewChat}
        newChatDisabled={isBusy || messages.length === 0}
        compareTarget={compareTarget}
        onCompareTarget={patchCompareTarget}
        onRunAll={onRunAll}
        runAllDisabled={
          isBusy ||
          missingKey ||
          Boolean(build.error) ||
          Boolean(buildChatBody(compareConfig, messages).error)
        }
        latestRun={latest.primary}
        partial={partial.primary}
        running={running.primary}
        gatewayUrl={gatewayUrl}
        messages={messages}
        runs={runs}
        onLoad={onLoad}
        onRerun={onRerun}
        onClearHistory={clearHistory}
      />
    </div>
  );
}

/** Routing facts captured with a completed answer, shown under its bubble. */
function metaFor(entry: PlaygroundRun): ChatMeta {
  return {
    provider: entry.route.provider,
    model: entry.route.model,
    totalTokens: entry.usage ? entry.usage.total_tokens : null,
    latencyMs: entry.durationMs,
    cacheHit: entry.route.cache_hit,
    fallbackUsed: entry.route.fallback_used,
  };
}
