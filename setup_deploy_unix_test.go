//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDockerShim(t *testing.T, binDir string) {
	t.Helper()
	shim := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SHIM_LOG"
if [[ $1 == compose && -n ${SHIM_FAIL:-} && $* == *"$SHIM_FAIL"* ]]; then exit 1; fi
if [[ $1 == inspect ]]; then
  n=0
  [[ ! -f $SHIM_STATE ]] || n=$(cat "$SHIM_STATE")
  n=$((n + 1))
  printf '%s' "$n" > "$SHIM_STATE"
  seq=${SHIM_HEALTH_SEQ:-${SHIM_HEALTH:-healthy}}
  IFS=',' read -ra states <<< "$seq"
  i=$((n - 1))
  (( i < ${#states[@]} )) || i=$(( ${#states[@]} - 1 ))
  printf '%s\n' "${states[$i]}"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(shim), 0700); err != nil {
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

func TestSetupDeployCreatesPrivateDirectories(t *testing.T) {
	fx := setupDeployFixture(t)
	if output, err := runDeploySetup(t, fx, deployHealthFastEnv, deploySetupArgs(false)...); err != nil {
		t.Fatalf("setup deploy failed: %v (%s)", err, output)
	}
	for _, name := range []string{"pool", "data"} {
		info, err := os.Stat(filepath.Join(fx.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%s mode: %o", name, info.Mode().Perm())
		}
	}
}

func TestSetupDeployPreservesExistingPool(t *testing.T) {
	fx := setupDeployFixture(t)
	poolDir := filepath.Join(fx.dir, "pool")
	if err := os.Mkdir(poolDir, 0755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(poolDir, "keep.json")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := runDeploySetup(t, fx, deployHealthFastEnv, deploySetupArgs(false)...); err != nil {
		t.Fatalf("setup deploy failed: %v (%s)", err, output)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatal("existing pool file changed")
	}
	info, err := os.Stat(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("existing pool directory mode changed: %o", info.Mode().Perm())
	}
}

func TestSetupDeployRejectsUnknownFlag(t *testing.T) {
	fx := setupDeployFixture(t)
	output, err := runDeploySetup(t, fx, nil, "--bogus")
	if err == nil {
		t.Fatal("unknown flag accepted")
	}
	if !strings.Contains(output, "Unknown option") {
		t.Fatalf("unexpected failure: %s", output)
	}
	if _, err := os.Stat(fx.envPath); !os.IsNotExist(err) {
		t.Fatal("unknown flag wrote environment")
	}
}
