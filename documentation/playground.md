# Playground

The Playground is the dashboard's endpoint testing console: choose a model, an
endpoint scope and a policy, send a prompt, and read the answer **and the
routing decision behind it**. It exists because a routing policy that looks right
on paper is worth nothing until you have watched it pick a provider.

It is deliberately not a second inference path and not a fixture layer. A run is
an ordinary `POST /v1/chat/completions` through a session-gated proxy, so it
exercises the same policy engine, cache, retries, tool execution and provider
adapters as production traffic. If a request behaves differently in the
Playground than it does in the API, the Playground is wrong — that is the whole
contract.

## Where the run goes

```
browser  ->  POST /api/playground/v1/chat/completions  (dashboard, session-gated)
                 ->  POST /v1/chat/completions         (gateway, tenant key)
                       ->  provider
```

Three properties follow from routing through the dashboard rather than calling
the gateway directly:

1. **No CORS surface.** The browser and the gateway never talk to each other, so
   streaming needs no preflight and the inference API gains no new origin.
2. **The address problem is solved once.** The API URL the browser can reach may
   not be the URL the server can reach; the server already knows both.
3. **The run is gated on the operator session.** Without that check this would be
   an open, unauthenticated inference endpoint that only requires an API key the
   caller supplies. With it, a logged-out browser cannot use the dashboard as a
   proxy to the gateway.

The target path is a hard whitelist — today `v1/chat/completions` only. There is
no free-form URL field, and there will not be one: it would turn a dashboard
route into an SSRF primitive.

### The API key

The public inference API authenticates with a **tenant API key**, so a run
requires one. You paste it in the Transport section; it is sent as a bearer token
on that single request and is held in memory only — never in history, never in
`localStorage`, never written to a log or to disk. Without a key the run button
is replaced by a statement of why it cannot be sent.

The session cookie authorises access to the *console*. The tenant key authorises
*inference*. They are different credentials for different things, and only the
key appears in the request to the gateway.

## Layout

The page is only a chat: a centered composer over a gradient, with the turns
above it as bubbles. Every completed answer is appended to the thread with its
routing chips (provider, model, tokens, latency), so follow-up turns carry the
same history a real client would send. A run in flight streams into the same
bubble shape; a failed run renders as an error card with the next step.

Everything else lives in the **Advanced** slide-over opened from the top bar:

- **Connection & target** — credential and mode (API key, streaming, debug)
  and the target (endpoint scope, provider filter, routing policy), plus a
  read-only statement of which model is targeted.
- **Sampling, tools & routing** — system prompt, sampling, response format,
  extra body, tools and the routing constraints.
- **Lane B** (compare mode only), **Route & debug**, **Request as curl** and
  **Session history**.

The top bar shows an amber dot on Advanced whenever it holds anything off the
defaults, so nothing that changes the request is hidden without being named.

**Run** sits under the composer and sends the conversation as it stands without
adding a turn. That is the path to use when an attempt was blocked — no key, no
model — and the configuration has since been corrected. **Cancel** appears
while a run is in flight.

The model is chosen in the composer's menu rather than in the drawer, so the
menu the operator is looking at is the one that decides, and the two can never
be shown in disagreement.

## What each control maps to

Every control produces either a body field or a header the gateway reads.
There is nothing in either surface that is decorative.

| Control | Sent as |
| --- | --- |
| Model | `model` |
| System prompt | a leading `system` message |
| Message list | `messages` |
| Streaming | `stream` (plus `stream_options.include_usage` when debug is on) |
| Temperature, top p, max tokens, stop | `temperature`, `top_p`, `max_tokens`, `stop` |
| response format | `response_format` |
| Tools | `tools` |
| Extra body | merged into the body |
| Endpoint scope | `X-Synapass-Endpoint` |
| Routing policy | `X-Synapass-Policy` |
| No fallback | `X-Synapass-No-Fallback` |
| Bypass cache | `X-Synapass-No-Cache` |
| Region | `X-Synapass-Region` |
| Sensitivity | `X-Synapass-Sensitivity` |
| Max cost, latency target | `X-Synapass-Max-Cost-USD`, `X-Synapass-Latency-Target-Ms` |
| Debug metadata | `X-Synapass-Debug` |

