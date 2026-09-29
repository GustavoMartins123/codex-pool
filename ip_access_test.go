package main

import (
	"strings"
	"testing"
)

// P1-01 regression: a malformed allowlist must be rejected, and a rejected
// reconfigure must keep the previously installed restriction.
func TestIPAccessInvalidPolicyRejectedKeepsPrevious(t *testing.T) {
	p := &ipAccessPolicy{}
	if err := p.configure([]string{"192.0.2.0/24"}, nil); err != nil {
		t.Fatal(err)
	}
	if !p.restricted() || p.permitted("198.51.100.20") {
		t.Fatal("valid allowlist control did not reject outsider")
	}
	for _, bad := range [][]string{{"192.0.2.0/33"}, {"not-an-ip"}, {" "}, {"198.51.100.1/24/25"}} {
		if err := p.configure(bad, nil); err == nil {
			t.Fatalf("invalid allowlist %q accepted", bad)
		}
	}
	if !p.restricted() {
		t.Fatal("rejected reconfigure dropped the restriction")
	}
	if p.permitted("198.51.100.20") {
		t.Fatal("fail-open: outsider permitted after invalid policy was rejected")
	}
}

func TestIPAccessInvalidEntriesNamedWithField(t *testing.T) {
	p := &ipAccessPolicy{}
	err := p.configure([]string{"192.0.2.0/33"}, nil)
	if err == nil || !strings.Contains(err.Error(), "PROXY_IP_ALLOW") || !strings.Contains(err.Error(), "192.0.2.0/33") {
		t.Fatalf("allow error lacks field and entry: %v", err)
	}
	err = p.configure(nil, []string{"203.0.113.5/40"})
	if err == nil || !strings.Contains(err.Error(), "PROXY_IP_DENY") || !strings.Contains(err.Error(), "203.0.113.5/40") {
		t.Fatalf("deny error lacks field and entry: %v", err)
	}
	if p.restricted() {
		t.Fatal("failed configure must not install a policy")
	}
}

func TestIPAccessSemantics(t *testing.T) {
	p := &ipAccessPolicy{}
	if err := p.configure([]string{"10.0.0.0/8", "2001:db8::/32"}, []string{"10.6.0.0/16"}); err != nil {
		t.Fatal(err)
	}
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
		if got := p.permitted(tc.ip); got != tc.want {
			t.Fatalf("permitted(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	if p.permitted("garbage") {
		t.Fatal("unparseable peer must be refused under an explicit allowlist")
	}
	if p.restricted() != true {
		t.Fatal("policy with entries must be restricted")
	}
	if err := p.configure(nil, nil); err != nil || p.restricted() {
		t.Fatal("empty policy must be accepted and unrestricted")
	}
}
