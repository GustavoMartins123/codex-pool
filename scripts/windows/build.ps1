param(
    [ValidateSet('amd64')]
    [string]$Architecture = 'amd64',
    [string]$CompilerBin = 'C:\msys64\ucrt64\bin'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$output = Join-Path $root "dist\windows-$Architecture\codex-pool.exe"
New-Item -ItemType Directory -Force -Path (Split-Path $output) | Out-Null
if (Test-Path -LiteralPath $output) { Remove-Item -LiteralPath $output }
$compiler = Join-Path $CompilerBin 'gcc.exe'
if (-not (Test-Path -LiteralPath $compiler -PathType Leaf)) {
    throw "MSYS2 UCRT64 GCC not found: $compiler"
}
$CompilerBin = (Resolve-Path -LiteralPath $CompilerBin).Path
$compiler = Join-Path $CompilerBin 'gcc.exe'

Push-Location (Join-Path $root 'web')
try {
    & npm.cmd ci
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed with exit code $LASTEXITCODE" }
    & npm.cmd run build
    if ($LASTEXITCODE -ne 0) { throw "npm run build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

$previous = @{}
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED', 'CC', 'PATH')) {
    $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
Push-Location $root
try {
    $env:GOOS = 'windows'
    $env:GOARCH = $Architecture
    $env:CGO_ENABLED = '1'
    $env:CC = $compiler
    $env:PATH = "$CompilerBin;$env:PATH"
    & go build -trimpath -ldflags '-s -w' -o $output .
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
} finally {
    foreach ($name in $previous.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
    }
    Pop-Location
}

if (-not (Test-Path -LiteralPath $output -PathType Leaf)) { throw "Build output missing: $output" }
Write-Output "Built $output"
