//go:build windows

package main

// BUG-AUDIT-104 regression (Windows-only): NTFS is case-insensitive, so a
// CONFIG_PATH whose casing differs from the on-disk name opens and watches
// fine, but ReadDirectoryChangesW reports events with the ON-DISK casing.
// The watcher must match the config path case-insensitively, or the change
// is misclassified and the hot-reload silently lost. POSIX systems are
// case-sensitive (a wrong-case CONFIG_PATH simply does not exist there), so
// this test only runs on Windows.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditWatcherMatchesConfigPathCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(onDisk, []byte("debug = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(dir, "Config.TOML")
	_, h := newAuditConfigWatcher(t, "", requested)

	if err := os.WriteFile(onDisk, []byte("debug = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 3*time.Second, func() bool { return h.cfg.debug.Load() }) {
		t.Fatal("BUG-AUDIT-104: config hot-reload lost because event name casing differs from CONFIG_PATH")
	}
}
