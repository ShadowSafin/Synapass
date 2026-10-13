# Synapass backup (Windows PowerShell).
#
#   .\scripts\backup.ps1 [dest-dir]
#
# Same behaviour as scripts/backup.sh: pg_dump + redis BGSAVE/RDB copy +
# ClickHouse native backup + tar of every named volume. The stack must be
# running. See scripts/restore.ps1 and documentation/backup.md.
param([string]$Dest = "")

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSCommandPath)
Set-Location -LiteralPath $RepoRoot

function Fail([string]$Step, [string]$Message) {
  Write-Host ""
  Write-Host "BACKUP FAILED during: $Step"
  Write-Host "  $Message"
  exit 1
}

function Get-DotEnv([string]$Key, [string]$Default = "") {
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

$TS = (Get-Date).ToUniversalTime().ToString("yyyyMMddTHHmmssZ")
if ([string]::IsNullOrEmpty($Dest)) { $Dest = Join-Path $RepoRoot (Join-Path "backups" $TS) }
$Project = if ([Environment]::GetEnvironmentVariable("COMPOSE_PROJECT_NAME")) { [Environment]::GetEnvironmentVariable("COMPOSE_PROJECT_NAME") } else { "synapass" }
$Volumes = @("postgres-data", "redis-data", "clickhouse-data", "clickhouse-logs", "nats-data", "prometheus-data", "loki-data", "grafana-data")

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { Fail "prerequisite checks" "docker not found." }
$PgId = (docker compose ps -q postgres) | Where-Object { $_ -ne "" }
if (-not $PgId) { Fail "prerequisite checks" "postgres container is not running; start the stack first." }
New-Item -ItemType Directory -Force -Path $Dest | Out-Null
$DestAbs = (Resolve-Path -LiteralPath $Dest).Path

$PgUser = Get-DotEnv "POSTGRES_USER" "synapass"
$PgDb = Get-DotEnv "POSTGRES_DB" "synapass"
$ChDb = Get-DotEnv "CLICKHOUSE_DB" "synapass"
$ChUser = Get-DotEnv "CLICKHOUSE_USER" "synapass"
$ChPass = Get-DotEnv "CLICKHOUSE_PASSWORD" "synapass"

# -- postgres (dump inside the container, then copy out: avoids PowerShell
# text-encoding redirection corrupting the dump) --
Write-Host "  dumping postgres ($PgDb)..."
$PgId = ((docker compose ps -q postgres) | Where-Object { $_ -ne "" } | Select-Object -First 1).Trim()
docker compose exec -T postgres sh -c "pg_dump -U $PgUser -d $PgDb | gzip > /tmp/synapass-dump.sql.gz"
if ($LASTEXITCODE -ne 0) { Fail "postgres pg_dump" "pg_dump failed." }
docker cp "${PgId}:/tmp/synapass-dump.sql.gz" (Join-Path $Dest "postgres.sql.gz")
docker compose exec -T postgres rm -f /tmp/synapass-dump.sql.gz | Out-Null
Write-Host "  OK  postgres.sql.gz"

# -- redis --
Write-Host "  triggering redis BGSAVE..."
$Before = docker compose exec -T redis redis-cli LASTSAVE
docker compose exec -T redis redis-cli BGSAVE | Out-Null
for ($i = 0; $i -lt 30; $i++) {
  Start-Sleep -Seconds 1
  $After = docker compose exec -T redis redis-cli LASTSAVE
  if ($After -ne $Before) { break }
}
$RedisId = ((docker compose ps -q redis) | Where-Object { $_ -ne "" } | Select-Object -First 1).Trim()
docker cp "${RedisId}:/data/dump.rdb" (Join-Path $Dest "redis-dump.rdb")
if ($LASTEXITCODE -ne 0) { Fail "redis snapshot" "no dump.rdb in the redis container. The volume tar below still covers /data." }
Write-Host "  OK  redis-dump.rdb"

# -- clickhouse (native backup; volume tar is the fallback) --
Write-Host "  backing up clickhouse database ($ChDb)..."
docker compose exec -T clickhouse clickhouse-client --user $ChUser --password $ChPass --query "BACKUP DATABASE $ChDb TO File('/tmp/synapass-backup.zip')" | Out-Null
if ($LASTEXITCODE -eq 0) {
  $ChId = ((docker compose ps -q clickhouse) | Where-Object { $_ -ne "" } | Select-Object -First 1).Trim()
  docker cp "${ChId}:/tmp/synapass-backup.zip" (Join-Path $Dest "clickhouse-backup.zip")
  docker compose exec -T clickhouse rm -f /tmp/synapass-backup.zip | Out-Null
  Write-Host "  OK  clickhouse-backup.zip"
} else {
  Write-Host "  WARN native BACKUP failed; the clickhouse-data volume tar below is the restore path."
}

# -- named volumes --
foreach ($vol in $Volumes) {
  docker volume inspect "$Project-$vol" 2>$null | Out-Null
  if ($LASTEXITCODE -eq 0) {
    docker run --rm -v "${Project}-${vol}:/v:ro" -v "${DestAbs}:/b" alpine tar czf "/b/vol-$vol.tgz" -C /v .
    if ($LASTEXITCODE -ne 0) { Fail "volume archives" "failed to archive $Project-$vol." }
    Write-Host "  OK  vol-$vol.tgz"
  } else {
    Write-Host "  volume $Project-$vol does not exist; skipping."
  }
}

# -- manifest --
$Manifest = Join-Path $Dest "backup-manifest.txt"
$Rev = try { (git rev-parse --short HEAD) } catch { "unknown" }
@(
  "synapass backup $TS"
  "commit: $Rev"
  "project: $Project"
  "volumes:"
  (docker volume ls --filter "label=com.docker.compose.project=$Project" 2>$null)
  "files:"
  (Get-ChildItem -LiteralPath $Dest | ForEach-Object { "$($_.Name)  $($_.Length) bytes" })
) | Set-Content -LiteralPath $Manifest

Write-Host ""
Write-Host "Backup complete: $Dest"
Write-Host "  Restore: .\scripts\restore.ps1 $Dest  (see documentation/backup.md)"
