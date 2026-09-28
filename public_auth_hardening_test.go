package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasskeyIssuanceRateLimitAndMethods(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{cfg: &config{}, passport: passport}
	for _, path := range []string{"/api/auth/passkey/begin", "/api/auth/passkey/finish", "/api/me/passkeys/register/begin", "/api/me/passkeys/register/finish"} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", path, response.Code)
		}
	}
	for i := 0; i < passkeyIssueLimit+1; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/passkey/begin", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if i < passkeyIssueLimit && response.Code != http.StatusOK {
			t.Fatalf("challenge %d = %d: %s", i, response.Code, response.Body.String())
		}
		if i == passkeyIssueLimit && response.Code != http.StatusTooManyRequests {
			t.Fatalf("excess challenge = %d, want 429", response.Code)
		}
	}
	other := httptest.NewRequest(http.MethodPost, "/api/auth/passkey/begin", nil)
	other.RemoteAddr = "192.0.2.2:1234"
	response := httptest.NewRecorder()
	h.ServeHTTP(response, other)
	if response.Code != http.StatusOK {
		t.Fatalf("other IP challenge = %d: %s", response.Code, response.Body.String())
	}
}

func TestFakeOAuthTokenBodyLimit(t *testing.T) {
	h := &proxyHandler{cfg: &config{}, passport: &PassportStore{}}
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"grant_type":"refresh_token","refresh_token":"other"}`, http.StatusOK},
		{strings.Repeat("x", 64<<10+1), http.StatusRequestEntityTooLarge},
	} {
		response := httptest.NewRecorder()
		h.serveFakeOAuthToken(response, httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(test.body)))
		if response.Code != test.want {
			t.Fatalf("body size %d = %d, want %d", len(test.body), response.Code, test.want)
		}
	}
}

func TestRecoveryPasswordWorkBusyDoesNotCountAsFailure(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	link, err := passport.createMemberLink("operator", "busy@example.com", "Busy", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(passport.passwordWork); i++ {
		passport.passwordWork <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(passport.passwordWork); i++ {
			<-passport.passwordWork
		}
	})
	tracker := newBruteForceTracker()
	t.Cleanup(tracker.stop)
	h := &proxyHandler{cfg: &config{}, passport: passport, bruteForce: tracker}
	request := httptest.NewRequest(http.MethodPost, "/api/auth/recover", strings.NewReader(`{"token":"`+link.Token+`","password":"valid replacement password"}`))
	request.RemoteAddr = "192.0.2.3:1234"
	response := httptest.NewRecorder()
	h.handleMemberRecovery(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy recovery = %d, want 503: %s", response.Code, response.Body.String())
	}
	tracker.mu.Lock()
	_, counted := tracker.attempts["192.0.2.3"]
	tracker.mu.Unlock()
	if counted {
		t.Fatal("password capacity exhaustion counted as bad credential")
	}
}
