/**
 * Playground request/response model.
 *
 * The Playground is a test client for the public inference API, not a second
 * inference path. Everything here builds the exact body `/v1/chat/completions`
 * already accepts and reads the exact envelope it already returns, so a run in
 * the Playground exercises the same routing, caching, retries and provider
 * adapters as production traffic. Nothing in this file is a simulation.
 *
 * The routing controls are the documented `X-Synapass-*` intent headers and
 * nothing else. Two fields were deliberately left out of the config:
 *
 *   - there is no provider header. `X-Synapass-Provider` is a *response*
 *     header naming the provider that served; there is no request-side
 *     equivalent. Provider preference is expressed through an endpoint scope
 *     (whose override carries preferred providers and a forced model) or a
 *     routing policy, so the Playground offers those instead and reports the
 *     provider that actually served.
 *   - there is no tenant header. The tenant is derived from the API key that
 *     authenticates the call, so it cannot be set per request.
 *
 * Sending headers the gateway ignores would make the console look like it
 * controls routing when it does not, which is the one thing a testing surface
 * must never do.
 */

/** A conversation turn as the operator edits it. */
export interface PlaygroundMessage {
  /** Stable key for React and for history replay. */
  id: string;
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: string;
}

/** Everything that shapes one test run. */
export interface PlaygroundConfig {
  model: string;
  /** Endpoint scope slug -> `X-Synapass-Endpoint`. Carries provider/model preference. */
  endpoint: string;
  /** Routing policy id or name -> `X-Synapass-Policy`. */
  policy: string;
  streaming: boolean;
  /** Asks the gateway to include routing internals in the response. */
  debug: boolean;
  /** Fail instead of failing over, to make a primary-provider failure visible. */
  noFallback: boolean;
  /** Force a cache miss, so a cached answer does not hide live behavior. */
  noCache: boolean;
  /** Pin provider geography -> `X-Synapass-Region`. */
  region: string;
  /** Comma-separated sensitivity labels -> `X-Synapass-Sensitivity`. */
  sensitivity: string;
  /** -1 means "do not send a cost ceiling". */
  maxCostUsd: number;
  /** 0 means "do not send a latency target". */
  latencyTargetMs: number;
  system: string;
  /** -1 means "do not send temperature", which is not the same as 0. */
  temperature: number;
  /** -1 means "do not send top_p". */
  topP: number;
  /** 0 means "do not send max_tokens". */
  maxTokens: number;
  /** Comma or newline separated stop sequences. */
  stop: string;
  /** A JSON object for response_format, or empty. */
  responseFormat: string;
  /** A JSON array of tool definitions, or empty. */
  tools: string;
  /**
   * A JSON object merged verbatim into the body, for extended sampling
   * controls (`top_k`, `min_p`, `repetition_penalty`), `seed`, `logprobs` and
   * anything else the gateway forwards but has no dedicated control for.
   */
  extraBody: string;
}

export const DEFAULT_CONFIG: PlaygroundConfig = {
  model: '',
  endpoint: '',
  policy: '',
  streaming: true,
  debug: true,
  noFallback: false,
  noCache: false,
  region: '',
  sensitivity: '',
  maxCostUsd: -1,
  latencyTargetMs: 0,
  system: '',
  temperature: -1,
  topP: -1,
  maxTokens: 0,
  stop: '',
  responseFormat: '',
  tools: '',
  extraBody: '',
};

/** Body keys the extra-body field may not replace: they identify the run. */
const RESERVED_BODY_KEYS = new Set(['model', 'messages', 'stream']);

/** Split a comma/newline separated list, dropping empties. */
export function splitList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((part) => part.trim())
    .filter((part) => part.length > 0);
}

/** True when every non-empty message has content. */
function firstEmptyMessage(messages: PlaygroundMessage[]): number {
  return messages.findIndex((message) => message.content.trim().length === 0);
}

export interface BuildResult {
  body: Record<string, unknown> | null;
  /** A human-readable reason the request cannot be built. */
  error: string | null;
}

/**
 * Build the `/v1/chat/completions` body for a run.
 *
 * Validation lives here rather than in the component so the rules are the same
 * for a single run, a compare run and a replayed history entry — and so they can
 * be unit tested without a browser.
 */
