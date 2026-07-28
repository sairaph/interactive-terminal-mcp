# interactive-terminal-mcp installer (Windows). Run in PowerShell:
#   irm https://github.com/sairaph/interactive-terminal-mcp/raw/main/install.ps1 | iex
$ErrorActionPreference = 'Stop'
# Invoke-WebRequest's built-in progress bar is slow and flickers badly in
# Windows PowerShell; the download below renders its own.
$ProgressPreference    = 'SilentlyContinue'

$Owner  = 'sairaph'
$Repo   = 'interactive-terminal-mcp'
$Binary = 'interactive-terminal-mcp'

# --- detect arch -----------------------------------------------------------
$arch = $env:PROCESSOR_ARCHITECTURE
switch ($arch) {
  { $_ -in 'AMD64','x64' } { $target = 'windows-amd64' }
  'ARM64'                  { $target = 'windows-arm64' }
  default                  { Write-Host "  Unsupported architecture: $arch" -ForegroundColor Red; return }
}

$Asset = "$Binary-$target.exe"
$Url   = "https://github.com/$Owner/$Repo/releases/latest/download/$Asset"

# --- install location ------------------------------------------------------
$InstallDir = Join-Path $env:LOCALAPPDATA $Repo
$Target     = Join-Path $InstallDir "$Binary.exe"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Write-Host ""
Write-Host "  interactive-terminal-mcp installer" -ForegroundColor Cyan
Write-Host ""

# A running daemon holds the executable open. Ask it to stop before writing so
# the common upgrade path does not hit a locked file at all.
if (Test-Path $Target) {
  try { & $Target daemon --stop 2>$null | Out-Null } catch {}
  Start-Sleep -Milliseconds 300
}

Write-Host "  Downloading $Asset..."

# If the old binary is still present, clear it so we can write fresh. If it is
# locked (a session is still attached), fall back to a temp file and swap.
$tempTarget = "$Target.new"
$usingTemp  = $false
Remove-Item $Target -Force -ErrorAction SilentlyContinue

$request = $null
$response = $null
$stream = $null
$fs = $null
try {
  $request = [System.Net.HttpWebRequest]::Create($Url)
  $request.Method = "GET"
  $response = $request.GetResponse()
  $total = [int]$response.ContentLength
  $stream = $response.GetResponseStream()
  try {
    $fs = [System.IO.File]::Create($Target)
  } catch {
    $fs = [System.IO.File]::Create($tempTarget)
    $usingTemp = $true
  }
  $buffer = New-Object byte[] 65536
  $downloaded = 0
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  $lastUpdate = 0

  $renderBar = {
    param($dl, $tot, $el)
    $pct = if ($tot -gt 0) { [math]::Round($dl / $tot * 100) } else { 0 }
    if ($pct -gt 100) { $pct = 100 }
    $filled = [math]::Floor($pct / 5)
    if ($filled -gt 20) { $filled = 20 }
    $bar = ('#' * $filled).PadRight(20)
    $dlMB = [math]::Round($dl / 1048576, 1)
    $totMB = [math]::Round($tot / 1048576, 1)
    $speed = if ($el -gt 0) { [math]::Round($dlMB / $el, 1) } else { 0 }
    $eta = if ($speed -gt 0) { [math]::Round((($totMB - $dlMB)) / $speed) } else { 0 }
    $line = ("  [{0}] {1,3}%  {2}/{3} MB  {4} MB/s  ETA {5:00}s" -f $bar, $pct, $dlMB, $totMB, $speed, $eta)
    # Pad so shorter lines fully overwrite the previous render (no stale chars).
    Write-Host ("`r{0}" -f $line.PadRight(72)) -NoNewline
  }

  while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
    $fs.Write($buffer, 0, $read)
    $downloaded += $read
    $now = [System.Environment]::TickCount
    if ($now - $lastUpdate -gt 200) {
      $lastUpdate = $now
      & $renderBar $downloaded $total $sw.Elapsed.TotalSeconds
    }
  }
  # Force a final 100% render.
  & $renderBar $downloaded $total $sw.Elapsed.TotalSeconds
  Write-Host ""
} catch {
  Write-Host ""
  Write-Host "  Download failed: $_" -ForegroundColor Red
  Write-Host "  URL: $Url" -ForegroundColor Red
  Remove-Item $Target -ErrorAction SilentlyContinue
  Remove-Item $tempTarget -ErrorAction SilentlyContinue
  return
} finally {
  if ($fs -ne $null) { $fs.Close() }
  if ($stream -ne $null) { $stream.Close() }
  if ($response -ne $null) { $response.Close() }
}

