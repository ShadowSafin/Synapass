# API reference

Every endpoint Synapass exposes, on three surfaces:

| Surface | Prefix | Credential |
| --- | --- | --- |
| Inference | `/v1/*` | An API key with the `inference` scope |
| Operations | `/health`, `/ready`, `/version`, `/metrics` | None |
| Administration | `/admin/v1/*` | An API key with an admin scope |

The inference surface is OpenAI-compatible: an existing OpenAI SDK works by
changing the base URL. Responses carry additional namespaced fields, which
OpenAI clients ignore.

**Related:** [Routing](routing.md) explains the `X-Synapass-*` headers ·
[Providers](providers.md) covers the management endpoints in detail ·
[Tools](tools.md) covers the tool plane.

## Contents

- [Authentication](#authentication)
- [Error envelope](#error-envelope)
- [Routing intent](#routing-intent)
- [Inference](#inference)
- [Operations](#operations)
- [Console authentication](#console-authentication)
- [Administration](#administration)

## Authentication

```http
Authorization: Bearer syn_live_xxxxxxxxxxxxxxxxxxxxxxxx
```

- The header name is configurable (`auth.header_name`) for deployments behind a
  proxy that reserves `Authorization`.
- Keys are stored as a SHA-256 digest; the plaintext is returned exactly once
  when created and cannot be retrieved afterwards.
- The minimum key length is enforced before a lookup (`auth.min_key_length`),
  which rejects hand-typed placeholders without a database round trip.
- Validated keys are cached (`auth.cache_ttl`, default 30s). Revocation drops the
  cache entry, so a revoked key stops working immediately rather than after the
  TTL.

A missing or invalid credential returns `401` with code `authentication_error`.
A valid credential without the required scope returns `403` with
`permission_error`.

## Error envelope

Every failure — inference, admin, even a 404 on an unknown path — returns:

```json
{
  "error": {
    "message": "the prompt exceeds the context window of the routed model",
    "type": "invalid_request_error",
    "param": "messages",
    "code": "context_length_exceeded"
  },
  "synapass": {
    "request_id": "req_01HX…",
    "trace_id": "4f1c…",
    "policy_id": "pol_…",
    "policy_name": "default-chat",
    "provider": "anthropic",
    "requested_model": "gpt-4o-mini",
    "routed_model": "claude-3-5-haiku-latest",
    "strategy": "priority",
    "attempts": 2,
    "fallback_used": true,
    "latency_ms": 912
  }
}
```

Two things worth noting:

- The `synapass` block is attached to **errors** as well as successes, so a
  client that logs a failure also captures which provider the request actually
  reached.
- `provider` is **who answered**, not who was tried first. After a failover the
  two differ, and this is the field that tells you which.

### Error codes

| Code | HTTP | Retryable | Fallback-eligible | Meaning |
| --- | --- | --- | --- | --- |
| `invalid_request` | 400 | no | no | The client sent something unusable. Every provider would reject it. |
| `context_length_exceeded` | 400 | no | no | The prompt exceeded the routed model's window. |
| `authentication_error` | 401 | no | no | Gateway or upstream credentials failed. |
| `permission_error` | 403 | no | no | Valid credential, missing entitlement or scope. |
| `not_found` | 404 | no | no | Unknown model or route. |
| `content_filtered` | 400 | no | no | The provider refused the content. |
| `rate_limited` | 429 | yes | yes | A throttle, upstream or gateway-side. |
| `quota_exceeded` | 402 | no | yes | A hard upstream quota, or an internal spend ceiling. |
| `timeout` | 504 | yes | yes | Connect, header or stream-idle timeout. |
| `upstream_error` | 502 | yes | yes | The provider returned 5xx or a malformed response. |
| `provider_unavailable` | 502 | yes | yes | The gateway judged the provider unhealthy and skipped it. |
| `request_canceled` | 499 | no | no | The client disconnected. Not a platform fault. |
| `internal_error` | 500 | no | no | An unexpected gateway fault. |
| `not_implemented` | 501 | no | no | Endpoint planned but not served. |

An upstream `503` is classified as `upstream_error`, **not**
`provider_unavailable`: the first means the provider itself answered badly, the
second is reserved for a provider the gateway's own health tracker has taken out
of rotation. Conflating them would make an upstream incident look like a local
health decision.

### Response headers

| Header | Meaning |
| --- | --- |
| `X-Request-ID` | The request id (echoed if you supplied one, otherwise generated). Sanitized before use. |
| `X-Trace-ID` | The trace id, for correlating with the log and trace stores. |
| `X-Synapass-Provider` | The provider that served (or failed) the request. |
| `X-Synapass-Retryable` | `true` when a retry is worth attempting, on failures. |
| `X-Synapass-Upstream-Status` | The original upstream status when Synapass translated it. |
| `X-Synapass-Thinking` | `present` on non-streaming responses that carry a thinking trace (`reasoning_content`). Streams cannot carry it — headers commit before thinking arrives — so stream clients detect thinking by `delta.reasoning_content` frames instead. |
| `Retry-After` | Present on `429` when the provider supplied one. |

## Routing intent

These are optional request headers an individual caller can use to guide the
router without an operator editing a policy:

| Header | Effect |
| --- | --- |
| `X-Synapass-Policy` | Force a specific policy by id or name. |
| `X-Synapass-Max-Cost-USD` | Refuse candidates whose projected cost exceeds this. |
| `X-Synapass-Latency-Target-Ms` | Tighten the latency target for this request. |
| `X-Synapass-No-Fallback` | Fail instead of failing over. Useful for latency-critical calls. |
| `X-Synapass-Endpoint` | Select an admin-managed endpoint scope with overrides. The scope's forced model, preferred lists, strategy, cost/latency caps and fallback block fold into this request's policy copy; unknown slugs are `404`, disabled scopes `403`. Scoped requests are cached per-scope (endpoint is part of the cache key). |
| `X-Synapass-Region` | Pin provider geography; rejected when policy forbids it. |
| `X-Synapass-Sensitivity` | Comma-separated sensitivity labels (`pii,phi,public`); sensitive payloads bypass cache. |
| `X-Synapass-Batch` | `true` marks batch/offline traffic for cost-aware routing. |
| `X-Synapass-No-Cache` | `true` forces a cache miss for this request. |
| `X-Synapass-Debug` | `true` restores full routing internals (`policy_*`, `strategy`, `attempts`, `task`, `shaping`, `route_reason`, `trace_id`) in the `synapass` response block. |

A caller that sets `X-Synapass-No-Fallback` gets a `502` when the primary
target fails rather than a slow success from another provider.

## Inference

### `POST /v1/chat/completions`

The body is the OpenAI chat completions body. Synapass forwards only what the
client sent — an absent field is not filled in, because inventing a value the
provider treats differently is a subtle correctness bug.

| Field | Notes |
| --- | --- |
| `model` | Required. A registry name or an alias. |
| `messages` | Required unless `prompt` is sent. `content` may be a string or an array of parts; unknown part types pass through untouched. |
| `prompt` | Shorthand for a single user message. Mutually exclusive with `messages`. |
| `system` | Shorthand for a leading system message; prepended when `messages` are also sent. |
| `stream` | `true` returns SSE. |
| `top_k` / `min_p` / `repetition_penalty` | Extended sampling controls (≥1, 0–1, ≥0). Accepted by the gateway and forwarded only to upstreams that implement them: Anthropic (`top_k`), Ollama (`top_k`, `min_p`, `repeat_penalty`), vLLM and OpenAI-compatible servers (all three). Never sent to OpenAI proper, whose API has none of them. |
| `stream_options.include_usage` | `true` emits a terminal chunk carrying token usage and the final provider attribution. |
| `tools`, `tool_choice`, `parallel_tool_calls` | Requires the `tools` capability; `parallel_tool_calls` requires `parallel_tool`. Tools are validated before routing: a malformed declaration is a `400` naming the tool, and an unknown `tool_choice` name is a `400`. |
| `tool_execution` | `{"mode": "manual"}` (default: the model's tool calls reach the client untouched -- this is how OpenCode, Claude Code and other agentic apps run their own tools) or `{"mode": "automatic"}` (safe tools run inside the gateway in a bounded loop, **only when `tools.gateway_execution: true`**; otherwise automatic is clamped to manual rather than rejected). The request may only narrow the policy, never widen it. With gateway execution enabled, automatic execution with `stream: true` is a `400` -- streamed frames cannot be un-sent. |
| `response_format` | `json_object` requires `json_mode`; `json_schema` requires `json_schema`. `json_object` demands a JSON object, not merely valid JSON. An answer that violates the contract is a `400`; conformance is reported in `synapass.structured`. |
| `seed` | Requires the `seed` capability. Not sent to servers that do not support it. |
| `max_completion_tokens` / `max_tokens` | `max_completion_tokens` wins when both are present. |
| `metadata` | Passed through; also used for the end-user identifier in logs. |

Required capabilities are **derived** from the request body, not accepted from
the client: a caller cannot claim to need vision while sending an image the
selected model cannot read.

**Non-streaming response**

```json
{
  "id": "chatcmpl_01HX…",
  "object": "chat.completion",
  "created": 1767225600,
  "model": "gpt-4o-mini",
  "choices": [
    { "index": 0, "message": { "role": "assistant", "content": "hello" }, "finish_reason": "stop" }
  ],
  "usage": { "prompt_tokens": 9, "completion_tokens": 2, "total_tokens": 11 },
  "synapass": { "request_id": "req_01HX…", "provider": "openai", "requested_model": "gpt-4o-mini", "routed_model": "gpt-4o-mini", "fallback_used": false, "latency_ms": 912, "estimated_cost_usd": 0.0000026 }
}
```

The default `synapass` block is stable attribution only
(`request_id`, `provider`, `requested_model`, `routed_model`,
`fallback_used`, `cache_hit`, `latency_ms`, `estimated_cost_usd`).
`X-Synapass-Debug: true` restores policy names, strategy, attempts, task,
shaping and route reasons. For worked curl and SDK examples, see
[Getting started](getting-started.md#5-make-a-call).

**Thinking traces**

When the upstream volunteers thinking, the gateway forwards it rather than
dropping it: `reasoning_content` on the assistant message (non-streaming) or
on `delta` frames ahead of the answer (streaming), following the
OpenAI-compatible convention. Thinking is never mixed into `content`, so
agents that render the trace show it separately and agents that do not
understand it ignore the unknown field safely. The trace is part of the
stored response, so cache hits replay it the same way. Only forwarded, never
requested: the gateway sends no thinking budget the client did not ask for
(`reasoning_effort` passes through when the client sends it).

**Streaming**

Standard `text/event-stream` with `data: {...}\n\n` frames and a terminal
`data: [DONE]`. The response sets `Cache-Control: no-cache, no-transform` and
`X-Accel-Buffering: no` so intermediaries do not buffer the stream.

Provider attribution is deliberately **absent from the first frame**: the first
token can arrive before the attempt has finished, so claiming a provider there
would be a guess. `routed_model` and `requested_model` are present (they come
from the decision); `provider`, `fallback_used` and `attempts` appear on the
terminal frame when `stream_options.include_usage` is set.

A failure *after* the stream has started cannot change the status code, so it is
delivered as an `event: error` frame followed by `[DONE]`:

```
event: error
data: {"error":{"message":"upstream connection reset","code":"upstream_error"}}

data: [DONE]
```

A client that understands named SSE events sees a clean error; one that does not
still sees a terminated stream rather than a hang.

#### Truncated answers

An answer that stopped because a limit was reached carries
`synapass.completion`, so a caller never has to infer truncation from a missing
sentence ending:

```json
"synapass": {
  "completion": {
    "finish_reason": "length",
    "truncated": true,
    "reason": "max_tokens",
    "requested_tokens": 256,
    "applied_tokens": 256,
    "prompt_tokens": 1240,
    "completion_tokens": 256,
    "budget_ms": 300000
  }
}
```

`truncated` is true only when generation was cut short. `reason` is
`max_tokens` (the provider stopped at the applied ceiling) or `timeout` (a
deadline expired mid-answer). On a streaming request the block rides on the
terminal usage frame, like provider attribution.

`requested_tokens` is what the client asked for and is absent when it asked for
nothing. `applied_tokens` is the ceiling actually sent upstream and is absent in
the same case: a silent client is given the provider's own default rather than a
gateway-chosen one. The block is omitted entirely from a response that was
neither capped nor interrupted.

### `GET /v1/models`

Returns servable models, with aliases expanded into their own entries. Models
that cannot currently be routed are excluded: listing a model here and then
failing to route it would leave the client unable to tell whose problem it is.

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-4o-mini",
      "object": "model",
      "created": 1767225600,
      "owned_by": "openai",
      "synapass_provider": "openai",
      "synapass_context_window": 128000,
      "synapass_input_cost_per_million": 0.15,
      "synapass_output_cost_per_million": 0.6,
      "synapass_capabilities": ["chat", "streaming", "tools", "json_mode"]
    }
  ]
}
```

### `GET /v1/models/{model}`

One model object, resolving aliases case-insensitively. `404` with code
`not_found` if unregistered. Requires the `inference` scope — listing models is
part of the standard client handshake, so it is not gated behind a separate read
scope.

### Not implemented

`POST /v1/completions`, `POST /v1/embeddings` and `POST /v1/responses` return
`501` with code `not_implemented`. They are declared so a client gets a clear
answer rather than a 404 that reads as a misconfigured base URL.

## Operations

| Endpoint | Purpose |
| --- | --- |
| `GET /` | Landing page. Returns HTML to a browser and JSON otherwise, identifying the service and listing the real endpoints. Exists because a gateway reached from a browser — a temporary tunnel, a pasted URL — must not answer a bare URL with a 404 that reads as a broken deployment. Public, and discloses nothing `/health` does not already. |
| `GET /health`, `GET /healthz` | Liveness. **Does not** check dependencies, so a Postgres blip cannot cause the orchestrator to kill every replica. Reports which optional components are configured. |
| `GET /ready`, `GET /readiness` | Readiness. **Does** check dependencies, because that is the question a load balancer asks. `503` when Postgres is unreachable or no provider is usable. |
| `GET /version` | Build identity, uptime, goroutine count, environment. |
| `GET /metrics` | Prometheus exposition. Present when `telemetry.prometheus_enabled`. |

`/ready` distinguishes "not configured" from "configured but unhealthy":

```json
{
  "status": "ready",
  "checks": { "postgres": "ok", "redis": "degraded: dial tcp …", "providers": "2 configured" }
}
```

Redis being degraded does **not** fail readiness — rate limiting falls back to a
per-process limiter and the gateway can still serve.

## Console authentication

Five endpoints under `/admin/v1/auth`. They are mounted **outside** the admin
group and outside `adminMiddleware`, because they are how an operator obtains a
credential in the first place and gating them would be circular. They are not
unprotected in the sense that matters: setup is latched shut after the first
operator, and login is rate limited and locked out.

The dashboard proxies these. See
[dashboard.md](dashboard.md#authentication).

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `GET` | `/auth/state` | none | `{setup_required, authenticated, username?, policy}`. Discloses no account detail. |
| `POST` | `/auth/setup` | none | Creates the single initial operator. `403` once setup is closed. |
| `POST` | `/auth/login` | none | Verifies credentials and sets the session cookie. |
| `POST` | `/auth/logout` | cookie | Revokes the session and clears the cookie. Always `204`. |
| `GET` | `/auth/me` | cookie | The current operator. |

**Setup** (`POST /admin/v1/auth/setup`):

```json
{ "username": "admin", "password": "…", "confirm": "…" }
```

`201` with `{username, expires_at}` and a session cookie. `400` for a mismatched
confirmation or a password that fails the policy. `403` when setup is already
closed — which includes the case where every operator has been deleted, because
the latch is stored in `settings` rather than inferred from the user count.

**Login** (`POST /admin/v1/auth/login`):

```json
{ "username": "admin", "password": "…", "confirm": "" }
```

`confirm` is accepted and ignored; the setup and login screens post the same three
fields. `401` for a wrong password *and* for an unknown username, with an
identical message, so the form cannot enumerate accounts. `429` with `Retry-After`
while the account is locked out.

**Session cookie.** `httpOnly` always, `SameSite=Lax`, `Path=/`, and `Secure`
whenever the request arrived over TLS — or unconditionally if
`cookie_secure: true`. The token is 32 bytes of CSPRNG output; only its SHA-256 is
stored.

```bash
# The whole first-run flow, by hand.
curl -s $GATEWAY/admin/v1/auth/state | jq
curl -s -c jar -X POST $GATEWAY/admin/v1/auth/setup \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"a-long-passphrase","confirm":"a-long-passphrase"}' | jq
curl -s -b jar $GATEWAY/admin/v1/auth/me | jq
curl -s -b jar -X POST $GATEWAY/admin/v1/auth/logout -o /dev/null -w '%{http_code}\n'
```

Metrics: `synapass_dashboard_auth_events_total{outcome}`. Labelled by outcome
only — a username label would let an attacker inflate cardinality by guessing
names.

## Administration

Mounted at `/admin/v1` when `admin.enabled`. Every route requires an admin-scoped
key; read routes accept `usage:read`, and write routes narrow further by method
and path. Administrative routes never fall back to anonymous access, even in
development.

Common query parameters:

| Parameter | Applies to | Meaning |
| --- | --- | --- |
| `tenant_id` | most reads | Scope to one tenant. |
| `from`, `to` | windowed reads | RFC 3339, `2006-01-02`, or a Go duration (`24h`, `168h`). **There is no day unit** — `7d` is rejected; send `168h`. |
| `interval` | series reads | Bucket width: `1m`, `5m`, `1h`, `1d`, `1w`. Chosen from the window width when omitted. |
| `limit`, `offset` | list reads | Pagination. |
| `outcome`, `provider`, `model`, `error_code`, `search`, `status_min` | request and error logs | Filters. |

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/overview` | Landing payload: summary, previous-window summary, series, provider and model breakdowns, provider health. |
| `GET` | `/system` | Build, uptime, runtime, dependency state, providers, and the **redacted** effective configuration. |
| `GET` | `/providers` | Providers with health and a live `adapter_ready` flag. |
| `GET` | `/providers/health` | Health snapshot; add `provider_id` for recent probe history. |
| `POST` | `/providers/{id}/probe` | Run an on-demand health probe and record the result. |
| `GET` | `/models` | The registry, including disabled entries; filter with `provider`. |
| `GET` | `/policies` | All policies. |
| `GET` | `/policies/{id}` | One policy. |
| `PUT` | `/policies` | Upsert by name. Refreshes the resolver so the change applies immediately. |
| `DELETE` | `/policies/{id}` | Delete a policy. |
| `POST` | `/policies/reload` | Force a policy cache refresh. |
| `GET` | `/tenants` | All tenants. |
| `GET` | `/keys` | List a tenant's keys. `tenant_id` is **required**: listing every key across tenants is not permitted. |
| `POST` | `/keys` | Mint a key. The response contains `plaintext` exactly once. |
| `DELETE` | `/keys/{id}` | Revoke a key. Invalidates the credential cache so it takes effect immediately. |
| `GET` | `/usage/summary` | Aggregate plus per-provider and per-model breakdowns. |
| `GET` | `/usage/series` | Bucketed time series. |
| `GET` | `/requests` | Request log. |
| `GET` | `/requests/{requestID}` | One request, including its routing decision. |
| `GET` | `/errors` | Failures, with a `by_code` rollup. Defaults to `status_min=400`. |
| `GET` | `/budgets` | Budgets for a tenant. |
| `PUT` | `/budgets` | Upsert a tenant budget plus an optional hard per-request cap (`hard_cap_usd` flows to guardrails). |
| `GET` | `/audit` | Administrative events, filterable by `tenant_id` and `resource`. |
| `GET` | `/requests/{requestID}/explain` | Routing explanation: task, policy verdict, shaping, cache, scores and rejections. |
| `GET` | `/scores` | Explainable provider and model scores with blended [0,1] ranks. |
| `GET` | `/cache/stats?tenant_id=` | Exact/prefix/semantic hits, bypasses by reason, reuse count, latency saved, top prompts and effective config (Phase 5). |
| `GET` | `/cache/inspect?tenant_id=&model=&provider=&limit=` | Entry metadata for the dashboard (no bodies; bodies stay in Redis). |
| `POST` | `/cache/invalidate` | Scoped flush with scope, tenant_id, model, provider, key and reason. Tenant flushes are namespace-isolated; every flush is audited and published on NATS. |
| `GET` | `/cache/policies?tenant_id=` | Per-scope cache rules (key over endpoint over tenant over global). |
| `PUT` | `/cache/policies` | Upsert a cache rule (name, ttl_seconds, enabled, scopes). |
| `DELETE` | `/cache/policies/{id}` | Delete a cache rule. |
| `GET` | `/cache/invalidations?tenant_id=&limit=` | Flush audit trail with scope, reason, actor and removed counts. |
| `POST` | `/replay` | Create a replay job (`request_ids`, `providers`, `models`). |
| `GET` | `/replay` | List replay jobs. |
| `GET` | `/replay/{id}` | One replay job. |
| `GET` | `/evaluations` | List evaluation runs. |
| `GET` | `/evaluations/{id}` | One run plus scored results with regression flags. |
| `POST` | `/providers/{id}/kill` | Kill switch (`{"kill":true,"reason":"…"}`) or revive (`{"kill":false}`). |
| `GET` | `/guardrails` | Recent overrides (kill switches, caps, blocks). |
| `GET` | `/endpoints` | Endpoint scopes with routing overrides. |
| `PUT` | `/endpoints` | Upsert an endpoint by slug. |
| `POST` | `/tunnels/create` | Create a temporary public tunnel (`{"target":"gateway"}`). 201 with the running session and public URL; 409 when one is active. |
| `POST` | `/tunnels/stop` | Stop the active tunnel (`{"id":"…"}` optional). The URL dies with the process. |
| `POST` | `/tunnels/restart` | Restart, optionally retargeted. Always mints a new URL. |
| `GET` | `/tunnels/status` | Never fails: `{enabled, binary_available, active}` for dashboards and scripts. |
| `GET` | `/tunnels/current-url` | Stable `{url, status, target, …}` shape with `url: null` when idle. |
| `GET` | `/tunnels/history?limit=` | Session audit trail: target, URL, status, reconnects, teardown reason. |
| `POST` | `/v1/feedback` | Submit feedback (`request_id`, `score`, `comment`); inference-scoped. |

**Minting a key**

```bash
curl -s localhost:8080/admin/v1/keys \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"…","name":"payments-service","scopes":["inference"],"expires_in_hours":720}' | jq
```

Key creation returns the plaintext exactly once and stores only its digest, which
is the reason a leaked database yields no usable credential — and the reason an
existing key can never be re-displayed.

**Audit**

Every control-plane mutation writes an audit event. Inference traffic does not:
its accountability need (who spent what) is satisfied by the usage records, and
auditing it would dwarf the signal. An audit write failure is logged and never
fails the mutation it accompanies, since refusing a completed action would leave
the system matching neither the operator's intent nor the audit log.

### Phase 3: management

Full CRUD for the catalogue — providers, models, tenants and keys — plus
sealed credentials, connectivity tests and generic overrides. Every route below
lives under `/admin/v1` and requires an admin key; the scope narrows further by
path and method. An unknown id returns `404` with code `not_found`.

| Prefix | Required scope |
| --- | --- |
| `/providers*` | `providers:admin` |
| `GET /models`, `GET /models/{id}` | `usage:read` |
| `POST /models`, `PUT`, `PATCH`, `DELETE /models/{id}` | `providers:admin` |
| `/keys*` | `keys:admin` |
| `/tenants*` | `tenants:admin` |
| `/policies*` | `policies:admin` |
| `GET /overrides` | `usage:read` |
| `POST /overrides` | `providers:admin` |
| `DELETE /endpoints/{id}` | `*` (superuser) |

Model reads keep the historical `usage:read` grant so existing read-only
integrations do not break. Model writes sit under `providers:admin` because a
model cannot exist without a provider; splitting them would force every
operator to hold two scopes for one workflow.

**Providers**

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/providers` | Create a provider. `201`. `name` is immutable afterwards. |
| `GET` | `/providers/{id}` | One provider with `has_credential` and credential metadata, never the secret. |
| `PUT` | `/providers/{id}` | Full replacement. `name` is immutable; delete and recreate to rename. |
| `PATCH` | `/providers/{id}` | Partial update, including `status` to enable or disable. |
| `DELETE` | `/providers/{id}` | Delete a provider. Models cascade; returns `{deleted, models_removed}`. |

An inline `api_key` on create or update is discarded — secrets travel only
through the credential endpoint, where they are sealed before storage. Every
response is the provider detail: full configuration plus `has_credential` and,
when present, a `credential` metadata object. The secret itself is never
returned.

```http
POST /admin/v1/providers
Content-Type: application/json

{"name": "openai-prod", "kind": "openai", "base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"}
```

`PATCH {"status":"disabled"}` takes a provider out of rotation; re-enabling
audits `enable` rather than a plain `update`, so the audit log answers who
turned it back on directly. Disabling audits `disable` the same way.

**Credentials**

| Method | Path | Purpose |
| --- | --- | --- |
| `PUT` | `/providers/{id}/credential` | Seal `{"secret":"…","name?":"primary"}` with AES-256-GCM. |
| `GET` | `/providers/{id}/credential` | Metadata only: `{has_credential, credential?}`. |
| `DELETE` | `/providers/{id}/credential` | Remove the stored secret. |

```http
PUT /admin/v1/providers/{id}/credential
Content-Type: application/json

{"secret": "sk-…", "name": "primary"}
```

A first write audits `create` on resource `credential`; overwriting an existing
secret audits `rotate`. Either way the response is `{provider_id,
has_credential, credential}` — metadata, never plaintext. A credential write
also marks the provider `api`-managed, so the bootstrapper never reverts the
setup it belongs to. Passing `"sync_models": true` discovers the provider's
remote models into the registry in the same call; the response then carries a
`sync` summary (or `sync_error` when discovery fails after the credential was
already stored).

Credential precedence is fixed: `api_key_env` wins when set, then the stored
encrypted credential, then nothing. Resolution happens once per catalogue
refresh, never on the request path; a provider with neither binding constructs
no adapter and is excluded from routing rather than failing requests.

**Connectivity tests**

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/providers/{id}/test` | Run checks. Returns `{success, results[]}`. |
| `GET` | `/providers/{id}/tests` | Test history. `limit` defaults to 50. |
| `POST` | `/providers/{id}/sync-models` | Discover remote models into the registry. Returns `{created, skipped, total, capability_source, capabilities_filled}`. |
| `POST` | `/providers/{id}/detect-capabilities` | Probe models with no declared capabilities (`{"models":[…]}` to scope). Returns `{attempted, proven, indeterminate, skipped}`. |

```http
POST /admin/v1/providers/{id}/test
Content-Type: application/json

{"checks": ["connectivity", "models", "sample"], "model": "gpt-4o-mini", "prompt": "say ok"}
```

`checks` defaults to all three when omitted; an unknown check name is `400`.
Each result is persisted with latency, status and check-specific detail, and
the run audits `test` on the provider. The handler reloads the catalogue first,
so a provider created moments ago already has an adapter when tested.

Model discovery lists the provider's remote models and inserts every missing
name as an active `api`-managed row inheriting the provider's environment.
Existing rows are never modified, so pricing, aliases, priorities and disables
survive re-syncs; delete a row to drop a model permanently. Kinds without
remote listing (none of the built-ins) return `501 not_implemented`.

**Models**

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/models` | Create a model. `201`. |
| `GET` | `/models/{id}` | One model. |
| `PUT` | `/models/{id}` | Full replacement. `name` and `provider_id` are immutable. |
| `PATCH` | `/models/{id}` | Partial update, including `status` to enable or disable. |
| `DELETE` | `/models/{id}` | Delete a model. Returns `{deleted}`. |

`name` plus provider is the natural key: renaming or moving a model means
delete and recreate. An omitted `priority` defaults to `100`, matching the
database default, so an operator omitting the field gets the same row the
bootstrapper would write. Status transitions audit `enable` and `disable` like
providers.

**Tenants**

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/tenants` | Create a tenant. `201`. |
| `GET` | `/tenants/{id}` | One tenant. |
| `PUT` | `/tenants/{id}` | Full replacement, except `slug` is immutable. An absent `plan` clears it. |
| `DELETE` | `/tenants/{id}` | Delete a tenant. Returns `{deleted, keys_removed}`. |

```http
POST /admin/v1/tenants
Content-Type: application/json

{"slug": "acme", "name": "Acme Inc", "plan": "scale"}
```

A delete is refused with `400` while the tenant still holds active API keys —
deleting it would orphan client configurations with no error pointing at the
cause. Revoke the keys first, or retry with `?force=true`.

**Keys**

| Method | Path | Purpose |
| --- | --- | --- |
| `PATCH` | `/keys/{id}` | Update `name`, `scopes`, `expires_at` (RFC 3339, or `null` to clear), `routing_policy_id`. |
| `POST` | `/keys/{id}/rotate` | Mint a replacement secret. `201`. The old secret stops working immediately. |

```http
PATCH /admin/v1/keys/{id}
Content-Type: application/json

{"name": "payments-service", "scopes": ["inference"], "expires_at": null}
```

Rotation keeps the key's identity, tenant, name and scopes; only the secret
changes. The response is `{key, plaintext, warning}` and the plaintext appears
exactly once, like creation — a leaked database still yields no usable
credential. The credential cache is invalidated as part of the rotation, so the
old secret stops working now rather than after the cache TTL. Rotation audits
`rotate` on resource `api_key`.

**Policies**

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/policies` | Create a policy. `201`. A name clash in the same tenant is `409`. |
| `PUT` | `/policies` | Upsert by name. Remains the idempotent path; refreshes the resolver immediately. |

`POST` is the guarded creation: a policy with the same name for the same tenant
is rejected so a retry cannot silently overwrite. `PUT` keeps the existing
upsert semantics for operators who mean "ensure this shape".

**Overrides and endpoints**

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/overrides` | Recent overrides. `limit` defaults to 100. |
| `POST` | `/overrides` | Write an override. `201`. |
| `DELETE` | `/endpoints/{id}` | Delete an endpoint scope. Requires `*`. |

```http
POST /admin/v1/overrides
Content-Type: application/json

{"kind": "kill", "target": "openai", "enabled": true, "reason": "upstream incident", "expires_at": "2026-10-01T00:00:00Z"}
```

`kind` is required; `target`, `tenant_id`, `reason` and RFC 3339 `expires_at`
are optional. An override is an event row, so revocation means writing the
inverse row (`enabled:false`) rather than editing history — the log keeps what
was true and when.

**Ownership, audit and reload**

Every provider, model and policy carries `managed_by`: `bootstrap` for rows
owned by the configuration seeder, `api` for rows created or edited through
this surface. The bootstrapper never overwrites an `api`-managed
provider, model or policy on restart (including models added to a
bootstrap-owned provider), which is what keeps a dashboard edit from being
silently reverted by the next deploy.

Phase 3 adds audit actions `test`, `enable` and `disable`, and resources
`credential` and `test_result`. Status flips audit as `enable` or `disable`
rather than `update`; credential creation audits `create` and replacement
audits `rotate`. Reads and test-history listing are not audited.

Every write above triggers a runtime reload — catalogue plus adapter plus
policy refresh — so an operator's save applies immediately. A failed reload is
logged, never returned: the background refresh loop converges anyway and acts
as the backstop.

### Phase 4: tools

Tool registry, tool policies, invocation history and agent runs. Single
resources return the bare object (the Phase 3 convention); lists return an
envelope. Executability is derived from `kind`, never granted by a write:
only `builtin` tools with a known handler run inside the gateway.

| Prefix | Required scope |
| --- | --- |
| `GET /tools`, `GET /tool-policies`, `GET /tool-invocations`, `GET /agent-runs` | `usage:read` |
| `POST/PUT/DELETE /tools*`, `PUT/DELETE /tool-policies*` | `providers:admin` |

**Registry**

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/tools` | Every registered tool: `{tools: [...]}`. |
| `POST` | `/tools` | Register a tool. `name` is immutable afterwards; a `builtin` requires a known `handler`. |
| `PUT` | `/tools/{id}` | Partial update (description, safety, parameters, flags). |
| `POST` | `/tools/{id}/enabled` | `{"enabled": bool}`. |
| `DELETE` | `/tools/{id}` | Remove a registry entry. History is kept. |

`parameters` must be a JSON Schema object and is compiled on write: an unknown
keyword is a `400`, because a constraint the gateway does not enforce must not
look enforced. `owner: tenant` requires `tenant_id`, which must exist.

**Policies**

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/tool-policies` | `{policies: [...]}`. |
| `PUT` | `/tool-policies` | Upsert by name + tenant. Bounds are clamped (`max_steps` 1–10, `max_tool_calls` 1–64, `max_run_seconds` 1–600). |
| `DELETE` | `/tool-policies/{id}` | Delete a policy. Requests fall back to the built-in manual default. |

Omitting `enabled` means enabled on create, and leaves the value alone on
update — creating a policy with only a mode and bounds stores a policy that
takes effect, not a silently inert one.

**History**

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/tool-invocations?limit=` | `{invocations, total}`: every model-requested call with arguments, status and outcome. |
| `GET` | `/tool-invocations/{id}` | `{invocation}` with its execution record. |
| `GET` | `/agent-runs?limit=` | `{runs, total}`: every bounded run with its terminal status and work done. |
| `GET` | `/agent-runs/{id}` | `{run, steps}`: the run plus its full model/tool step trace. |

**Response metadata**

A request that involved tools carries `synapass.tool_run`:

```json
"tool_run": {
  "mode": "automatic",
  "status": "gateway_executed",
  "run_id": "731b8540-…",
  "steps": 3, "calls": 3, "executed": 3,
  "stop_reason": "stopped at the 3-step limit",
  "tools": ["now", "now", "now"],
  "latency_ms": 8383
}
```

`status` is `client_executed` when the model called tools and the client runs
them, `gateway_executed` when the loop ran them, `gateway_failed` when the run
itself errored, and `denied` when policy refused. A bounded run that ended
with the model still asking for tools answers with a plain-text explanation
and `finish_reason: stop`, because a dangling `tool_calls` the client cannot
satisfy is worse than an honest summary.

A request with `response_format` carries `synapass.structured`
(`requested`, `valid`, `schema`, `error`, `repaired`): the gateway extracts a
fenced JSON object when it is unambiguous and says so, rather than failing a
response the caller could have used.

---

Related: [Getting started](getting-started.md) · [Routing](routing.md) · [Providers](providers.md) · [Tools](tools.md) · [Dashboard](dashboard.md) · [Troubleshooting](troubleshooting.md) · [Back to README](../README.md)