export function buildChatBody(
  config: PlaygroundConfig,
  messages: PlaygroundMessage[],
): BuildResult {
  const trimmedModel = config.model.trim();
  if (!trimmedModel) return { body: null, error: 'Choose a model before running.' };

  const conversation = messages.filter((message) => message.role !== 'system');
  if (conversation.length === 0) {
    return { body: null, error: 'Add at least one message before running.' };
  }
  // Every message is validated, system ones included: a system message that was
  // checked only by omission and then dropped would look like a bug in the
  // model rather than in the composer.
  const emptyIndex = firstEmptyMessage(messages);
  if (emptyIndex >= 0) {
    return { body: null, error: `Message ${emptyIndex + 1} is empty.` };
  }

  // The system prompt is a separate control in the UI; it is folded into the
  // message list here so the wire body has exactly one shape.
  const out: Array<Record<string, unknown>> = [];
  if (config.system.trim()) {
    out.push({ role: 'system', content: config.system });
  }
  for (const message of messages) {
    out.push({ role: message.role, content: message.content });
  }

  const body: Record<string, unknown> = { model: trimmedModel, messages: out };

  if (config.streaming) body.stream = true;
  if (config.streaming && config.debug) {
    // Usage only arrives on the stream when it is asked for.
    body.stream_options = { include_usage: true };
  }
  if (config.temperature >= 0) body.temperature = config.temperature;
  if (config.topP >= 0) body.top_p = config.topP;
  if (config.maxTokens > 0) body.max_tokens = config.maxTokens;
  const stop = splitList(config.stop);
  if (stop.length > 0) body.stop = stop;

  if (config.tools.trim()) {
    const parsed = parseJson(config.tools);
    if (!parsed.ok) return { body: null, error: `Tools is not valid JSON: ${parsed.error}` };
    if (!Array.isArray(parsed.value)) return { body: null, error: 'Tools must be a JSON array.' };
    body.tools = parsed.value;
  }

  if (config.responseFormat.trim()) {
    const parsed = parseJson(config.responseFormat);
    if (!parsed.ok) {
      return { body: null, error: `Response format is not valid JSON: ${parsed.error}` };
    }
    if (typeof parsed.value !== 'object' || parsed.value === null || Array.isArray(parsed.value)) {
      return { body: null, error: 'Response format must be a JSON object.' };
    }
    body.response_format = parsed.value;
  }

  // Applied last so it can override a dedicated control, which is the point of
  // an escape hatch — but not the three keys that define the run itself.
  if (config.extraBody.trim()) {
    const parsed = parseJson(config.extraBody);
    if (!parsed.ok) return { body: null, error: `Extra body is not valid JSON: ${parsed.error}` };
    if (typeof parsed.value !== 'object' || parsed.value === null || Array.isArray(parsed.value)) {
      return { body: null, error: 'Extra body must be a JSON object.' };
    }
    for (const [key, value] of Object.entries(parsed.value as Record<string, unknown>)) {
      if (RESERVED_BODY_KEYS.has(key)) {
        return {
          body: null,
          error: `Extra body cannot set "${key}": use the control for it instead.`,
        };
      }
      body[key] = value;
    }
  }

  return { body, error: null };
}

type ParsedJson =
  | { ok: true; value: unknown }
  | { ok: false; error: string };

function parseJson(raw: string): ParsedJson {
  try {
    return { ok: true, value: JSON.parse(raw) };
  } catch (cause) {
    return { ok: false, error: cause instanceof Error ? cause.message : 'invalid JSON' };
  }
}

/** Canonical header names, so the UI can show and the wire can send the same set. */
export const INTENT_HEADER_NAMES = [
  'X-Synapass-Debug',
  'X-Synapass-Endpoint',
  'X-Synapass-Policy',
  'X-Synapass-No-Fallback',
  'X-Synapass-No-Cache',
  'X-Synapass-Region',
  'X-Synapass-Sensitivity',
  'X-Synapass-Max-Cost-USD',
  'X-Synapass-Latency-Target-Ms',
] as const;

/**
 * The routing-intent headers this run will send.
 *
 * Every header here is read by the gateway. Kept separate from the transport
 * headers so the UI can display exactly the intent it is about to express.
 */
