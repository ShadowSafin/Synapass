# Self-hosting: HA scope and safeguards

The Compose stack is a **single-node** deployment: one copy of every service,
durable through restarts, with no failover. That is a deliberate scope choice,
not a missing feature — this page states what survives what, and what real HA
would require.

## What survives what

| Event | Outcome |
| --- | --- |
| Container crash / `restart` | `restart: unless-stopped` brings it back; named volumes keep data |
| Host reboot | everything returns with the daemon |
| `docker compose down` | stopped, data kept |
| `docker compose down -v` / `deploy.sh reset` | all data deleted (explicit only) |
| `docker compose ps` shows `unhealthy` | `depends_on: service_healthy` kept dependants from starting against it; check `logs` |

Every service carries `deploy.resources.limits` + `reservations` (gateway:
2 CPU / 1G; ClickHouse: 2 CPU / 2G; postgres: 1 CPU / 1G; the rest scaled to
their steady state — see `docker-compose.yml`). Limits are ceilings, so their
sum may exceed the host; the reservations sum (~1.6 GB) is what a 4 GB host
must actually have free. Health checks gate every dependency: postgres,
redis, ClickHouse and NATS report healthy before the gateway starts, the
workers gate on NATS, and the dashboard gates on the gateway. The workers'
image and compose config both probe `:9101/metrics`, which doubles as
liveness — a worker whose loop died stops serving it.

## The single-node tradeoffs

**NATS JetStream runs one copy (`replicas: 1`, in
`workers/synapass_workers/streams.py` and `internal/storage/nats.go`).**
A lost `nats-data` volume loses in-flight eval/replay jobs and buffered
telemetry — never inference itself, which never publishes synchronously. The
volume tar in [Backup and restore](backup.md) is the recovery path.

**Postgres, Redis and ClickHouse are single instances.** A host loss is a
restore-from-backup event, not a failover event.

## What HA would require

- **NATS:** a 3-node cluster (`--routes`, `--cluster_name`), `stream_replicas: 3`
  in config, and workers/gateway pointed at the seed list (`nats.urls`).
  JetStream still needs a quorum: two nodes down means writes block.
- **Postgres:** an external managed instance or streaming replication
  (Patroni / Stolon); point `SYNAPASS_DATABASE_DSN` at it and turn off
  `SYNAPASS_POSTGRES_AUTO_MIGRATE` in favour of `synapass migrate` as a
  release step.
- **Redis:** Sentinel (`sentinel_addrs` + `master_name`) or managed Redis with
  TLS (`tls: true`); the gateway already supports both.
- **Gateway/workers:** horizontally scalable once the above are external —
  run more replicas behind a load balancer asking `/ready`.
- **ClickHouse:** replicated tables + ZooKeeper/ClickHouse Keeper; analytics
  only, so most installs accept restore-from-backup instead.

## Hardened defaults

- `SYNAPASS_ADMIN_KEY` must not be the shipped placeholder
  (`syn_admin_change_me_in_production_000000`): the gateway refuses it in
  production, and `deploy.sh` / `deploy.ps1` refuse it too. The only bypass is
  an explicit throwaway evaluation —
  `SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY=true` — which prints a loud warning
  and must never be exposed. Generate a real one with `openssl rand -hex 24`.
- Set `SYNAPASS_CREDENTIALS_KEY` (32 bytes) in any deployment that stores
  provider credentials and may rotate its admin key; otherwise rotation
  orphans stored secrets.

---

Related: [Backup and restore](backup.md) · [Installation: Docker](installation/docker.md) · [Observability](observability.md) · [Troubleshooting](troubleshooting.md)
