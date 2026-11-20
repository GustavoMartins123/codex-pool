package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestBuildProxyServerReleasesUsageStoreOnError(t *testing.T) {
	t.Setenv("ANALYTICS_EMERGENCY_RESERVE_BYTES", "invalid")
	dir := t.TempDir()
	cfg := &config{
		poolDir:       dir,
		storePath:     filepath.Join(dir, "usage.db"),
		retentionDays: 30,
	}

	app, err := buildProxyServer(context.Background(), cfg)
	if app != nil || err == nil || !strings.Contains(err.Error(), "invalid ANALYTICS_EMERGENCY_RESERVE_BYTES") {
		t.Fatalf("build returned app = %t and err = %v, want invalid analytics reserve error", app != nil, err)
	}

	db, err := bbolt.Open(cfg.storePath, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("reopen usage store after failed startup: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