export function intentHeaders(config: PlaygroundConfig): Record<string, string> {
  const headers: Record<string, string> = {};
  if (config.debug) headers['X-Synapass-Debug'] = 'true';
  if (config.endpoint.trim()) headers['X-Synapass-Endpoint'] = config.endpoint.trim();
  if (config.policy.trim()) headers['X-Synapass-Policy'] = config.policy.trim();
  if (config.noFallback) headers['X-Synapass-No-Fallback'] = 'true';
  if (config.noCache) headers['X-Synapass-No-Cache'] = 'true';
  if (config.region.trim()) headers['X-Synapass-Region'] = config.region.trim();
  if (config.sensitivity.trim()) headers['X-Synapass-Sensitivity'] = config.sensitivity.trim();
  if (config.maxCostUsd >= 0) headers['X-Synapass-Max-Cost-USD'] = String(config.maxCostUsd);
  if (config.latencyTargetMs > 0) {
    headers['X-Synapass-Latency-Target-Ms'] = String(config.latencyTargetMs);
  }
  return headers;
}

/** Headers for a run, mirroring what a real client would send. */
export function buildHeaders(config: PlaygroundConfig, apiKey: string): Record<string, string> {
  const headers: Record<string, string> = {
    'content-type': 'application/json',
    accept: config.streaming ? 'text/event-stream' : 'application/json',
  };
  if (apiKey.trim()) headers.authorization = `Bearer ${apiKey.trim()}`;
  return { ...headers, ...intentHeaders(config) };
}

/**
 * Render a run as a curl command.
 *
 * The key is redacted: this string is meant to be pasted into a ticket or a
 * chat, and a real credential pasted there is a leak.
 */
export function buildCurl(
  config: PlaygroundConfig,
  messages: PlaygroundMessage[],
  baseUrl: string,
): string {
  const { body } = buildChatBody(config, messages);
  const headers = buildHeaders(config, '');
  const lines = [`curl -sS ${JSON.stringify(`${baseUrl.replace(/\/+$/, '')}/v1/chat/completions`)}`];
  for (const [name, value] of Object.entries(headers)) {
    lines.push(`  -H ${JSON.stringify(`${name}: ${value}`)}`);
  }
  lines.push(`  -H 'authorization: Bearer $SYNAPASS_API_KEY'`);
  lines.push(`  -d ${JSON.stringify(JSON.stringify(body ?? {}, null, 2))}`);
  return lines.join(' \\\n');
}

/** Token usage as Synapass reports it. */
export interface PlaygroundUsage {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
}

/** The routing internals the gateway attaches when debug is requested. */
export interface PlaygroundRoute {
  request_id: string;
  trace_id: string;
  fallback_used: boolean;
  cache_hit: boolean;
  latency_ms: number;
  provider: string;
  model: string;
  attempts: number;
  /** Present only under the debug header; the full decision object. */
  decision: unknown;
}

export const EMPTY_ROUTE: PlaygroundRoute = {
  request_id: '',
  trace_id: '',
  fallback_used: false,
  cache_hit: false,
  latency_ms: 0,
  provider: '',
  model: '',
  attempts: 0,
  decision: null,
};

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

/** Read the assistant text out of a completion envelope. */
export function extractContent(payload: unknown): string {
  const record = asRecord(payload);
  if (!record) return '';
  const choices = record.choices;
  if (!Array.isArray(choices) || choices.length === 0) return '';
  const first = asRecord(choices[0]);
  if (!first) return '';
  const message = asRecord(first.message);
  if (message && typeof message.content === 'string') return message.content;
  // Some providers answer with the non-streaming `text` shape.
  if (typeof first.text === 'string') return first.text;
  return '';
}

/** Read token usage, tolerating providers that omit it. */
export function extractUsage(payload: unknown): PlaygroundUsage | null {
  const record = asRecord(payload);
  const usage = record ? asRecord(record.usage) : null;
  if (!usage) return null;
  return {
    prompt_tokens: asNumber(usage.prompt_tokens),
    completion_tokens: asNumber(usage.completion_tokens),
    total_tokens: asNumber(usage.total_tokens),
  };
}

/**
 * Read the finish reason, which is how truncation shows itself.
 *
 * `length` rather than `stop` is the single most common explanation for a cut-off
 * answer, so a testing console has to surface it instead of leaving the operator
 * guessing between the model and their own `max_tokens`.
 */
