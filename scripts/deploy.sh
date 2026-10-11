#!/usr/bin/env bash
# Synapass one-command deploy (Linux / macOS / Windows Git Bash).
#
#   ./scripts/deploy.sh            # bootstrap .env, start the stack, wait, print URLs
#   ./scripts/deploy.sh <command>  # stop | restart | logs | status | reset | help
#
# The script is idempotent: running it twice reuses .env, volumes and healthy
# containers, and only rebuilds images whose sources changed. First-run work
# (Postgres migrations, catalogue seeding) happens inside the gateway itself
# (SYNAPASS_POSTGRES_AUTO_MIGRATE=true + the idempotent config seeder), so there is
# nothing to trigger by hand -- this script only waits until it is done.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ADMIN_PLACEHOLDER="syn_admin_change_me_in_production_000000"
STEP="starting up"

# ---------------------------------------------------------------------------
# Logging + failure reporting
# ---------------------------------------------------------------------------
info()    { printf '  %s\n' "$*"; }
success() { printf '  OK  %s\n' "$*"; }
warn()    { printf '  WARN %s\n' "$*"; }
fail() {
  printf '\nDEPLOY FAILED during: %s\n' "$STEP"
  printf '  %s\n' "$*"
  printf 'Next steps:\n'
  printf '  - docker compose ps                    # which container is unhealthy\n'
  printf '  - docker compose logs --tail=100 <svc> # e.g. gateway, dashboard, postgres\n'
  printf '  - bash scripts/smoke.sh                # end-to-end check once it is up\n'
  exit 1
}
trap 'fail "unexpected error (exit $?)."' ERR

# Read a KEY from .env without executing it (values here are simple KEY=VALUE).
env_get() { # $1=key [$2=default]
  local key="$1" def="${2:-}" val=""
  if [ -f "$REPO_ROOT/.env" ]; then
    val="$(grep -E "^${key}=" "$REPO_ROOT/.env" | tail -n1 | cut -d= -f2- || true)"
  fi
  [ -z "$val" ] && val="${!key:-$def}"
  [ -z "$val" ] && val="$def"
  printf '%s' "$val"
}

# True when something on this host already answers on 127.0.0.1:$1.
port_in_use() { # $1=port
  (echo > "/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
}

# Detect the machine's current LAN IPv4 for the summary line (best effort,
# never fails the deploy). Prefers the source address of the default route.
detect_lan_ip() {
  local ip=""
  if command -v ip >/dev/null 2>&1; then
    ip="$(ip route get 1.1.1.1 2>/dev/null | grep -oP '(?<=src )\d+\.\d+\.\d+\.\d+' | head -n1 || true)"
  fi
  if [ -z "$ip" ] && command -v hostname >/dev/null 2>&1; then
    ip="$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -vE '^(127\.|169\.254\.)' | head -n1 || true)"
  fi
  printf '%s' "$ip"
}

need_cmd() { # $1=cmd [$2=hint]
  command -v "$1" >/dev/null 2>&1 || fail "missing required tool: $1. ${2:-Install it and re-run.}"
}

# Poll $2 until curl succeeds or $3 seconds elapse. Reports which service failed.
wait_for() { # $1=label $2=url $3=timeout_s
  local label="$1" url="$2" timeout="$3" waited=0
  printf '  ... waiting for %s (%s)\n' "$label" "$url"
  until curl -sf -m 5 -o /dev/null "$url" 2>/dev/null; do
    sleep 5
    waited=$((waited + 5))
    if [ "$waited" -ge "$timeout" ]; then
      fail "$label never became ready at $url after ${timeout}s."
    fi
  done
  success "$label is up"
}

# ---------------------------------------------------------------------------
# Step 1: prerequisites
# ---------------------------------------------------------------------------
check_prereqs() {
  STEP="prerequisite checks"
  need_cmd docker "Install Docker Desktop (or the Engine) from https://docs.docker.com/get-docker/"
  docker info >/dev/null 2>&1 || fail "the Docker daemon is not reachable. Start Docker Desktop (or dockerd) and re-run."
  docker compose version >/dev/null 2>&1 || fail "the 'docker compose' v2 plugin is missing. Update Docker Desktop / install docker-compose-plugin."
  need_cmd curl "Install curl and re-run."
  success "docker + compose + curl present"
}

# ---------------------------------------------------------------------------
# Step 2: host ports
# ---------------------------------------------------------------------------
check_ports() {
  STEP="host port checks"
  local gw_port dash_port graf_port
  gw_port="$(env_get GATEWAY_PORT 8080)"
  dash_port=3000   # fixed host port in docker-compose.yml (only the bind interface varies)
  graf_port=3001
  if port_in_use "$gw_port"; then
    fail "host port $gw_port is already in use (gateway). Move the host port instead of the container port, e.g. GATEWAY_PORT=18080 with NEXT_PUBLIC_SYNAPASS_API_URL=http://localhost:18080 in .env, then re-run."
  fi
  if port_in_use "$dash_port"; then
    fail "host port $dash_port is already in use (dashboard). Stop whatever serves it, then re-run."
  fi
  if port_in_use "$graf_port"; then
    warn "host port $graf_port is already in use (Grafana will fail to bind; the rest of the stack is unaffected)."
  fi
  success "required host ports are free"
}

# ---------------------------------------------------------------------------
# Step 3: .env bootstrap
# ---------------------------------------------------------------------------
gen_admin_key() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 24
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c "import secrets; print(secrets.token_hex(24))"
  else
    fail "cannot generate SYNAPASS_ADMIN_KEY: neither openssl nor python3 found. Install one, or create .env from .env.example and set SYNAPASS_ADMIN_KEY by hand."
  fi
}

