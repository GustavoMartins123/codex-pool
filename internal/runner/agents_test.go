package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeScript(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 1\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func lookupFor(t *testing.T, path map[string]string, home string) AgentLookup {
	return AgentLookup{goos: "linux", goarch: "amd64", home: func() (string, error) { return home, nil },
		lookPath: func(name string) (string, error) {
			if p, ok := path[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		probe: func(context.Context, string) error { return nil }}
}

func TestAgentLookupPrefersManagedThenOfficialThenPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix scripts as agents")
	}
	root := t.TempDir()
	home := t.TempDir()
	onPath := writeScript(t, filepath.Join(root, "bin", "codex"), 0755)
	claudeOnPath := writeScript(t, filepath.Join(root, "bin", "claude"), 0755)
	lookup := lookupFor(t, map[string]string{"codex": onPath, "claude": claudeOnPath}, home)
	found, _ := lookup.Resolve(context.Background())
	if found["codex"] != onPath || found["claude"] != claudeOnPath {
		t.Fatalf("PATH agents = %v", found)
	}

	official := writeScript(t, filepath.Join(home, ".local", "bin", "claude"), 0755)
	managed := filepath.Join(root, "agents")
	installer := &AgentInstaller{Root: managed, goos: "linux", goarch: "amd64"}
	release, _ := installer.Release("codex")
	managedExe := writeScript(t, installer.executable(release), 0755)
	receipt, _ := json.Marshal(installReceipt{Agent: "codex", Version: release.Version, Triple: release.Triple, SHA512: release.SHA512})
	_ = os.WriteFile(filepath.Join(installer.dir(release), agentReceipt), receipt, 0600)
	lookup.ManagedRoot = managed
	found, _ = lookup.Resolve(context.Background())
	if found["codex"] != managedExe || found["claude"] != official {
		t.Fatalf("preferred agents = %v", found)
	}
}

func TestAgentLookupRunsTheNativeBuildBehindNpmLaunchers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("npm uses .cmd shims on Windows")
	}
	prefix := t.TempDir()
	pkg := filepath.Join(prefix, "lib", "node_modules", "@openai", "codex")
	launcher := writeScript(t, filepath.Join(pkg, "bin", "codex.js"), 0755)
	native := writeScript(t, filepath.Join(pkg, "node_modules", "@openai", "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin", "codex"), 0755)
	link := filepath.Join(prefix, "bin", "codex")
	_ = os.MkdirAll(filepath.Dir(link), 0755)
	if err := os.Symlink(launcher, link); err != nil {
		t.Fatal(err)
	}
	found, _ := lookupFor(t, map[string]string{"codex": link}, t.TempDir()).Resolve(context.Background())
	if found["codex"] != native {
		t.Fatalf("codex = %s, want the native build %s", found["codex"], native)
	}

	windows := AgentLookup{goos: "windows", goarch: "amd64"}
	shimDir := t.TempDir()
	exe := writeScript(t, filepath.Join(shimDir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe"), 0755)
	if got, ok := windows.npmNative("codex", filepath.Join(shimDir, "codex.cmd")); !ok || got != exe {
		t.Fatalf("Windows shim resolved to %q", got)
	}
}

func TestAgentLookupRejectsWhatRunCannotStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix scripts as agents")
	}
	root := t.TempDir()
	notExecutable := writeScript(t, filepath.Join(root, "codex"), 0644)
	lookup := lookupFor(t, map[string]string{"codex": notExecutable, "claude": root}, t.TempDir())
	found, tried := lookup.Resolve(context.Background())
	if len(found) != 0 {
		t.Fatalf("accepted %v", found)
	}
	if len(tried) == 0 {
		t.Fatal("no candidates reported")
	}

	relative := lookupFor(t, nil, t.TempDir())
	relative.lookPath = func(string) (string, error) { return "codex", exec.ErrDot }
	if found, _ := relative.Resolve(context.Background()); len(found) != 0 {
		t.Fatal("agent from a relative PATH entry accepted")
	}

	failing := lookupFor(t, map[string]string{"codex": writeScript(t, filepath.Join(root, "broken"), 0755)}, t.TempDir())
	failing.probe = func(context.Context, string) error { return errors.New("failed to run --version") }
	if found, _ := failing.Resolve(context.Background()); len(found) != 0 {
		t.Fatal("agent that fails --version accepted")
	}

	shim := AgentLookup{goos: "windows", goarch: "amd64"}
	cmd := writeScript(t, filepath.Join(root, "codex.cmd"), 0755)
	if _, err := shim.validate(context.Background(), cmd); err == nil {
		t.Fatal("Windows shell shim accepted")
	}
}

func TestMatchesMachineReadsTheExecutableHeader(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	if err := matchesMachine(self, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("own test binary rejected: %v", err)
	}
	if runtime.GOOS == "linux" && other != "" {
		if matchesMachine(self, "linux", other) == nil {
			t.Fatal("binary for another architecture accepted")
		}
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if goos != runtime.GOOS && matchesMachine(self, goos, runtime.GOARCH) == nil {
			t.Fatalf("binary for %s accepted on %s", runtime.GOOS, goos)
		}
	}
}

func TestProbeAgentGivesUpOnHungAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix script")
	}
	hung := filepath.Join(t.TempDir(), "hung")
	_ = os.WriteFile(hung, []byte("#!/bin/sh\nexec sleep 30\n"), 0755)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := probeAgent(ctx, hung); err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("probe = %v after %s", err, time.Since(started))
	}
}
