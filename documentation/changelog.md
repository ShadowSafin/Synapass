# Changelog

What each delivery phase added, and why. For current behaviour, follow the
[documentation index](README.md) instead — this page is history.

The project's delivery is organised in phases. Each one is independently
deployable and additive; nothing in a later phase is required by an earlier one.

## Phase 1 — the gateway

The synchronous inference path.

| Area | Delivered |
| --- | --- |
| Gateway | OpenAI-compatible `/v1/chat/completions` (streaming and buffered), `/v1/models` |
| Adapters | OpenAI, Anthropic Messages, Ollama, vLLM, any OpenAI-compatible server |
| Routing | Five strategies, retry, per-attempt and total timeouts, fallback chains |
| Health | Active probes plus passive observation, circuit breaker per provider |
| Policy | Match by model, request type, tenant, key, prompt size, capability |
| Cost | Per-request ceilings, request and token rate limits, daily and monthly budgets |
| Auth | SHA-256-hashed API keys with scopes, revocation, caching |
| Observability | Prometheus, OTLP traces, JSON logs, ClickHouse analytics, Grafana, alert rules |
| Console | Next.js dashboard across usage, providers, policies, budgets, tenants and keys |
| Deployment | Docker Compose and native systemd |

## Phase 2 — the policy-driven control plane

Turns the router into something that explains itself. Every request now flows
through ten visible stages: authenticate → policy → classify → shape → cache →
health and scores → route → execute → fall back → persist.

| Area | Delivered |
| --- | --- |
| Policy engine | Tenant/key/endpoint rules, model and provider allow/deny, cost and latency ceilings, region and sensitivity constraints, batch vs interactive |
| Classification | Rules-first, deterministic task inference across eleven task types, mirrored in Python |
| Intelligent routing | Task-, cost-, latency-, capability- and score-aware ordering with derived timeout, retry and shaping strategies |
| Prompt shaping | Normalization, compression, context trimming, history summarization, guardrail injection — all visible in traces |
| Caching | Exact, prefix and semantic tiers with sensitive-request bypass, stats and invalidation |
| Provider scoring | Explainable success/latency/cost/feedback blends with per-task breakdowns |
| Eval and replay | Async replay jobs over NATS, offline comparisons, golden cases, regression flags |
| Guardrails | Kill switches, tenant overrides, hard caps, fallback blocks, forced circuits, visible deny reasons |
| Dashboard | Scores, cache analytics, replay, evaluations, endpoints, audit, kill/revive, explain view |

## Phase 3 — management and provisioning

Turns a view-only gateway into a manageable control plane. Providers, models,
tenants, keys, policies, endpoints and overrides are created, edited, disabled
and deleted through the API and the dashboard: no config-file edits, no restarts.

| Area | Delivered |
| --- | --- |
| CRUD | Full management for providers, models, tenants, keys and policies |
| Credentials | AES-256-GCM sealed provider secrets, metadata-only reads, audit on create and rotate |
| Connectivity tests | Three checks — connectivity, model listing, sample completion — persisted per run |
| Model discovery | Populate the registry from a provider's remote catalogue, preserving local edits |
| Ownership | `managed_by` so a dashboard edit survives a deploy |
| Key rotation | Swap a secret in place; the old one stops working immediately |
| Dashboard | Provider/model/tenant/key/policy/endpoint/override management |

## Phase 4 — tool calling and agent runs

Turns the gateway from a request router into an agent runtime. Models can call
tools, answers can be contract-checked against a JSON Schema, and every
gateway-side run leaves a durable step trace.

| Area | Delivered |
| --- | --- |
| Registry | Tool registration with an allow-listed JSON Schema subset |
| Execution modes | Client-executed (default) and bounded gateway-side execution |
| Safety | Executability derived from `kind`, never granted; `now` and `echo` are the only built-ins |
| Bounds | Steps, calls, wall clock, allowed/denied globs — operator-owned, client-narrowable |
| Structured output | `json_object` and `json_schema` with conformance reporting and unambiguous repair |
| Durability | `tool_invocations` → `tool_executions` → `agent_runs` → `agent_steps` |
| Dashboard | Tools, tool policies, agent runs with step traces |

## Phase 5 — the local response cache

Makes Synapass faster and cheaper by reusing prior responses wherever it is
safe, with safety as the first consideration rather than an afterthought.