A field left at "not sent" is omitted rather than sent as a zero. Filling in
`temperature` when the operator did not choose it would silently change the
answer the operator was trying to reproduce, which is a subtle correctness bug
that shows up as a mystery regression.

System messages are sent from **both** places: the Advanced system-prompt field
is placed first, then the message list in order. A system message added to the
conversation is no longer dropped — an earlier version filtered system turns
out of the wire body, so the console would accept one and never send it.

**Extra body** is the escape hatch for fields with no dedicated control:
`top_k`, `min_p`, `repetition_penalty`, `seed`, `logprobs`, `tool_choice`. It is
merged last, so it can override a dedicated control. It cannot set `model`,
`messages` or `stream`, because those identify the run itself and a hidden
override there would make the panel lie.

## Why there is no provider picker

This is the most common expectation and the most important omission, so it is
stated plainly.

`X-Synapass-Provider` exists — as a **response** header naming the provider
that served the request. There is no request-side equivalent. Similarly, the
tenant is derived from the API key that authenticates the call; it cannot be set
per request.

Showing a provider dropdown that the gateway ignores would be worse than not
show one: it would let an operator "pin" a provider, watch a different one serve,
and report a routing bug that does not exist. So:

- **Provider preference is expressed through an endpoint scope.** An endpoint
  scope's `routing_override` carries `preferred_providers`, `preferred_models`,
  `force_model`, `strategy`, `max_cost_usd`, `block_fallback` and friends. The
  endpoint selector shows what the selected scope will do to routing.
- **Routing policy** pins the rule set for a request.
- **Provider filter** narrows the model dropdown only, and the panel says so —
  it does not affect routing.
- **The response panel reports the provider that actually served**, which is the
  fact you were after.

To add a true provider pin would mean a new routing-intent header and an
allow/deny rule in the engine — a real routing change, not a UI change, and one
that belongs next to the endpoint scope rather than inside the console.

## Streaming and non-streaming

Both modes go through the same run path.

- **Streaming** requests SSE and folds each `data:` frame into a running state as
  it arrives. One state update per *network read* rather than per frame — a fast
  model emits dozens of frames per read, and re-rendering each individually makes
  the stream look slower than it is. A frame split across two reads is buffered
  and parsed whole; a malformed frame is skipped rather than aborting the answer.
- **Non-streaming** reads the completed envelope and extracts content, usage and
  the route block.

Either way the final message is preserved, and **Cancel** aborts the fetch, which
the proxy propagates to the gateway via `request.signal` — so generation actually
stops upstream instead of the UI merely abandoning the response. A cancelled run
is recorded as cancelled, not as a gateway failure, and **keeps the partial
answer it had already received** under a "Run stopped" card rather than
discarding it. In compare mode each lane has its own Stop button, and `Esc`
cancels a run in flight. A duplicate submit while a run is active is ignored,
so double-clicking Run cannot fork a run against itself.

While a stream is open the viewer shows a live first-token chip from the
moment the first frame arrives; completed turns are memoized so a fast stream
does not re-render finished answers on every chunk. Partial text is
copyable mid-stream, fenced code blocks have their own copy button, and an
`event: error` frame from the gateway surfaces as a failure card with a
Retry button rather than a truncated success.

## Reading a run

The response viewer has three views of the same run:

| View | Answers |
| --- | --- |
| Answer | What the user would see, rendered as Markdown |
| Raw response | What the gateway actually returned, verbatim |
| Request | What was sent — target, headers, body |

The answer view renders a safe subset of Markdown — headings, fenced code, lists,
quotes, rules, emphasis, links — from a small local parser rather than a
dependency. Model output is untrusted input, so nothing becomes live HTML and
only `http`, `https`, `mailto` and site-relative links survive; a `javascript:`
or `data:` link is dropped to its label.

Beside it, the header strip shows provider, model, cache, fallback and total
tokens, and the footer reports duration and **time to first token**. Those two
numbers answer different questions: a slow total with a fast first token is a
long answer, while a slow first token is a routing or provider problem.

## Debug metadata

The right-hand panel is the reason the console exists:

- request id (copyable) and trace id
- provider, model, attempts, fallback, cache hit or miss, gateway-reported latency
- prompt / completion / total tokens
- the intent headers this run expressed
- the **routing decision**, as a readable tree or as raw JSON

