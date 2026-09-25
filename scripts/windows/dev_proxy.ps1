$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not $env:PROXY_DEBUG) { $env:PROXY_DEBUG = '1' }
if (-not $env:POOL_DIR) { $env:POOL_DIR = '.\pool' }
if (-not $env:PROXY_LISTEN_ADDR) { $env:PROXY_LISTEN_ADDR = '127.0.0.1:8989' }

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
Push-Location $root
try {
    & go run .
    if ($LASTEXITCODE -ne 0) { throw "go run failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}
