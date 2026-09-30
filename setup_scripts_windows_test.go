package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

const setupScriptName = "setup.ps1"

func setupTestCommand(script string, args ...string) *exec.Cmd {
	arguments := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}
	cmd := exec.Command("powershell.exe", append(arguments, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}

func checkSetupPermissions(t *testing.T, path string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "check-acl.ps1")
	data := []byte(`$ErrorActionPreference='Stop'
$acl=Get-Acl -LiteralPath $args[0]
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
if (-not $acl.AreAccessRulesProtected -or $acl.Access.Count -ne 1 -or $acl.Access[0].IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value -ne $sid -or $acl.Access[0].AccessControlType -ne 'Allow') { exit 1 }
`)
	if err := os.WriteFile(script, data, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := setupTestCommand(script, path).CombinedOutput(); err != nil {
		t.Fatalf("environment permissions are not private: %v (%s)", err, output)
	}
}
