# Caching

Synapass can reuse a prior response instead of calling a provider again. The
cache is a policy-aware part of the request flow, not a layer bolted on beside
routing: most requests deliberately bypass it, and every reuse is scoped so one
tenant can never see another's answer.

Off by default. Turn it on deliberately:

```yaml
cache:
  response_cache: true
  exact_enabled: true
  response_ttl: 5m
```

## Request flow

1. Authenticate the request.
2. Apply tenant and endpoint policy.
3. Evaluate the cache decision — sensitivity, tools, live data,
   determinism, policy and endpoint switches. Streams participate fully:
   a hit replays as SSE and a clean completion is stored (see below).
4. Compute the full cache key.
5. Check **exact → prefix → semantic**.
6. On a hit, return the cached response with `synapass.cache_hit: true`.
7. On a miss, route to the provider as usual.
8. Store the final response with serving metadata when eligible.
9. Record metrics, traces and entry metadata.
10. Surface everything on the dashboard's `/cache` page.

The public inference API does not change. A hit returns the same JSON shape as a
live response; only the `synapass` block gains `cache_hit`, `cache_kind`,
`cache_similarity` and `cache_reuse_count`.

## Tiers

| Tier | Key | Serves | Gate |
| --- | --- | --- | --- |
| **Exact** | Full normalized request hash | Byte-identical requests | `response_cache` + `exact_enabled` |
| **Prefix** | System/developer prefix hash | Short prompts only | `prefix_enabled`, `prefix_length` |
| **Semantic** | Word-bag cosine over an in-process index | Similar prompts | `semantic_enabled`, `semantic_threshold` |

**Prefix** serves a full response only when the normalized prompt length is
`<= prefix_length` (default 256). A shared 256-character head with a different
tail never hits — that was a real correctness gap, and the length gate closes it.
Long chats still benefit through prefix accounting and system-prefix reuse
visibility.

**Semantic** is a deterministic word-bag cosine, tenant- and model-isolated,
behind `semantic_threshold` (default 0.92). No ML model runs on the request path.
Every hit carries its similarity score in traces and metrics, so a surprising hit
is always explicable.

## The cache key

`internal/cache/key.go` builds the key from every field that can change the
answer:

- tenant and API key — isolation is structural, not conventional
- requested model
- full message content, including system and developer turns and tool-call history
- tool definitions (full schemas, not just names) and `tool_choice`
- generation settings: max tokens, temperature, top_p/top_k/min_p, repetition,
  presence and frequency penalties, seed, n
- response contract: `response_format` type and schema hash, reasoning effort
- routing policy id and version, endpoint scope, sensitivity, end user

`PromptHash` and `ToolsHash` are stored beside the body so a hit can validate
that the world has not changed — model swap, policy edit, tool schema change,
endpoint move — and turn into a miss instead of a stale hit.

Redis keys are tenant-namespaced: `response:tenant:<id>:exact:<hash>`. A tenant
flush deletes exactly that namespace.

## When the cache is skipped

Safety first. Any of these bypasses reuse:

| Bypass reason | Meaning |
| --- | --- |
| `bypass_requested` | `X-Synapass-No-Cache: true` |
| `sensitive_request` | Any sensitivity label other than `public`. |
| `policy_disabled` | The routing policy has caching off. |
| `endpoint_cache_disabled` | An endpoint override sets `use_cache: false`. |
| `tool_request` | Tools attached and not verifiably safe. |
| `live_data_request` | The prompt looks like it asks for live facts. |
| `nondeterministic_request` | `temperature > 0` unseeded, without opt-in. |
| `multi_sample_request` | `n > 1`. |
| `multimodal_request` | Image parts present. |
| `response_too_large` | Body over `max_response_bytes`. |

Two of these are tunable, and both default to the safe answer:

- `bypass_tool_requests: true` — but deterministic built-ins (`now`, `echo`, or a
  registry-verified `safety: safe && executable`) are reusable, so a tool-backed
  assistant still gets a cache.
- `allow_nondeterministic: false` — set it true only if your workload prefers
  speed over sampling variance.

Endpoint scopes are namespaced in the key rather than bypassed, so a scoped
request caches safely per scope.

## Streams and the cache

Streams are cached — looked up like any other request, with two
stream-specific rules:

- **A hit replays the stored body as SSE** (`data: {...}` frames, then
  `data: [DONE]`), so a repeated `stream: true` request can return instantly
  with `synapass.cache_hit: true`. That instant second answer is reuse, not
  a bug. Send `X-Synapass-No-Cache: true` when you want to exercise the live
  provider path.
- **Only a cleanly completed stream is stored**, asynchronously after
  `[DONE]`, so storing never delays the last byte. A cancelled, errored or
  truncated stream is never stored; a cancelled stream still records
  estimated usage for the prefix it delivered.

(A `CacheBypassStreaming` constant still exists for historical label
compatibility, but the decision path emits no streaming bypass.)

## Policy resolution

