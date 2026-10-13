# Routing

Routing is the decision of which provider serves a request, computed from the
request, the matched policy and observable state. This page covers strategies,
candidates, policies, fallback chains, budgets and how to inspect a decision.

## The shape of a decision

```
policy.Targets
   │
   ▼  resolve against the catalogue
candidates (provider + model + pricing + capabilities + health)
   │
   ▼  filter
   · model registered and usable
   · provider enabled, with a working adapter
   · capabilities intersect what the request needs
   · prompt + output fit the context window
   · health state eligible
   · projected cost fits the ceiling
   · rate limits and budgets not exhausted
   │
   ▼  order by strategy
priority · weighted · lowest_cost · lowest_latency · highest_quality
   │
   ▼
RouteDecision { chosen, candidates, chain, retry, timeout, fallback, reason }
```

The engine is a pure function of the candidate set plus observable signals. No
network calls happen inside it, which is what makes routing unit-testable and
reproducible when replaying a historical request.

## Strategies

| Strategy | Picks | Use when |
| --- | --- | --- |
| `priority` | The lowest `priority` number on the target | You have decided the order and want it followed literally. |
| `weighted` | Deterministically by weight, seeded from the request id | You want load spread across peers you trust equally. |
| `lowest_cost` | The cheapest projected cost per request | Cost is the constraint. |
| `lowest_latency` | The lowest observed or prior latency | Interactive latency is what matters. |
| `highest_quality` | The highest operator-assigned quality tier | Answer quality dominates. |

`weighted` is deterministic **for a given request id**. A retry inside one
request makes the same choice as the original attempt rather than re-rolling, and
a replayed trace reproduces the decision exactly.

## Policies

A routing policy is the rule set a request matches. Policies match on model
(exact or glob), request type, tenant, API key, estimated prompt size, required
capability, region, data sensitivity, endpoint scope and streaming. All populated
fields must match; empty fields are wildcards.

```json
{
  "name": "interactive-chat",
  "enabled": true,
  "strategy": "lowest_latency",
  "match": {
    "models": ["gpt-4o*", "claude-*"],
    "request_types": ["chat_completion"],
    "tenant_id": "…"
  },
  "limits": {
    "max_output_tokens": 32768,
    "max_cost_per_request_usd": 0.25,
    "latency_target_ms": 4000,
    "requests_per_minute": 600
  },
  "timeout": { "total": "10m", "per_attempt": "5m", "first_token": "60s" },
  "retry": { "max_attempts": 2, "initial_backoff": "150ms", "multiplier": 2.0 },
  "fallback": { "enabled": true, "max_attempts": 2, "on_error_codes": ["rate_limited", "timeout"] },
  "targets": [
    { "provider_name": "openai-prod", "model": "gpt-4o-mini", "priority": 1 },
    { "provider_name": "anthropic-prod", "model": "claude-sonnet-5", "priority": 2 }
  ]
}
```

Write one:

```bash
curl -s -X PUT $GATEWAY/admin/v1/policies \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d @policy.json | jq
```

`PUT` upserts by name and refreshes the resolver, so a change applies immediately
with no restart. `POST /policies` is the guarded create: a name clash in the same
tenant is `409`, so a retry cannot silently overwrite.

### Resolution order

Matching policies are ordered by **specificity first, then priority**.
Specificity is a coarse weighted sum: API key `1000` > tenant `500` > model `200`
> capability `100` > streaming `50` > type `25` > size `10`. The weights only need
to produce a total order between obviously-different rules, not a meaningful
cardinal scale.

Ordering by specificity before priority is what makes a rule set safe to grow. A
specific rule added later still beats a catch-all added earlier, regardless of
insertion order. Sort by priority alone and a new broad rule silently captures the
traffic the specific rules were written to protect.

After resolution, the fine-grained engine (`internal/policy/engine.go`) evaluates
rules in a fixed order — scope → batch/interactive → size → model/provider
allow/deny → region → sensitivity → cost/latency — so a denial reason is
deterministic. The first failure denies with a visible `deny_reason`; warnings
such as "near budget" do not block.

## Fallback chains

The chain is the ordered list of targets the executor may try.

| Setting | Meaning |
| --- | --- |
| `enabled` | Whether failover is permitted at all. |
| `max_attempts` | How many providers may be tried. |
| `on_error_codes` | **Exhaustive** list when present. |
| `budget_aware` | Skip a fallback whose projected cost would exceed the ceiling. |

`on_error_codes` is exhaustive, not additive. An operator who enumerates the codes
that justify failover is narrowing behaviour deliberately, so a code outside the
list must not fail over even if the error's own flag would allow it.

The worst case is `max_attempts` providers × `retry.max_attempts` calls each,
bounded in practice by the total timeout.

### A streaming attempt is committed

Once a provider has begun sending a stream, the executor **will not fail over**.
Retrying would append a second attempt's tokens to the first, producing an answer
that is duplicated and incoherent. The executor instead reports a stream-started
error, and the client sees an `event: error` frame. This is deliberate: a
correctly-terminated partial answer plus an error beats a plausible-looking
corrupted one.

## Timeouts

Three independent budgets, because they fail differently and diagnosing a single
collapsed knob is impossible.

