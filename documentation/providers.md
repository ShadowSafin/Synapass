# Providers

A provider is one upstream inference service. Synapass talks to it through an
adapter, knows what it costs and what it can do, and routes requests to it
according to policy. This page covers adding providers, credentials, kinds,
testing and day-to-day management.

> **Secrets are referenced, never embedded.** A provider record names an
> environment variable (`api_key_env`) or holds an encrypted credential. A
> plaintext key on a provider create or update is discarded. That keeps the
> config file and the database free of credentials you would have to rotate.

## Provider kinds

| Kind | Base URL | Auth style | Notes |
| --- | --- | --- | --- |
| `openai` | `https://api.openai.com/v1` | `Authorization: Bearer` | Sends `seed`; omits `top_k`, `min_p`, `repetition_penalty`, which OpenAI's API does not have. |
| `anthropic` | `https://api.anthropic.com` | `x-api-key` + `anthropic-version` | `max_tokens` is mandatory. Supports `top_k`. |
| `ollama` | `http://host:11434` | none by default | Native NDJSON streaming. Supports `top_k`, `min_p`, `repeat_penalty`. |
| `vllm` | your server | `Authorization: Bearer` | OpenAI-compatible plus `seed`, `top_k`, `min_p`, `repetition_penalty`. |
| `openai_compatible` | your server | `Authorization: Bearer` | The escape hatch for anything that speaks the OpenAI schema but is not OpenAI. |

Sampling controls are forwarded only to kinds that implement them. Sending
`top_k` to a provider that rejects it turns a working request into a `400`.

## Adding a provider

### Through the dashboard

Open **Providers** → Add. The form takes a name, kind, base URL and credential
source. Tick **Sync models** to discover the provider's remote models in the same
call; the per-row **Sync** button re-runs discovery later.

### Through the API

```bash
curl -s $GATEWAY/admin/v1/providers \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "openai-prod",
    "kind": "openai",
    "base_url": "https://api.openai.com/v1",
    "api_key_env": "OPENAI_API_KEY",
    "sync_models": true
  }' | jq
```

`name` is immutable. To rename, delete and recreate.

Discovery inserts every missing remote name as an active, `api`-managed row
inheriting the provider's environment. Existing rows are never modified, so your
pricing, aliases, priorities and disables survive a re-sync. Delete a row to drop
a model permanently.

A discovered model arrives knowing its name and nothing else — the OpenAI
`/v1/models` response carries no capability metadata — so it inherits the
provider kind's defaults until someone says otherwise. Three ways to say otherwise,
in order of preference:

1. **Declare it** when you add the model, or `PATCH` the row later. What you
write is final: nothing automatic ever overwrites a declared list.
2. **Read it from the provider** (`POST …/sync-models`, or tick **Sync models**
on create). Routers that publish it — LiteLLM-style `/model/info` with
`supports_function_calling` and friends — have it recorded per model for free.
When the catalogue carries names only, sync guesses known families
(`claude-*`, `gpt-*`, `gemini-*`, `deepseek-*`, `grok-*`, `kimi-*`,
`minimax-*`) for free — zero upstream calls, recorded as
`capabilities_source: inferred`. Image/video/audio models are never guessed.
A guess sits below published metadata in trust, so a later catalogue (or your
edit) replaces it.
3. **Ask the model** (`POST …/detect-capabilities`). It sends each silent model
minimal probes — a one-token completion carrying tools, an event-stream
attempt, then JSON modes — and records only what a successful call proves.
   A 400 that names the feature means it cannot do it; auth failures, rate
   limits and 5xx mean nothing was decided and the row is left alone. Provenance
   lands in the row metadata as
   `capabilities_source: provider | probed | declared`. Chat is recorded
   whenever anything succeeds, because every probe is itself a chat completion —
   a proven list without it would unroute the model from plain requests.

