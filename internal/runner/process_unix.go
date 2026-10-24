//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

func hideProcess(command *exec.Cmd) {}
func startManaged(command *exec.Cmd) (func() error, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }, nil
}
