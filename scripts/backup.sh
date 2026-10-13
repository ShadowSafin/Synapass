#!/usr/bin/env bash
# Synapass backup (Linux / macOS / Windows Git Bash).
#
#   ./scripts/backup.sh [dest-dir]
#
# Takes a consistent-enough single-node backup of all state:
#   * postgres:   logical pg_dump (portable across versions, the primary restore path)
#   * redis:      BGSAVE + copy of dump.rdb (rate-limit/budget counters)
#   * clickhouse: native BACKUP to a zip (analytics/traces), volume tar as fallback
#   * volumes:    tar of every named volume (catches NATS JetStream, Grafana, Loki...)
#
# The stack must be running. See scripts/restore.sh and documentation/backup.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

STEP="setup"
fail() { printf '\nBACKUP FAILED during: %s\n  %s\n' "$STEP" "$*"; exit 1; }
trap 'fail "unexpected error (exit $?)"' ERR
info() { printf '  %s\n' "$*"; }
success() { printf '  OK  %s\n' "$*"; }

TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="${1:-$REPO_ROOT/backups/$TS}"
PROJECT="${COMPOSE_PROJECT_NAME:-synapass}"
VOLUMES="postgres-data redis-data clickhouse-data clickhouse-logs nats-data prometheus-data loki-data grafana-data"

dotenv() { # $1=name [$2=default]
  local v="" def="${2:-}"
  if [ -f "$REPO_ROOT/.env" ]; then
    v="$(grep -E "^$1=" "$REPO_ROOT/.env" | tail -n1 | cut -d= -f2- || true)"
  fi
  [ -z "$v" ] && v="${!1:-$def}"
  [ -z "$v" ] && v="$def"
  printf '%s' "$v"
}

STEP="prerequisite checks"
command -v docker >/dev/null 2>&1 || fail "docker not found."
docker compose ps -q postgres >/dev/null 2>&1 || fail "compose stack is not running (docker compose ps shows nothing)."
[ -n "$(docker compose ps -q postgres)" ] || fail "postgres container is not running; start the stack first."
mkdir -p "$DEST"
DEST_ABS="$(cd "$DEST" && pwd)"

PG_USER="$(dotenv POSTGRES_USER synapass)"
PG_DB="$(dotenv POSTGRES_DB synapass)"
CH_DB="$(dotenv CLICKHOUSE_DB synapass)"
CH_USER="$(dotenv CLICKHOUSE_USER synapass)"
CH_PASS="$(dotenv CLICKHOUSE_PASSWORD synapass)"

# -- postgres (logical dump; restores anywhere) -------------------------------
STEP="postgres pg_dump"
info "dumping postgres ($PG_DB)..."
docker compose exec -T postgres pg_dump -U "$PG_USER" -d "$PG_DB" | gzip > "$DEST/postgres.sql.gz"
success "postgres.sql.gz ($(du -h "$DEST/postgres.sql.gz" | cut -f1))"

# -- redis (BGSAVE then copy the snapshot) ------------------------------------
STEP="redis snapshot"
info "triggering redis BGSAVE..."
BEFORE="$(docker compose exec -T redis redis-cli LASTSAVE | tr -d '\r')"
docker compose exec -T redis redis-cli BGSAVE >/dev/null
for _ in $(seq 1 30); do
  sleep 1
  AFTER="$(docker compose exec -T redis redis-cli LASTSAVE | tr -d '\r')"
  [ "$AFTER" != "$BEFORE" ] && break
done
REDIS_CID="$(docker compose ps -q redis)"
docker cp "$REDIS_CID:/data/dump.rdb" "$DEST/redis-dump.rdb" 2>/dev/null \
  || fail "no dump.rdb in the redis container (appendonly-only save?). Volume tar below still covers /data."
success "redis-dump.rdb"

# -- clickhouse (native backup; volume tar is the fallback) -------------------
STEP="clickhouse backup"
info "backing up clickhouse database ($CH_DB)..."
if docker compose exec -T clickhouse clickhouse-client --user "$CH_USER" --password "$CH_PASS" \
    --query "BACKUP DATABASE $CH_DB TO File('/tmp/synapass-backup.zip')" >/dev/null 2>&1; then
  docker cp "$(docker compose ps -q clickhouse):/tmp/synapass-backup.zip" "$DEST/clickhouse-backup.zip"
  docker compose exec -T clickhouse rm -f /tmp/synapass-backup.zip >/dev/null 2>&1 || true
  success "clickhouse-backup.zip ($(du -h "$DEST/clickhouse-backup.zip" | cut -f1))"
else
  info "native BACKUP failed; the clickhouse-data volume tar below is the restore path."
fi

# -- named volumes (catches everything, incl. NATS JetStream) -----------------
STEP="volume archives"
for vol in $VOLUMES; do
  if docker volume inspect "$PROJECT-$vol" >/dev/null 2>&1; then
    docker run --rm -v "$PROJECT-$vol:/v:ro" -v "$DEST_ABS:/b" alpine \
      tar czf "/b/vol-$vol.tgz" -C /v . >/dev/null
    success "vol-$vol.tgz"
  else
    info "volume $PROJECT-$vol does not exist; skipping."
  fi
done

# -- manifest -----------------------------------------------------------------
STEP="manifest"
{
  echo "synapass backup $TS"
  echo "commit: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  echo "project: $PROJECT"
  echo "volumes:"; docker volume ls --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null || true
  echo "files:"; ls -lh "$DEST"
} > "$DEST/backup-manifest.txt"

printf '\nBackup complete: %s\n' "$DEST"
info "Verify:  ls -lh $DEST"
info "Restore: ./scripts/restore.sh $DEST  (see documentation/backup.md)"
