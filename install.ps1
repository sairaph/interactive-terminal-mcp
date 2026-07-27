$ErrorActionPreference = "Stop"
$owner = "sairaph"
$repo = "interactive-terminal-mcp"
$binary = "interactive-terminal-mcp"

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

$installDir = Join-Path $env:LOCALAPPDATA $repo
$target = Join-Path $installDir "$binary.exe"
$temporary = "$target.new"
$asset = "$binary-windows-$arch.exe"
$url = "https://github.com/$owner/$repo/releases/latest/download/$asset"

New-Item -ItemType Directory -Force -Path $installDir | Out-Null
Write-Host "Downloading $asset..."
Invoke-WebRequest -Uri $url -OutFile $temporary

# A running daemon holds this executable open, so stop it before replacing.
if (Test-Path $target) {
  try { & $target daemon --stop 2>$null | Out-Null } catch {}
  Start-Sleep -Milliseconds 300
}
Move-Item -Force $temporary $target

$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if (($userPath -split ";") -notcontains $installDir) {
  $nextPath = if ($userPath) { "$installDir;$userPath" } else { $installDir }
  [Environment]::SetEnvironmentVariable("PATH", $nextPath, "User")
  Write-Host "Added $installDir to your user PATH"
}

Write-Host "Installed $target"
try {
  & $target configure
} catch {
  Write-Warning "Run '$binary configure' later to choose your AI clients: $_"
}

Write-Host ""
Write-Host "Open a new terminal, then run '$binary' to browse your sessions."