| Budget | Scope | Meaning when exceeded |
| --- | --- | --- |
| `retry.max_attempts` | one provider | Retry the same target with backoff, jittered and seeded from the request id. |
| `fallback.max_attempts` | the chain | Move to a different provider. |
| `timeout.per_attempt` | one provider call | Stop. |
| `timeout.total` | whole request, including retries and fallbacks | Stop. |
| `timeout.connect` | TCP + TLS | Stop before sending. |
| `timeout.first_token` | time to first byte | Stop. |
| `timeout.stream_idle` | gap between stream chunks | Stop. |

Defaults: total `10m`, per-attempt `5m`, connect `10s`, first token `60s`, stream
idle `90s`. They are generous on purpose — a per-attempt deadline shorter than a
long generation truncates the answer mid-sentence, and that is indistinguishable
from a bug.

### Latency targets are not deadlines

`limits.latency_target_ms` is a **ranking signal** for choosing between healthy
candidates. It is recorded on the decision and used for ordering. It is never
used as a hard deadline.

> **This was a real bug.** The default 10s latency target was being reused as the
> per-attempt deadline, which cancelled every generation longer than ten seconds.
> Generation budgets now come only from the timeout policy. If you have set a
> short `per_attempt` yourself, that is honoured — a truncation you configured is
> your decision, and the response reports it as one.

### Output token ceilings

`limits.max_output_tokens` is a **ceiling**, not a target:

- It bounds what a client may request and what the gateway budgets for.
- It is **not** sent upstream when the client asked for no limit, so a silent
  client still gets the provider's own default.
- A request above the ceiling is clamped to it.
- Whenever a ceiling does bound generation, the response says so in
  `synapass.completion`.

Anthropic is the exception: `max_tokens` is mandatory in its API, so the adapter
supplies a generous default when nothing was requested. Setting
`max_output_tokens` still overrides it.

## Cost control

Three mechanisms, checked before any spend:

| Mechanism | Setting | Behaviour |
| --- | --- | --- |
| Per-request ceiling | `limits.max_cost_per_request_usd` | Applied against the *projected* cost of a candidate, so an expensive candidate is skipped rather than overspending after the fact. |
| Rate limits | `limits.requests_per_minute`, `tokens_per_minute` | Enforced in Redis, with an in-process fallback when Redis is down. |
| Budgets | Dashboard **Budgets**, or `PUT /admin/v1/budgets` | Daily and monthly spend per tenant and per policy. `hard_cap_usd` flows into guardrails. |

Budget counters are keyed by a rendered period label (`d:2026-03-15`,
`w:2026-…-W11`, `m:2026-03`, `total`) and Redis sets the TTL from that label.
Writing a counter under one key and reading it under another would silently
produce a counter that is incremented faithfully and never consulted, so
`policy.PeriodKey` is the single function that renders it.

Cost is an estimate from the registry's price table, not a billing statement. It
is deliberately the same number budgets enforce against, so the console and the
limiter never disagree.

## Request-level controls

Callers can guide a single request without an operator editing a policy.

| Header | Effect |
| --- | --- |
| `X-Synapass-Policy` | Pin a policy by id or name. |
| `X-Synapass-Endpoint` | Apply a named endpoint scope: forced model, preferred lists, strategy, caps, fallback block. Unknown slug `404`, disabled `403`. |
| `X-Synapass-Max-Cost-USD` | Refuse candidates above this projected cost. |
| `X-Synapass-Latency-Target-Ms` | Prefer lower-latency targets. |
| `X-Synapass-No-Fallback` | Exactly one attempt. A primary failure is a `502`, not a slow success elsewhere. |
| `X-Synapass-Region` | Pin provider geography; rejected when policy forbids it. |
| `X-Synapass-Sensitivity` | Comma-separated labels (`pii,phi,public`). Sensitive payloads bypass cache. |
| `X-Synapass-Batch` | Mark batch/offline traffic for cost-aware routing. |
| `X-Synapass-No-Cache` | Force a cache miss. |
| `X-Synapass-Debug` | Restore full routing internals in the `synapass` block. |

## Inspecting a decision

```bash
curl -s "$GATEWAY/admin/v1/requests/$REQUEST_ID/explain" \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
```

That returns the task classification, the policy verdict, the prompt-shaping plan,
the cache decision, the candidate scores and the reason each rejected target was
rejected. The same payload is on the dashboard's **Requests** page when you expand
a row.

## Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| Every request goes to one provider | Targets all share the same priority, or the strategy is `priority` | Set distinct priorities, or change strategy |
| `no route` despite configured providers | All candidates filtered | Read the `explain` payload for per-candidate rejections |
| Traffic avoids a healthy provider | Circuit breaker still open | Probe it: `POST /admin/v1/providers/{id}/probe` |
| A failover did not happen | The code was not in `on_error_codes` | Add it, or clear the list to allow the defaults |
| Answers stop mid-sentence | A configured `per_attempt` or `max_output_tokens` | Raise the budget; the response reports it in `synapass.completion` |
| `403` with a deny reason | Fine-grained policy denied | The message names the rule that fired |

---

Related: [Providers](providers.md) · [Caching](caching.md) · [API reference](api.md) · [Dashboard](dashboard.md) · [Back to README](../README.md)