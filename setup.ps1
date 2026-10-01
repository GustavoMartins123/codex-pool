param([string]$EnvPath = (Join-Path $PSScriptRoot '.env'), [switch]$Monitoring)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not ('CodexPoolSetupPermissions' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class CodexPoolSetupPermissions {
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool SetFileSecurity(string path, uint information, byte[] descriptor);
    public static void Protect(string path, byte[] descriptor) {
        if (!SetFileSecurity(path, 0x80000004, descriptor))
            throw new Win32Exception(Marshal.GetLastWin32Error());
    }
}
'@
}
$EnvPath = [IO.Path]::GetFullPath($EnvPath)
$lockPath = "$EnvPath.setup-lock"
$tempPath = $null
$lockStream = $null

function Assert-RegularEnv {
    if (Test-Path -LiteralPath $EnvPath) {
        $item = Get-Item -LiteralPath $EnvPath -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'The environment path must be a regular file, without a symlink.'
        }
    }
}

function Protect-EnvFile([string]$Path) {
    $acl = New-Object Security.AccessControl.FileSecurity
    $acl.SetAccessRuleProtection($true, $false)
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $rule = New-Object Security.AccessControl.FileSystemAccessRule($identity, 'FullControl', 'Allow')
    $acl.AddAccessRule($rule)
    [CodexPoolSetupPermissions]::Protect($Path, $acl.GetSecurityDescriptorBinaryForm())
}

try {
    Assert-RegularEnv
    if (-not (Test-Path -LiteralPath ([IO.Path]::GetDirectoryName($EnvPath)) -PathType Container)) {
        throw 'The destination directory does not exist.'
    }
    $lockStream = [IO.File]::Open($lockPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    Protect-EnvFile $lockPath
    $sourcePath = $EnvPath
    if (-not (Test-Path -LiteralPath $sourcePath)) { $sourcePath = Join-Path $PSScriptRoot '.env.example' }
    $utf8 = New-Object Text.UTF8Encoding($false, $true)
    $content = $utf8.GetString([IO.File]::ReadAllBytes($sourcePath)).TrimStart([char]0xFEFF)
    if ($content.Contains([string][char]0)) { throw 'The environment file contains NUL bytes; use UTF-8 text.' }
    $newline = "`n"
    if ($content.Contains("`r`n")) { $newline = "`r`n" }
    $lines = New-Object 'Collections.Generic.List[string]'
    $lines.AddRange([string[]]($content -split '\r?\n'))
    if ($lines.Count -gt 0 -and $lines[$lines.Count - 1] -eq '') { $lines.RemoveAt($lines.Count - 1) }
    $keys = @('ADMIN_TOKEN', 'POOL_AUTH_ENCRYPTION_KEY', 'POOL_JWT_SECRET', 'POOL_CREDENTIAL_KEY')
    if ($Monitoring) { $keys += @('MONITORING_METRICS_TOKEN', 'GRAFANA_ADMIN_PASSWORD') }
    $positions = @{}
    $values = @{}
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -cmatch '^\s*(?:export\s+)?(ADMIN_TOKEN|POOL_AUTH_ENCRYPTION_KEY|POOL_JWT_SECRET|POOL_CREDENTIAL_KEY|MONITORING_METRICS_TOKEN|GRAFANA_ADMIN_PASSWORD)\s*=(.*)$') {
            $key = $Matches[1]
            if ($positions.ContainsKey($key)) { throw "Duplicate $key assignment." }
            $positions[$key] = $i
            $value = $Matches[2].Trim()
            if ($value -match '^"([^"]*)"\s*(?:#.*)?$' -or $value -match '^''([^'']*)''\s*(?:#.*)?$') {
                $value = $Matches[1]
            } else { $value = ($value -replace '\s+#.*$', '').TrimEnd() }
            if ($value.Length -gt 0) {
                if (@('changeme', 'change-me', 'change_me', 'example', 'placeholder', 'replace-me', 'your-secret-here', 'your-key-here') -contains $value) {
                    throw "Invalid placeholder for $key."
                }
                $minimum = 32
                if ($key -eq 'ADMIN_TOKEN') { $minimum = 16 }
                if ($value.Length -lt $minimum -or $value -match '[\x00-\x1f\x7f$<>"''\\]') {
                    throw "Invalid $key; edit it explicitly before rerunning setup."
                }
            }
            $values[$key] = $value
        } elseif ($lines[$i] -cmatch '^\s*(?:export\s+)?(ADMIN_TOKEN|POOL_AUTH_ENCRYPTION_KEY|POOL_JWT_SECRET|POOL_CREDENTIAL_KEY|MONITORING_METRICS_TOKEN|GRAFANA_ADMIN_PASSWORD)(?:[\s:]|$)') {
            throw 'Malformed secret assignment.'
        }
    }
    $generated = 0
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        foreach ($key in $keys) {
            if ($values.ContainsKey($key) -and $values[$key].Length -gt 0) { continue }
            $bytes = New-Object byte[] 32
            $rng.GetBytes($bytes)
            $value = [BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()
            if ($positions.ContainsKey($key)) { $lines[$positions[$key]] = "$key=$value" }
            else { $lines.Add("$key=$value") }
            $generated++
        }
    } finally { $rng.Dispose() }
    if (-not (Test-Path -LiteralPath $EnvPath) -or $generated -gt 0) {
        $tempPath = "$EnvPath.tmp.$([Guid]::NewGuid().ToString('N'))"
        $stream = [IO.File]::Open($tempPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try {
            Protect-EnvFile $tempPath
            $encoded = $utf8.GetBytes(([string]::Join($newline, $lines) + $newline))
            $stream.Write($encoded, 0, $encoded.Length)
            $stream.Flush($true)
        } finally { $stream.Dispose() }
        Assert-RegularEnv
        if (Test-Path -LiteralPath $EnvPath) {
            Protect-EnvFile $EnvPath
            [IO.File]::Replace($tempPath, $EnvPath, [System.Management.Automation.Language.NullString]::Value)
        } else { [IO.File]::Move($tempPath, $EnvPath) }
        $tempPath = $null
    }
    Protect-EnvFile $EnvPath
    Write-Output "Environment ready; generated $generated secret(s)."
} catch {
    [Console]::Error.WriteLine("Setup failed: $($_.Exception.Message)")
    exit 1
} finally {
    if ($tempPath -and (Test-Path -LiteralPath $tempPath)) { Remove-Item -LiteralPath $tempPath -Force }
    if ($lockStream) { $lockStream.Dispose(); Remove-Item -LiteralPath $lockPath -Force }
}
