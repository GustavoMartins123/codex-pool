[CmdletBinding()]
param([string]$OutputDirectory = (Join-Path $PSScriptRoot '../dist/runner-release'))

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$output = if ([IO.Path]::IsPathRooted($OutputDirectory)) {
    [IO.Path]::GetFullPath($OutputDirectory)
} else {
    [IO.Path]::GetFullPath((Join-Path $root $OutputDirectory))
}
$unpacked = Join-Path $output 'unpacked'
Remove-Item -LiteralPath $output -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $output | Out-Null
& (Join-Path $root 'scripts/build-runner.ps1') -OutputDirectory $unpacked
if ($LASTEXITCODE -ne 0) { throw "runner build failed with exit code $LASTEXITCODE" }

foreach ($target in @('windows-amd64', 'windows-arm64')) {
    Compress-Archive -LiteralPath (Join-Path $unpacked "$target/runnerd.exe") `
        -DestinationPath (Join-Path $output "runnerd-$target.zip") -CompressionLevel Optimal
}
foreach ($target in @('linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64')) {
    & tar.exe -C (Join-Path $unpacked $target) -czf (Join-Path $output "runnerd-$target.tar.gz") runnerd
    if ($LASTEXITCODE -ne 0) { throw "archive failed for $target" }
}

$checksums = Get-ChildItem -LiteralPath $output -File -Include 'runnerd-*.zip', 'runnerd-*.tar.gz' |
    Get-FileHash -Algorithm SHA256 |
    ForEach-Object { "$($_.Hash.ToLowerInvariant())  $($_.Path | Split-Path -Leaf)" }
$checksums | Set-Content -LiteralPath (Join-Path $output 'SHA256SUMS') -Encoding ascii
Remove-Item -LiteralPath $unpacked -Recurse -Force
Write-Output "Runner release artifacts written to $output"
