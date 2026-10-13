# Installation: Docker

Docker Compose is the fastest path to a complete Synapass: the gateway, four
datastores, the intelligence workers, the dashboard and an observability stack.
This page covers the whole stack; for a host install see
[Installation: native](native.md).

## Prerequisites

- Docker with Compose v2
- A provider API key, for real answers
- Ports free on your host: `8080` (gateway), `3000` (dashboard), `3001`
  (Grafana), `9090` (Prometheus), `9101` (worker metrics)

## Services

`docker-compose.yml` starts eleven services:

| Service | Role | Default URL |
| --- | --- | --- |
| `gateway` | The Go gateway: inference, health, admin | `http://127.0.0.1:8080` |
| `workers` | Python intelligence tier | metrics on `:9101/metrics` |
| `dashboard` | Next.js operator console | `http://127.0.0.1:3000` |
| `postgres` | System of record | internal |
| `redis` | Limits, budgets, credential cache, response cache | internal |
| `clickhouse` | Traces and analytics | internal |
| `nats` | JetStream event bus | internal |
| `prometheus`, `grafana`, `loki`, `promtail`, `otel-collector` | Observability | Grafana `:3001`, Prometheus `:9090` |

The datastores are not published to the host by default. Compose brings them up
for you with health checks and dependency ordering, and the gateway waits for
Postgres and Redis to report healthy before it starts serving.

## Start

```bash
cp .env.example .env
openssl rand -hex 24    # paste the result into SYNAPASS_ADMIN_KEY in .env
$EDITOR .env            # add SYNAPASS_ADMIN_KEY and your provider key

docker compose up -d --build
docker compose ps
```

The first build compiles the Go binary, installs the dashboard and builds a
Python virtualenv, so expect it to take a few minutes. Subsequent starts reuse
the images.

### Port conflicts

Move the host port, not the container port — gateway configuration is unchanged:

```dotenv
GATEWAY_PORT=18080
NEXT_PUBLIC_SYNAPASS_API_URL=http://localhost:18080
```

```bash
GATEWAY=http://127.0.0.1:18080
```

## Verify

```bash
curl -s $GATEWAY/health | jq    # liveness; does not touch dependencies
curl -s $GATEWAY/ready  | jq    # readiness; what a load balancer asks
```

```json
{
  "status": "ready",
  "components": {
    "clickhouse": true, "nats": true, "postgres": true, "redis": true, "tracing": true
  }
}
```

Then run the smoke suite, which checks the admin surface, the dashboard proxy,
worker metrics, a full management cycle and the tool plane without needing a
provider key:

```bash
bash scripts/smoke.sh
# or on Windows:
powershell -ExecutionPolicy Bypass -File scripts/smoke.ps1
```

## Configuration

Three layers, lowest precedence first:

1. Built-in defaults — the stack boots with no config file at all.
2. A config file — `config.example.yaml` documents every setting.
3. Environment variables — every `SYNAPASS_*` variable wins.

`docker-compose.yml` passes the file in and lets the environment override it. To
see what the gateway actually resolved, with secrets redacted:

```bash
docker compose exec gateway synapass config
```

Secrets are referenced, never embedded. A provider record names an environment
variable (`api_key_env`) rather than carrying a key, which keeps the config file
committable.

To use a configuration file other than the example:

```dotenv
SYNAPASS_CONFIG_FILE=/etc/synapass/config.yaml
```

and mount it into the container in `docker-compose.yml`.

## Migrations

Compose sets `SYNAPASS_POSTGRES_AUTO_MIGRATE=true`, so the gateway applies pending
migrations at startup — convenient for a single host.

Under change control, turn it off and migrate as an explicit release step:

```dotenv
SYNAPASS_POSTGRES_AUTO_MIGRATE=false
```

```bash
docker compose run --rm gateway synapass migrate
```

Migrations are applied in order, recorded with a checksum, and a mismatch on an
already-applied migration is an error rather than a silent re-run.

## Day-to-day operations

