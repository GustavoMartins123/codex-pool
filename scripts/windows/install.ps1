param(
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\codex-pool'),
    [string]$CompilerBin = 'C:\msys64\ucrt64\bin'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
& (Join-Path $PSScriptRoot 'build.ps1') -CompilerBin $CompilerBin
$source = Join-Path $root 'dist\windows-amd64\codex-pool.exe'
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$target = Join-Path $InstallDir 'codex-pool.exe'
$temp = Join-Path $InstallDir "codex-pool.$([guid]::NewGuid().ToString('N')).exe"
try {
    Copy-Item -LiteralPath $source -Destination $temp
    Move-Item -LiteralPath $temp -Destination $target -Force
} finally {
    Remove-Item -LiteralPath $temp -ErrorAction SilentlyContinue
}
Write-Output "Installed $target"