# If we downloaded to a temp file (the old exe was locked), swap it into place.
if ($usingTemp) {
  Remove-Item $Target -Force -ErrorAction SilentlyContinue
  try {
    Move-Item $tempTarget $Target -Force
  } catch {
    Write-Host ""
    Write-Host "  Could not replace $Binary.exe: $_" -ForegroundColor Red
    Write-Host "  Close any attached session and re-run the installer." -ForegroundColor Yellow
    Remove-Item $tempTarget -ErrorAction SilentlyContinue
    return
  }
}
# Clean up a partial / zero-byte download so a later retry starts fresh.
if (-not (Test-Path $Target) -or (Get-Item $Target).Length -eq 0) {
  Remove-Item $Target -ErrorAction SilentlyContinue
  Write-Host "  Download did not complete; nothing was installed." -ForegroundColor Red
  return
}

# Stop the daemon again, now that the new binary is the one on disk.
#
# The stop before the download releases the locked executable, but it cannot be
# the last word: the download takes seconds, and any tool call an AI client
# makes in that window starts a daemon from the binary that is still there --
# the old one. It would then keep serving old code long after this script
# reported success, which reads as an upgrade that silently did nothing.
#
# Stopping here closes that window. Whatever is running is asked to stop, and
# the next tool call starts the version just installed.
try {
  $stopped = (& $Target daemon --stop 2>$null | Out-String).Trim()
  if ($stopped -and $stopped -notmatch 'No session daemon') {
    Write-Host ""
    Write-Host "  $stopped"
    # The daemon carries session behaviour and restarts on the next tool call,
    # so that part is already current. Tool descriptions come from the MCP
    # server process the AI client spawned, which is still the old binary and
    # will be until the client restarts it.
    Write-Host "  Restart your AI client so it picks up the new tool descriptions." -ForegroundColor Yellow
  }
} catch {}

# --- add to user PATH if missing -------------------------------------------
# SetEnvironmentVariable updates the stored user PATH, which only new processes
# read. This shell keeps the PATH it started with, so the command is not found
# here no matter what we do -- the user has to be told, or the very next thing
# they type fails with "not recognized".
$pathChanged = $false
$userPath = [Environment]::GetEnvironmentVariable('PATH', 'User')
if ($userPath -notlike "*$InstallDir*") {
  $newPath = if ($userPath) { "$InstallDir;$userPath" } else { $InstallDir }
  [Environment]::SetEnvironmentVariable('PATH', $newPath, 'User')
  $pathChanged = $true
}
# Make it work in this session too, so `configure` below and anything the user
# tries immediately afterwards can find the binary without reopening a console.
if ($env:PATH -notlike "*$InstallDir*") {
  $env:PATH = "$InstallDir;$env:PATH"
}

# --- launch the configurer --------------------------------------------------
Write-Host ""
try {
  & $Target configure
} catch {
  Write-Host "  configure did not complete: $_" -ForegroundColor Red
  Write-Host "  Re-run '$Binary configure' later to finish setup." -ForegroundColor Yellow
}

# --- tell the user what to do next ------------------------------------------
Write-Host ""
if ($pathChanged) {
  Write-Host "  Added to your PATH: $InstallDir" -ForegroundColor Cyan
  Write-Host ""
  Write-Host "  Open a NEW PowerShell window before running $Binary." -ForegroundColor Yellow
  Write-Host "  This window still has the PATH it started with, so the command" -ForegroundColor Yellow
  Write-Host "  will not be found here." -ForegroundColor Yellow
  Write-Host ""
  Write-Host "  In a new window:  $Binary"
  Write-Host "  Or right now:     & '$Target'"
} else {
  Write-Host "  Run '$Binary' to browse and use your sessions."
}
