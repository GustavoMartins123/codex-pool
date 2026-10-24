package runner

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func checkCodexSandbox(ctx context.Context, exe, workspace, control string) error {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return errors.New("Windows command processor unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, exe, "-c", `windows.sandbox="mxc"`, "sandbox", "--include-managed-config", "--permission-profile", ":workspace", "--", filepath.Join(system, "cmd.exe"), "/d", "/c", "echo", "runner-sandbox-check")
	command.Dir = workspace
	command.Env = cleanEnvironment(map[string]string{"CODEX_HOME": control, "OTEL_SDK_DISABLED": "true"})
	hideProcess(command)
	if command.Run() != nil {
		return errors.New("Codex Windows MXC sandbox unavailable; execution stopped")
	}
	return nil
}
