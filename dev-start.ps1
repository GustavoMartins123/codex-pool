$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

$exe = Join-Path $root "codex-pool-dev.exe"
go build -o $exe .
if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }

# Detached window: the server outlives this shell and does not hold its stdout.
$p = Start-Process -FilePath $exe -WorkingDirectory $root -WindowStyle Hidden -PassThru
"STARTED PID=$($p.Id)"
