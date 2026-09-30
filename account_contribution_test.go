package main

import (
	"codex-pool-proxy/internal/credstore"
	"context"
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func attachContributionFixture(t *testing.T, h *proxyHandler, actor string) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "contribution-test-key")
	store := testUsageStore(t)
	p, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	addTestPassportOperator(t, p, actor)
	h.passport = p
	if h.pool == nil {
		h.pool = newPoolState(nil, false)
	}
	if err := p.initializeAccountAuthority(h.pool.allAccounts()); err != nil {
		t.Fatal(err)
	}
	h.pool.accountAuthority = p
	useKeyedVault(t, hexKey(t, 1, 'c'), nil)
}

func contributionRequestActor(r *http.Request, actor string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, actor))
}

func TestPrivateContributionCapabilityAndDeduplication(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir(), kimiBase: base}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base)), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	attachContributionFixture(t, h, "operator")
	session, csrf := addTestPassportOperator(t, h.passport, "member")
	h.passport.mu.Lock()
	member := *h.passport.principals["member"]
	member.Kind = PrincipalMember
	h.passport.principals[member.ID] = &member
	h.passport.mu.Unlock()
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error { return putJSON(tx.Bucket([]byte(bucketPrincipals)), member.ID, &member) }); err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		return passportContributionRequest("POST", "/api/pool/accounts/kimi/add", session, csrf, []byte(`{"api_key":"private-key","owner_id":"operator"}`))
	}
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, request())
	if denied.Code != 403 {
		t.Fatalf("missing capability: %d", denied.Code)
	}
	opSession, opCSRF := addTestPassportOperator(t, h.passport, "policy-operator")
	permission := httptest.NewRecorder()
	h.ServeHTTP(permission, passportContributionRequest("PUT", "/api/console/account-contribution", opSession, opCSRF, []byte(`{"principal_id":"member","can_contribute":true}`)))
	if permission.Code != 200 {
		t.Fatalf("grant: %d %s", permission.Code, permission.Body.String())
	}
	saved := httptest.NewRecorder()
	h.ServeHTTP(saved, request())
	if saved.Code != 200 {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts=%d", len(accounts))
	}
	if err := h.passport.authorizeAccount("member", accounts[0], "use"); err != nil {
		t.Fatal(err)
	}
	if h.passport.authorizeAccount("operator", accounts[0], "use") == nil {
		t.Fatal("body changed ownership")
	}
	assertEncryptedCredentialFile(t, accounts[0].File, "private-key")
	duplicate := httptest.NewRecorder()
	h.ServeHTTP(duplicate, request())
	if duplicate.Code != 409 {
		t.Fatalf("duplicate: %d %s", duplicate.Code, duplicate.Body.String())
	}
}

func TestContributionPendingSurvivesFailureAndCanResume(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base))}
	attachContributionFixture(t, h, "alice")
	_, err := h.persistContribution("alice", AccountTypeKimi, "key", func(path string) error {
		if err := writeAccountFile(path, []byte(`{"api_key":"key"}`)); err != nil {
			return err
		}
		return errors.New("interrupted before activation")
	})
	if err == nil {
		t.Fatal("failure hidden")
	}
	h.reloadAccounts()
	if len(h.pool.visiblePool("alice").allAccounts()) != 0 {
		t.Fatal("pending contribution admitted")
	}
	p, err := newPassportStore(h.passport.db)
	if err != nil {
		t.Fatal(err)
	}
	h.passport = p
	h.pool.accountAuthority = p
	r := contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice")
	id, err := h.saveContribution(r, AccountTypeKimi, "key", map[string]any{"api_key": "key"})
	if err != nil || id == "" {
		t.Fatalf("resume: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(h.cfg.poolDir, "kimi"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%d err=%v", len(files), err)
	}
	if _, err := h.saveContribution(r, AccountTypeKimi, "key", map[string]any{"api_key": "key"}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestContributionConcurrentDuplicateAndInvalidSecret(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base))}
	attachContributionFixture(t, h, "alice")
	r := contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := h.saveContribution(r, AccountTypeKimi, "same-key", map[string]any{"api_key": "same-key"})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d", accepted)
	}
	if _, err := h.saveContribution(r, AccountTypeKimi, "invalid", map[string]any{}); err == nil {
		t.Fatal("invalid credential activated")
	}
	if len(h.pool.visiblePool("alice").allAccounts()) != 1 {
		t.Fatal("invalid secret admitted")
	}
	entries, _ := os.ReadDir(filepath.Join(h.cfg.poolDir, "kimi"))
	for _, e := range entries {
		raw, _ := os.ReadFile(filepath.Join(h.cfg.poolDir, "kimi", e.Name()))
		var envelope map[string]any
		if json.Unmarshal(raw, &envelope) != nil || envelope["cpvault"] == nil || strings.Contains(string(raw), "same-key") {
			t.Fatal("vault bypassed")
		}
	}
}

func TestContributionVaultAndOAuthSessionAreFailClosed(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base))}
	attachContributionFixture(t, h, "alice")
	r := contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice")
	keyed := accountCredentialStore
	accountCredentialStore = credstore.PlainStore{}
	_, err := h.saveContribution(r, AccountTypeKimi, "key", map[string]any{"api_key": "key"})
	accountCredentialStore = keyed
	if err == nil {
		t.Fatal("plaintext contribution accepted")
	}
	_, verifier, _ := startCodexOAuthSession("alice", "")
	codexOAuthSessions.Lock()
	codexOAuthSessions.sessions[verifier].CreatedAt = time.Now().Add(-11 * time.Minute)
	codexOAuthSessions.Unlock()
	body, _ := json.Marshal(map[string]string{"code": "unused", "verifier": verifier})
	request := contributionRequestActor(httptest.NewRequest("POST", "/", strings.NewReader(string(body))), "alice")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.handleCodexExchange(response, request)
	if response.Code != 400 {
		t.Fatalf("expired session: %d", response.Code)
	}
	codexOAuthSessions.RLock()
	_, exists := codexOAuthSessions.sessions[verifier]
	codexOAuthSessions.RUnlock()
	if exists {
		t.Fatal("consumed session retained")
	}
}