Detection spends real upstream tokens (four tiny calls per model by default),
so the automatic post-sync run is off: set `detection.enabled: true` in the
config (or `SYNAPASS_DETECTION_ENABLED=true`), or call the endpoint with an
explicit `{"models": [...]}`. Bounds
(`max_models_per_run`, `concurrency`, `timeout_per_model`) keep one click from
becoming a bill, and an explicit model list scopes it further. Vision is never
probed — it needs an image payload, a different cost class from a one-token
text probe.

Every sync — and every detect run — ends with provider reconciliation: the
union of what the provider's models declare is written to the provider row
when the provider declares nothing. Routing gates on the provider-wide list
first, so without this step models that learned tools still could not serve
them. A provider list you wrote by hand is final and never touched.

## Credentials

Two sources, with fixed precedence.

| Source | How to set it |
| --- | --- |
| Environment binding | `api_key_env: "OPENAI_API_KEY"` on the provider record |
| Stored credential | `PUT /admin/v1/providers/{id}/credential` |

**The environment binding wins when both are present.** Setting both is allowed
and the detail view shows both facts, so the precedence is documented rather than
silent.

```bash
curl -s -X PUT $GATEWAY/admin/v1/providers/$ID/credential \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"secret":"sk-…","name":"primary"}' | jq
```

Stored credentials are sealed with AES-256-GCM in `provider_credentials`, one
active credential per provider. Reads return metadata only:

```bash
curl -s $GATEWAY/admin/v1/providers/$ID/credential -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
# → {"has_credential": true, "credential": {"name": "primary", "key_version": 3}}
```

The plaintext never appears in a response, a log, an audit event or the UI. The
first write audits `create`; overwriting audits `rotate`.

### Key material

| Setting | Behaviour |
| --- | --- |
| `SYNAPASS_CREDENTIALS_KEY` (32 bytes, raw/hex/base64) | Used directly. Set this for serious deployments. |
| Unset | The data key is derived from `SYNAPASS_ADMIN_KEY` via HKDF-SHA256, so stock deployments need no new configuration. |

Rotating `SYNAPASS_ADMIN_KEY` without setting `SYNAPASS_CREDENTIALS_KEY` orphans stored
credentials — re-save them after a rotation.

### Resolution timing

Credentials are resolved once per catalogue refresh, never on the request path. A
provider with neither binding constructs no adapter and is excluded from routing
rather than failing requests.

## Models

A model is a servable name, its context window, its price and its capabilities.

```bash
curl -s $GATEWAY/admin/v1/models \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider_id": "'"$PROVIDER_ID"'",
    "name": "gpt-4o-mini",
    "context_window": 128000,
    "max_output_tokens": 16384,
    "input_cost_per_million": 0.15,
    "output_cost_per_million": 0.6,
    "capabilities": ["chat", "streaming", "tools", "json_mode"]
  }' | jq
```

`name` plus provider is the natural key, so renaming or moving a model means
delete and recreate. An omitted `priority` defaults to `100`, matching the
database default, so you get the same row the bootstrapper would write.

Capabilities the router actually checks:

| Capability | Meaning |
| --- | --- |
| `chat` | Serves chat completions. |
| `streaming` | Serves SSE. |
| `tools` | Accepts tool definitions and can emit `tool_calls`. |
| `parallel_tools` | Honours `parallel_tool_calls`. |
| `json_mode` | Accepts `response_format: {"type":"json_object"}`. |
| `json_schema` | Accepts a `json_schema`. |
| `vision` | Accepts image parts. |
| `seed` | Honours a `seed` for reproducible sampling. |

Required capabilities are **derived from the request body**, not claimed by the
client. A caller cannot declare that it needs vision while sending an image the
selected model cannot read.

## Testing

Three checks, each persisted with latency and status:

| Check | What it does |
| --- | --- |
| `connectivity` | The adapter's health check against the provider. |
| `models` | Remote model listing. Skipped with a note for kinds that have none. |
| `sample` | A 16-token completion on the first usable stored model, an explicit `model`, or the first remote name. |

```bash
curl -s -X POST $GATEWAY/admin/v1/providers/$ID/test \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"checks":["connectivity","models","sample"],"model":"gpt-4o-mini","prompt":"say ok"}' | jq
```

