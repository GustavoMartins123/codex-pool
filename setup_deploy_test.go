package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type deployFixture struct {
	dir       string
	envPath   string
	binDir    string
	logPath   string
	statePath string
}

var deployHealthFastEnv = []string{"CODEX_POOL_HEALTH_RETRIES=3", "CODEX_POOL_HEALTH_INTERVAL=0"}

func setupDeployFixture(t *testing.T) deployFixture {
	t.Helper()
	dir, envPath := setupFixture(t)
	fx := deployFixture{
		dir:       dir,
		envPath:   envPath,
		binDir:    filepath.Join(dir, "bin"),
		logPath:   filepath.Join(dir, "docker-shim.log"),
		statePath: filepath.Join(dir, "docker-shim.state"),
	}
	for _, name := range []string{"scripts/deploy.sh", "scripts/windows/deploy.ps1"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(fx.binDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeDockerShim(t, fx.binDir)
	return fx
}

func runDeploySetup(t *testing.T, fx deployFixture, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := setupTestCommand(filepath.Join(fx.dir, setupScriptName), args...)
	cmd.Dir = fx.dir
	cmd.Env = append(os.Environ(),
		"PATH="+fx.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SHIM_LOG="+fx.logPath,
		"SHIM_STATE="+fx.statePath,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func deploySetupArgs(monitoring bool) []string {
	deploy, mon := "--deploy", "--monitoring"
	if runtime.GOOS == "windows" {
		deploy, mon = "-Deploy", "-Monitoring"
	}
	if monitoring {
		return []string{deploy, mon}
	}
	return []string{deploy}
}

func readShimLog(t *testing.T, fx deployFixture) string {
	t.Helper()
	data, err := os.ReadFile(fx.logPath)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertDeployOutput(t *testing.T, fx deployFixture, output string) {
	t.Helper()
	if !strings.Contains(output, "http://127.0.0.1:8989/") || !strings.Contains(output, "ADMIN_TOKEN") {
		t.Fatalf("missing next steps: %s", output)
	}
	content := readSetupEnv(t, fx.envPath)
	for _, key := range []string{"ADMIN_TOKEN", "POOL_AUTH_ENCRYPTION_KEY", "POOL_JWT_SECRET", "POOL_CREDENTIAL_KEY"} {
		matches := regexp.MustCompile(`(?m)^` + key + `=([0-9a-f]{64})\r?$`).FindStringSubmatch(content)
		if len(matches) != 2 {
			t.Fatalf("missing or invalid %s", key)
		}
		if strings.Contains(output, matches[1]) {
			t.Fatalf("%s was printed", key)
		}
	}
	if _, err := os.Stat(fx.envPath + ".setup-lock"); !os.IsNotExist(err) {
		t.Fatal("setup left its lock behind")
	}
}

func assertEnvPreservedAfterFailure(t *testing.T, fx deployFixture, output string) {
	t.Helper()
	if !strings.Contains(output, "docker compose logs") {
		t.Fatalf("failure does not point at logs: %s", output)
	}
	content := readSetupEnv(t, fx.envPath)
	for _, key := range []string{"ADMIN_TOKEN", "POOL_AUTH_ENCRYPTION_KEY", "POOL_JWT_SECRET", "POOL_CREDENTIAL_KEY"} {
		if !regexp.MustCompile(`(?m)^` + key + `=[0-9a-f]{64}\r?$`).MatchString(content) {
			t.Fatalf("failed deploy lost %s", key)
		}
	}
	if _, err := os.Stat(fx.envPath + ".setup-lock"); !os.IsNotExist(err) {
		t.Fatal("failed setup left its lock behind")
	}
}

func runDeploySuccessCase(t *testing.T) {
	fx := setupDeployFixture(t)
	output, err := runDeploySetup(t, fx, deployHealthFastEnv, deploySetupArgs(false)...)
	if err != nil {
		t.Fatalf("setup deploy failed: %v (%s)", err, output)
	}
	assertDeployOutput(t, fx, output)
	log := readShimLog(t, fx)
	if !strings.Contains(log, "compose build codex-pool") || !strings.Contains(log, "compose up -d codex-pool") {
		t.Fatalf("deploy not invoked: %s", log)
	}
}

func runDeployMonitoringCase(t *testing.T) {
	fx := setupDeployFixture(t)
	output, err := runDeploySetup(t, fx, deployHealthFastEnv, deploySetupArgs(true)...)
	if err != nil {
		t.Fatalf("monitoring deploy failed: %v (%s)", err, output)
	}
	log := readShimLog(t, fx)
	for _, want := range []string{"compose.monitoring.yml", "prometheus", "grafana"} {
		if !strings.Contains(log, want) {
			t.Fatalf("monitoring deploy missing %q: %s", want, log)
		}
	}
}

func runDeployCustomPathRejectedCase(t *testing.T) {
	fx := setupDeployFixture(t)
	custom := filepath.Join(fx.dir, "custom env")
	args := deploySetupArgs(false)
	if runtime.GOOS == "windows" {
		args = append(args, "-EnvPath", custom)
	} else {
		args = append(args, custom)
	}
	if _, err := runDeploySetup(t, fx, nil, args...); err == nil {
		t.Fatal("custom env path with deploy accepted")
	}
	for _, path := range []string{custom, fx.envPath, filepath.Join(fx.dir, "pool"), filepath.Join(fx.dir, "data")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("rejected setup created %s", path)
		}
	}
	if readShimLog(t, fx) != "" {
		t.Fatal("docker invoked for rejected setup")
	}
}

func runDeployBuildFailureCase(t *testing.T) {
	fx := setupDeployFixture(t)
	output, err := runDeploySetup(t, fx, []string{"SHIM_FAIL=build"}, deploySetupArgs(false)...)
	if err == nil {
		t.Fatal("deploy failure accepted")
	}
	assertEnvPreservedAfterFailure(t, fx, output)
}

func runDeployHealthTimeoutCase(t *testing.T) {
	fx := setupDeployFixture(t)
	env := []string{"SHIM_HEALTH=starting", "CODEX_POOL_HEALTH_RETRIES=2", "CODEX_POOL_HEALTH_INTERVAL=0"}
	output, err := runDeploySetup(t, fx, env, deploySetupArgs(false)...)
	if err == nil {
		t.Fatal("health timeout accepted")
	}
	assertEnvPreservedAfterFailure(t, fx, output)
}

func runDeployHealthSequenceCase(t *testing.T) {
	fx := setupDeployFixture(t)
	env := append([]string{"SHIM_HEALTH_SEQ=starting,healthy"}, deployHealthFastEnv...)
	output, err := runDeploySetup(t, fx, env, deploySetupArgs(false)...)
	if err != nil {
		t.Fatalf("health sequence failed: %v (%s)", err, output)
	}
	assertDeployOutput(t, fx, output)
}

func runEnvOnlyRegressionCase(t *testing.T) {
	fx := setupDeployFixture(t)
	if output, err := runDeploySetup(t, fx, nil); err != nil {
		t.Fatalf("env-only setup failed: %v (%s)", err, output)
	}
	for _, name := range []string{"pool", "data"} {
		if _, err := os.Stat(filepath.Join(fx.dir, name)); !os.IsNotExist(err) {
			t.Fatalf("env-only setup created %s", name)
		}
	}
	if readShimLog(t, fx) != "" {
		t.Fatal("env-only setup invoked docker")
	}
}
