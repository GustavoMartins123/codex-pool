package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestClaudeProviderLoadAccountFlags(t *testing.T) {
	t.Parallel()

	baseURL, err := url.Parse("https://api.anthropic.com")
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	p := NewClaudeProvider(baseURL)

	data := []byte(`{
  "dead": true,
  "disabled": true,
  "allowed_ip": "199.45.144.95",
  "allowed_source_ips": ["199.45.144.95"],
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat-test",
    "refreshToken": "rt-test",
    "expiresAt": 1893456000000,
    "subscriptionType": "pro"
  }
}`)

	acc, err := p.LoadAccount("claude_test.json", "/tmp/claude_test.json", data)
	if err != nil {
		t.Fatalf("LoadAccount error: %v", err)
	}
	if acc == nil {
		t.Fatal("LoadAccount returned nil account")
	}
	if !acc.Dead {
		t.Fatal("expected dead=true")
	}
	if !acc.Disabled {
		t.Fatal("expected disabled=true")
	}
	if len(acc.AllowedSourceIPs) != 2 || acc.AllowedSourceIPs[0] != "199.45.144.95" || acc.AllowedSourceIPs[1] != "199.45.144.95" {
		t.Fatalf("expected allowed source IPs loaded, got %#v", acc.AllowedSourceIPs)
	}
}

func TestClaudeInjectMetadataAddsClaudeCodeShape(t *testing.T) {
	id := &claudeIdentity{}
	body := map[string]any{}
	id.injectMetadata(body, "", "user-abc", "sess-xyz")

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing or wrong type: %#v", body["metadata"])
	}
	userID, ok := metadata["user_id"].(string)
	if !ok || userID == "" {
		t.Fatalf("metadata.user_id missing: %#v", metadata["user_id"])
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(userID), &payload); err != nil {
		t.Fatalf("unmarshal metadata.user_id: %v", err)
	}
	if payload["device_id"] == "" {
		t.Fatal("device_id missing")
	}
	if payload["device_id"] != id.userDeviceID("user-abc") {
		t.Fatalf("device_id = %q, want stable per-user id", payload["device_id"])
	}
	if payload["session_id"] != "sess-xyz" {
		t.Fatalf("session_id = %q, want %q", payload["session_id"], "sess-xyz")
	}
	if payload["account_uuid"] != "" {
		t.Fatalf("account_uuid = %q, want empty", payload["account_uuid"])
	}
}

func TestClaudeInjectMetadataPreservesExistingMetadata(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{"user_id": "keep-me"},
	}
	(&claudeIdentity{}).injectMetadata(body, "ignored", "user-abc", "sess-xyz")

	metadata := body["metadata"].(map[string]any)
	if metadata["user_id"] != "keep-me" {
		t.Fatalf("metadata overwritten: %#v", metadata)
	}
}

func TestCCSessionHeaderPrefersClientHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", nil)
	req.Header.Set("X-Claude-Code-Session-Id", "client-supplied")
	if got := (&claudeIdentity{}).sessionHeader(req, "user-abc"); got != "client-supplied" {
		t.Fatalf("sessionHeader = %q, want client-supplied", got)
	}
}

func TestCCSessionHeaderFallsBackToStablePerUserID(t *testing.T) {
	id := &claudeIdentity{}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", nil)
	got := id.sessionHeader(req, "user-abc")
	again := id.sessionHeader(req, "user-abc")
	if got == "" || got != again {
		t.Fatalf("expected stable per-user fallback, got %q vs %q", got, again)
	}
	other := id.sessionHeader(req, "user-different")
	if other == got {
		t.Fatalf("expected distinct ids per user, both = %q", got)
	}
}

func TestClaudeIdentityIsScopedToItsInstance(t *testing.T) {
	first, second := &claudeIdentity{}, &claudeIdentity{}
	if first.session() == second.session() {
		t.Fatal("distinct identities share a session fallback")
	}
	if first.device() == second.device() {
		t.Fatal("distinct identities share a device fallback")
	}
	if got, want := first.userFallbackSessionID("user-abc"), second.userFallbackSessionID("user-abc"); got != want {
		t.Fatalf("per-user session id differs across identities: %q vs %q", got, want)
	}
	if got, want := first.userDeviceID("user-abc"), second.userDeviceID("user-abc"); got != want {
		t.Fatalf("per-user device id differs across identities: %q vs %q", got, want)
	}
}
