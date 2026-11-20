package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestQwenRemoteBrowserLogin(t *testing.T) {
	if os.Getenv("CHROME_PATH") == "" {
		t.Skip("CHROME_PATH is required for the HTTPS browser integration test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node.js is required for the HTTPS browser integration test")
	}
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me/clients", "/api/me/passkeys":
			respondJSON(w, []any{})
		case "/api/me/usage":
			respondJSON(w, map[string]any{"hourly": []any{}})
		default:
			h.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	certificateKey := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	target, _ := url.Parse(server.URL)
	target.Host = "pool.example.test:" + target.Port()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, filepath.Join("web", "qwen-remote.browser.mjs"))
	command.Env = append(os.Environ(), "QWEN_TEST_ORIGIN="+target.String(), "QWEN_TEST_SESSION="+session, "QWEN_TEST_CSRF="+csrf, "QWEN_TEST_TLS_SPKI="+base64.StdEncoding.EncodeToString(certificateKey[:]))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("HTTPS browser flow failed: %v\n%s", err, output)
	}
	t.Log(string(output))
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 || accounts[0].Type != AccountTypeQwen || accounts[0].qwen.TokenPlan.CloudOAuth == nil || accounts[0].qwen.Credits == nil || accounts[0].qwen.Credits.Remaining != 33750 || len(accounts[0].Models) != 1 {
		t.Fatal("browser flow did not persist the private Qwen account, discovered models and automatic credits")
	}
	if err := h.passport.authorizeAccount("browser-owner", accounts[0], "manage"); err != nil {
		t.Fatal("browser flow lost private account ownership")
	}
}
