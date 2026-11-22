package main

import (
	"strings"
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

func TestIPAccessRejectsMalformedAllowlist(t *testing.T) {
	for _, bad := range [][]string{{"192.0.2.0/33"}, {"not-an-ip"}, {" "}, {"198.51.100.1/24/25"}} {
		if _, err := clientip.ParseSettings(bad, nil, nil, false); err == nil {
			t.Fatalf("invalid allowlist %q accepted", bad)
		}
	}
}

func TestIPAccessInvalidEntriesNamedWithField(t *testing.T) {
	_, err := clientip.ParseSettings([]string{"192.0.2.0/33"}, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "PROXY_IP_ALLOW") || !strings.Contains(err.Error(), "192.0.2.0/33") {
		t.Fatalf("allow error lacks field and entry: %v", err)
	}
	_, err = clientip.ParseSettings(nil, []string{"203.0.113.5/40"}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "PROXY_IP_DENY") || !strings.Contains(err.Error(), "203.0.113.5/40") {
		t.Fatalf("deny error lacks field and entry: %v", err)
	}
}

func TestIPAccessSemantics(t *testing.T) {
	p := testIPAccessSettings(t, []string{"10.0.0.0/8", "2001:db8::/32"}, []string{"10.6.0.0/16"}, nil)
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.1.2.3", true},
		{"10.6.1.1", false},
		{"2001:db8::1", true},
		{"198.51.100.20", false},
		{"127.0.0.1", false},
		{"::1", false},
	}
	for _, tc := range cases {
		if got := p.Permitted(tc.ip); got != tc.want {
			t.Fatalf("permitted(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	if p.Permitted("garbage") {
		t.Fatal("unparseable peer must be refused under an explicit allowlist")
	}
	if p.Restricted() != true {
		t.Fatal("policy with entries must be restricted")
	}
	if p := testIPAccessSettings(t, nil, nil, nil); p.Restricted() {
		t.Fatal("empty policy must be accepted and unrestricted")
	}
}
