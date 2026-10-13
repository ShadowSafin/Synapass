# Synapass one-command deploy (Windows PowerShell).
#
#   .\scripts\deploy.ps1            # bootstrap .env, start the stack, wait, print URLs
#   .\scripts\deploy.ps1 <command>  # stop | restart | logs | status | reset | help
#
# Same behaviour as scripts/deploy.sh: idempotent, .env is created from
# .env.example on first run (with a generated SYNAPASS_ADMIN_KEY) and reused after
# that. Migrations + catalogue seeding run inside the gateway, so this script
# only waits until /ready reports them done.
param(
  [string]$Command = "up",
  [string]$Service = "",
  [switch]$Yes
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSCommandPath)
Set-Location -LiteralPath $RepoRoot
$AdminPlaceholder = "syn_admin_change_me_in_production_000000"

function Fail([string]$Step, [string]$Message) {
  Write-Host ""
  Write-Host "DEPLOY FAILED during: $Step"
  Write-Host "  $Message"
  Write-Host "Next steps:"
  Write-Host "  - docker compose ps                    # which container is unhealthy"
  Write-Host "  - docker compose logs --tail=100 <svc> # e.g. gateway, dashboard, postgres"
  Write-Host "  - powershell -ExecutionPolicy Bypass -File scripts/smoke.ps1"
  exit 1
}

# Read a KEY from .env (simple KEY=VALUE lines; no execution).
function Get-EnvValue([string]$Key, [string]$Default = "") {
  $EnvFile = Join-Path $RepoRoot ".env"
  if (Test-Path -LiteralPath $EnvFile) {
    $Line = Select-String -LiteralPath $EnvFile -Pattern "^$Key=" | Select-Object -Last 1
    if ($Line) {
      $Val = ($Line.Line -split "=", 2)[1]
      if ($Val -ne "") { return $Val }
    }
  }
  $FromProcess = [Environment]::GetEnvironmentVariable($Key)
  if ($FromProcess) { return $FromProcess }
  return $Default
}

function Test-PortInUse([int]$Port) {
  $Client = New-Object Net.Sockets.TcpClient
  try {
    $Task = $Client.ConnectAsync("127.0.0.1", $Port)
    # Wait() alone is not enough: it returns true for a fast refusal too, so
    # the Connected flag is the actual answer.
    return ($Task.Wait(500) -and $Client.Connected)
  } catch { return $false } finally { $Client.Close() }
}

# Detect the machine's current LAN IPv4: the address on the interface that
# carries the default route (Wi-Fi on a laptop), skipping loopback, link-local
# and virtual adapters. Recomputed at every launch so a network change is
# picked up without editing .env; returns "" when nothing suitable exists.
function Get-LanIPv4 {
  try {
    $Route = Get-NetRoute -DestinationPrefix "0.0.0.0/0" -AddressFamily IPv4 `
      -ErrorAction SilentlyContinue | Sort-Object RouteMetric | Select-Object -First 1
    if ($Route) {
      $Addr = Get-NetIPAddress -InterfaceIndex $Route.InterfaceIndex -AddressFamily IPv4 `
        -ErrorAction SilentlyContinue |
        Where-Object { $_.IPAddress -notmatch "^(127\.|169\.254\.)" } |
        Select-Object -First 1
      if ($Addr) { return $Addr.IPAddress }
    }
  } catch { }
  return ""
}

function New-AdminKey {
  $Bytes = New-Object byte[] 24
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($Bytes)
  return ([BitConverter]::ToString($Bytes)).Replace("-", "").ToLower()
}

function Wait-For([string]$Label, [string]$Url, [int]$TimeoutSec) {
  Write-Host "  ... waiting for $Label ($Url)"
  $Deadline = (Get-Date).AddSeconds($TimeoutSec)
  while ((Get-Date) -lt $Deadline) {
    try {
      $Resp = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 5
      if ($Resp.StatusCode -ge 200 -and $Resp.StatusCode -lt 400) {
        Write-Host "  OK  $Label is up"
        return
      }
    } catch { Start-Sleep -Seconds 5 }
  }
  Fail "readiness checks" "$Label never became ready at $Url after ${TimeoutSec}s."
}

