<div align="center">

<img src="desktop/assets/icon.png" alt="Synapass logo" width="96" />

# Synapass

**An OpenAI-compatible gateway that decides, per request, which AI provider serves it — and proves it.**

Routing · failover · caching · policy · budgets · full request lineage

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![CI](https://github.com/ShadowSafin/Synapass/actions/workflows/ci.yml/badge.svg)](https://github.com/ShadowSafin/Synapass/actions/workflows/ci.yml)
[![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)
[![Docs](https://img.shields.io/badge/docs-documentation%2F-informational.svg)](documentation/README.md)

[Quick start](#quick-start) · [Documentation](documentation/README.md) · [Troubleshooting](documentation/troubleshooting.md)

**Linux**

```bash
curl -fsSL https://raw.githubusercontent.com/shadowsafin/synapass/main/scripts/install.sh | bash
```

**macOS**

```bash
curl -fsSL https://raw.githubusercontent.com/shadowsafin/synapass/main/scripts/install.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/shadowsafin/synapass/main/scripts/install.ps1 | iex
```

One command per OS: clones, configures, and starts the full stack. Details in [Quick start](#quick-start).

</div>

---

## What Synapass does

Synapass is an **inference gateway** and **control plane**. It sits between your
applications and your model providers, presents a single OpenAI-compatible
endpoint, and decides — per request, under policy — which provider serves it,
what happens when that provider fails, and what it cost.

It is also where providers, models, policies, tenants, keys, tools and caches are
managed. Changing which model answers production traffic is a dashboard edit
rather than a deployment.

Any client that speaks OpenAI works by changing its base URL. The extra response
fields are namespaced under `synapass`, so OpenAI SDKs ignore them.

## Why it exists

Inference stacks rarely get chosen. They accrete. By the time a few applications
are live you have:

- **A provider integration per service**, each with its own error handling, so a
  `429` means something slightly different everywhere.
- **No failover.** One upstream hiccup becomes an outage, because nothing knows a
  second provider exists.
- **Routing logic in application code** — a `if model == "gpt-4o"` here, a
  provider SDK there, copied into every service.
- **No cost control.** A runaway loop produces a bill nobody notices until it
  arrives.
- **No central answer** to "what did we spend, and which model produced the bad
  answers?"

Each is small. Together they are an operating burden that competes with the
product. Synapass moves all of it into one layer you can operate.

## Key features

| | |
| --- | --- |
| **OpenAI-compatible API** | `POST /v1/chat/completions`, streaming and buffered, plus `/v1/models`. An SDK swap, not a rewrite. |
| **Multi-provider routing** | Five strategies — `priority`, `weighted`, `lowest_cost`, `lowest_latency`, `highest_quality` — over a registry that knows each model's real context window, capabilities and price. |
| **Fallback chains** | Ordered failover with bounded retry and deterministic jitter. A started stream is never appended to; it is reported instead. |
| **Policy engine** | Match on model, tenant, key, task, size, region, sensitivity and endpoint. Resolved by specificity, so a specific rule always beats a broad one. |
| **Cost control** | Per-request ceilings checked against projected cost *before* the call, token and request rate limits, daily and monthly budgets. |
| **Provider & model management** | Add, test, probe, kill and revive providers. Discover remote models while preserving your local pricing, aliases and disables. |
| **Tool calling** | Client-executed by default. Optional bounded gateway-side execution with operator-owned limits and a durable step trace per run. |
| **Local caching** | Exact, prefix and semantic tiers, tenant-isolated by construction, invalidatable by scope with an audit trail. Off by default. |
| **Observability** | Task classification, policy verdict, cache decision and routing reason on every request. Prometheus, OTLP traces, JSON logs, Grafana dashboards and alert rules. |
| **Dashboard** | A full control plane for providers, models, policies, tenants, keys, tools, cache and tunnels, behind a first-run operator account. |
| **Deployment** | Docker Compose, native systemd, and temporary Cloudflare tunnels. One configuration model across all three. |

## Tech stack

| Layer | Technology |
| --- | --- |
| Gateway | Go 1.27 |
| Dashboard | Next.js, React, TypeScript, Tailwind |
| Intelligence workers | Python 3.11+ |
| System of record | PostgreSQL 16 |
| Limits, budgets, cache | Redis 7 |
| Traces and analytics | ClickHouse 24.8 |
| Event bus | NATS with JetStream |
| Telemetry | OpenTelemetry, Prometheus, Grafana, Loki |

Each store was chosen for the shape of what it holds — see
[documentation/database.md](documentation/database.md).

## Architecture

The request path, end to end:

```
   client
     │  OpenAI SDK · curl · any OpenAI-compatible client
     ▼
┌──────────────────────────────────────────────┐
│  POST /v1/chat/completions                   │
│                                              │
│  1  authenticate      API key → tenant        │
│  2  resolve policy    most specific match     │
│  3  enforce budgets   rate limits, ceilings   │
│  4  check cache       exact → prefix → semantic
│  5  classify task     rules-first             │
│  6  shape prompt      normalize, trim         │
│  7  route             filter → order → pick   │
│  8  execute           retry · fail over      │
│  9  tools loop        bounded, when allowed   │
└──────────────────────────────────────────────┘
     │                          │
     ▼                          ▼
  provider                response + synapass
  adapter call            metadata block
        │
        └──────────────────────────────────────┐
                                               ▼
                              telemetry · asynchronously
                              usage · traces · metrics · logs
```

The deployment topology:

```
   client ──▶ gateway :8080 ──┬──▶ PostgreSQL   system of record
        ▲     │              ├──▶ Redis         limits, cache, credentials
        │     ▼              ├──▶ NATS          usage, eval jobs, audit
   dashboard  └──▶ workers ──┴──▶ ClickHouse ──▶ OTel ──▶ Prometheus · Loki · Grafana
```

Every request records why it was routed the way it was. Read it back with
`GET /admin/v1/requests/{id}/explain`.

## Quick start

### Docker (one command)

Zero to running stack with a single paste — no clone needed:

```bash
curl -fsSL https://raw.githubusercontent.com/shadowsafin/synapass/main/scripts/install.sh | bash
```

```powershell
irm https://raw.githubusercontent.com/shadowsafin/synapass/main/scripts/install.ps1 | iex
```

Top command for Linux / macOS / Windows Git Bash, bottom for native Windows
PowerShell. (In cmd.exe or PowerShell, a bare `bash` is the WSL stub, which is
why the pipe form needs Git Bash or the PowerShell installer instead.) Either
fetches Synapass into `~/synapass` (override with `SYNAPASS_DIR` /
`$env:SYNAPASS_DIR`), then runs the deploy script: prerequisite and port
checks, `.env` bootstrap with a generated admin key, full Compose startup,
readiness waits, and the URL summary. Re-running it updates the checkout and
re-verifies the stack without touching `.env` or data.

Already cloned? The same flow from the repo root:

```bash
./scripts/deploy.sh        # Linux / macOS / Windows Git Bash
# .\scripts\deploy.ps1    # native Windows PowerShell instead
# make deploy             # same script through the Makefile
```

That single command deploys the whole platform: it checks for Docker,
Docker Compose and curl, verifies the host ports are free, creates `.env`
from `.env.example` on first run (with a generated `SYNAPASS_ADMIN_KEY`), starts
every Compose service together (gateway, dashboard, Postgres, Redis,
ClickHouse, NATS, workers, Grafana/Prometheus/Loki), waits until the gateway
reports ready — which only happens after migrations and catalogue seeding
finish — and prints the access URLs. Add a provider key to `.env` afterwards
(`OPENAI_API_KEY` / `ANTHROPIC_API_KEY`); the stack boots without one, with
providers reporting unconfigured until then. The script is idempotent, so
re-running it is a safe restart-and-verify.

| Service | URL |
| --- | --- |
| Dashboard | <http://127.0.0.1:3000> |
| Gateway | <http://127.0.0.1:8080> (`/health` · `/ready` · `/metrics`) |
| Grafana | <http://127.0.0.1:3001> |

Manage the stack with `./scripts/deploy.sh {stop|restart|logs|status|reset}`
(`reset` deletes volumes, so data, after a `--yes` confirmation).

If port 8080 is taken, move the host port rather than the container port — gateway
configuration does not change:

```dotenv
GATEWAY_PORT=18080
NEXT_PUBLIC_SYNAPASS_API_URL=http://localhost:18080
```

<details>
<summary>Manual equivalent (what the script runs for you)</summary>

```bash
cp .env.example .env
openssl rand -hex 24        # paste into SYNAPASS_ADMIN_KEY, then add your provider key
$EDITOR .env

docker compose up -d --build
curl -sf http://127.0.0.1:8080/ready   # ready only after migrations + seeding
curl -sf http://127.0.0.1:3000/        # dashboard
```

</details>

### Native

```bash
make build
synapass native install   # templates, builds, migrations (idempotent)
synapass native up        # foreground supervisor: gateway + dashboard
```

Production Linux hosts use the systemd units instead (`deploy/systemd/`,
including `synapass-dashboard.service`); Windows hosts run `native up` from
Task Scheduler.

### Desktop app (Windows)

```powershell
.\SynapassSetup.exe   # setup only: installs the app, then exits
.\Synapass.exe        # the app: dashboard + backend in its own window
```

Two separate programs. The setup executable carries the app and its runtime
(gateway, portable Node, built dashboard) and installs them; the standalone app
runs them with everything supervised. Full walkthrough:
**[Desktop app](documentation/installation/desktop.md)**

Full walkthroughs: **[Docker](documentation/installation/docker.md)** ·
**[Native](documentation/installation/native.md)** ·
**[Desktop](documentation/installation/desktop.md)** ·
**[Tunnel](documentation/installation/cloudflare-tunnel.md)** ·
**[Getting started](documentation/getting-started.md)**

On first launch the dashboard asks you to create the console administrator. There
are no default credentials; the password you choose is stored as an Argon2id hash,
and the setup screen closes itself permanently afterwards. See
[Dashboard → Authentication](documentation/dashboard.md#authentication).

Verify a running stack end to end — no provider key needed:

```bash
bash scripts/smoke.sh                                        # Linux / macOS
powershell -ExecutionPolicy Bypass -File scripts/smoke.ps1  # Windows
```

## Example request

Mint a tenant key, then call the public endpoint:

```bash
export GATEWAY=http://127.0.0.1:8080
export SYNAPASS_ADMIN_KEY=<from .env>

TENANT=$(curl -s $GATEWAY/admin/v1/tenants \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" | jq -r '.tenants[0].id')

export SYNAPASS_KEY=$(curl -s $GATEWAY/admin/v1/keys \
  -H "Authorization: Bearer $SYNAPASS_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"tenant_id\":\"$TENANT\",\"name\":\"first-key\",\"scopes\":[\"inference\"]}" \
  | jq -r .plaintext)
```

```bash
curl -s $GATEWAY/v1/chat/completions \
  -H "Authorization: Bearer $SYNAPASS_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "Summarize refunds in one line."}],
    "max_tokens": 256,
    "temperature": 0.3
  }' | jq
```

```json
{
  "choices": [{ "index": 0, "message": { "role": "assistant", "content": "…" }, "finish_reason": "stop" }],
  "usage": { "prompt_tokens": 26, "completion_tokens": 81, "total_tokens": 107 },
  "synapass": {
    "request_id": "5e0804c7-…",
    "provider": "openai-prod",
    "requested_model": "gpt-4o-mini",
    "routed_model": "gpt-4o-mini",
    "fallback_used": false,
    "latency_ms": 2851,
    "estimated_cost_usd": 0.0000026
  }
}
```

`provider` is who **answered**, not who was chosen first. An answer cut short by a
limit says so rather than looking short:

```json
"completion": { "truncated": true, "reason": "max_tokens", "budget_ms": 300000 }
```

Reference: **[documentation/api.md](documentation/api.md)**

## Dashboard

<http://127.0.0.1:3000> — the operator console.

| Page | What it is for |
| --- | --- |
| **Overview** | Volume, success rate, latency, spend, provider health |
| **Requests** | Every request, with a per-request **explain** view |
| **Errors** | Failures with a `by_code` rollup |
| **Providers** | Add, test, sync models, probe, kill or revive |
| **Models** | Context window, pricing, capabilities, priority |
| **Policies** | Targets, limits, timeouts, retry, fallback |
| **Endpoints** | Named routing scopes with copy-paste examples |
| **Tenants** | Create, edit and remove tenants |
| **API keys** | Mint, rotate and revoke |
| **Budgets** | Daily and monthly ceilings, hard per-request caps |
| **Scores** | Provider and model quality with explanations |
| **Cache** | Hit rates, bypass reasons, per-scope rules, scoped flushes |
| **Tools** | Registry, policies, agent runs with step traces |
| **Analytics** | Throughput, latency percentiles, spend over time |
| **Tunnels** | Temporary public access |

The browser never holds the admin key — Next route handlers proxy `/admin/v1/*`
server-side. Details: **[documentation/dashboard.md](documentation/dashboard.md)**

## Documentation

Everything deeper lives in [`documentation/`](documentation/README.md).

| | |
| --- | --- |
| **[Overview](documentation/overview.md)** | What it is for and the problem it solves |
| **[Getting started](documentation/getting-started.md)** | Empty directory to working call in five minutes |
| **[Architecture](documentation/architecture.md)** | How the pieces fit and why |
| **[Installation](documentation/installation/docker.md)** | [Docker](documentation/installation/docker.md) · [Native](documentation/installation/native.md) · [Tunnel](documentation/installation/cloudflare-tunnel.md) |
| **[Providers](documentation/providers.md)** | Kinds, credentials, testing, health |
| **[Routing](documentation/routing.md)** | Strategies, policies, fallback, cost control |
| **[Caching](documentation/caching.md)** | Tiers, policy, invalidation, visibility |
| **[Tools](documentation/tools.md)** | Registry, execution modes, safety, bounds |
| **[API reference](documentation/api.md)** | Every endpoint, header and error code |
| **[Dashboard](documentation/dashboard.md)** | Pages and workflows |
| **[Database](documentation/database.md)** | Tables, migrations, persistence boundaries |
| **[Observability](documentation/observability.md)** | Metrics, traces, logs, alerting |
| **[Troubleshooting](documentation/troubleshooting.md)** | Symptoms, causes, checks |
| **[FAQ](documentation/faq.md)** | Short practical answers |
| **[Glossary](documentation/glossary.md)** | What each term means here |
| **[Changelog](documentation/changelog.md)** | What each delivery phase added |

Stuck? Start at **[documentation/troubleshooting.md](documentation/troubleshooting.md)**.

## Security

- **API key authentication.** Keys are stored as a SHA-256 digest; the plaintext is
  returned exactly once and never again. Scopes narrow further by method and path.
- **Tenant isolation.** It is structural, not conventional — in routing, in the
  cache key and in the Redis namespace. A cache hit never crosses tenants.
- **Policy enforcement.** Denials return `403`/`429` with an explaining message
  and zero provider calls. The rule that fired is named.
- **Secret handling.** Provider credentials are referenced by environment variable
  or sealed with AES-256-GCM. An inline `api_key` on a write is discarded. The
  gateway refuses to start on unsafe production configuration — a CORS wildcard,
  a missing admin key, tracing with no OTLP endpoint.
- **Audit logging.** Every control-plane mutation is recorded with before and
  after values. Inference traffic is not audited; usage records serve that need.

Report vulnerabilities privately — see **[SECURITY.md](SECURITY.md)**.

## Contributing

Pull requests are welcome. Read **[CONTRIBUTING.md](CONTRIBUTING.md)** first: it
covers the commit style, which test suite to expect for which kind of change, and
which document owns which kind of fact.

```bash
make test              # Go + Python
make lint              # go vet, golangci-lint, ruff, mypy
make dashboard-typecheck
```

Start reading at `internal/api/chat.go` for the request path, `internal/routing`
for the decision logic, `internal/domain` for the vocabulary.

## Code of conduct

Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md), which
sets expectations for technical disagreement as well as conduct.

## Support

| | |
| --- | --- |
| Questions | [FAQ](documentation/faq.md), then [GitHub Issues](https://github.com/shadowsafin/synapass/issues) |
| Something broken | [Troubleshooting](documentation/troubleshooting.md) — start with the `request_id` from the failing response |
| Security | **Do not open an issue.** See [SECURITY.md](SECURITY.md) |
| Contributing | [CONTRIBUTING.md](CONTRIBUTING.md) |

## License

[Apache License 2.0](LICENSE). Apache 2.0 rather than MIT because it includes an
express patent grant and a patent-termination clause, which matters for
infrastructure software that companies deploy and redistribute.
