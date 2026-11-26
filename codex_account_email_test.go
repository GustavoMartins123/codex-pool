package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexAccountsExposeChatGPTEmail(t *testing.T) {
	file := filepath.Join(t.TempDir(), "member.json")
	data, err := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token":  jwtWithExp(time.Now().Add(time.Hour).Unix()),
		"id_token":      codexTestIDToken(t, testCodexWorkspace, "user-a", " a@example.com "),
		"refresh_token": "refresh",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://auth.example")
	provider := NewCodexProvider(base, base, base)
	acc, err := provider.LoadAccount(filepath.Base(file), file, data)
	if err != nil || acc == nil {
		t.Fatalf("load: %v", err)
	}
	if acc.Email != "a@example.com" {
		t.Fatalf("loaded email=%q", acc.Email)
	}

	acc.vault = plainTestVault()
	refreshed := codexTestIDToken(t, testCodexWorkspace, "user-a", "renamed@example.com")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]string{"access_token": jwtWithExp(time.Now().Add(2 * time.Hour).Unix()), "refresh_token": "next", "id_token": refreshed})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if err := provider.RefreshToken(context.Background(), acc, transport); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if acc.Email != "renamed@example.com" {
		t.Fatalf("refreshed email=%q", acc.Email)
	}

	withoutEmail := codexTestIDToken(t, testCodexWorkspace, "user-a", "")
	transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]string{"access_token": jwtWithExp(time.Now().Add(3 * time.Hour).Unix()), "id_token": withoutEmail})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if err := provider.RefreshToken(context.Background(), acc, transport); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if acc.Email != "renamed@example.com" {
		t.Fatalf("email cleared by a token without one: %q", acc.Email)
	}
}
