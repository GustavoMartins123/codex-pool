package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedactSecrets(t *testing.T) {
	cases := []struct{ name, in, wantSub string }{
		{"bearer header", `Authorization: Bearer eyJhbGciOi.eyJzdWIi.sigpart`, "Bearer <redacted>"},
		{"basic header", `proxy-authorization: Basic dXNlcjpwYXNzd29yZA==`, "Basic <redacted>"},
		{"openai key", `key=sk-proj-AbCdEf1234567890123456`, "sk-<redacted>"},
		{"jwt", `token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4`, "<redacted-jwt>"},
		{"json access token", `{"access_token":"ya29.a0AfH6SMBx1234567890"}`, `{"access_token":"<redacted>"}`},
		{"json refresh token", `{"refresh_token":"1//0abcDEF1234567890"}`, `{"refresh_token":"<redacted>"}`},
		{"kv password", `password=SuperSecretValue12345`, "password=<redacted>"},
		{"query api key", `url?api_key=AIzaSyABCDEF1234567890`, "api_key=<redacted>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("redactSecrets(%q) = %q, want substring %q", tc.in, got, tc.wantSub)
			}
			if strings.Contains(got, "SuperSecretValue12345") || strings.Contains(got, "AbCdEf1234567890123456") {
				t.Fatalf("secret survived redaction: %q", got)
			}
		})
	}
}

func TestRedactSecretsKeepsBenignText(t *testing.T) {
	benign := []string{
		`{"error":{"code":"token_expired","message":"upstream token expired"}}`,
		`data: {"type":"response.completed","sequence_number":42}`,
		`claude usage fetch codex-1: 5hr=42.0% 7day=12.3%`,
		`route selected account=codex-2 score=1.37 reason=conversation_pin`,
	}
	for _, s := range benign {
		if got := redactSecrets(s); got != s {
			t.Errorf("redactSecrets changed benign text:\n in: %s\ngot: %s", s, got)
		}
	}
}

func TestSafeTextRedactsSecrets(t *testing.T) {
	body := []byte(`{"model":"gpt-5","api_key":"sk-abcdef1234567890abcdef"}`)
	got := safeText(body)
	if strings.Contains(got, "sk-abcdef") {
		t.Fatalf("safeText leaked secret: %s", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Fatalf("safeText dropped benign content: %s", got)
	}
}

func TestWithHashWindow(t *testing.T) {
	// Fixed times avoid hour-boundary flakiness with time.Now().
	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	if got := withHashWindow("salt", 0, base); got != "salt" {
		t.Fatalf("zero window must keep salt stable, got %q", got)
	}
	a := withHashWindow("salt", time.Hour, base)
	b := withHashWindow("salt", time.Hour, base.Add(30*time.Minute))
	if a != b {
		t.Fatalf("salt must be stable within the window: %q vs %q", a, b)
	}
	c := withHashWindow("salt", time.Hour, base.Add(2*time.Hour))
	if a == c {
		t.Fatalf("salt must rotate across windows: %q == %q", a, c)
	}
}

func TestTraceClientIPHashesInPrivacyMode(t *testing.T) {
	h := &proxyHandler{cfg: &config{}}
	if got := h.traceClientIP("203.0.113.9"); got != "203.0.113.9" {
		t.Fatalf("raw IP must be preserved when privacy is off, got %q", got)
	}
	h.cfg.ipPrivacy = true
	hashed := h.traceClientIP("203.0.113.9")
	if hashed == "" || hashed == "203.0.113.9" || strings.Contains(hashed, ".") {
		t.Fatalf("privacy mode must return a hashed IP, got %q", hashed)
	}
	if again := h.traceClientIP("203.0.113.9"); again != hashed {
		t.Fatalf("hash must be deterministic within the window: %q vs %q", hashed, again)
	}
	if other := h.traceClientIP("198.51.100.1"); other == hashed {
		t.Fatal("different IPs must hash differently")
	}
}

func TestWipeOriginRawIPsAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := s.recordOriginMetadata("ip_old", "203.0.113.1", "user-a", "ua", "/v1/responses", old); err != nil {
		t.Fatal(err)
	}
	if err := s.recordOriginMetadata("ip_fresh", "203.0.113.2", "user-b", "ua", "/v1/responses", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Simulate a restart with privacy mode on.
	s2, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.wipeOriginRawIPs(); err != nil {
		t.Fatal(err)
	}
	metas, err := s2.getAllOriginMetadata()
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range metas {
		if meta.RawIP != "" {
			t.Fatalf("raw IP survived the wipe: %+v", meta)
		}
	}

	// Retention prunes the stale entry only.
	if err := s2.pruneOriginMetadata(48 * time.Hour); err != nil {
		t.Fatal(err)
	}
	metas, err = s2.getAllOriginMetadata()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, meta := range metas {
		ids[meta.OriginID] = true
	}
	if ids["ip_old"] {
		t.Fatal("stale origin metadata survived retention prune")
	}
	if !ids["ip_fresh"] {
		t.Fatal("fresh origin metadata was pruned unexpectedly")
	}
}
