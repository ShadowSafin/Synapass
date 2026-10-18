<#
.SYNOPSIS
  Builds the Synapass desktop executables into desktop/dist/.

.DESCRIPTION
  Produces TWO separate programs, as different products:

    dist/Synapass.exe       the standalone app (runtime only)
    dist/SynapassSetup.exe  the setup-only installer

  The installer embeds the standalone app plus its runtime (gateway, portable
  Node, built dashboard, templates) and installs them. The app embeds no
  installer: it only runs what is installed.

  Docker files are never touched; this only reads the dashboard source and the
  main Go module.

  Requirements: Go 1.24+, Node 20+ with npm, network access.
#>
[CmdletBinding()]
param(
  [string]$Dist = (Join-Path $PSScriptRoot "dist"),
  [string]$NodeMajor = "22"
)
if (-not $PSScriptRoot) {
  $RepoPath =Split-Path $MyInvocation.MyCommand.Definition -Parent
  if ($RepoPath -and (Test-Path (Join-Path $RepoPath "build.ps1")) ) {
    $PSScriptRoot = $RepoPath
    $Dist = (Join-Path $PSScriptRoot "dist")
  }
}

$ErrorActionPreference = "Stop"
$Desktop = $PSScriptRoot
$Repo = Split-Path $Desktop -Parent
$Payload = Join-Path $Desktop "internal\payload\files"
$Go = "C:\Program Files\Go\bin\go.exe"
if (-not (Test-Path $Go)) { $Go = "go" }

function Step($msg) { Write-Host "`n=== $msg === " -ForegroundColor Cyan }

# ---------------------------------------------------------------------------
# Version resource
#
# Without a VERSIONINFO block both exes report an empty Product name, empty
# Company and no description, so Explorer, Task Manager and Add/Remove Programs
# all show a blank publisher. That reads as an unsigned mystery binary, which is
# the opposite of the point of shipping a branded installer.
#
# The resource is generated here rather than checked in because every field that
# changes per build (the version) would otherwise be stale in the repository.
# ---------------------------------------------------------------------------

# The human-readable build identity, shared with VERSION.txt and the payload.
$stamp = (Get-Date -Format o)
try { $stamp += "_" + (git -C $Repo rev-parse --short HEAD 2>$null) } catch {}

# VERSIONINFO needs numeric components, so a tag is used when there is one and a
# date-derived build number otherwise. An unstamped local build is deliberately
# 0.0.0.<date> rather than a guessed release number.
# 
# For this build the requested desktop version is V1.0.0, so the version fields
# are fixed instead of derived from git tags or from the build date.
$major = 1; $minor = 0; $patch = 0
$build = [int](Get-Date -Format "yyyyMMdd")
$fileVersion = "$major.$minor.$patch.$build"

function New-VersionResource($outSyso, $description, $originalName) {
  $icon = (Join-Path $Desktop "assets\icon.ico")
  $cfg = @{
    FixedFileInfo = @{
      FileVersion    = @{ Major = $major; Minor = $minor; Patch = $patch; Build = $build }
      ProductVersion = @{ Major = $major; Minor = $minor; Patch = $patch; Build = $build }
      FileFlagsMask  = "3f"
      FileFlags      = "00"
      FileOS         = "040004"
      FileType       = "01"
      FileSubType    = "00"
    }
    StringFileInfo = @{
      CompanyName      = "Synapass"
      ProductName      = "Synapass"
      FileDescription  = $description
      InternalName     = "Synapass"
      OriginalFilename = $originalName
      LegalCopyright   = "Apache-2.0"
      FileVersion      = $fileVersion
      ProductVersion   = $fileVersion
      Comments         = ""
    }
    VarFileInfo = @{ Translation = @{ LangID = "0409"; CharsetID = "04B0" } }
    IconPath   = $icon
  }
  $cfgPath = [IO.Path]::ChangeExtension($outSyso, ".versioninfo.json")
  # Written without a BOM: Windows PowerShell 5.1's `Set-Content -Encoding UTF8`
  # emits U+FEFF, which goversioninfo's JSON parser rejects outright.
  $json = $cfg | ConvertTo-Json -Depth 6
  [IO.File]::WriteAllText($cfgPath, $json, (New-Object System.Text.UTF8Encoding($false)))
  try {
    & $Go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -o $outSyso $cfgPath
    if ($LASTEXITCODE -ne 0) { throw "goversioninfo failed for $originalName" }
  } finally {
    Remove-Item $cfgPath -Force -ErrorAction SilentlyContinue
  }
}

