[CmdletBinding()]
param([string]$OutputDirectory = (Join-Path $PSScriptRoot '../bin/runnerd'))

$ErrorActionPreference = 'Stop'
$runnerRoot = Split-Path $PSScriptRoot -Parent
$runnerTargets = @('windows/amd64', 'windows/arm64', 'linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64')
$runnerOldCGO = $env:CGO_ENABLED
$runnerOldOS = $env:GOOS
$runnerOldArch = $env:GOARCH
Push-Location -LiteralPath $runnerRoot
try {
    $env:CGO_ENABLED = '0'
    foreach ($runnerTarget in $runnerTargets) {
        $runnerParts = $runnerTarget.Split('/')
        $env:GOOS = $runnerParts[0]
        $env:GOARCH = $runnerParts[1]
        $runnerDirectory = Join-Path $OutputDirectory ($runnerParts -join '-')
        New-Item -ItemType Directory -Path $runnerDirectory -Force | Out-Null
        $runnerFilename = if ($env:GOOS -eq 'windows') { 'runnerd.exe' } else { 'runnerd' }
        & go build -trimpath -ldflags='-s -w' -o (Join-Path $runnerDirectory $runnerFilename) ./cmd/runnerd
        if ($LASTEXITCODE -ne 0) { throw "runnerd build failed for $runnerTarget" }
        Write-Output "Built runnerd for $runnerTarget"
    }
} finally {
    $env:CGO_ENABLED = $runnerOldCGO
    $env:GOOS = $runnerOldOS
    $env:GOARCH = $runnerOldArch
    Pop-Location
}