export function extractFinishReason(payload: unknown): string {
  const record = asRecord(payload);
  const choices = record?.choices;
  if (!Array.isArray(choices) || choices.length === 0) return '';
  const first = asRecord(choices[0]);
  return first ? asString(first.finish_reason) : '';
}

/** A tool call the model asked for. */
export interface PlaygroundToolCall {
  name: string;
  /** The raw argument string, kept as-is: parsing it would hide a malformed one. */
  arguments: string;
}

/**
 * Read tool calls out of a completion envelope.
 *
 * Kept for a non-streamed response, where there is no delta to watch. The
 * argument string is deliberately returned unparsed — a model that emits
 * invalid JSON here is a finding, not something to smooth over.
 */
export function extractToolCalls(payload: unknown): PlaygroundToolCall[] {
  const record = asRecord(payload);
  const choices = record?.choices;
  if (!Array.isArray(choices) || choices.length === 0) return [];
  const message = asRecord(asRecord(choices[0])?.message);
  const calls = message?.tool_calls;
  if (!Array.isArray(calls)) return [];
  const out: PlaygroundToolCall[] = [];
  for (const call of calls) {
    const fn = asRecord(asRecord(call)?.function);
    if (!fn) continue;
    const name = asString(fn.name);
    if (!name) continue;
    // Some providers send arguments as an object rather than a JSON string.
    const rawArgs = fn.arguments;
    const args =
      typeof rawArgs === 'string'
        ? rawArgs
        : rawArgs == null
          ? ''
          : JSON.stringify(rawArgs);
    out.push({ name, arguments: args });
  }
  return out;
}

/**
 * Read the `synapass` metadata block.
 *
 * The block is present on every response, but only carries the routing detail
 * when the debug header asked for it — without debug it is just
 * `request_id`, `trace_id`, `cache_hit`, `fallback_used` and `latency_ms`.
 *
 * The debug fields arrive **flattened** (`provider`, `routed_model`,
 * `attempts`, `strategy`, `route_reason`, `policy_name`, `task`, `shaping`,
 * `completion`), not under a nested `decision` object. The nested shape is
 * still read as well so a response that carries it is not dropped, but the
 * flat fields are the ones the gateway actually sends — reading only the
 * nested form leaves the provider and model rows empty on every run.
 */
export function extractRoute(payload: unknown): PlaygroundRoute {
  const record = asRecord(payload);
  const meta = record ? asRecord(record.synapass) : null;
  if (!meta) return { ...EMPTY_ROUTE };

  const decision = asRecord(meta.decision);
  const chosen = decision ? asRecord(decision.chosen) : null;

  const provider =
    asString(meta.provider) || asString(chosen?.provider_name) || asString(chosen?.provider_id);
  const model =
    asString(meta.routed_model) || asString(chosen?.model) || asString(meta.requested_model);
  const attempts = typeof meta.attempts === 'number' ? meta.attempts : asNumber(decision?.attempts);

  // Which debug fields the gateway chose to flatten in front of us, so the
  // panel can say "no decision" rather than show an empty table.
  const decisionFields = [
    'policy_name',
    'provider',
    'strategy',
    'route_reason',
    'task',
    'shaping',
    'routed_model',
    'attempts',
  ];
  const hasDecision =
    decisionFields.some((key) => meta[key] !== undefined && meta[key] !== null) || decision != null;

  return {
    request_id: asString(meta.request_id),
    trace_id: asString(meta.trace_id),
    fallback_used: meta.fallback_used === true,
    cache_hit: meta.cache_hit === true,
    latency_ms: asNumber(meta.latency_ms),
    provider,
    model,
    attempts,
    decision: hasDecision ? (decision ?? meta) : null,
  };
}

/** One parsed SSE frame from the event stream. */
export interface SseEvent {
  /** The raw JSON text, or '[DONE]'. */
  data: string;
  /** The `event:` name when the gateway set one (`error` for post-commit failures). */
  event?: string;
}

/**
 * Split a streaming buffer into complete frames.
 *
 * SSE frames are bounded by a blank line and `data:` values can span several
 * physical lines. The caller keeps the returned `rest` and prepends it to the
 * next network chunk, so a frame split across two reads is still parsed whole.
 */
