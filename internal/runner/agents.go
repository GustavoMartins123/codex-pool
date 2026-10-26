package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// FindAgents looks up Codex and Claude Code on PATH for a runner started
// without a launch configuration. It keeps only what Run accepts: absolute
// regular files, native .exe files on Windows. exec.LookPath never returns a
// match from the current directory. Installing missing agents is not part
// of this release.
func FindAgents() map[string]string {
	found := map[string]string{}
	for _, agent := range []string{"codex", "claude"} {
		path, err := exec.LookPath(agent)
		if err != nil {
			continue
		}
		path, err = filepath.Abs(path)
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if runtime.GOOS == "windows" && strings.ToLower(filepath.Ext(path)) != ".exe" {
			continue
		}
		found[agent] = path
	}
	return found
}
