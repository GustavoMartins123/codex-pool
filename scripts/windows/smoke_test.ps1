# Adversarial smoke test for the built Windows executable.
# Runs codex-pool.exe from a throwaway directory, probes health endpoints,
# verifies storage files are created, then terminates the process and
# confirms no orphan remains. Test-scoped infrastructure only.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$exe = Join-Path $PSScriptRoot '..\..\dist\windows-amd64\codex-pool.exe' | Resolve-Path
$runDir = Join-Path $env:TEMP ("codex-pool-smoke-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $runDir | Out-Null

$port = 18923
$envs = @{
    'PROXY_LISTEN_ADDR' = "127.0.0.1:$port"
    'POOL_DIR'          = 'pool'
    'DATA_DIR'          = 'data'
    'PROXY_DEBUG'       = '1'
}

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $exe.Path
$psi.WorkingDirectory = $runDir
$psi.UseShellExecute = $false
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
foreach ($kv in $envs.GetEnumerator()) { $psi.EnvironmentVariables[$kv.Key] = $kv.Value }

$proc = [System.Diagnostics.Process]::Start($psi)
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
    Write-Output "SMOKE-OK: /livez returned 200"

    $bolt = Test-Path (Join-Path $runDir 'data\proxy.db')
    Write-Output ("SMOKE-BOLT: data\proxy.db present = {0}" -f $bolt)
    $duck = Test-Path (Join-Path $runDir 'data\usage.duckdb')
    Write-Output ("SMOKE-DUCK: data\usage.duckdb present = {0}" -f $duck)
    if (-not $duck) { Write-Output 'SMOKE-WARN: duckdb file not created' }
} finally {
    if (-not $proc.HasExited) {
        $proc.Kill()
        $proc.WaitForExit(10000) | Out-Null
    }
    Start-Sleep -Milliseconds 800
    $orphan = Get-Process -Name codex-pool -ErrorAction SilentlyContinue
    if ($orphan) {
        Write-Output "SMOKE-FAIL: orphan process(es) remain"
        $orphan | Stop-Process -Force
        exit 1
    }
    Write-Output "SMOKE-EXIT: process terminated, no orphans"
    Remove-Item -Recurse -Force -LiteralPath $runDir -ErrorAction SilentlyContinue
}
