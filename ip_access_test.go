package main

import (
	"testing"

	"codex-pool-proxy/internal/clientip"
)

func testIPAccessSettings(t *testing.T, allow, deny, proxies []string) clientip.Settings {
	t.Helper()
	settings, err := clientip.ParseSettings(allow, deny, proxies, false)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}
