param([switch]$Monitoring)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
Push-Location $root
try {
    $composeArgs = @()
    $services = @('codex-pool')
    if ($Monitoring) {
        $composeArgs = @('-f', 'docker-compose.yml', '-f', 'compose.monitoring.yml')
        $services += @('prometheus', 'grafana')
    }
    & docker compose @composeArgs build codex-pool
    if ($LASTEXITCODE -ne 0) { throw "docker compose build failed with exit code $LASTEXITCODE" }
    & docker compose @composeArgs up -d @services
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}
