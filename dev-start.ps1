$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

# Everything stays inside the repo: pool/ for provider accounts, data/ for the
# Passport and usage stores. No temp directories.
$env:POOL_DIR = "$root\pool"
$env:PROXY_DB_PATH = "$root\data\proxy.db"
$env:PROXY_LISTEN_ADDR = "127.0.0.1:8989"

$secretsPath = Join-Path $root '.dev-secrets.ps1'
if (-not (Test-Path -LiteralPath $secretsPath -PathType Leaf)) {
    throw "Missing $secretsPath. Copy .dev-secrets.ps1.example and set fresh secrets."
}
. $secretsPath
foreach ($name in @('POOL_JWT_SECRET', 'POOL_AUTH_ENCRYPTION_KEY', 'ADMIN_TOKEN')) {
    if (-not [Environment]::GetEnvironmentVariable($name, 'Process')) {
        throw "Missing $name in $secretsPath"
    }
}
$env:DEBUG = "1"

New-Item -ItemType Directory -Force -Path "$root\pool" | Out-Null
New-Item -ItemType Directory -Force -Path "$root\data" | Out-Null

$exe = Join-Path $root "codex-pool-dev.exe"
if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) {
    go build -o $exe .
}

# Detached window: the server outlives this shell and does not hold its stdout.
$p = Start-Process -FilePath $exe -WorkingDirectory $root -WindowStyle Hidden -PassThru
"STARTED PID=$($p.Id)"
