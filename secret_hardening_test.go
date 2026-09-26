package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecureSecretEquals(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"equal secrets", "correct-horse-battery-staple", "correct-horse-battery-staple", true},
		{"same length different value", "correct-horse-battery-staple", "wrong-horse-battery-staple", false},
		{"different lengths", "short", "a-much-longer-secret-value", false},
		{"both empty", "", "", true},
		{"one empty", "", "secret", false},
		{"common prefix", "secret-prefix-a", "secret-prefix-b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := secureSecretEquals(tc.a, tc.b); got != tc.want {
				t.Fatalf("secureSecretEquals(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestValidateSecretStrength(t *testing.T) {
	previous := globalConfigFile
	globalConfigFile = nil
	t.Cleanup(func() { globalConfigFile = previous })

	strong := strings.Repeat("s", 32)

	t.Run("strong secrets accepted", func(t *testing.T) {
		t.Setenv("POOL_JWT_SECRET", strong)
		t.Setenv("POOL_AUTH_ENCRYPTION_KEY", strong)
		if err := validateSecretStrength(&config{adminToken: strings.Repeat("a", 16)}); err != nil {
			t.Fatalf("strong secrets rejected: %v", err)
		}
	})

	t.Run("weak jwt secret rejected", func(t *testing.T) {
		t.Setenv("POOL_JWT_SECRET", "bigfarts")
		t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "")
		if err := validateSecretStrength(&config{}); err == nil {
			t.Fatal("short POOL_JWT_SECRET must refuse startup")
		}
	})

	t.Run("weak encryption key rejected", func(t *testing.T) {
		t.Setenv("POOL_JWT_SECRET", "")
		t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "short-key")
		if err := validateSecretStrength(&config{}); err == nil {
			t.Fatal("short POOL_AUTH_ENCRYPTION_KEY must refuse startup")
		}
	})

	t.Run("weak admin token rejected", func(t *testing.T) {
		t.Setenv("POOL_JWT_SECRET", "")
		t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "")
		if err := validateSecretStrength(&config{adminToken: "admin"}); err == nil {
			t.Fatal("short ADMIN_TOKEN must refuse startup")
		}
	})

	t.Run("unset secrets accepted", func(t *testing.T) {
		t.Setenv("POOL_JWT_SECRET", "")
		t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "")
		if err := validateSecretStrength(&config{}); err != nil {
			t.Fatalf("unset secrets rejected: %v", err)
		}
	})
}

// The admin gate must keep working exactly the same with the constant-time
// comparison: matching token passes, mismatched token fails.
func TestAdminTokenComparisonSemantics(t *testing.T) {
	h := &proxyHandler{cfg: &config{adminToken: "operator-break-glass-token"}}

	valid := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	valid.Header.Set("X-Admin-Token", "operator-break-glass-token")
	if !h.checkAdminAuth(httptest.NewRecorder(), valid) {
		t.Fatal("matching admin token was rejected")
	}

	invalid := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	invalid.Header.Set("X-Admin-Token", "operator-break-glass-tokenX")
	if h.checkAdminAuth(httptest.NewRecorder(), invalid) {
		t.Fatal("mismatched admin token was accepted")
	}
}
