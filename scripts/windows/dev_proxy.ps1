$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path

Push-Location $root
try {
    & go run . @args
    if ($LASTEXITCODE -ne 0) { throw "go run failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}