Per-scope `cache_policies` rows override the global config, resolved in this
order:

```
key  >  endpoint  >  tenant+model  >  tenant+provider  >  tenant  >  global
```

A matching *disabled* row bypasses. No match means the global defaults apply.

```bash
curl -s "$GATEWAY/admin/v1/cache/policies?tenant_id=$TENANT" \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq

curl -s -X PUT $GATEWAY/admin/v1/cache/policies \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"support-bot","tenant_id":"'"$TENANT"'","ttl_seconds":900,"enabled":true,"scopes":["model:gpt-4o-mini"]}' | jq
```

## Invalidation

Entries also expire by TTL, but when something they depend on changes, flush the
affected scope:

```bash
curl -s -X POST $GATEWAY/admin/v1/cache/invalidate \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"scope":"model","model":"gpt-4o-mini","reason":"fine-tune deployed"}' | jq
```

| Scope | Effect |
| --- | --- |
| `tenant` | Deletes `response:tenant:<id>:*` plus that tenant's semantic index. |
| `model` / `provider` | Tenant-scoped when a tenant is given, else global. |
| `key` | One exact entry. |
| `all` | Everything known. |

Hashes cannot be mapped back to models without an index, so a model or provider
flush records its blast radius honestly in the reason rather than pretending to be
surgical.

Every flush writes a `cache_invalidations` row (scope, target, reason, actor,
removed count), publishes `ar.cache.invalidated` on NATS, bumps
`synapass_cache_invalidations_total{scope,reason}` and clears the `cache_entries`
metadata for that scope.

**Flush when you change** the provider, a model, a policy, tenant settings or tool
definitions. Every flush is mirrored into the platform cache below, so both
layers converge on the same write.

## Platform cache

Beside the response cache sits a second cache for everything else the hot
paths read: tenant metadata, workspace metadata, route mappings, the
provider/model catalogue, pricing snapshots, feature flags, tenant settings,
usage aggregates, dependency health and host/path → tenant resolution.
It is `internal/platcache`, always on, with no configuration switch: without
Redis it runs L1 plus database fallback only, and every hook is nil-safe.

### Layers

| Layer | What | Lifetime |
| --- | --- | --- |
| **L1** | In-process memory | Ultra-short: one sixth of the family soft TTL, clamped to 2–10s, so a bad write converges almost immediately |
| **L2** | Redis, shared across replicas | The family hard TTL below |
| **L3** | opt-in HTTP response cache | 30s hard / 15s soft, safe `GET` endpoints only |

Reads are single-flighted (one loader run per key no matter how many
goroutines ask) and serve stale-while-revalidating past the soft TTL: a
slow database never blocks the request, it just serves the last good value
and refreshes in the background.

### Families and keys

Every key is namespaced, versioned (bumping the version retires a whole
generation without a flush), and tenant-aware where it must be — a
tenant-scoped write without the tenant id in the key is refused.

| Family | Key shape | Hard / soft TTL |
| --- | --- | --- |
| Tenant metadata, routes, graph | `tenant:{id}:meta:v1`, `tenant:{id}:routes:v1`, `tenant:{id}:graph:v1` | 120s / 60s |
| Tenant settings | `tenant:{id}:settings:v1` | 180s / 90s |
| Workspace metadata, slug | `workspace:{id}:meta:v1`, `workspace:slug:{slug}:tenant:v1` | 120s / 60s |
| Gateway/host resolution | `gateway:resolve:{host}:{path}:v1` | 60s / 30s |
| Provider catalogue | `modelcatalog:providers:v3`, `modelcatalog:provider:{id}:caps:v3`, `modelcatalog:models:v3` | 15m / 5m |
| Pricing snapshot | `modelcatalog:pricing:v3` | 30m / 10m |
| Feature flags | `featureflags:global:v2`, `featureflags:tenant:{id}:v2` | 120s / 30s |
| Usage aggregates | `usage:{tenant}:{window}:v1` | 30s / 10s |
| Dependency health | `health:{dependency}:v1` | 15s / 5s |

### Safety rules

Enforced in code, not advisory: secrets (API keys, tokens, passwords,
session and payment material, raw bodies, prompts and webhook payloads) are
refused by key and payload-family screening; a Redis outage degrades reads
to the database loader instead of failing the request; a serialization
failure bypasses the cache for that request.

### Retiring entries

Explicit, on the write path, best-effort — it never fails the write that
triggered it. L1 clears synchronously, L2 best-effort:

| What changed | Platform effect |
| --- | --- |
| Response-cache flush, `tenant` scope | That tenant's meta, routes, settings, graph, flags, usage |
| Response-cache flush, `provider` scope | Provider caps plus the shared catalogue |
| Response-cache flush, `model` scope | Model list plus pricing snapshots |
| Response-cache flush, `all` | Catalogue plus flags |
| Response-cache flush, `key` scope | Response cache only — key material is never platform-cached |
| Tenant created | Stale entries retired, then the new tenant's meta/settings/graph prewarmed from the just-committed rows |
| Tenant updated or deleted | That tenant's entries retired |

