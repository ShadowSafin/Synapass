# Observability

Synapass emits Prometheus metrics, OpenTelemetry traces and structured JSON
logs, and ships Grafana dashboards and alert rules. This page covers what each
signal tells you and how to correlate them when something is wrong.

> **Cardinality is bounded by construction.** Tenants, providers and models are
> labels. Request ids, user ids and prompts never are. Nothing per-request is a
> label — that is the single most common way an observability stack is destroyed.

## Where the signals come from

| Signal | Endpoint / sink | Lossy? |
| --- | --- | --- |
| Metrics | `GET /metrics` on the gateway and worker listeners | Never — counters are in-process |
| Traces | OTLP to the collector in `deploy/otel/collector-config.yaml` | Best effort |
| Logs | JSON to stdout; Promtail ships them to Loki | Never — stdout is the source of truth |
| Usage and traces | ClickHouse, written asynchronously | **Yes, by design** |
| Events | NATS JetStream | Yes, if the bus is down |

The record pipeline batches and counts what it drops rather than blocking a
response. `synapass_async_queue_depth` and `synapass_async_dropped_total` make
that trade-off visible instead of invisible. What is synchronous —
authentication, policy, budgets — is never lossy.

## Metrics

All metrics are prefixed `synapass_`. The workers use `synapass_worker_*`, so
one scrape config and one dashboard cover both.

### Gateway

| Metric | Labels | Use it to |
| --- | --- | --- |
| `synapass_gateway_requests_total` | tenant, provider, model, type, outcome, status | See traffic and success rate |
| `synapass_gateway_request_duration_seconds` | tenant, provider, type, outcome | See latency distribution |
| `synapass_gateway_requests_in_flight` | — | See saturation |
| `synapass_gateway_request_size_bytes` / `response_size_bytes` | tenant, type | See payload growth |
| `synapass_build_info` | version, commit, go_version | Answer "which build is running?" |

### Provider

| Metric | Labels | Use it to |
| --- | --- | --- |
| `synapass_provider_attempts_total` | tenant, provider, model, outcome, attempt | See attempts per provider |
| `synapass_provider_duration_seconds` | tenant, provider, model, outcome | See provider latency |
| `synapass_provider_time_to_first_token_seconds` | tenant, provider, model | See streaming startup |
| `synapass_provider_health` | provider | See breaker state (1 healthy, 0.5 degraded, 0 unhealthy) |
| `synapass_provider_truncations_total` | tenant, provider, reason | **See answers cut short** by `max_tokens` or `timeout` |
| `synapass_provider_completion_tokens_ratio` | tenant, provider | See headroom; a pile-up at `1.0` means the ceiling is too tight |

### Streaming

| Metric | Labels | Use it to |
| --- | --- | --- |
| `synapass_stream_ttft_seconds` | provider | Time from accept to first downstream byte (TTFT percentiles) |
| `synapass_stream_first_text_seconds` | provider | Time from accept to first visible text (excludes meta-only frames) |
| `synapass_stream_duration_seconds` | provider, outcome | Stream lifetime by `completed`, `error`, `cancelled`, `truncated` |
| `synapass_stream_active` | - | Currently open streams (saturation, leak detection) |
| `synapass_stream_errors_total` | provider, stage | Failures by `upstream`, `downstream`, `timeout` stage |
| `synapass_stream_cancels_total` | provider | Client-cancelled streams (not provider faults) |
| `synapass_stream_backpressure_total` | provider | Downstream writes slower than 500ms (slow readers, buffering proxies) |

### Routing, policy and usage

| Metric | Use it to |
| --- | --- |
| `synapass_routing_decisions_total` | Count routing decisions by strategy and outcome |
| `synapass_routing_candidates` | See how many candidates survived filtering |
| `synapass_routing_fallbacks_total` | See failover rate and which codes triggered it |
| `synapass_policy_rate_limited_total` | See rate-limit rejections |
| `synapass_policy_budget_blocked_total` | See budget denials |
| `synapass_usage_tokens_total` | See prompt and completion token throughput |
| `synapass_usage_cost_usd_total` | See estimated spend |

### Cache

`synapass_cache_hits_total{kind}`, `misses_total`, `bypass_total{reason}`,
`lookup_duration_seconds`, `invalidations_total{scope,reason}`,
`latency_saved_seconds{tenant,kind}`, `semantic_similarity`. See
[Caching](caching.md).

The platform cache (tenant/catalog/route/flags) reports through the same
metrics with `platform:`-prefixed kind and scope labels, and its health
(L1 size, hit rate, stale hits, background refreshes, DB fallbacks, L2
reachability) arrives in the `platform` block of `GET /admin/v1/cache/stats`.

### Intelligence