function Invoke-Up {
  Write-Host "Synapass deploy: starting the full stack"

  # 1. Prerequisites.
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Fail "prerequisite checks" "docker not found. Install Docker Desktop from https://docs.docker.com/get-docker/"
  }
  try { docker info 2>$null | Out-Null } catch { Fail "prerequisite checks" "the Docker daemon is not reachable. Start Docker Desktop and re-run." }
  if ($LASTEXITCODE -ne 0) { Fail "prerequisite checks" "the Docker daemon is not reachable. Start Docker Desktop and re-run." }
  docker compose version 2>$null | Out-Null
  if ($LASTEXITCODE -ne 0) { Fail "prerequisite checks" "the 'docker compose' v2 plugin is missing. Update Docker Desktop."
  }
  Write-Host "  OK  docker + compose present"

  # 2. Host ports (dashboard 3000 / grafana 3001 are fixed host ports in compose).
  $GwPort = Get-EnvValue "GATEWAY_PORT" "8080"
  if (Test-PortInUse $GwPort) {
    Fail "host port checks" "host port $GwPort is already in use (gateway). Set GATEWAY_PORT=18080 with NEXT_PUBLIC_SYNAPASS_API_URL=http://localhost:18080 in .env, then re-run."
  }
  if (Test-PortInUse 3000) { Fail "host port checks" "host port 3000 is already in use (dashboard). Stop whatever serves it, then re-run." }
  if (Test-PortInUse 3001) { Write-Host "  WARN host port 3001 is in use (Grafana will fail to bind; rest of stack unaffected)." }
  Write-Host "  OK  required host ports are free"

  # 3. .env bootstrap.
  $EnvFile = Join-Path $RepoRoot ".env"
  if (-not (Test-Path -LiteralPath $EnvFile)) {
    $Example = Join-Path $RepoRoot ".env.example"
    if (-not (Test-Path -LiteralPath $Example)) { Fail "environment bootstrap" ".env.example is missing; cannot bootstrap .env. Re-clone the repository." }
    Copy-Item -LiteralPath $Example -Destination $EnvFile
    $Key = New-AdminKey
    $Content = Get-Content -LiteralPath $EnvFile
    $Content = $Content -replace "^SYNAPASS_ADMIN_KEY=.*$", "SYNAPASS_ADMIN_KEY=$Key"
    Set-Content -LiteralPath $EnvFile -Value $Content
    Write-Host "  OK  created .env from .env.example with a generated SYNAPASS_ADMIN_KEY"
    Write-Host "  Add a provider key to .env when ready (OPENAI_API_KEY / ANTHROPIC_API_KEY)."
  } else {
    Write-Host "  OK  reusing existing .env"
  }
  $Admin = Get-EnvValue "SYNAPASS_ADMIN_KEY"
  if ([string]::IsNullOrEmpty($Admin)) { Fail "environment bootstrap" "SYNAPASS_ADMIN_KEY is empty in .env. Set it to a long random value." }
  if ($Admin -eq $AdminPlaceholder) {
    # Explicit local evaluation only: $env:SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY=true
    # keeps the shipped placeholder usable for a throwaway checkout. Anything
    # else -- and the gateway's own production validation -- refuses it.
    if ([Environment]::GetEnvironmentVariable("SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY") -eq "true") {
      Write-Host "  WARN SYNAPASS_ADMIN_KEY is the shipped placeholder AND SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY=true."
      Write-Host "  WARN This install is NOT secured: anyone with the example file is admin. Never expose it."
    } else {
      Fail "environment bootstrap" "SYNAPASS_ADMIN_KEY still holds the .env.example placeholder, which production validation refuses. Set a real value in .env. For a throwaway local evaluation only, set `$env:SYNAPASS_ALLOW_PLACEHOLDER_ADMIN_KEY='true'."
    }
  }

  # 4. Start.
  # The dashboard shows a LAN URL for other devices; its host part is detected
  # fresh here and exported for this compose invocation only (process env beats
  # .env, and .env itself is never rewritten). A pinned GATEWAY_LAN_URL in the
  # environment keeps working: explicit configuration always wins over detection.
  if (-not [Environment]::GetEnvironmentVariable("GATEWAY_LAN_URL")) {
    $LanIP = Get-LanIPv4
    if ($LanIP) {
      $GwPort = Get-EnvValue "GATEWAY_PORT" "8080"
      $env:GATEWAY_LAN_URL = "http://${LanIP}:${GwPort}"
      Write-Host "  OK  detected LAN address $env:GATEWAY_LAN_URL"
    } else {
      Write-Host "  WARN could not detect a LAN address; the dashboard falls back to .env"
    }
  } else {
    Write-Host "  OK  using pinned GATEWAY_LAN_URL from the environment"
  }
  Write-Host "  Building images if needed (first run takes several minutes -- normal)..."
  docker compose up -d --build
  if ($LASTEXITCODE -ne 0) { Fail "starting the compose stack" "docker compose failed. Inspect the build output above." }
  Write-Host "  OK  containers started"

  # 5. Readiness (/ready gates on Postgres + seeded providers, so polling it IS
  #    waiting for first-run migrations/seeding to finish).
  Wait-For "postgres (via gateway readiness)" "http://127.0.0.1:$GwPort/ready" 300
  Wait-For "gateway liveness" "http://127.0.0.1:$GwPort/health" 120
  Wait-For "dashboard" "http://127.0.0.1:3000/" 180
  try { Invoke-WebRequest -Uri "http://127.0.0.1:9101/metrics" -UseBasicParsing -TimeoutSec 5 | Out-Null; Write-Host "  OK  workers metrics reachable" }
  catch { Write-Host "  WARN workers metrics (:9101/metrics) not reachable yet; check 'docker compose ps workers'." }
  try { Invoke-WebRequest -Uri "http://127.0.0.1:3001/login" -UseBasicParsing -TimeoutSec 5 | Out-Null; Write-Host "  OK  grafana reachable" }
  catch { Write-Host "  WARN grafana (http://127.0.0.1:3001) not reachable yet; observability is optional for the core stack." }

  Write-Host ""
  Write-Host "========================================"
  Write-Host "  Synapass is up"
  Write-Host "========================================"
  Write-Host "  Dashboard   http://127.0.0.1:3000"
  try {
    $DashLan = Get-LanIPv4
    if ($DashLan) { Write-Host "  Dashboard (LAN) http://${DashLan}:3000  (other devices on this network)" }
  } catch { }
  Write-Host "  Gateway     http://127.0.0.1:$GwPort  (/health /ready /metrics)"
  Write-Host "  Grafana     http://127.0.0.1:3001  (admin / value of GRAFANA_PASSWORD in .env)"
  Write-Host "  Prometheus  http://127.0.0.1:9090"
  Write-Host ""
  Write-Host "  First visit: the dashboard asks you to create the console administrator."
  Write-Host "  There are no default credentials."
  Write-Host ""
  Write-Host "  Manage: .\scripts\deploy.ps1 {stop|restart|logs|status|reset}"
}