`checks` defaults to all three; an unknown check name is a `400`. Checks never
abort the run early — each persists to `provider_test_results` and the run audits
`test` once with a summary. History is `GET …/tests?limit=` (default 50).

The handler reloads the catalogue first, so a provider created moments ago
already has an adapter when tested.

## Health

Every provider carries a circuit breaker fed by two sources: active probes on
`health_check_interval`, and passive observation of live traffic.

```
consecutive failures ≥ 3   → degraded   (still eligible, deprioritized)
consecutive failures ≥ 9   → unhealthy  (excluded from routing)
rolling error rate > 50%   → degraded
```

Marking a provider unhealthy too eagerly offloads its traffic onto its peers,
which is worse than occasionally routing to one that is merely struggling, so the
thresholds are asymmetric on purpose. A successful probe does not by itself close
an open circuit — a probe that succeeds while live traffic fails would flap.

Run an on-demand probe:

```bash
curl -s -X POST $GATEWAY/admin/v1/providers/$ID/probe -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
```

Inspect current state:

```bash
curl -s "$GATEWAY/admin/v1/providers/health?provider_id=$ID" -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
```

## Stream liveness

Every streaming adapter (OpenAI, Anthropic, Ollama) enforces the timeout
policy's `first_token` and `stream_idle` budgets: the first event must
arrive within `first_token`, and no gap between events may exceed
`stream_idle`, or the attempt fails fast with a `timeout` rather than
burning the whole per-attempt budget. A watchdog also aborts zero-byte
stalls where the transport itself goes quiet. Anthropic `ping` frames keep
the connection alive but do not count as activity. Tune the budgets in the
routing policy (`timeout.first_token`, `timeout.stream_idle`) — see
[Routing](routing.md).

## Managing providers

| Method | Path | Effect |
| --- | --- | --- |
| `GET` | `/providers` | List with health and `adapter_ready`. |
| `GET` | `/providers/{id}` | Detail with `has_credential`, never the secret. |
| `PUT` | `/providers/{id}` | Full replacement. `name` immutable. |
| `PATCH` | `/providers/{id}` | Partial update, including `status`. |
| `DELETE` | `/providers/{id}` | Delete. Models cascade; returns `{deleted, models_removed}`. |
| `POST` | `/providers/{id}/kill` | Kill switch (`{"kill":true,"reason":"…"}`) or revive (`{"kill":false}`). |
| `POST` | `/providers/{id}/sync-models` | Discover remote models. Returns `{created, skipped, total, capability_source, capabilities_filled}`. |
| `POST` | `/providers/{id}/detect-capabilities` | Probe silent models (`{"models":[…]}` to scope, plus `max_models`, `concurrency`, `timeout_per_model` overrides). Returns `{attempted, proven, indeterminate, skipped}`. |

`PATCH {"status":"disabled"}` takes a provider out of rotation; re-enabling audits
`enable` rather than a plain `update`, so the audit log answers who turned it
back on.

### Ownership

Every provider, model and policy carries `managed_by`: `bootstrap` for rows owned
by the configuration seeder, `api` for rows created or edited through this
surface. The bootstrapper never overwrites an `api`-managed row on restart,
which is what keeps a dashboard edit from being silently reverted by the next
deploy. To hand a row back to the config file, delete it through the API and let
the seeder recreate it.

## Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| `adapter_ready: false` | No credential resolvable | Set `api_key_env` or store a credential |
| `/ready` says `providers: 0 configured` | No provider has a working adapter | Check each provider's `adapter_ready` and credential |
| Every request fails with `401` from upstream | Wrong or expired credential | Re-store it and confirm with `…/test` |
| A model is missing from `/v1/models` | Disabled, or its provider is | Enable it; the list only contains servable models |
| `sync-models` returns `501` | The kind has no remote listing | Add models by hand |
| Cost looks wrong | Prices are estimates from the registry | Update `input_cost_per_million` / `output_cost_per_million` |

---

Related: [Routing](routing.md) · [API reference](api.md) · [Dashboard](dashboard.md) · [Troubleshooting](troubleshooting.md) · [Back to README](../README.md)