It reports what the gateway returned. With debug off the decision is absent, and
the panel says exactly that and how to get it — it does not reconstruct a
plausible-looking decision. Turning debug on changes only the response envelope;
the request is otherwise identical, so a debug run and a plain run are
comparable.

Debug runs also ask for usage on the stream (`stream_options.include_usage`),
which is how streaming runs get token counts at all.

## Tool calling

Attach tools either by typing the JSON array or from the registry. The gateway
validates `tools` before routing, so the console shows the parse error instead of
sending a request that cannot succeed. Attached tools appear as removable chips.

A model's tool call is shown as the function name and its **raw** argument string
— unparsed, because a model emitting invalid JSON there is a finding rather than
something to smooth over. Tool execution still passes the policy engine, so a
tool the tenant may not run is denied here exactly as it would be elsewhere; the
success status in the invocation log is `executed`.

## Compare mode

Compare runs the *same conversation* against two targets side by side. Lane A
uses the main configuration. Lane B varies only:

- model
- endpoint scope
- routing policy

Everything else — the conversation, the transport mode, the sampling controls —
is shared. Comparing two answers that differ in both prompt and target tells you
nothing about either, so the console refuses the ambiguity.

A comparison table reads duration, first token, total tokens, cache, attempts and
result side by side, marking the faster lane. Tokens are shown as totals: a
shorter answer is not automatically the cheaper one.

## History

Each run is recorded with the configuration that produced it, and can be loaded
back into the composer or re-executed as-is. This is what makes repeated testing
practical — reproducing a run precisely beats re-typing it.

History is **in-memory only** and capped at 40 entries. A test prompt may carry
production data, and a dashboard should not quietly persist that to disk. Reload
the page and it is gone. The API key is never part of a history entry.

## Failures

Failures are shown as prominently as successes, with status code, error code,
error type and the gateway's message, plus a next step specific to the code:
unknown model, disabled endpoint scope, rejected key, rate limit, budget blocked,
all candidates failed, unreachable gateway. An unrecognised code still gets its
message and status — guessing at a cause would be worse than silence. Error
and cancelled cards carry **Retry**, which re-sends the same conversation
without rebuilding it.

## Security surface, summarised

- Session-gated, POST-only, hard-whitelisted path.
- The tenant key travels as a header for one request and is never stored.
- No CORS exposure: the gateway sees only the dashboard server as a caller.
- `cache-control: no-store` on every response, so nothing is cached by intermediaries.
- No free-form URL, so no SSRF.

## Limitations

- One inference surface: `POST /v1/chat/completions`. `/v1/completions`,
  `/v1/embeddings` and `/v1/responses` are not implemented in the gateway, so the
  console does not offer them either.
- Provider pinning requires an endpoint scope (see above).
- History is not persisted, and there are no saved presets or pinned prompts yet.
- The chat-completions body is the only shape; there is no `responses` or
  agent-run surface in this console. Bounded multi-step flows belong to
  [Agent runs](dashboard.md#agent-runs).

## Extending

- **Another inference surface** — add it to `ALLOWED_ROUTES` in
  `dashboard/src/app/api/playground/[...path]/route.ts`, then add a body builder
  beside `buildChatBody` in `dashboard/src/lib/playground.ts`. The run lifecycle
  (`usePlayground`) already handles both streaming and buffered responses.
- **A new routing-intent header** — add it to `intentHeaders()` in
  `lib/playground.ts` and to `INTENT_HEADERS` in the proxy. Keeping them in two
  places is the cost of forwarding verbatim; if that ever drifts, the honest fix
  is to derive the proxy's list from the shared one.
- **A true provider pin** — a new request header read in `applyRoutingIntent`,
  with an allow/deny rule in the routing engine and an error for an unknown
  provider. That is a routing change, not a console change.
- **Persisted history** — a `playground_runs` table would work, but it would
  store prompts. If that is wanted, make it explicit and opt-in.

## See also

- [API reference](api.md) — the intent headers and error codes the console uses
- [Routing](routing.md) — how a request actually picks a provider
- [Endpoints](dashboard.md#endpoints) — the scopes the endpoint selector reads
- [Dashboard](dashboard.md) — the rest of the console