bootstrap_env() {
  STEP="environment bootstrap"
  if [ ! -f "$REPO_ROOT/.env" ]; then
    [ -f "$REPO_ROOT/.env.example" ] || fail ".env.example is missing; cannot bootstrap .env. Re-clone the repository."
    cp "$REPO_ROOT/.env.example" "$REPO_ROOT/.env"
    local key
    key="$(gen_admin_key)"
    # -i.bak (then remove the backup) is the portable spelling: it works with
    # both GNU and BSD sed, unlike bare -i.
    sed -i.bak "s/^SYNAPASS_ADMIN_KEY=.*/SYNAPASS_ADMIN_KEY=${key}/" "$REPO_ROOT/.env" && rm -f "$REPO_ROOT/.env.bak"
    success "created .env from .env.example with a generated SYNAPASS_ADMIN_KEY"
    info "Add a provider key to .env when ready (OPENAI_API_KEY / ANTHROPIC_API_KEY)."
    info "Without one the stack still starts; providers report unconfigured until then."
  else
    success "reusing existing .env"
  fi
  local admin
  admin="$(env_get SYNAPASS_ADMIN_KEY)"
  [ -n "$admin" ] || fail "SYNAPASS_ADMIN_KEY is empty in .env. Set it to a long random value (e.g. the output of: openssl rand -hex 24)."
  [ "$admin" != "$ADMIN_PLACEHOLDER" ] || fail "SYNAPASS_ADMIN_KEY still holds the .env.example placeholder, which production validation refuses. Set a real value in .env (e.g. the output of: openssl rand -hex 24)."
}

# ---------------------------------------------------------------------------
# Step 4: start + readiness
# ---------------------------------------------------------------------------
compose_up() {
  STEP="starting the compose stack (docker compose up -d --build)"
  info "Building images if needed (first run downloads bases and compiles the dashboard -- several minutes is normal)..."
  docker compose up -d --build || fail "docker compose failed. Inspect the build output above."
  success "containers started"
}

