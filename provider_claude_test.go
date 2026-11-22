package main

import (
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