```bash
docker compose ps                        # status
docker compose logs -f gateway          # tail the gateway
docker compose logs -f workers          # tail the workers
docker compose restart gateway          # restart one service
docker compose down                     # stop, keep volumes
docker compose down -v                  # stop and delete data
```

### Starting and restarting (Windows)

Use the wrapper instead of bare `docker compose up` so the Endpoints page keeps
showing the machine's current LAN address after a network change:

```powershell
.\scripts\up.ps1           # detect the LAN IPv4, then docker compose up -d
.\scripts\up.ps1 gateway   # restart one service with a fresh LAN address
```

The script detects the IPv4 on the default-route interface and exports it as
`GATEWAY_LAN_URL` for that compose invocation only. `.env` is never rewritten
— leave `GATEWAY_LAN_URL` empty there for auto-detect, or pin it (in `.env` or
the shell) to override detection. The dashboard also derives the LAN URL from
the browser's own address when you open it via a LAN IP, so phones and other
devices on the same network get a copy-paste URL that works.

### Upgrading

```bash
git pull
docker compose up -d --build
docker compose ps
curl -s $GATEWAY/ready | jq
```

The gateway drains on `SIGTERM`: it stops accepting, finishes in-flight streaming
responses, flushes the telemetry buffer, then exits. Compose gives it
`stop_grace_period` for that.

### Secrets

`SYNAPASS_ADMIN_KEY` is not optional in production: with `admin.require_scope: true`,
validation refuses to start without it — and refuses the shipped placeholder
(`syn_admin_change_me_in_production_000000`) as well. That is deliberate — a
production install that cannot be administered is a worse outcome than one that
refuses to boot. The only bypass is an explicit throwaway evaluation
(`SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY=true`), which warns loudly and must
never be exposed.

```bash
openssl rand -hex 24
```

Set `SYNAPASS_CREDENTIALS_KEY` (32 bytes, raw/hex/base64) for serious deployments so
stored provider credentials do not derive from the admin key. Otherwise the data
key is derived via HKDF-SHA256 from `SYNAPASS_ADMIN_KEY`, which means rotating the admin
key orphans stored secrets and they must be re-saved.

## Exposing it outside your host

### LAN access (other devices on the same network)

The dashboard listens on the LAN by default: open `http://<lan-ip>:3000`
from a phone or another computer (find the IP with `ipconfig` on Windows or
`hostname -I` on Linux). `http://localhost:3000` keeps working on the host
itself. No extra setting is needed; the dashboard proxies the admin API
server-side, so its pages work over LAN out of the box. If the OS firewall
prompts, allow Node.js / Docker access for private networks.

To reach the gateway API from LAN devices too (e.g. the curl examples on the
Endpoints page), two more steps are required — the gateway serves
unauthenticated inference, so do this only behind a firewall:

```dotenv
GATEWAY_BIND=0.0.0.0
SYNAPASS_HTTP_CORS_ALLOWED_ORIGINS=http://localhost:3000,http://192.168.1.20:3000
```

with `192.168.1.20` replaced by this machine's LAN IP. `GATEWAY_LAN_URL`
(e.g. `http://192.168.1.20:8080`) controls the LAN address shown on the
Endpoints page; `.\scripts\up.ps1` / `scripts/deploy.ps1` detect it
automatically on Windows.

### Public access

The gateway publishes on the host's loopback interface only (the dashboard is
the LAN exception above). To reach the gateway from elsewhere:

- **[Cloudflare tunnel](cloudflare-tunnel.md)** — a temporary public URL, no
  port forwarding, no DNS. Best for demos and phones.
- A reverse proxy (nginx, Caddy, Traefik) terminating TLS in front of the
  published port, for a permanent deployment.

The tunnel feature ships enabled, but still needs an explicit create action before
anything is public: set `SYNAPASS_TUNNEL_ENABLED=false` to remove the capability
entirely.

---

Related: [Getting started](../getting-started.md) · [Installation: native](native.md) · [Cloudflare tunnel](cloudflare-tunnel.md) · [Self-hosting: HA scope](../self-hosting.md) · [Backup and restore](../backup.md) · [Troubleshooting](../troubleshooting.md) · [Back to README](../../README.md)