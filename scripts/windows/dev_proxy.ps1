$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path

if (-not $env:PROXY_DEBUG) { $env:PROXY_DEBUG = '1' }
if (-not $env:POOL_DIR) { $env:POOL_DIR = '.\pool' }
if (-not $env:PROXY_LISTEN_ADDR) { $env:PROXY_LISTEN_ADDR = '127.0.0.1:8989' }

# The vault is mandatory: default to a stable per-workspace dev key so local
# pool files stay decryptable across restarts. Delete .dev-vault-key to
# rotate (existing files then need POOL_CREDENTIAL_KEY_PREVIOUS).
if (-not $env:POOL_CREDENTIAL_KEY) {
    $keyFile = Join-Path $root '.dev-vault-key'
    if (-not (Test-Path $keyFile) -or (Get-Item $keyFile).Length -eq 0) {
        $bytes = New-Object byte[] 32
        [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
        ($bytes | ForEach-Object { $_.ToString('x2') }) -join '' | Set-Content -NoNewline -Path $keyFile
        Write-Output "generated dev vault key: $keyFile"
    }
    $env:POOL_CREDENTIAL_KEY = (Get-Content -Raw $keyFile).Trim()
}

Push-Location $root
try {
    & go run .
    if ($LASTEXITCODE -ne 0) { throw "go run failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}
