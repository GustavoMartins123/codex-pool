package runner

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"codex-pool-proxy/internal/runnerproto"
)

func prepareWorkspace(ctx context.Context, root string, allowed []string, session runnerproto.Session) (string, error) {
	if session.Workspace.Mode == "path" {
		if session.PermissionMode != "gated" {
			return "", errors.New("path workspace requires gated permissions")
		}
		if !filepath.IsAbs(session.Workspace.Target) {
			return "", errors.New("workspace path must be absolute")
		}
		target, err := filepath.EvalSymlinks(session.Workspace.Target)
		if err != nil {
			return "", errors.New("workspace path unavailable")
		}
		for _, path := range allowed {
			canonical, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", errors.New("allowed path unavailable")
			}
			if target == canonical {
				info, err := os.Stat(target)
				if err != nil || !info.IsDir() {
					return "", errors.New("workspace is not a directory")
				}
				return target, nil
			}
		}
		return "", errors.New("workspace path is not in the runner allowlist")
	}
	if session.Workspace.Mode != "ephemeral" && session.Workspace.Mode != "repo" {
		return "", errors.New("unsupported workspace mode")
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("workspace root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(canonical, session.ID)
	if filepath.Dir(path) != canonical || strings.ContainsAny(session.ID, "/\\") || session.ID == "" {
		return "", errors.New("invalid session workspace identifier")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return "", errors.New("session workspace already exists or cannot be created")
	}
	if session.Workspace.Mode == "ephemeral" {
		return path, nil
	}
	repo, err := url.Parse(session.Workspace.Target)
	if err != nil || repo.Scheme != "https" || repo.Hostname() == "" || repo.User != nil || repo.RawQuery != "" || repo.Fragment != "" {
		return "", errors.New("repository target must be an HTTPS URL without credentials, query or fragment")
	}
	command := exec.Command("git", "-c", "core.hooksPath=", "-c", "credential.helper=", "-c", "protocol.file.allow=never", "-c", "http.followRedirects=false", "clone", "--no-recurse-submodules", "--", repo.String(), path)
	command.Env = cleanEnvironment(map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull})
	kill, err := startManaged(command)
	if err != nil {
		return "", errors.New("repository clone failed to start")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err = <-done:
		_ = kill()
	case <-ctx.Done():
		_ = kill()
		<-done
		return "", ctx.Err()
	}
	if err != nil {
		return "", errors.New("repository clone failed")
	}
	return path, nil
}

func cleanupWorkspace(root, path, mode string) error {
	if mode != "ephemeral" || path == "" {
		return nil
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Dir(absolute) != canonical || absolute == canonical {
		return errors.New("refusing cleanup outside session workspace root")
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing cleanup of replaced workspace")
	}
	return os.RemoveAll(absolute)
}

// Only operating-system essentials reach the CLI. Existing provider credentials,
// proxy overrides, executable hooks and agent configuration are not inherited.
func cleanEnvironment(extra map[string]string) []string {
	keys := []string{"PATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "TMPDIR", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "LANG", "LC_ALL", "SHELL"}
	result := []string{}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			if _, replaced := extra[key]; !replaced {
				result = append(result, key+"="+value)
			}
		}
	}
	for key, value := range extra {
		result = append(result, key+"="+value)
	}
	return result
}