| Area | Delivered |
| --- | --- |
| Tiers | Exact, prefix (short prompts only) and semantic over Redis |
| Keying | Tenant, key, model, tools, generation settings, response contract, policy and endpoint — everything that can change an answer |
| Policy | Eleven named bypass reasons; per-scope rules resolved key → endpoint → tenant → global |
| Invalidation | Scoped, audited, published on NATS, with honest blast-radius recording |
| Observability | Hit/miss/bypass metrics, `CacheTrace`, and a full `/cache` dashboard page |

## Public landing page

A browser reaching the gateway root now gets a small HTML page naming the real
endpoints, and JSON for everything else. A gateway reached from a tunnel URL or
typed into a browser should not answer with a `404` that reads as a broken
deployment.

## Cloudflare tunnels

Temporary public exposure of one local service through a supervised `cloudflared`
child process. Opt-in twice — feature flag and explicit create — and loopback
targets only, so a tunnel can never become a proxy for someone else's
infrastructure. See [installation/cloudflare-tunnel.md](installation/cloudflare-tunnel.md).

## Truncation fix

Long answers were stopping mid-sentence for three independent reasons, all fixed:

1. **The soft latency target was reused as a hard per-attempt deadline.** The
   default 10s target cancelled every generation longer than ten seconds. It is
   now a ranking signal only, and generation budgets come from the timeout
   policy. An explicitly configured budget is never raised behind the operator's
   back.
2. **The shared transport set `ResponseHeaderTimeout` to the first-token budget.**
   For a buffered request a provider only sends headers once the whole answer
   exists, so long completions were aborted before a byte of body arrived.
   Removed; per-attempt deadlines come from the request context.
3. **The policy output ceiling was sent as the client's `max_tokens`.** A client
   that asked for no limit was given one and nothing reported the cut. A ceiling
   is now only sent when the client requested one — and Anthropic's mandatory
   field gets a generous default rather than a small constant.

Truncation is now always reported in `synapass.completion` (`truncated`,
`reason`, `requested_tokens`, `applied_tokens`, `budget_ms`), counted in
`synapass_provider_truncations_total`, and never conflated with a completed
answer. A stream that has begun is committed: the executor reports a
stream-started error rather than appending a second attempt's tokens to the first.

## Cost intelligence, compare mode, and offline replay

| Area | Delivered |
| --- | --- |
| Cost intelligence | Exact per-request costing (micro-USD rounding, no FX), versioned pricing with tenant → model → provider → global precedence, budgets, anomaly detection, savings plans — see [cost-analysis.md](cost-analysis.md) |
| Playground compare | One prompt against two models concurrently; lane B inherits lane A's setup and overrides only model/target/policy, answers stay side by side |
| Offline replay | Request payloads (messages, max tokens) are captured at serve time, so any pasted request ID replays across providers × models with versioned-sheet pricing — no manual prompt reconstruction |
| LAN address | The Endpoints page derives the gateway's LAN URL from the browser's own address, and `scripts/up.ps1` / `scripts/deploy.ps1` detect the current LAN IPv4 at launch — no hardcoded IP survives a network change |

## Platform cache

A second cache beside the response cache, for everything else the hot paths
read — tenant and workspace metadata, route mappings, the provider/model
catalogue, pricing snapshots, feature flags, tenant settings, usage
aggregates, dependency health and host/path resolution. Three layers
(in-process L1, shared Redis L2, opt-in HTTP L3), single-flighted reads with
stale-while-revalidate, versioned tenant-aware keys, secrets refused by
construction, and graceful degradation to database fallback when Redis is
down. Tenant writes retire and prewarm their own entries; response-cache
flushes mirror into the matching platform scope; health and counters arrive
in the `platform` block of cache stats. See [Caching](caching.md).

## Streaming overhaul

Low-latency, observable, cancellable streams on every provider:

- Liveness on all three streaming adapters: `first_token` and `stream_idle`
  budgets enforced per attempt, with a watchdog for zero-byte stalls.
- Gateway SSE hardening: `: ping` keepalives, slow-write accounting,
  async post-`[DONE]` cache store, estimated usage for cancelled prefixes,
  cancel-aware outcomes.
- Streams join the response cache: a hit replays as SSE, a clean completion
  is stored — never a cancelled or errored one.
- Seven `synapass_stream_*` metrics (TTFT, first text, duration by outcome,
  active gauge, errors by stage, cancels, backpressure) plus alerts.
- Playground keeps partial answers on cancel, retries failures in place,
  stops lanes individually, and shows live first-token timing.

## Documentation

This documentation set. The previous phase-by-phase notes were folded into
topic-based pages so each fact is documented once and cross-linked, rather than
restated per phase.

---

Related: [Documentation index](README.md) · [Architecture](architecture.md) · [Back to README](../README.md)