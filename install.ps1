# amem installer for Windows: irm https://raw.githubusercontent.com/xiaoxin031/amem/main/install.ps1 | iex
$ErrorActionPreference = 'Stop'
$repo = 'xiaoxin031/amem'
$dir = if ($env:AMEM_BIN) { $env:AMEM_BIN } else { Join-Path $HOME '.local\bin' }
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = Join-Path $dir 'amem.exe'
$src = if ($PSScriptRoot) { $PSScriptRoot } else { $null }

if ($src -and (Test-Path (Join-Path $src 'go.mod')) -and (Get-Command go -ErrorAction SilentlyContinue)) {
  Write-Host "building from $src"
  Push-Location $src
  try { $env:CGO_ENABLED = '0'; go build -trimpath -ldflags='-s -w' -o $exe . } finally { Pop-Location }
} else {
  $arch = if ([Environment]::Is64BitOperatingSystem -and $env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
  $url = "https://github.com/$repo/releases/latest/download/amem_windows_$arch.exe"
  Write-Host "downloading $url"
  Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $exe
}
Write-Host "installed $exe"

& $exe install
if ($LASTEXITCODE -ne 0) { Write-Host "Claude Code / Codex not found yet: run 'amem install' after installing one." }

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($userPath -split ';') -contains $dir)) {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
  Write-Host "added $dir to your PATH (open a new terminal)"
}