switch ($Command) {
  "up" {
    Invoke-Up
  }
  "stop" {
    docker compose down
    if ($LASTEXITCODE -ne 0) { Fail "stopping the stack" "docker compose down failed." }
    Write-Host "  OK  stack stopped (volumes kept; data survives a restart)"
  }
  "restart" {
    docker compose restart
    if ($LASTEXITCODE -ne 0) { Fail "restarting the stack" "docker compose restart failed." }
    Write-Host "  OK  stack restarted"
  }
  "logs" {
    if ($Service -ne "") { docker compose logs -f $Service } else { docker compose logs -f gateway workers }
  }
  "status" { docker compose ps }
  "reset" {
    if (-not $Yes) {
      Write-Host "This stops the stack and DELETES ALL VOLUMES (postgres, redis, clickhouse, grafana data)."
      Write-Host "Re-run as: .\scripts\deploy.ps1 reset -Yes"
      exit 2
    }
    docker compose down -v
    if ($LASTEXITCODE -ne 0) { Fail "resetting the stack" "docker compose down -v failed." }
    Write-Host "  OK  stack reset; next '.\scripts\deploy.ps1' starts from a clean slate"
  }
  default {
    # Header comment only (lines starting with #); the param block is not help.
    Get-Content -LiteralPath $PSCommandPath | Where-Object { $_ -match "^\s*#" } | Select-Object -First 10
    Write-Host ""
    Write-Host "Usage: .\scripts\deploy.ps1 [up|stop|restart|logs|status|reset|help] [-Service <name>] [-Yes]"
  }
}
