# Backup and restore

All state lives in named volumes (`docker volume ls` shows the `synapass-*`
set); `docker compose down` keeps them and `down -v` deletes them. Backups are
two wrapper scripts with the same behaviour on both shells:

| Task | bash | PowerShell |
| --- | --- | --- |
| Backup | `./scripts/backup.sh [dest]` | `.\scripts\backup.ps1 [dest]` |
| Restore | `./scripts/restore.sh <dest> [target]` | `.\scripts\restore.ps1 <dest> [target]` |
| Targets | `make backup` / `make restore SRC=<dir> [WHAT=...]` | `make backup-ps` / `make restore-ps SRC=<dir>` |

Default destination: `./backups/<UTC-timestamp>/`.

## What a backup contains

| File | Source | Restore path |
| --- | --- | --- |
| `postgres.sql.gz` | `pg_dump` of the system of record | `psql` into an empty database (the primary path: portable across versions) |
| `redis-dump.rdb` | `BGSAVE` + copy of `dump.rdb` | stop redis, copy back, start |
| `clickhouse-backup.zip` | ClickHouse native `BACKUP DATABASE` | `RESTORE DATABASE ... FROM File(...)` |
| `vol-*.tgz` | tar of every named volume | untar over stopped containers |
| `backup-manifest.txt` | timestamp, commit, volume list, file sizes | inspection |

The volume tars are the catch-all: they cover NATS JetStream, Grafana,
Prometheus, Loki and anything a logical dump misses. If the native ClickHouse
backup fails, its volume tar is the fallback.

## Procedures

Back up (the stack must be running):

```bash
./scripts/backup.sh
ls -lh backups/<timestamp>/
```

Restore one service (the stack must be running; postgres must be empty —
`./scripts/deploy.sh reset --yes`, then up, then restore):

```bash
./scripts/restore.sh backups/<timestamp> postgres
./scripts/restore.sh backups/<timestamp> redis
./scripts/restore.sh backups/<timestamp> clickhouse
```

Full volume-level restore (stops the stack; start it again afterwards):

```bash
./scripts/restore.sh backups/<timestamp> volumes
docker compose up -d
```

> **Warning** — a restore overwrites. The postgres path assumes an empty
> target, and the volume path replaces live data wholesale. Back up the
> current state first if it matters.

Schedule `backup.sh` from cron / Task Scheduler for anything beyond a laptop;
the scripts are idempotent and timestamp every run. Test a restore on a
scratch checkout at least once — an untested backup is a rumour.

---

Related: [Self-hosting: HA scope](self-hosting.md) · [Installation: Docker](installation/docker.md) · [Database](database.md) · [Troubleshooting](troubleshooting.md)
