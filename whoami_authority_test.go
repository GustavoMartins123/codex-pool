package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWhoamiUsesLivePassportCredential(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-for-whoami")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, client, _, err := passport.createGuest("operator", "Guest", "Guest", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := principal.ID + "-c-" + client.ID
	h := &proxyHandler{cfg: &config{}, passport: passport}
	check := func(token, want string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/pool/whoami", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		var body struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Type != want {
			t.Fatalf("whoami type = %q, want %q", body.Type, want)
		}
	}
	active := generateClaudePoolTokenAt(getPoolJWTSecret(), identity, time.Now().Add(-time.Minute))
	check(active, "pool_user")
	if err := passport.revokeClient("operator", principal.ID, client.ID); err != nil {
		t.Fatal(err)
	}
	check(active, "anonymous")
	second, err := passport.createClient(principal.ID, "Second", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity := principal.ID + "-c-" + second.ID
	preCutoff := generateClaudePoolTokenAt(getPoolJWTSecret(), secondIdentity, time.Now().Add(-time.Minute))
	check(preCutoff, "pool_user")
	if _, err := passport.setPrincipalStatus("operator", principal.ID, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	check(preCutoff, "anonymous")
	if _, err := passport.setPrincipalStatus("operator", principal.ID, PrincipalActive); err != nil {
		t.Fatal(err)
	}
	check(preCutoff, "anonymous")
}
