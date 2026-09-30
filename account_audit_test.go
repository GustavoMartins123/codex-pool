package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func TestAccountActionsHaveAtomicSanitizedAudit(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base))}
	attachContributionFixture(t, h, "alice")
	alice, csrf := addTestPassportOperator(t, h.passport, "alice")
	bob, bobCSRF := addTestPassportOperator(t, h.passport, "bob")
	id, err := h.saveContribution(contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice"), AccountTypeKimi, "secret-key", map[string]any{"api_key": "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/accounts", "/admin/kimi", "/api/pool/stats"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, passportContributionRequest("GET", path, bob, "", nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), id) || strings.Contains(w.Body.String(), hashAccountID(id)) {
			t.Fatalf("foreign visibility %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/admin/accounts/" + id + "/disable", "/admin/kimi/" + id + "/remove"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, passportContributionRequest("POST", path, bob, bobCSRF, nil))
		if w.Code != 403 {
			t.Fatalf("foreign mutation %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("POST", "/api/me/accounts/kimi/"+id+"/withdraw", alice, csrf, []byte(`{"revision":2}`)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	entries, err := h.passport.recentAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		if e.SubjectID == id {
			counts[e.Action]++
			if e.ActorID != "alice" {
				t.Fatal(e)
			}
		}
	}
	for _, action := range []string{"account.contribution_started", "account.contributed", "account.withdrawn"} {
		if counts[action] != 1 {
			t.Fatalf("audit %s: %+v", action, counts)
		}
	}
	raw, _ := json.Marshal(entries)
	if strings.Contains(string(raw), "secret-key") || strings.Contains(string(raw), ".json") {
		t.Fatal("audit leaked credentials")
	}
}

func TestMissingAuditRollsBackAccountChanges(t *testing.T) {
	p, pool := ownershipFixture(t)
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte(bucketPassportAudit)) }); err != nil {
		t.Fatal(err)
	}
	if err := p.withdrawAccount("alice", AccountTypeCodex, "private", 1); err == nil {
		t.Fatal("withdrawal succeeded without audit")
	}
	if err := p.authorizeAccount("alice", pool.allAccounts()[1], "use"); err != nil {
		t.Fatal("failed audit left tombstone")
	}
}