| Metric | Use it to |
| --- | --- |
| `synapass_classifier_requests_total{task}` | See the task mix |
| `synapass_shaping_requests_total{step}` | See which shaping steps fire |
| `synapass_guardrail_blocks_total{kind}` | See kill switches and caps firing |
| `synapass_scoring_provider_score` | See quality scores |
| `synapass_eval_jobs_total`, `synapass_replay_jobs_total` | See offline work |
| `synapass_feedback_events_total` | See user feedback volume |

### Tools and tunnels

`synapass_tools_runs_total{tenant,mode,status}`,
`run_duration_seconds{mode}`, `invocations_total{tenant,tool,status}`,
`persist_errors_total{tenant}`. `synapass_tunnel_up{target}`,
`tunnel_sessions_total{target,outcome}`, `tunnel_restarts_total{target}`.

### Async pipeline

`synapass_async_queue_depth`, `dropped_total`, `flushed_total`. A rising queue
depth with drops means a dependency is slow or down.

## Traces

Spans cover `classifier`, `shaping`, `cache`, `routing`, `guardrail`, `provider`,
`tools` and `eval`. Each carries the request id and trace id.

`deploy/otel/collector-config.yaml` fans out to a file sink by default. Point it at
Tempo, Jaeger or another collector by uncommenting one exporter — the collector
is the seam, so swapping backends is never a Go change.

With tracing enabled and no OTLP endpoint configured, **the gateway refuses to
start**. Losing traces silently is worse than not starting.

## Logs

JSON to stdout. In Compose, `docker compose logs -f gateway`; under systemd,
`journalctl -u synapass -f`.

| Field | Why it is there |
| --- | --- |
| `request_id`, `trace_id` | Correlate a log line with a trace and a request record |
| `tenant`, `provider`, `model` | Filter without reading the message |
| `duration_ms` | Spot slow paths |
| `error` | The normalized message |

Request bodies are not logged by default. A configurable header deny-list plus
value redaction keep credentials out; the workers additionally redact values,
because captured prompts would otherwise be the most likely path for personal data
to escape.

## Grafana

Compose brings up Grafana on `:3001` with `deploy/grafana/dashboards/synapass-overview.json`
preloaded, and Prometheus scraping both the gateway and the workers.

The overview dashboard covers throughput, latency percentiles, success rate, token
throughput, cost, provider health, fallback rate and cache hit rate.

## Alert rules

`deploy/prometheus/rules/synapass.yml` ships with alerts for the failures that
matter:

| Alert | Fires when |
| --- | --- |
| Gateway down | `/ready` fails |
| High error rate | 5xx ratio above threshold |
| Provider unhealthy | Breaker state stays unhealthy |
| Latency regression | p95 above threshold |
| Spend anomaly | Cost rate far above baseline |
| Telemetry loss | Async drops climbing |
| Truncation spike | `truncations_total` climbing |
| Stream stall spike | `stream_errors_total{stage="timeout"}` climbing — providers stalling past `first_token`/`stream_idle` |
| Slow downstream | `stream_backpressure_total` climbing — a buffering proxy or slow reader, not the provider |

Load them into your own Prometheus if you are not using the Compose one.

## Debugging by symptom

**Answers are stopping mid-sentence.** This is the one symptom that used to be
silent. Check, in order:

1. `synapass_provider_truncations_total{reason}` — is it `max_tokens` or
   `timeout`?
2. The response's `synapass.completion` block — `requested_tokens`,
   `applied_tokens`, `budget_ms`, `finish_reason`.
3. The routing decision's timeout policy — a `per_attempt` shorter than a long
   generation truncates by definition.

**A provider is getting no traffic.** Check `synapass_provider_health`, then
whether the policy's target list actually names it, then whether the capability
filter excludes it for these requests.

**Failover is not happening.** Compare the error code against the policy's
`on_error_codes`. That list is exhaustive when present.

**Latency is worse than expected.** Read `synapass_routing_candidates` — a
single candidate means no choice was available, so latency is that provider's.

**Telemetry seems to be missing rows.** Check `synapass_async_dropped_total`
and `queue_depth` before suspecting the queries.

## Scripts

```bash
# Health, build identity, and where the process thinks it is
curl -s $GATEWAY/health | jq
curl -s $GATEWAY/version | jq

# Traffic and errors
curl -s $GATEWAY/metrics | grep synapass_gateway_requests_total
curl -s $GATEWAY/metrics | grep synapass_provider_health
curl -s $GATEWAY/metrics | grep synapass_provider_truncations_total

# One request, end to end
curl -s "$GATEWAY/admin/v1/requests/$REQUEST_ID" -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
curl -s "$GATEWAY/admin/v1/requests/$REQUEST_ID/explain" -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq
```

---

Related: [Architecture](architecture.md) · [Database](database.md) · [Troubleshooting](troubleshooting.md) · [Back to README](../README.md)