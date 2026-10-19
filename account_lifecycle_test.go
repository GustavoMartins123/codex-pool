package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpstreamTokenRevoked(t *testing.T) {
	for _, header := range []string{"X-Openai-Ide-Error-Code", "X-Openai-Ide-Root-Error-Code"} {
		resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)}
		resp.Header.Set(header, "token_revoked")
		if !upstreamTokenRevoked(resp) {
			t.Fatalf("%s was not recognized", header)
		}
	}
	if upstreamTokenRevoked(&http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)}) {
		t.Fatal("missing token error code was treated as revoked")
	}
	if upstreamTokenRevoked(&http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Openai-Ide-Error-Code": {"token_revoked"}}}) {
		t.Fatal("forbidden response was treated as token revoked")
	}
}

func TestApplyProxyAuthFailure(t *testing.T) {
	t.Run("codex proxy auth failure stays lightweight", func(t *testing.T) {
		acc := &Account{Type: AccountTypeCodex}

		markedDead, penaltyNow := applyProxyAuthFailure(acc, false)

		if markedDead {
			t.Fatal("codex account should not be marked dead from proxy auth failure")
		}
		if acc.Dead {
			t.Fatal("codex account unexpectedly marked dead")
		}
		if penaltyNow != 0.2 || acc.Penalty != 0.2 {
			t.Fatalf("codex penalty = %v, want 0.2", acc.Penalty)
		}
	})

	t.Run("static API key proxy auth failure never retires account", func(t *testing.T) {
		for _, accountType := range []AccountType{AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi} {
			acc := &Account{Type: accountType}
			markedDead, penaltyNow := applyProxyAuthFailure(acc, true)
			if markedDead || acc.Dead {
				t.Fatalf("%s account should require provider validation before being marked dead", accountType)
			}
			if penaltyNow != 0.2 || acc.Penalty != 0.2 {
				t.Fatalf("%s penalty = %v, want 0.2", accountType, acc.Penalty)
			}
		}
	})

	t.Run("non-codex proxy auth failure stays severe before refresh failure", func(t *testing.T) {
		acc := &Account{Type: AccountTypeClaude}

		markedDead, penaltyNow := applyProxyAuthFailure(acc, false)

		if markedDead {
			t.Fatal("account should not be marked dead before refresh failure")
		}
		if acc.Dead {
			t.Fatal("account unexpectedly marked dead")
		}
		if penaltyNow != 10.0 || acc.Penalty != 10.0 {
			t.Fatalf("penalty = %v, want 10.0", acc.Penalty)
		}
	})

	t.Run("non-codex refresh failure marks account dead", func(t *testing.T) {
		acc := &Account{Type: AccountTypeClaude}

		markedDead, penaltyNow := applyProxyAuthFailure(acc, true)

		if !markedDead {
			t.Fatal("expected account to be marked dead after refresh failure")
		}
		if !acc.Dead {
			t.Fatal("account should be marked dead")
		}
		if penaltyNow != 1.0 || acc.Penalty != 1.0 {
			t.Fatalf("penalty = %v, want 1.0", acc.Penalty)
		}
	})
}

func TestRestoreValidatedStaticAccountPersistsHealthyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minimax.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"secret","dead":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	account := &Account{ID: "minimax", Type: AccountTypeMinimax, File: path, AccessToken: "secret", Dead: true, Penalty: 100}

	restoreValidatedAccount(account, "test")

	if account.Dead || account.Penalty != 0 {
		t.Fatalf("account state = dead %v penalty %v", account.Dead, account.Penalty)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"dead"`) {
		t.Fatalf("persisted account still dead: %s", data)
	}
}

func TestMarkAccountNeedsLoginPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.json")
	if err := os.WriteFile(path, []byte(`{"tokens":{"access_token":"a","refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := &Account{ID: "codex", Type: AccountTypeCodex, File: path, AccessToken: "a", RefreshToken: "r"}

	markAccountNeedsLogin(acc, "refresh failed: 400 Bad Request: invalid_grant")

	if !acc.NeedsLogin || acc.AuthError == "" {
		t.Fatalf("needs login marker missing: %+v", acc)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"needs_login": true`) || !strings.Contains(string(data), `"auth_error"`) {
		t.Fatalf("needs-login marker not persisted: %s", data)
	}

	// A fresh load restores the marker so restarts keep the relogin warning.
	loaded, err := (&CodexProvider{}).LoadAccount("codex", path, data)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.NeedsLogin || loaded.AuthError == "" {
		t.Fatalf("loaded account lost needs-login state: %+v", loaded)
	}

	// Re-marking with the same reason must not rewrite the file: strip
	// auth_error on disk and confirm a deduped mark leaves it untouched.
	if err := os.WriteFile(path, []byte(`{"tokens":{"access_token":"a","refresh_token":"r"},"needs_login":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	markAccountNeedsLogin(acc, "refresh failed: 400 Bad Request: invalid_grant")
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "auth_error") {
		t.Fatalf("identical needs-login marker rewrote the account file: %s", data)
	}

	clearAccountNeedsLogin(loaded)
	if loaded.NeedsLogin || loaded.AuthError != "" {
		t.Fatalf("needs-login state not cleared: %+v", loaded)
	}
}
