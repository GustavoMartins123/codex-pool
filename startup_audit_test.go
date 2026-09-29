package main

// Startup and persistence regressions for bare-executable deployments
// (Windows runs the .exe directly, no Docker image that pre-creates dirs).

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAuditFreshDirectoryStartupCreatesStorage
// BUG-AUDIT-108 regression (cross-platform): a bare codex-pool executable
// started in a fresh working directory — the documented Windows deployment
// ("Run the binary from a directory where you want pool/, data/ ... to
// live") — must create the storage parent directories itself. The Docker
// image masks this with `mkdir -p /app/data`, so the bug only surfaced
// outside Docker.
func TestAuditFreshDirectoryStartupCreatesStorage(t *testing.T) {
	base := t.TempDir()
	absolute := filepath.Join(base, "abs", "deeper", "proxy.db")
	cases := []struct {
		name string
		path string
	}{
		{"default-data-subdir", filepath.Join(base, "fresh-run", "data", "proxy.db")},
		{"nested-custom-parent", filepath.Join(base, "custom", "nested", "deeper", "proxy.db")},
		{"spaces-in-path", filepath.Join(base, "codex pool data", "proxy.db")},
		{"absolute-path", absolute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if filepath.IsAbs(tc.path) && !filepath.IsAbs(base) {
				t.Skipf("cannot build an absolute-path case from base %q", base)
			}
			store, err := newUsageStore(tc.path, 30)
			if err != nil {
				t.Fatalf("BUG-AUDIT-108: fresh-directory startup cannot open usage store at %s: %v", tc.path, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(tc.path); err != nil {
				t.Fatalf("store file missing after open: %v", err)
			}
		})
	}
}

// TestAuditAnalyticsDBPathFollowsUsageStore verifies the analytics database
// resolves next to the configured usage store (custom PROXY_DB_PATH keeps
// all databases together) and honors ANALYTICS_DB_PATH as an override.
func TestAuditAnalyticsDBPathFollowsUsageStore(t *testing.T) {
	t.Setenv("ANALYTICS_DB_PATH", "")
	if got := analyticsDBPathFor("./data/proxy.db"); got != filepath.Join("data", "analytics.db") {
		t.Fatalf("default analytics path = %q, want data/analytics.db", got)
	}
	if got, want := analyticsDBPathFor("/srv/pool/proxy.db"), filepath.Join("/srv/pool", "analytics.db"); got != want {
		t.Fatalf("custom store analytics path = %q, want %q", got, want)
	}
	t.Setenv("ANALYTICS_DB_PATH", "/elsewhere/analytics.db")
	if got := analyticsDBPathFor("./data/proxy.db"); got != "/elsewhere/analytics.db" {
		t.Fatalf("override analytics path = %q", got)
	}
}
