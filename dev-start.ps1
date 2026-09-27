$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

# Everything stays inside the repo: pool/ for provider accounts, data/ for the
# Passport and usage stores. No temp directories.
$env:POOL_DIR = "$root\pool"
$env:PROXY_DB_PATH = "$root\data\proxy.db"
$env:PROXY_LISTEN_ADDR = "127.0.0.1:8989"

# dev-preview secrets - replace before exposing this beyond localhost
$env:POOL_JWT_SECRET = "<REDACTED_LOCAL_SECRET>"
$env:POOL_AUTH_ENCRYPTION_KEY = "<REDACTED_LOCAL_SECRET>"
# The setup screen sends X-Admin-Token: <REDACTED_LOCAL_SECRET>, so the configured admin
# token has to be that value for the stock bootstrap path to authorize.
$env:ADMIN_TOKEN = "<REDACTED_LOCAL_SECRET>"
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