export function parseSseChunk(buffer: string): { events: SseEvent[]; rest: string } {
  const events: SseEvent[] = [];
  const normalized = buffer.replace(/\r\n/g, '\n');
  const parts = normalized.split('\n\n');
  // The final element is either an incomplete frame or an empty string after a
  // trailing separator; either way it is what remains to be read.
  const rest = parts.pop() ?? '';
  for (const part of parts) {
    const dataLines: string[] = [];
    let eventName = '';
    for (const line of part.split('\n')) {
      if (line.startsWith('data:')) {
        dataLines.push(line.slice(5).trimStart());
      } else if (line.startsWith('event:')) {
        eventName = line.slice(6).trim();
      }
      // Comment lines (`: ping` keepalives) carry no payload and are
      // skipped: they must not look like model output.
    }
    if (dataLines.length > 0) {
      const frame: SseEvent = { data: dataLines.join('\n') };
      if (eventName) frame.event = eventName;
      events.push(frame);
    }
  }
  return { events, rest };
}

/**
 * Accumulate tool calls from one chunk's `delta.tool_calls`.
 *
 * Streaming tool calls arrive as fragments: the name comes in the first chunk
 * and the argument string is spread across subsequent ones, keyed by `index`.
 * They are merged positionally so a completed call reads as one call rather
 * than as a stack of argument slivers.
 */
function mergeDeltaToolCalls(
  current: PlaygroundToolCall[],
  raw: unknown,
): PlaygroundToolCall[] {
  if (!Array.isArray(raw) || raw.length === 0) return current;
  const next = [...current];

  for (const call of raw) {
    const record = asRecord(call);
    if (!record) continue;
    const fn = asRecord(record.function);
    const name = fn ? asString(fn.name) : '';
    const args = fn && typeof fn.arguments === 'string' ? fn.arguments : '';
    const index = typeof record.index === 'number' ? record.index : -1;

    const existing = index >= 0 ? next[index] : undefined;
    if (existing) {
      next[index] = {
        name: existing.name || name,
        arguments: existing.arguments + args,
      };
      continue;
    }

    if (index >= 0) {
      next[index] = { name, arguments: args };
      continue;
    }
    next.push({ name, arguments: args });
  }

  return next;
}

/** Read a complete `tool_calls` array, as a provider sends it in one piece. */
function toolCallsFromList(raw: unknown): PlaygroundToolCall[] {
  if (!Array.isArray(raw)) return [];
  const out: PlaygroundToolCall[] = [];
  for (const call of raw) {
    const fn = asRecord(asRecord(call)?.function);
    if (!fn) continue;
    const name = asString(fn.name);
    if (!name) continue;
    const rawArgs = fn.arguments;
    const args =
      typeof rawArgs === 'string' ? rawArgs : rawArgs == null ? '' : JSON.stringify(rawArgs);
    out.push({ name, arguments: args });
  }
  return out;
}

/** Accumulated stream state, built one frame at a time. */
export interface StreamState {
  content: string;
  usage: PlaygroundUsage | null;
  route: PlaygroundRoute;
  done: boolean;
  finishReason: string;
  toolCalls: PlaygroundToolCall[];
  /**
   * A post-commit failure delivered as an `event: error` frame (or an error
   * envelope in a data frame). Without this, a stream that fails halfway
   * would read as a truncated success once `[DONE]` arrives.
   */
  error: PlaygroundError | null;
}

export function emptyStreamState(): StreamState {
  return {
    content: '',
    usage: null,
    route: { ...EMPTY_ROUTE },
    done: false,
    finishReason: '',
    toolCalls: [],
    error: null,
  };
}

/**
 * Fold one SSE frame into the running stream state.
 *
 * Returns a new state rather than mutating, so React sees a changed reference
 * and a compare run can hold two independent streams.
 */