Step "version resource + icon (app, installer)"
New-VersionResource (Join-Path $Desktop "cmd\app\rsrc.syso") "Synapass" "Synapass.exe"
New-VersionResource (Join-Path $Desktop "cmd\installer\rsrc.syso") "Synapass Setup" "SynapassSetup.exe"
Write-Host "version $fileVersion"

$Bin = Join-Path ([IO.Path]::GetTempPath()) "synapass-build"
if (Test-Path $Bin) { Remove-Item $Bin -Recurse -Force }
New-Item -ItemType Directory $Bin -Force | Out-Null
# NOTE: the app and gateway MUST stage in separate directories. "Synapass.exe"
# (app) and "synapass.exe" (gateway) are the same filename on Windows'
# case-insensitive filesystem, so building both into $Bin flat makes the gateway
# silently overwrite the app and the installer ships the gateway as the app
# (double-click then flashes a console and exits).
$BinApp = Join-Path $Bin "app-stage"
$BinGw = Join-Path $Bin "gw-stage"
New-Item -ItemType Directory $BinApp, $BinGw -Force | Out-Null

Step "standalone app exe (runtime, no payload)"
if (Test-Path $Dist) {
  try {
    Remove-Item $Dist -Recurse -Force -ErrorAction Stop
  } catch {
    # A stray directory handle (Explorer, AV scan) can block removing the
    # folder itself; empty it instead and carry on.
    Get-ChildItem $Dist -Force -ErrorAction Stop | Remove-Item -Recurse -Force -ErrorAction Stop
  }
}
New-Item -ItemType Directory $Dist -Force | Out-Null
Push-Location $Desktop
try {
  & $Go build ./...
  if ($LASTEXITCODE -ne 0) { throw "desktop go build failed" }
  & $Go build -trimpath -ldflags "-H windowsgui" -o (Join-Path $BinApp "Synapass.exe") ./cmd/app
  if ($LASTEXITCODE -ne 0) { throw "app build failed" }
  Copy-Item (Join-Path $BinApp "Synapass.exe") (Join-Path $Dist "Synapass.exe") -Force
} finally { Pop-Location }

Step "gateway binary"
Push-Location $Repo
try {
  & $Go build -trimpath -o (Join-Path $BinGw "synapass.exe") ./cmd/synapass
  if ($LASTEXITCODE -ne 0) { throw "gateway build failed" }
} finally { Pop-Location }

Step "dashboard build"
Push-Location (Join-Path $Repo "dashboard")
try {
  if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw "npm not found (install Node.js 20+)" }
  npm ci --no-audit --no-fund
  if ($LASTEXITCODE -ne 0) { throw "npm ci failed" }
  npm run build
  if ($LASTEXITCODE -ne 0) { throw "dashboard build failed" }
} finally { Pop-Location }

Step "portable node runtime"
$index = Invoke-RestMethod "https://nodejs.org/dist/index.json"
$ver = ($index | Where-Object { $_.version -like "v$NodeMajor.*" } | Select-Object -First 1).version
if (-not $ver) { throw "no node v$NodeMajor found on nodejs.org" }
Write-Host "node $ver"
$zip = Join-Path ([IO.Path]::GetTempPath()) "node-$ver-win-x64.zip"
Invoke-WebRequest "https://nodejs.org/dist/$ver/node-$ver-win-x64.zip" -OutFile $zip
$unzip = Join-Path ([IO.Path]::GetTempPath()) "node-unzip"
if (Test-Path $unzip) { Remove-Item $unzip -Recurse -Force }
Expand-Archive $zip $unzip -Force
$NodeExe = Join-Path $unzip "node-$ver-win-x64\node.exe"
Remove-Item $zip -Force

