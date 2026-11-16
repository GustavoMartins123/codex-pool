package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDockerShim(t *testing.T, binDir string) {
	t.Helper()
	wrapper := "@echo off\r\npowershell -NoProfile -ExecutionPolicy Bypass -File \"%~dp0docker-shim.ps1\" %*\r\nexit /b %ERRORLEVEL%\r\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker.cmd"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	shim := `$line = $args -join ' '
Add-Content -LiteralPath $env:SHIM_LOG -Value $line
if ($args.Count -gt 0 -and $args[0] -eq 'compose' -and $env:SHIM_FAIL -and $line.Contains($env:SHIM_FAIL)) { exit 1 }
if ($args.Count -gt 0 -and $args[0] -eq 'inspect') {
    $n = 0
    if (Test-Path -LiteralPath $env:SHIM_STATE) { $n = [int]((Get-Content -LiteralPath $env:SHIM_STATE -Raw).Trim()) }
    $n++
    Set-Content -LiteralPath $env:SHIM_STATE -Value $n -NoNewline
    $seq = $env:SHIM_HEALTH_SEQ
    if (-not $seq) { $seq = $env:SHIM_HEALTH }
    if (-not $seq) { $seq = 'healthy' }
    $states = $seq -split ','
    $i = [Math]::Min($n - 1, $states.Count - 1)
    Write-Output $states[$i]
}
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker-shim.ps1"), []byte(shim), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupDeployBuildsAndStartsPool(t *testing.T) { runDeploySuccessCase(t) }

func TestSetupDeployMonitoringPassthrough(t *testing.T) { runDeployMonitoringCase(t) }

func TestSetupDeployRejectsCustomEnvPath(t *testing.T) { runDeployCustomPathRejectedCase(t) }

func TestSetupDeployFailurePreservesEnvironment(t *testing.T) { runDeployBuildFailureCase(t) }

func TestSetupDeployHealthTimeout(t *testing.T) { runDeployHealthTimeoutCase(t) }

func TestSetupDeployWaitsForHealthy(t *testing.T) { runDeployHealthSequenceCase(t) }

func TestSetupWithoutDeployStaysEnvOnly(t *testing.T) { runEnvOnlyRegressionCase(t) }

func TestSetupDeployKeepsEnvironmentPrivate(t *testing.T) {
	fx := setupDeployFixture(t)
	if output, err := runDeploySetup(t, fx, deployHealthFastEnv, deploySetupArgs(false)...); err != nil {
		t.Fatalf("setup deploy failed: %v (%s)", err, output)
	}
	checkSetupPermissions(t, fx.envPath)
}