export function applySseEvent(state: StreamState, event: SseEvent): StreamState {
  if (event.data === '[DONE]') return { ...state, done: true };

  let payload: unknown;
  try {
    payload = JSON.parse(event.data);
  } catch {
    // A malformed frame is skipped rather than aborting the stream: one bad
    // chunk should not lose the whole answer.
    return state;
  }

  const record = asRecord(payload);
  if (!record) return state;

  const next: StreamState = { ...state };

  // A post-commit failure arrives as `event: error` with an error envelope
  // in the data frame. It must be recorded: the terminating `[DONE]` that
  // follows would otherwise make a failed stream read as a truncated
  // success. Content accumulated so far is kept.
  const envelope = asRecord(record.error);
  if (event.event === 'error' || envelope) {
    next.error = {
      message: asString(envelope?.message) || 'the stream failed after it started',
      status: 0,
      code: asString(envelope?.code) || 'upstream_error',
      type: asString(envelope?.type) || 'upstream_error',
    };
  }

  const usage = extractUsage(payload);
  if (usage) next.usage = usage;

  const route = extractRoute(payload);
  if (route.request_id) next.route = route;

  const choices = record.choices;
  if (Array.isArray(choices) && choices.length > 0) {
    const first = asRecord(choices[0]);
    if (first) {
      if (typeof first.finish_reason === 'string' && first.finish_reason) {
        next.finishReason = first.finish_reason;
      }
      const delta = asRecord(first.delta);
      const deltaContent = delta?.content;
      if (typeof deltaContent === 'string') {
        next.content = next.content + deltaContent;
      }

      // Two shapes are live: a fragmented call under `delta.tool_calls`, and a
      // complete call under `message.tool_calls` from providers that emit it in
      // one piece. The complete list wins when both are present, because it is
      // authoritative and merging the fragments into it would double the args.
      const complete = toolCallsFromList(asRecord(first.message)?.tool_calls);
      if (complete.length > 0) {
        next.toolCalls = complete;
      } else {
        const merged = mergeDeltaToolCalls(state.toolCalls, delta?.tool_calls);
        if (merged.length > 0) next.toolCalls = merged;
      }
    }
  }

  return next;
}

/** The gateway's error envelope, flattened for display. */
export interface PlaygroundError {
  message: string;
  status: number;
  code: string;
  type: string;
}

/**
 * Pull a usable message out of any failure, including a non-JSON body.
 *
 * The Playground exists to make failures legible, so an unparseable error is
 * reported with its status and text rather than swallowed.
 */
export function extractError(status: number, rawBody: string): PlaygroundError {
  const fallback = `the gateway returned HTTP ${status}`;
  if (!rawBody.trim()) return { message: fallback, status, code: '', type: '' };
  try {
    const parsed = asRecord(JSON.parse(rawBody));
    const envelope = parsed ? asRecord(parsed.error) : null;
    if (envelope) {
      return {
        message: asString(envelope.message) || fallback,
        status,
        code: asString(envelope.code),
        type: asString(envelope.type),
      };
    }
    return { message: fallback, status, code: '', type: '' };
  } catch {
    return { message: rawBody.slice(0, 400), status, code: '', type: '' };
  }
}

/** One entry in the Playground's session history. */
export interface PlaygroundRun {
  id: string;
  config: PlaygroundConfig;
  messages: PlaygroundMessage[];
  startedAt: number;
  durationMs: number;
  /**
   * Time until the first token arrived, 0 when there was none to time.
   *
   * Separated from durationMs because they answer different questions: a slow
   * total with a fast first token is a long answer, while a slow first token is
   * a routing or provider problem.
   */
  firstTokenMs: number;
  content: string;
  raw: unknown;
  usage: PlaygroundUsage | null;
  route: PlaygroundRoute;
  /**
   * Captured at run time rather than re-derived from `raw`, because a streamed
   * run's raw form is a transcript rather than one envelope.
   */
  toolCalls: PlaygroundToolCall[];
  /** `stop` vs `length` is how truncation shows itself, so it is kept. */
  finishReason: string;
  error: PlaygroundError | null;
  /** Which pane produced it, so a compare run keeps its sides apart. */
  lane: 'primary' | 'compare';
  label: string;
}

/** A short human label for a run, used in the history list headers. */
export function runLabel(config: PlaygroundConfig, messages: PlaygroundMessage[]): string {
  const firstUser = messages.find((message) => message.role === 'user');
  const preview = firstUser ? firstUser.content.trim().slice(0, 48) : 'no prompt';
  const model = config.model.trim() || 'no model';
  return `${model} · ${preview}${firstUser && firstUser.content.trim().length > 48 ? '…' : ''}`;
}
