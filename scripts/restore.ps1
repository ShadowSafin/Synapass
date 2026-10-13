# Synapass restore (Windows PowerShell).
#
#   .\scripts\restore.ps1 <backup-dir> [all|postgres|redis|clickhouse|volumes]
#
# Same behaviour as scripts/restore.sh. The stack must be RUNNING for
# postgres/redis/clickhouse restores; `volumes` stops it first. Postgres
# restores into an EMPTY database (reset with .\scripts\deploy.ps1 reset -Yes
# first). See documentation/backup.md.
param(
  [string]$Src = "",
  [string]$What = "all"
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent (Split-Path -Parent $PSCommandPath)
Set-Location -LiteralPath $RepoRoot

function Fail([string]$Step, [string]$Message) {
  Write-Host ""
  Write-Host "RESTORE FAILED during: $Step"
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
  return $Default
}

if ([string]::IsNullOrEmpty($Src)) { Fail "setup" "usage: .\scripts\restore.ps1 <backup-dir> [all|postgres|redis|clickhouse|volumes]" }
if (-not (Test-Path -LiteralPath $Src)) { Fail "setup" "backup dir not found: $Src" }
$SrcAbs = (Resolve-Path -LiteralPath $Src).Path
$Project = if ([Environment]::GetEnvironmentVariable("COMPOSE_PROJECT_NAME")) { [Environment]::GetEnvironmentVariable("COMPOSE_PROJECT_NAME") } else { "synapass" }

$PgUser = Get-DotEnv "POSTGRES_USER" "synapass"
$PgDb = Get-DotEnv "POSTGRES_DB" "synapass"
$ChDb = Get-DotEnv "CLICKHOUSE_DB" "synapass"
$ChUser = Get-DotEnv "CLICKHOUSE_USER" "synapass"
$ChPass = Get-DotEnv "CLICKHOUSE_PASSWORD" "synapass"

function Restore-Postgres {
  if (-not (Test-Path -LiteralPath (Join-Path $Src "postgres.sql.gz"))) { Fail "postgres restore" "postgres.sql.gz missing in $Src" }
  $PgId = (docker compose ps -q postgres) | Where-Object { $_ -ne "" }
  if (-not $PgId) { Fail "postgres restore" "postgres is not running; start the stack first." }
  Write-Host "  restoring postgres ($PgDb) -- target must be empty (deploy.ps1 reset -Yes first)."
  # Copy in, then pipe inside the container: avoids host text-encoding issues.
  $PgId = ((docker compose ps -q postgres) | Where-Object { $_ -ne "" } | Select-Object -First 1).Trim()
  docker cp (Join-Path $SrcAbs "postgres.sql.gz") "${PgId}:/tmp/synapass-restore.sql.gz"
  docker compose exec -T postgres sh -c "gunzip -c /tmp/synapass-restore.sql.gz | psql -U $PgUser -d $PgDb -q -v ON_ERROR_STOP=1"
  if ($LASTEXITCODE -ne 0) { Fail "postgres restore" "psql failed." }
  docker compose exec -T postgres rm -f /tmp/synapass-restore.sql.gz | Out-Null
  Write-Host "  OK  postgres restored"
}

function Restore-Redis {
  if (-not (Test-Path -LiteralPath (Join-Path $Src "redis-dump.rdb"))) { Fail "redis restore" "redis-dump.rdb missing in $Src" }
  docker compose stop redis | Out-Null
  $RedisId = ((docker compose ps -aq redis) | Where-Object { $_ -ne "" } | Select-Object -First 1).Trim()
  docker cp (Join-Path $SrcAbs "redis-dump.rdb") "${RedisId}:/data/dump.rdb"
  if ($LASTEXITCODE -ne 0) { Fail "redis restore" "docker cp failed." }
  docker compose start redis | Out-Null
  Write-Host "  OK  redis restored (restarted with the snapshot)"
}

function Restore-ClickHouse {
  if (-not (Test-Path -LiteralPath (Join-Path $Src "clickhouse-backup.zip"))) { Fail "clickhouse restore" "clickhouse-backup.zip missing in $Src (fall back to: restore.ps1 $Src volumes)" }
  $ChId = (docker compose ps -q clickhouse) | Where-Object { $_ -ne "" }
  if (-not $ChId) { Fail "clickhouse restore" "clickhouse is not running; start the stack first." }
  $ChId = ($ChId | Select-Object -First 1).Trim()
  docker cp (Join-Path $SrcAbs "clickhouse-backup.zip") "${ChId}:/tmp/synapass-restore.zip"
  docker compose exec -T clickhouse clickhouse-client --user $ChUser --password $ChPass --query "RESTORE DATABASE $ChDb FROM File('/tmp/synapass-restore.zip')"
  if ($LASTEXITCODE -ne 0) { Fail "clickhouse restore" "RESTORE failed." }
  docker compose exec -T clickhouse rm -f /tmp/synapass-restore.zip | Out-Null
  Write-Host "  OK  clickhouse restored"
}

function Restore-Volumes {
  Write-Host "  stopping the stack (volumes cannot be restored under live containers)..."
  docker compose down | Out-Null
  $Archives = Get-ChildItem -LiteralPath $SrcAbs -Filter "vol-*.tgz"
  if (-not $Archives) { Fail "volume restore" "no vol-*.tgz archives in $Src" }
  foreach ($a in $Archives) {
    $vol = "$Project-" + ($a.BaseName -replace "^vol-", "")
    docker volume inspect $vol 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { docker volume create $vol | Out-Null }
    docker run --rm -v "${vol}:/v" -v "${SrcAbs}:/b:ro" alpine sh -c "rm -rf /v/* 2>/dev/null; tar xzf /b/$($a.Name) -C /v"
    if ($LASTEXITCODE -ne 0) { Fail "volume restore" "failed to restore $vol." }
    Write-Host "  OK  $vol restored"
  }
  Write-Host "  start the stack again with: docker compose up -d"
}

switch ($What) {
  "all" { Restore-Postgres; Restore-Redis; Restore-ClickHouse; Write-Host "  volume tars left untouched (use '$PSCommandPath $Src volumes' for a full volume-level restore on a stopped stack)." }
  "postgres" { Restore-Postgres }
  "redis" { Restore-Redis }
  "clickhouse" { Restore-ClickHouse }
  "volumes" { Restore-Volumes }
  default { Fail "setup" "unknown target '$What' (want all|postgres|redis|clickhouse|volumes)." }
}

Write-Host ""
Write-Host "Restore ($What) complete from $Src"
