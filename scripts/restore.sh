#!/usr/bin/env bash
# Synapass restore (Linux / macOS / Windows Git Bash).
#
#   ./scripts/restore.sh <backup-dir> [all|postgres|redis|clickhouse|volumes]
#
# Restores a backup taken by scripts/backup.sh. The stack must be RUNNING for
# postgres/redis/clickhouse restores (start it with ./scripts/deploy.sh first);
# for `volumes` the stack must be STOPPED (the script stops it itself) because
# files are written under live containers otherwise.
#
# postgres restore targets an EMPTY database: reset first with
# ./scripts/deploy.sh reset --yes, then up, then restore. See
# documentation/backup.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

STEP="setup"
fail() { printf '\nRESTORE FAILED during: %s\n  %s\n' "$STEP" "$*"; exit 1; }
trap 'fail "unexpected error (exit $?)"' ERR
info() { printf '  %s\n' "$*"; }
success() { printf '  OK  %s\n' "$*"; }

SRC="${1:-}"; WHAT="${2:-all}"
[ -n "$SRC" ] || fail "usage: ./scripts/restore.sh <backup-dir> [all|postgres|redis|clickhouse|volumes]"
[ -d "$SRC" ] || fail "backup dir not found: $SRC"
SRC_ABS="$(cd "$SRC" && pwd)"
PROJECT="${COMPOSE_PROJECT_NAME:-synapass}"

dotenv() { # $1=name [$2=default]
  local v="" def="${2:-}"
  if [ -f "$REPO_ROOT/.env" ]; then
    v="$(grep -E "^$1=" "$REPO_ROOT/.env" | tail -n1 | cut -d= -f2- || true)"
  fi
  [ -z "$v" ] && v="${!1:-$def}"
  [ -z "$v" ] && v="$def"
  printf '%s' "$v"
}

PG_USER="$(dotenv POSTGRES_USER synapass)"
PG_DB="$(dotenv POSTGRES_DB synapass)"
CH_DB="$(dotenv CLICKHOUSE_DB synapass)"
CH_USER="$(dotenv CLICKHOUSE_USER synapass)"
CH_PASS="$(dotenv CLICKHOUSE_PASSWORD synapass)"

restore_postgres() {
  STEP="postgres restore"
  [ -f "$SRC/postgres.sql.gz" ] || fail "postgres.sql.gz missing in $SRC"
  [ -n "$(docker compose ps -q postgres)" ] || fail "postgres is not running; start the stack first."
  info "restoring postgres ($PG_DB) -- target must be empty (deploy.sh reset --yes first)."
  gunzip -c "$SRC/postgres.sql.gz" | docker compose exec -T postgres psql -U "$PG_USER" -d "$PG_DB" -q -v ON_ERROR_STOP=1
  success "postgres restored"
}

restore_redis() {
  STEP="redis restore"
  [ -f "$SRC/redis-dump.rdb" ] || fail "redis-dump.rdb missing in $SRC"
  [ -n "$(docker compose ps -q redis)" ] || fail "redis is not running; start the stack first."
  docker compose stop redis >/dev/null
  docker cp "$SRC/redis-dump.rdb" "$(docker compose ps -q redis 2>/dev/null || docker compose ps -aq redis):/data/dump.rdb" 2>/dev/null \
    || docker cp "$SRC/redis-dump.rdb" "$(docker compose ps -aq redis):/data/dump.rdb"
  docker compose start redis >/dev/null
  success "redis restored (restarted with the snapshot)"
}

restore_clickhouse() {
  STEP="clickhouse restore"
  [ -f "$SRC/clickhouse-backup.zip" ] || fail "clickhouse-backup.zip missing in $SRC (fall back to: restore.sh $SRC volumes)"
  [ -n "$(docker compose ps -q clickhouse)" ] || fail "clickhouse is not running; start the stack first."
  CH_CID="$(docker compose ps -q clickhouse)"
  docker cp "$SRC/clickhouse-backup.zip" "$CH_CID:/tmp/synapass-restore.zip"
  docker compose exec -T clickhouse clickhouse-client --user "$CH_USER" --password "$CH_PASS" \
    --query "RESTORE DATABASE $CH_DB FROM File('/tmp/synapass-restore.zip')"
  docker compose exec -T clickhouse rm -f /tmp/synapass-restore.zip >/dev/null 2>&1 || true
  success "clickhouse restored"
}

restore_volumes() {
  STEP="volume restore"
  info "stopping the stack (volumes cannot be restored under live containers)..."
  docker compose down >/dev/null
  for archive in "$SRC_ABS"/vol-*.tgz; do
    [ -e "$archive" ] || fail "no vol-*.tgz archives in $SRC"
    vol="$PROJECT-$(basename "$archive" .tgz | sed 's/^vol-//')"
    docker volume inspect "$vol" >/dev/null 2>&1 || docker volume create "$vol" >/dev/null
    docker run --rm -v "$vol:/v" -v "$SRC_ABS:/b:ro" alpine sh -c "rm -rf /v/* /v/..?* 2>/dev/null; tar xzf /b/$(basename "$archive") -C /v"
    success "$vol restored"
  done
  info "start the stack again with: docker compose up -d"
}

case "$WHAT" in
  all)        restore_postgres; restore_redis; restore_clickhouse; info "volume tars left untouched (use '$0 $SRC volumes' for a full volume-level restore on a stopped stack)." ;;
  postgres)   restore_postgres ;;
  redis)      restore_redis ;;
  clickhouse) restore_clickhouse ;;
  volumes)    restore_volumes ;;
  *) fail "unknown target '$WHAT' (want all|postgres|redis|clickhouse|volumes)." ;;
esac

printf '\nRestore (%s) complete from %s\n' "$WHAT" "$SRC"
