package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func newNonceTestPassport(t *testing.T) (*PassportStore, *ClientCredential) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := passport.createMemberLink("operator", "nonce@example.com", "N", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	client, err := passport.createClient(passport.byEmail("nonce@example.com").ID, "machine", nil)
	if err != nil {
		t.Fatal(err)
	}
	return passport, client
}

func TestConfigDownloadNonceIsSingleUse(t *testing.T) {
	passport, client := newNonceTestPassport(t)
	nonce, expires, err := passport.mintConfigDownloadNonce(client)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(expires) <= 0 || time.Until(expires) > configDownloadNonceTTL {
		t.Fatalf("expiry = %v", expires)
	}
	if got := passport.redeemConfigDownloadNonce(nonce); got == nil || got.ID != client.ID {
		t.Fatalf("first redeem = %+v", got)
	}
	if got := passport.redeemConfigDownloadNonce(nonce); got != nil {
		t.Fatal("nonce was reusable")
	}
	if got := passport.redeemConfigDownloadNonce(""); got != nil {
		t.Fatal("empty nonce redeemed")
	}
	if got := passport.redeemConfigDownloadNonce("totally-unknown-nonce"); got != nil {
		t.Fatal("unknown nonce redeemed")
	}
}

func TestConfigDownloadNonceMintRevokesPrevious(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-config-nonce")
	passport, client := newNonceTestPassport(t)
	handler := &proxyHandler{cfg: &config{}, passport: passport}

	first, _, err := passport.mintConfigDownloadNonce(client)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := handler.mintSetupURLs(httptest.NewRequest(http.MethodGet, "/", nil), client); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	handler.serveConfigDownload(recorder, httptest.NewRequest(http.MethodGet, "/config/codex/"+first, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("stale nonce after regeneration status = %d, want 404", recorder.Code)
	}
}

func TestConfigDownloadNonceRejectsExpired(t *testing.T) {
	passport, client := newNonceTestPassport(t)
	nonce, _, err := passport.mintConfigDownloadNonce(client)
	if err != nil {
		t.Fatal(err)
	}
	nonceDigest := hashToken(nonce)
	digest := hexEncode(nonceDigest[:])
	stale := configDownloadNonce{ClientID: client.ID, PrincipalID: client.PrincipalID, ExpiresAt: time.Now().UTC().Add(-time.Minute)}
	value, _ := json.Marshal(stale)
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketConfigNonces)).Put([]byte(digest), value)
	}); err != nil {
		t.Fatal(err)
	}
	if got := passport.redeemConfigDownloadNonce(nonce); got != nil {
		t.Fatal("expired nonce redeemed")
	}
}

func TestConfigDownloadNonceRevokedClient(t *testing.T) {
	passport, client := newNonceTestPassport(t)
	if err := passport.revokeClient(client.PrincipalID, client.PrincipalID, client.ID); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := passport.mintConfigDownloadNonce(client)
	if err != nil {
		t.Fatal(err)
	}
	if got := passport.redeemConfigDownloadNonce(nonce); got != nil {
		t.Fatal("revoked client nonce redeemed")
	}
}

func TestConfigDownloadNonceHTTPFlow(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-config-nonce")
	passport, client := newNonceTestPassport(t)
	session, csrf, err := passport.createSession(client.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	handler := &proxyHandler{cfg: &config{}, passport: passport}

	request := httptest.NewRequest(http.MethodPost, "/api/me/clients", strings.NewReader(`{"label":"machine"}`))
	request.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
	request.AddCookie(&http.Cookie{Name: "pool_csrf", Value: csrf})
	request.Header.Set("X-CSRF-Token", csrf)
	recorder := httptest.NewRecorder()
	handler.handlePassportClients(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create response: %s", recorder.Body.String())
	}

	linkRequest := httptest.NewRequest(http.MethodPost, "/api/me/clients/"+created.ID+"/setup-link", nil)
	linkRequest.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
	linkRequest.AddCookie(&http.Cookie{Name: "pool_csrf", Value: csrf})
	linkRequest.Header.Set("X-CSRF-Token", csrf)
	linkRecorder := httptest.NewRecorder()
	handler.handlePassportClientItem(linkRecorder, linkRequest)
	if linkRecorder.Code != http.StatusOK {
		t.Fatalf("setup-link status = %d body=%s", linkRecorder.Code, linkRecorder.Body.String())
	}
	var linked struct {
		SetupURLs map[string]string `json:"setup_urls"`
	}
	if err := json.Unmarshal(linkRecorder.Body.Bytes(), &linked); err != nil {
		t.Fatal(err)
	}
	url, ok := linked.SetupURLs["codex"]
	if !ok || !strings.Contains(url, "/config/codex/") {
		t.Fatalf("setup_urls = %#v", linked.SetupURLs)
	}

	download := httptest.NewRequest(http.MethodGet, url, nil)
	first := httptest.NewRecorder()
	handler.serveConfigDownload(first, download)
	if first.Code != http.StatusOK || !strings.Contains(first.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("nonce download = %d %s", first.Code, first.Body.String())
	}

	replay := httptest.NewRequest(http.MethodGet, url, nil)
	second := httptest.NewRecorder()
	handler.serveConfigDownload(second, replay)
	if second.Code != http.StatusNotFound {
		t.Fatalf("nonce replay status = %d, want 404", second.Code)
	}
}

func TestConfigDownloadLegacyTokenIsRejected(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-config-nonce")
	passport, client := newNonceTestPassport(t)
	handler := &proxyHandler{cfg: &config{}, passport: passport}
	token, err := passport.clientDownloadToken(client)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.serveConfigDownload(recorder, httptest.NewRequest(http.MethodGet, "/config/codex/"+token, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("legacy download token status = %d, want 404 (nonce-only)", recorder.Code)
	}
}
