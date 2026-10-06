# Troubleshooting

Symptoms grouped by where they appear. Each entry gives what to check and what
usually causes it.

## Contents

- [Startup and health](#startup-and-health)
- [Native installs](#native-installs)
- [Providers](#providers)
- [Requests failing](#requests-failing)
- [Streaming](#streaming)
- [Short or wrong answers](#short-or-wrong-answers)
- [Caching](#caching)
- [Tools](#tools)
- [Data and dashboards](#data-and-dashboards)
- [Tunnels](#tunnels)

---

## Startup and health

### The gateway will not start

Check the log for the specific refusal. Validation is fail-fast where a mistake is
dangerous:

| Message | Cause | Fix |
| --- | --- | --- |
| `admin key required` | `admin.require_scope: true` and no `SYNAPASS_ADMIN_KEY` | Set it, even in a dev environment you intend to operate |
| `tracing enabled but no otlp endpoint` | `telemetry.tracing_enabled: true` with no endpoint | Set an endpoint or disable tracing |
| `CORS wildcard not allowed in production` | `*` origin with `environment: production` | Name your origins |
| `failed to reach postgres` | System of record unavailable | Fix connectivity; the gateway will not serve without it |

Missing ClickHouse, NATS or Redis is **not** a startup failure. Those are
reported as degraded in `/ready`.

### `/health` fails but `/ready` would have been the better check

`/health` is liveness and deliberately does not touch dependencies, so a database
blip cannot cause an orchestrator to restart every replica. Use `/ready` for
anything that depends on the outside world.

### `/ready` returns 503

```json
{
  "status": "unready",
  "checks": { "postgres": "dial tcp …: connect: connection refused", "providers": "0 configured" }
}
```

- Postgres unreachable → nothing works. Fix it.
- `providers: 0 configured` → at least one provider must have a working adapter.
  A provider with no resolvable credential is configured but has no adapter,
  which is the usual cause. Check `adapter_ready` per provider.

### `/ready` says `redis: degraded` but readiness passes

Intended. Rate limits fall back to a per-process limiter and caching stops.
Inference still works; limits are per-replica rather than global until Redis
returns.

## Native installs

Run `synapass native doctor` first: it checks configuration, binaries,
datastores and ports without changing anything, and every failure names its
fix.

### `native up` exits before anything is ready

Read the first failure, not the last log line: the supervisor stops the whole
stack on the first terminal error, so the cause is at the top. Usual causes
are a refused datastore connection (PostgreSQL not running or wrong
credentials in `native.env`) and a failed migration (fix the database, then
re-run `native install` — it resumes).

### `port is already in use` from doctor or `up`

Another process owns the gateway or dashboard port. Free it, or move ours:
`SYNAPASS_HTTP_ADDR` for the gateway, `PORT` (or `--dashboard-port`) for the
dashboard. `native doctor` reports exactly which bind failed.

### Dashboard shows a blank page or API errors in native mode

The dashboard proxies the gateway server-side, so this is almost always
`SYNAPASS_API_URL` pointing somewhere the dashboard server cannot reach
(Docker service names do not resolve on a host), or a mismatched
`SYNAPASS_ADMIN_KEY`. Both live in `native.env`; `native up` defaults the
URL to the local gateway listener when unset.

### `dashboard not built` in native mode

The supervisor launches `.next/standalone/server.js`, which only exists after
`npm ci && npm run build` in the dashboard directory. `native install` does
this; a manual checkout needs it by hand. Pointing at the wrong directory
(`NATIVE_DASHBOARD_DIR`) fails the same way.

### Workers never start under `native up`

Workers are opt-in: pass `--with-workers` and provide the venv
(`NATIVE_WORKER_VENV`, default `/opt/synapass/venv`, `bin/python` on Linux,
`Scripts\python.exe` on Windows). Without the flag the supervisor runs
gateway + dashboard only, which is a complete install.

## Providers

### `adapter_ready: false`

No credential resolvable. Either `api_key_env` names a variable that is not set
in the gateway's environment, or no credential has been stored. See
[Providers](providers.md#credentials).

### Every request returns an upstream `401`

Wrong or expired credential. Re-store it and confirm with a connectivity test:

```bash
curl -s -X POST $GATEWAY/admin/v1/providers/$ID/test \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" -d '{"checks":["connectivity"]}' | jq
```

### A provider gets no traffic

1. `synapass_provider_health` — is the breaker open?
2. Is it in the policy's `targets`?
3. Does it have the capabilities the request needs?
4. Does prompt + expected output fit its context window?
5. Read the `explain` payload for the per-candidate rejection reasons.

### `sync-models` returns 501

The provider kind has no remote model listing. Add models by hand.

### An agent talks about calling tools but never calls any

The upstream accepted the tools shape and returned an empty completion
(no text, no calls, no refusal) — common on OpenAI-compatible backends
whose tool translation is broken for a model family. The gateway treats
this as an upstream failure, not a success: non-streaming requests fail
over to the next candidate (or get a clear `502` naming the model), and
streaming requests get an in-stream error instead of an empty stop. If a
model proves unable to serve tools, remove `tools` from its row
(`PATCH /admin/v1/models/{id}`) so tool traffic routes to capable models
immediately instead of burning attempts.

## Requests failing

### `503` from the gateway itself

Read the error envelope — it carries a `synapass` block naming the policy,
strategy, provider and attempts:

```json
{
  "error": { "message": "the request exceeded its total time budget", "type": "timeout", "code": "timeout" },
  "synapass": { "provider": "openai-prod", "attempts": 2, "fallback_used": true }
}
```

### `429 rate_limited`

Either the gateway's own limit or the provider's. Check `Retry-After`, then
`synapass_policy_rate_limited_total` versus the provider's status. If the
provider is throttling and fallback did not engage, check the policy's
`on_error_codes` — that list is exhaustive when present.

### `403 permission_error`

The key lacks the scope, or a policy denied the request. The message names the
rule that fired for policy denials.

### `402 quota_exceeded`

Either a hard upstream quota or your own spend ceiling. Check **Budgets** and
`synapass_policy_budget_blocked_total`.

### `502 upstream_error` with `fallback_used: true`

The primary failed and another provider answered. That is the system working.
Look at the primary provider's health.

## Streaming

### The stream ends early with no error

Older gateways had this bug: the soft latency target was reused as the hard
per-attempt deadline, so a stream was cancelled at the default 10s mark with
nothing reporting it. Fixed — the deadline now comes only from the timeout policy.

If you still see it, check the response's `synapass.completion` block:

```json
"completion": { "truncated": true, "reason": "timeout", "budget_ms": 300000 }
```

`budget_ms` tells you which budget applied. If it is short, a policy set it
explicitly; raise `timeout.per_attempt`.

### `event: error` part way through a stream

The failure happened after bytes reached the client, so the status code can no
longer be changed. Synapass does not fail over in this situation: retrying
would append a second attempt's tokens to the first, producing an answer that is
duplicated and incoherent. A terminated stream plus a clear error beats a
plausible-looking corrupted answer.

### `400` on an automatic tool request with `stream: true`

Streamed tool-call frames have already reached the client, so a multi-step
gateway-side run cannot be un-sent. Stream the model's tool calls and execute
them client-side instead. See [Tools](tools.md).

### Frames arrive but no `[DONE]`

A proxy or ingress is buffering. Synapass sets `Cache-Control: no-cache,
no-transform` and `X-Accel-Buffering: no`; make sure nothing in front of it
rewrites those.

## Short or wrong answers

This was the original bug, and it is worth being precise about.

### Answers stop mid-sentence

Check in order:

1. `synapass_provider_truncations_total{reason}` — `max_tokens` or `timeout`?
2. `synapass.completion` in the response — `requested_tokens`, `applied_tokens`,
   `budget_ms`, `finish_reason`.
3. The routing decision's `timeout.per_attempt`. A per-attempt deadline shorter
   than a long generation truncates by definition.
4. The client's own `max_tokens`.

A common remaining cause is a **second** wall: the shared transport used to set
`ResponseHeaderTimeout` to the first-token budget, which aborts a *buffered*
request before headers arrive, because a provider only sends them once the whole
answer exists. That is removed; a buffered 60-second generation now works.

### `applied_tokens` is 0 but the answer is short

Then the gateway did not cap it — the provider's own default stopped generation.
`finish_reason` will be `length`. Raise the client's `max_tokens` or the model's
`max_output_tokens`.

### The answer ignores most of the prompt

Prompt shaping trimmed it. Check `synapass.shaping` in the response, or the
explain view. Trimming preserves system messages and reports each step with its
token delta, so this should be visible rather than mysterious.

### The wrong provider answered

Read `routed_model` and `provider` in the `synapass` block, then the explain
view. `provider` is who **answered**, not who was chosen first.

## Caching

See [Caching](caching.md#common-problems) for the full table. The two that catch
people out:

- **Everything bypasses with `nondeterministic_request`.** Unseeded
  `temperature > 0` bypasses by default. Seed it, or set
  `allow_nondeterministic: true`.
- **A stale answer after a model change.** Entries are not invalidated by the
  change itself. Flush the model or provider scope.

## Tools

### `tool_choice: "required"` returns 403

The policy has gateway execution disabled, so automatic execution is not
permitted. Either enable `tools.gateway_execution` or use `auto`.

### A tool never runs

Only `builtin` tools with a known handler run inside the gateway. Everything else
is advertised, argument-validated, and handed back to you. `now` and `echo` are
the only built-ins.

### A run stopped with a text explanation instead of `tool_calls`

A bound was reached — steps, calls or wall clock. The tool results live in the
gateway's run and you cannot resume it, so the gateway explains itself rather
than returning a dangling `tool_calls` you cannot satisfy. Check
`synapass.tool_run.stop_reason`.

### An external tool was rejected with "not executable"

That is deliberate. Executability is derived from `kind`, never granted by a
write; the field is not writable.

## Data and dashboards

### The dashboard loads but panels are empty

`SYNAPASS_API_URL` must be the gateway as seen from the dashboard **server**,
not from the browser. From inside Compose the dashboard reaches the gateway at
`http://gateway:8080`, not `localhost`.

### Dashboard edits vanish after a restart

The row is `bootstrap`-managed, so the configuration seeder recreates it. Re-create
it through the API so it becomes `api`-managed. See [Database](database.md).

### `migration checksum mismatch`

A migration file changed after it was applied. Restore the original file, or
reconcile the database deliberately.

### `synapass_async_dropped_total` is climbing

ClickHouse or NATS is unreachable, or the queue is saturated. Restore the
dependency. Inference is unaffected by design — that is the trade-off the
asynchronous pipeline makes.

## Tunnels

### Creating a tunnel returns 404

`cloudflared` is not on the gateway host. The response names the binary and where
to get it; the gateway itself is unaffected. See
[Cloudflare tunnel](installation/cloudflare-tunnel.md).

### `409` on create

One tunnel is already active. One active tunnel per gateway by design.

### The URL changed

A restart always mints a new URL. Quick tunnel URLs are per-session and die with
the process; treat a live URL as public.

### The URL works but requests are unauthorized

Expected. Authentication is unchanged through the tunnel: inference needs an API
key, admin needs the admin credential. The tunnel is a network path, not a bypass.

## Still stuck

1. `curl -s $GATEWAY/version | jq` — confirm the build.
2. `curl -s $GATEWAY/admin/v1/system -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq`
   — dependency state and the redacted effective configuration.
3. The request's explain payload — the most informative single artifact.
4. `docker compose logs --tail=200 gateway`, or `journalctl -u synapass -n 200`.

---

Related: [FAQ](faq.md) · [Observability](observability.md) · [Providers](providers.md) · [Back to README](../README.md)