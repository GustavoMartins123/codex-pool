package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestMyAccountsWithdrawalIsPrivateAndDurable(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base))}
	attachContributionFixture(t, h, "alice")
	alice, csrf := addTestPassportOperator(t, h.passport, "alice")
	bob, bobCSRF := addTestPassportOperator(t, h.passport, "bob")
	id, err := h.saveContribution(contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice"), AccountTypeKimi, "key", map[string]any{"api_key": "key"})
	if err != nil {
		t.Fatal(err)
	}
	list := httptest.NewRecorder()
	h.ServeHTTP(list, passportContributionRequest("GET", "/api/me/accounts", alice, "", nil))
	var accounts []myAccountView
	if json.Unmarshal(list.Body.Bytes(), &accounts) != nil || len(accounts) != 1 {
		t.Fatalf("list: %s", list.Body.String())
	}
	foreign := httptest.NewRecorder()
	h.ServeHTTP(foreign, passportContributionRequest("GET", "/api/me/accounts", bob, "", nil))
	if foreign.Body.String() != "[]\n" {
		t.Fatalf("foreign list: %s", foreign.Body.String())
	}
	path := "/api/me/accounts/kimi/" + id + "/withdraw"
	body, _ := json.Marshal(map[string]any{"revision": accounts[0].Revision})
	forbidden := httptest.NewRecorder()
	h.ServeHTTP(forbidden, passportContributionRequest("POST", path, bob, bobCSRF, body))
	if forbidden.Code != 403 {
		t.Fatalf("foreign withdraw: %d", forbidden.Code)
	}
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, passportContributionRequest("POST", path, alice, csrf, []byte(`{"revision":1}`)))
	if stale.Code != 409 {
		t.Fatalf("stale revision: %d", stale.Code)
	}
	h.pool.pin("conversation", id)
	withdrawn := httptest.NewRecorder()
	h.ServeHTTP(withdrawn, passportContributionRequest("POST", path, alice, csrf, body))
	if withdrawn.Code != 200 {
		t.Fatalf("withdraw: %d %s", withdrawn.Code, withdrawn.Body.String())
	}
	if h.pool.candidateForUser("alice", "conversation", nil, AccountTypeKimi, "", "") != nil {
		t.Fatal("withdrawn account admitted")
	}
	reloaded, err := newPassportStore(h.passport.db)
	if err != nil {
		t.Fatal(err)
	}
	h.passport = reloaded
	h.pool.accountAuthority = reloaded
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	if len(h.pool.visiblePool("alice").allAccounts()) != 0 {
		t.Fatal("restart resurrected withdrawal")
	}
	if _, err := h.saveContribution(contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice"), AccountTypeKimi, "key", map[string]any{"api_key": "key"}); err == nil {
		t.Fatal("withdrawn identity reactivated")
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, passportContributionRequest("GET", "/api/me/accounts", alice, "", nil))
	if json.Unmarshal(response.Body.Bytes(), &accounts) != nil || accounts[0].Status != "withdrawn" {
		t.Fatalf("durable state: %s", response.Body.String())
	}
}