### Visibility

`GET /admin/v1/cache/stats` carries a `platform` block beside the response
stats: `l1_entries`, `hit_rate`, `hits`, `misses`, `stale_hits`,
`refreshes`, `db_fallbacks`, `coalesced`, `invalidations`, plus L2
reachability (`l2_ok`, `l2_latency_ms`, or `absent (db-fallback)` mode).
Platform activity folds into the same `synapass_cache_*` metrics with
`platform:`-prefixed kind/scope labels, so one dashboard covers both.

## Visibility

The dashboard's `/cache` page shows hit rate, the exact/prefix/semantic split,
bypass reasons, reuse count, latency saved, average lookup time, top prompts, TTL
and tier switches, per-scope policies, recent entries (metadata only) and the
invalidation trail — plus scoped flush controls.

```bash
curl -s "$GATEWAY/admin/v1/cache/stats?tenant_id=$TENANT" -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
curl -s "$GATEWAY/admin/v1/cache/inspect?tenant_id=$TENANT&limit=20" -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
curl -s "$GATEWAY/admin/v1/cache/invalidations?limit=10" -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
```

`inspect` returns entry metadata, never bodies — bodies stay in Redis.

Metrics:

| Metric | Meaning |
| --- | --- |
| `synapass_cache_hits_total{tenant,kind}` | Hits by tier |
| `synapass_cache_misses_total{tenant}` | Misses |
| `synapass_cache_bypass_total{tenant,reason}` | Skips and why |
| `synapass_cache_lookup_duration_seconds{tenant,outcome}` | Lookup latency |
| `synapass_cache_invalidations_total{scope,reason}` | Flushes |
| `synapass_cache_latency_saved_seconds{tenant,kind}` | Value delivered |
| `synapass_cache_semantic_similarity{tenant}` | Score distribution |

## Configuration

```yaml
cache:
  response_cache: false        # master switch
  exact_enabled: true          # exact reuse needs both switches
  response_ttl: 5m
  max_response_bytes: 262144
  semantic_enabled: true
  semantic_threshold: 0.92
  prefix_enabled: true
  prefix_length: 256
  bypass_tool_requests: true
  allow_nondeterministic: false
  bypass_live_data: true
  max_semantic_entries: 2000
```

Environment overrides: `SYNAPASS_CACHE_RESPONSE_ENABLED`, `SYNAPASS_CACHE_RESPONSE_TTL`,
`SYNAPASS_CACHE_EXACT_ENABLED`, `SYNAPASS_CACHE_SEMANTIC_ENABLED`,
`SYNAPASS_CACHE_SEMANTIC_THRESHOLD`, `SYNAPASS_CACHE_PREFIX_ENABLED`,
`SYNAPASS_CACHE_PREFIX_LENGTH`, `SYNAPASS_CACHE_BYPASS_TOOLS`,
`SYNAPASS_CACHE_ALLOW_NONDETERMINISTIC`, `SYNAPASS_CACHE_BYPASS_LIVE_DATA`,
`SYNAPASS_CACHE_MAX_SEMANTIC_ENTRIES`.

## Turning it on safely

A sensible first deployment:

```yaml
cache:
  response_cache: true
  exact_enabled: true
  response_ttl: 5m
  semantic_enabled: false        # start with exact only
  prefix_enabled: false          # add prefix once exact is understood
  bypass_tool_requests: true
  allow_nondeterministic: false
```

Exact-only reuse is the tier whose correctness is easiest to argue. Enable prefix
and semantic separately, watch `synapass_cache_hits_total{kind}`, and raise
`semantic_threshold` before lowering it.

## Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| Hit rate is zero | `response_cache` is off, or `exact_enabled` is | Both switches must be on |
| Everything bypasses with `tool_request` | Tools attached | Verify the tools are deterministic built-ins, or accept the bypass |
| Everything bypasses with `nondeterministic_request` | Unseeded temperature > 0 | Seed it, or set `allow_nondeterministic: true` |
| Semantic hits look wrong | Threshold too low | Raise `semantic_threshold` toward `0.95`+ |
| A stale answer after a model change | Entries not flushed | Flush the model or provider scope |
| A repeated stream returns instantly | Cache replay, not a stuck provider | Check `synapass.cache_hit`; bypass with `X-Synapass-No-Cache: true` to test live |
| Stale catalogue/tenant reads after a write | Platform entries not yet retired | They retire on the write path; flush the tenant/provider/model scope to force it, and check the `platform` block in cache stats |
| `platform` shows L2 absent | No Redis | Expected: L1 plus database fallback; inference is unaffected |
| Hits across tenants | Not possible by construction | Check `tenant_id` is set on the key; entries are namespaced |

---

Related: [Routing](routing.md) · [Tools](tools.md) · [Database](database.md) · [Dashboard](dashboard.md) · [Back to README](../README.md)