Step "stage installer payload"
if (Test-Path $Payload) { Get-ChildItem $Payload -Force -Exclude ".keep" | Remove-Item -Recurse -Force }
New-Item -ItemType Directory $Payload -Force | Out-Null
$pBin = Join-Path $Payload "bin"
$pStandalone = Join-Path $Payload "dashboard\.next\standalone"
New-Item -ItemType Directory $pBin -Force | Out-Null
New-Item -ItemType Directory $pStandalone -Force | Out-Null
Copy-Item (Join-Path $BinGw "synapass.exe") (Join-Path $pBin "synapass.exe") -Force
Copy-Item $NodeExe (Join-Path $pBin "node.exe") -Force
# The standalone app is what the installer installs.
Copy-Item (Join-Path $BinApp "Synapass.exe") (Join-Path $Payload "Synapass.exe") -Force
# Guard against the case-collision regression above: the staged app must never
# be the gateway binary under a different name.
$appHash = (Get-FileHash (Join-Path $Payload "Synapass.exe")).Hash
$gwHash = (Get-FileHash (Join-Path $pBin "synapass.exe")).Hash
if ($appHash -eq $gwHash) { throw "payload app and gateway are byte-identical; staging collision (see NOTE above)" }
# Next resolves static assets relative to server.js, so they live INSIDE it.
Copy-Item (Join-Path $Repo "dashboard\.next\standalone\*") $pStandalone -Recurse -Force
if (-not (Test-Path (Join-Path $pStandalone "server.js"))) { throw "standalone server.js missing after build" }
$pStatic = Join-Path $pStandalone ".next\static"
New-Item -ItemType Directory $pStatic -Force | Out-Null
Copy-Item (Join-Path $Repo "dashboard\.next\static\*") $pStatic -Recurse -Force
if (Test-Path (Join-Path $Repo "dashboard\public")) {
  $pPublic = Join-Path $pStandalone "public"
  New-Item -ItemType Directory $pPublic -Force | Out-Null
  Copy-Item (Join-Path $Repo "dashboard\public\*") $pPublic -Recurse -Force
}
Copy-Item (Join-Path $Desktop "assets") (Join-Path $Payload "assets") -Recurse -Force
Copy-Item (Join-Path $Desktop "templates") (Join-Path $Payload "templates") -Recurse -Force
Remove-Item $NodeExe -ErrorAction SilentlyContinue
Remove-Item $unzip -Recurse -Force -ErrorAction SilentlyContinue

Step "setup-only installer exe"
# NOTE: joined with "_" (no spaces) because a space inside the -X value breaks
# go's argv parsing when this script runs under Windows PowerShell 5.1, which
# does not re-quote embedded quotes for native commands (the hash would be
# treated as a package path: "package <hash> is not in std").
# $stamp is computed once, above, so the version resource and the payload agree.
Push-Location $Desktop
try {
  $vetted = "github.com/shadowsafin/synapass/desktop/internal/payload.version=$stamp"
  & $Go build -trimpath -ldflags "-H windowsgui -X `"$vetted`"" -o (Join-Path $Dist "SynapassSetup.exe") ./cmd/installer
  if ($LASTEXITCODE -ne 0) { throw "installer build failed" }
} finally { Pop-Location }

Step "package extras"
Copy-Item (Join-Path $Desktop "README.md") (Join-Path $Dist "README.md") -Force
"desktop built $stamp" | Set-Content (Join-Path $Dist "VERSION.txt")
Remove-Item $Bin -Recurse -Force -ErrorAction SilentlyContinue

Write-Host "`nbuild ready: $Dist" -ForegroundColor Green
Get-ChildItem $Dist -File | ForEach-Object { Write-Host ("  {0}  {1:N1} MB" -f $_.Name, ($_.Length / 1MB)) }
