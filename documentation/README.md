# Synapass documentation

Everything about Synapass beyond the root [README](../README.md): how it is
built, how to run it, and how each part behaves.

New here? Start with [Getting started](getting-started.md) — it takes you from
empty directory to a working inference call in about five minutes.

## Contents

### Understand the system

| Document | Read it when you want to know |
| --- | --- |
| [Overview](overview.md) | What Synapass is for, and the problem it solves |
| [Architecture](architecture.md) | How the pieces fit, and why each datastore exists |
| [Glossary](glossary.md) | What a term means in Synapass specifically |

### Run it

| Document | Read it when you want to know |
| --- | --- |
| [Getting started](getting-started.md) | The shortest path to a working stack |
| [Installation: Docker](installation/docker.md) | How to run the Compose stack |
| [Installation: native](installation/native.md) | How to run it on a host under systemd |
| [Installation: Cloudflare tunnel](installation/cloudflare-tunnel.md) | How to expose a local service temporarily |

### Configure it

| Document | Read it when you want to know |
| --- | --- |
| [Providers](providers.md) | How to add, secure and test model providers |
| [Routing](routing.md) | How a request picks a provider, and how it fails over |
| [Caching](caching.md) | When responses are reused, and how to invalidate them |
| [Tools](tools.md) | Tool calling, the registry, and gateway-side execution |
| [Database](database.md) | Tables, migrations and persistence boundaries |

### Operate it

| Document | Read it when you want to know |
| --- | --- |
| [API reference](api.md) | Every endpoint, header and error code |
| [Dashboard](dashboard.md) | The operator console and its workflows |
| [Analytics](analytics.md) | Where the report's numbers come from, and their limits |
| [Playground](playground.md) | How the endpoint testing console runs a real request |
| [Observability](observability.md) | Metrics, traces, logs and dashboards |
| [Self-hosting: HA scope](self-hosting.md) | Single-node guarantees, resource limits, what HA would require |
| [Backup and restore](backup.md) | What gets backed up, procedures, scheduling |
| [Troubleshooting](troubleshooting.md) | Something is broken and you need the cause |
| [FAQ](faq.md) | A short answer to a common question |
| [Changelog](changelog.md) | What each delivery phase added |

### Security and governance

These stay at the repository root, where tooling expects to find them.

| Document | Covers |
| --- | --- |
| [SECURITY.md](../SECURITY.md) | Reporting a vulnerability, the credential model, console operator login, hardening before production |
| [CONTRIBUTING.md](../CONTRIBUTING.md) | Conventions, tests, commit style, which document owns which fact |
| [CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md) | Expectations for conduct, including technical disagreement |
| [LICENSE](../LICENSE) | Apache 2.0 |

## Conventions used here

**Ports.** Examples use `http://127.0.0.1:8080` for the gateway and
`http://127.0.0.1:3000` for the dashboard. The Compose stack moves the gateway to
`18080` when `8080` is taken; set `GATEWAY` accordingly.

**Credentials.** `$SYNAPASS_ADMIN_KEY` is the control-plane key from `.env`.
`$SYNAPASS_KEY` is a tenant inference key minted through the admin API.

**Callouts.**

> **Note** — something worth knowing that is not essential to proceed.

> **Warning** — something that will cost you time if you ignore it.

**Cross-links.** Every document links back here and to its siblings, so you can
always find the neighbouring concept. Nothing important is documented only once:
if a fact appears in several places, the other places link to the document that
owns it.