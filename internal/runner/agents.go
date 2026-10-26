package runner

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AgentLookup finds runnable agents for a runner started without a launch
// configuration. It never searches the disk or the current directory: it
// checks the runner's managed install, then the official installers'
// locations, then PATH, and keeps the first candidate that is a native
// binary for this machine (or, outside Windows, a script) and answers
// --version in time.
type AgentLookup struct {
	ManagedRoot string

	goos   string
	goarch string
	home   func() (string, error)
	paths  func(string) []string
	probe  func(context.Context, string) error
}

// AgentCandidate records why a location was or was not used.
type AgentCandidate struct {
	Agent  string
	Path   string
	Source string
	Err    error
}

func (l *AgentLookup) defaults() {
	if l.goos == "" {
		l.goos, l.goarch = runtime.GOOS, runtime.GOARCH
	}
	if l.home == nil {
		l.home = os.UserHomeDir
	}
	if l.paths == nil {
		l.paths = agentPaths
	}
	if l.probe == nil {
		l.probe = probeAgent
	}
}

// Resolve returns the executables Run accepts and every candidate it tried.
func (l AgentLookup) Resolve(ctx context.Context) (map[string]string, []AgentCandidate) {
	l.defaults()
	found := map[string]string{}
	var tried []AgentCandidate
	for _, agent := range []string{"codex", "claude"} {
		for _, c := range l.candidates(agent) {
			if ctx.Err() != nil {
				return found, tried
			}
			if c.Err == nil {
				_, c.Err = l.validate(ctx, c.Path)
			}
			tried = append(tried, c)
			if c.Err == nil {
				found[agent] = c.Path
				break
			}
		}
	}
	return found, tried
}

func (l AgentLookup) exe(name string) string {
	if l.goos == "windows" {
		return name + ".exe"
	}
	return name
}

func (l AgentLookup) candidates(agent string) []AgentCandidate {
	var out []AgentCandidate
	add := func(path, source string, err error) {
		out = append(out, AgentCandidate{Agent: agent, Path: path, Source: source, Err: err})
	}
	if agent == "codex" && l.ManagedRoot != "" {
		installer := &AgentInstaller{Root: l.ManagedRoot, goos: l.goos, goarch: l.goarch}
		if path, ok := installer.Installed(agent); ok {
			add(path, "managed", nil)
		}
	}
	if home, err := l.home(); err == nil && agent == "claude" {
		// Claude Code's native installer links its current version here.
		add(filepath.Join(home, ".local", "bin", l.exe("claude")), "official installer", nil)
	}
	paths := l.paths(agent)
	if len(paths) == 0 {
		add("", "PATH", exec.ErrNotFound)
	}
	for _, path := range paths {
		if native, ok := l.npmNative(agent, path); ok {
			add(native, "npm native build", nil)
		}
		add(path, "PATH", nil)
	}
	return out
}

// agentPaths enumerates absolute PATH entries so an unusable earlier agent
// cannot hide a working later one. Relative entries never become candidates.
func agentPaths(agent string) []string {
	names := []string{agent}
	if runtime.GOOS == "windows" {
		names = []string{agent + ".exe", agent + ".cmd"}
	}
	var paths []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			key := path
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, err := os.Lstat(path); err == nil {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// npmNative maps an npm-installed codex launcher (a .cmd shim on Windows,
// a symlink to bin/codex.js elsewhere) to the native binary it starts, so
// the runner needs neither Node nor a shell shim.
func (l AgentLookup) npmNative(agent, launcher string) (string, bool) {
	if agent != "codex" {
		return "", false
	}
	var platform CodexPlatform
	for _, p := range codexPlatforms {
		if p.GOOS == l.goos && p.GOARCH == l.goarch {
			platform = p
		}
	}
	if platform.Triple == "" {
		return "", false
	}
	var root string
	if l.goos == "windows" {
		root = filepath.Join(filepath.Dir(launcher), "node_modules", "@openai", "codex")
	} else {
		resolved, err := filepath.EvalSymlinks(launcher)
		if err != nil || filepath.Base(resolved) != "codex.js" || filepath.Base(filepath.Dir(resolved)) != "bin" {
			return "", false
		}
		root = filepath.Dir(filepath.Dir(resolved))
	}
	vendor := filepath.Join("vendor", platform.Triple, "bin", l.exe("codex"))
	for _, candidate := range []string{
		filepath.Join(root, "node_modules", "@openai", "codex-"+platform.NPM, vendor),
		filepath.Join(filepath.Dir(root), "codex-"+platform.NPM, vendor),
		filepath.Join(root, vendor),
	} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

func (l AgentLookup) validate(ctx context.Context, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("not an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("not found")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if l.goos == "windows" && !strings.EqualFold(filepath.Ext(resolved), ".exe") {
		return "", errors.New("Windows agents must be native .exe files; shell shims are not run")
	}
	if l.goos != "windows" && info.Mode().Perm()&0111 == 0 {
		return "", errors.New("not executable")
	}
	if err := matchesMachine(resolved, l.goos, l.goarch); err != nil {
		return "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := l.probe(probeCtx, resolved); err != nil {
		return "", err
	}
	return path, nil
}

// matchesMachine reads only the executable header: a binary for another
// architecture or OS would fail later, mid-session.
func matchesMachine(path, goos, goarch string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(file, head); err != nil {
		return errors.New("not an executable")
	}
	wrong := errors.New("built for another platform")
	// Arm Macs and Windows on Arm also run x86-64 builds.
	archOK := func(arch string) bool {
		return arch == goarch || arch == "amd64" && goarch == "arm64" && goos != "linux"
	}
	switch {
	case bytes.HasPrefix(head, []byte("#!")):
		if goos == "windows" {
			return wrong
		}
		return nil
	case bytes.Equal(head, []byte("\x7fELF")):
		f, err := elf.NewFile(file)
		if err != nil || goos != "linux" {
			return wrong
		}
		arch := map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}[f.Machine]
		if !archOK(arch) {
			return wrong
		}
		return nil
	case head[0] == 'M' && head[1] == 'Z':
		f, err := pe.NewFile(file)
		if err != nil || goos != "windows" {
			return wrong
		}
		arch := map[uint16]string{pe.IMAGE_FILE_MACHINE_AMD64: "amd64", pe.IMAGE_FILE_MACHINE_ARM64: "arm64"}[f.Machine]
		if !archOK(arch) {
			return wrong
		}
		return nil
	}
	if goos != "darwin" {
		return wrong
	}
	arches := []macho.Cpu{}
	if fat, err := macho.NewFatFile(file); err == nil {
		for _, a := range fat.Arches {
			arches = append(arches, a.Cpu)
		}
	} else if f, err := macho.NewFile(file); err == nil {
		arches = append(arches, f.Cpu)
	} else {
		return wrong
	}
	for _, cpu := range arches {
		if archOK(map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}[cpu]) {
			return nil
		}
	}
	return wrong
}

// probeAgent runs "<agent> --version" directly, without a shell, and keeps
// at most 4 KiB of output.
func probeAgent(ctx context.Context, path string) error {
	command := exec.CommandContext(ctx, path, "--version")
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("did not answer --version in time")
		}
		return errors.New("failed to run --version")
	}
	return nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 4<<10 - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
