# Adversarial smoke test for the built Windows executable.
# Runs codex-pool.exe from a throwaway directory, probes the health endpoint,
# verifies storage files are created, then terminates the process it started.
#
# Safety rules (this script must never touch unrelated processes):
#   - only the exact $proc object started below is ever signaled;
#   - no Get-Process/Stop-Process by name;
#   - the child inherits the console (no redirected pipes that an undrained
#     buffer could block on);
#   - the temporary run directory is always cleaned up.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$exe = Join-Path $PSScriptRoot '..\..\dist\windows-amd64\codex-pool.exe' | Resolve-Path
$runDir = Join-Path $env:TEMP ("codex-pool-smoke-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $runDir | Out-Null

# Pick a free dynamic port instead of a fixed one: a fixed port collides
# with any real codex-pool already running on the machine.
$listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$listener.Start()
$port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
$listener.Stop()

# Ephemeral vault key for the throwaway run (the vault is mandatory).
$bytes = New-Object byte[] 32
[System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
$vaultKey = ($bytes | ForEach-Object { $_.ToString('x2') }) -join ''

$envs = @{
    'PROXY_LISTEN_ADDR' = "127.0.0.1:$port"
    'POOL_DIR'          = 'pool'
    'PROXY_DB_PATH'     = 'data/proxy.db'
    'DUCKDB_PATH'       = 'data/usage.duckdb'
    'PROXY_DEBUG'       = '1'
    'POOL_CREDENTIAL_KEY' = $vaultKey
}

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $exe.Path
$psi.WorkingDirectory = $runDir
$psi.UseShellExecute = $false
$psi.RedirectStandardOutput = $false
$psi.RedirectStandardError = $false
foreach ($kv in $envs.GetEnumerator()) { $psi.EnvironmentVariables[$kv.Key] = $kv.Value }

$proc = [System.Diagnostics.Process]::Start($psi)
$finished = $false
try {
    $healthy = $false
    $deadline = (Get-Date).AddSeconds(25)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 500
        if ($proc.HasExited) { break }
        try {
            $resp = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/livez" -TimeoutSec 2
            if ($resp.StatusCode -eq 200) { $healthy = $true; break }
        } catch [System.Net.WebException] { }
    }
    if (-not $healthy) {
        Write-Output "SMOKE-FAIL (BUG-AUDIT-108): server did not become healthy in a fresh directory (exited=$($proc.HasExited))"
        if ($proc.HasExited) { Write-Output "exit code: $($proc.ExitCode)" }
        exit 1
    }
    Write-Output "SMOKE-OK: /livez returned 200 on dynamic port $port"

    $bolt = Test-Path (Join-Path $runDir 'data\proxy.db')
    Write-Output ("SMOKE-BOLT: data\proxy.db present = {0}" -f $bolt)
    if (-not $bolt) {
        Write-Output 'SMOKE-FAIL: usage store file not created'
        exit 1
    }
    $duck = Test-Path (Join-Path $runDir 'data\usage.duckdb')
    Write-Output ("SMOKE-DUCK: data\usage.duckdb present = {0}" -f $duck)
    if (-not $duck) { Write-Output 'SMOKE-WARN: duckdb file not created' }
    $finished = $true
} finally {
    # Kill ONLY the process this script started, by its own object. Never
    # enumerate or stop processes by name.
    if ($null -ne $proc -and -not $proc.HasExited) {
        $proc.Kill()
        $null = $proc.WaitForExit(10000)
    }
    if ($null -ne $proc) {
        if ($proc.HasExited) {
            Write-Output ("SMOKE-EXIT: own process pid={0} terminated (exit={1})" -f $proc.Id, $proc.ExitCode)
        } else {
            Write-Output ("SMOKE-EXIT: own process pid={0} did not exit after kill" -f $proc.Id)
        }
    }
    Start-Sleep -Milliseconds 300
    Remove-Item -Recurse -Force -LiteralPath $runDir -ErrorAction SilentlyContinue
    if (-not $finished) { exit 1 }
}