wait_ready() {
  STEP="readiness checks"
  local gw_port dash_host graf_host
  gw_port="$(env_get GATEWAY_PORT 8080)"
  dash_host="http://127.0.0.1:3000"
  graf_host="http://127.0.0.1:3001"
  # Migrations + catalogue seeding run inside the gateway on boot; /ready only
  # reports ready once Postgres answers and providers are seeded, so polling it
  # IS waiting for first-run setup to finish.
  wait_for "postgres (via gateway readiness)" "http://127.0.0.1:${gw_port}/ready" 300
  wait_for "gateway liveness" "http://127.0.0.1:${gw_port}/health" 120
  wait_for "dashboard" "$dash_host/" 180
  if curl -sf -m 5 -o /dev/null "http://127.0.0.1:9101/metrics" 2>/dev/null; then
    success "workers metrics reachable"
  else
    warn "workers metrics (:9101/metrics) not reachable yet; check 'docker compose ps workers'."
  fi
  if curl -sf -m 5 -o /dev/null "$graf_host/login" 2>/dev/null; then
    success "grafana reachable"
  else
    warn "grafana ($graf_host) not reachable yet; observability is optional for the core stack."
  fi
}

print_summary() {
  local gw_port lan_ip
  gw_port="$(env_get GATEWAY_PORT 8080)"
  lan_ip="$(detect_lan_ip)"
  printf '\n========================================\n'
  printf '  Synapass is up\n'
  printf '========================================\n'
  printf '  Dashboard   http://127.0.0.1:3000\n'
  if [ -n "$lan_ip" ]; then
    printf '  Dashboard (LAN) http://%s:3000  (other devices on this network)\n' "$lan_ip"
  fi
  printf '  Gateway     http://127.0.0.1:%s  (/health /ready /metrics)\n' "$gw_port"
  printf '  Grafana     http://127.0.0.1:3001  (admin / value of GRAFANA_PASSWORD in .env)\n'
  printf '  Prometheus  http://127.0.0.1:9090\n'
  printf '\n  First visit: the dashboard asks you to create the console administrator.\n'
  printf '  There are no default credentials. Then add a provider key to .env and run:\n'
  printf '    docker compose up -d --build dashboard   # re-bake only if the API URL changed\n'
  printf '    bash scripts/smoke.sh                   # end-to-end verification\n'
  printf '\n  Manage: ./scripts/deploy.sh {stop|restart|logs|status|reset}\n'
}

cmd_up() {
  echo "Synapass deploy: starting the full stack"
  check_prereqs
  check_ports
  bootstrap_env
  compose_up
  wait_ready
  print_summary
}

# ---------------------------------------------------------------------------
# Helper commands
# ---------------------------------------------------------------------------
cmd_stop() {
  check_prereqs
  STEP="stopping the stack"
  docker compose down || fail "docker compose down failed."
  success "stack stopped (volumes kept; data survives a restart)"
}

cmd_restart() {
  check_prereqs
  STEP="restarting the stack"
  docker compose restart || fail "docker compose restart failed."
  success "stack restarted"
  wait_ready
}

cmd_logs() {
  check_prereqs
  # Logs for one service when named, else the interesting core (gateway+workers).
  if [ "${1:-}" != "" ]; then
    docker compose logs -f "$1"
  else
    docker compose logs -f gateway workers
  fi
}

cmd_status() {
  check_prereqs
  docker compose ps
}

cmd_reset() {
  check_prereqs
  if [ "${1:-}" != "--yes" ]; then
    printf 'This stops the stack and DELETES ALL VOLUMES (postgres, redis, clickhouse, grafana data).\n'
    printf 'Re-run as: ./scripts/deploy.sh reset --yes\n'
    exit 2
  fi
  STEP="resetting the stack (containers + volumes)"
  docker compose down -v || fail "docker compose down -v failed."
  success "stack reset; next './scripts/deploy.sh' starts from a clean slate"
}

usage() {
  sed -n '2,10p' "${BASH_SOURCE[0]}"
  echo ""
  echo "Usage: ./scripts/deploy.sh [up|stop|restart|logs [service]|status|reset --yes|help]"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
cmd="${1:-up}"
case "$cmd" in
  up)      cmd_up ;;
  stop)    cmd_stop ;;
  restart) cmd_restart ;;
  logs)    cmd_logs "${2:-}" ;;
  status)  cmd_status ;;
  reset)   cmd_reset "${2:-}" ;;
  help|--help|-h) usage ;;
  *) fail "unknown command '$cmd'." ;;
esac
