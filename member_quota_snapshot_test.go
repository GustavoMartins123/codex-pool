package main

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func memberQuotaOperator(t *testing.T, h *proxyHandler) string {
	t.Helper()
	h.passport.mu.RLock()
	defer h.passport.mu.RUnlock()
	for _, principal := range h.passport.principals {
		if principal.Kind == PrincipalOperator {
			return principal.ID
		}
	}
	t.Fatal("fixture has no operator")
	return ""
}

func TestMemberQuotaSnapshotReadCount(t *testing.T) {
	for _, extra := range []int{0, 20} {
		t.Run(fmt.Sprintf("extra_accounts_%d", extra), func(t *testing.T) {
			h, id, session, _, _, _ := memberQuotaFixture(t)
			saveQuotaPolicy(t, h, id, ClientPolicy{})
			accounts := h.pool.allAccounts()
			for i := 0; i < extra; i++ {
				accounts = append(accounts, &Account{ID: fmt.Sprintf("extra-%d", i), Type: AccountTypeClaude, Usage: accounts[0].Usage})
			}
			if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
				for _, account := range accounts[3:] {
					resource := accountResource{Version: 2, ID: account.ID, Provider: account.Type, OwnerID: "operator-managed", AddedBy: "operator-managed", OperatorManaged: true, Revision: 1, Status: "active"}
					if err := putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(account.Type, account.ID), resource); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i, account := range accounts[3:] {
				grant := testGrant(fmt.Sprintf("extra-grant-%d", i), id, account)
				grant.Models = []string{"*"}
				if _, err := h.passport.createAccountGrant(memberQuotaOperator(t, h), 1, grant); err != nil {
					t.Fatal(err)
				}
			}
			h.pool.replace(accounts)
			quotaHTTP(t, h, session) // Warm the effective policy cache.
			before := h.passport.db.Stats().TxN
			hits := h.passport.policyCache.hits
			w := httptest.NewRecorder()
			h.handleMyQuotas(w, passportContributionRequest("GET", "/api/me/quotas", session, "", nil))
			if w.Code != 200 {
				t.Fatalf("quota: %d %s", w.Code, w.Body.String())
			}
			if reads := h.passport.db.Stats().TxN - before; reads != 2 {
				t.Fatalf("got %d database reads; want one session read and one authority snapshot", reads)
			}
			if evaluations := h.passport.policyCache.hits - hits; evaluations != 1 {
				t.Fatalf("effective policy evaluated %d times; want one", evaluations)
			}
		})
	}
}

func TestMemberQuotaSnapshotIsolation(t *testing.T) {
	h, id, session, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, id, ClientPolicy{})
	original := h.pool.allAccounts()[0]
	original.Models = map[string]DiscoveredModel{"claude-opus-5-5": {ID: "claude-opus-5-5", Modalities: []string{"text"}}}
	original.ModelRateLimits = map[string]time.Time{"claude-opus-5-5": time.Now().Add(time.Minute)}
	snapshot, err := h.memberQuotaSnapshot(id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	account := snapshot.allAccounts()[0]
	if account == original || account.AccessToken != "" || account.Email != "" {
		t.Fatal("snapshot retained the live account or its credentials")
	}
	original.mu.Lock()
	original.Usage.PrimaryUsedPercent = .95
	original.Models["claude-opus-5-5"].Modalities[0] = "image"
	delete(original.Models, "claude-opus-5-5")
	delete(original.ModelRateLimits, "claude-opus-5-5")
	original.mu.Unlock()
	if account.Usage.PrimaryUsedPercent != .25 || account.Models["claude-opus-5-5"].Modalities[0] != "text" || len(account.ModelRateLimits) != 1 {
		t.Fatal("live mutation changed the request snapshot")
	}
	if err := h.passport.revokeAccountGrant(memberQuotaOperator(t, h), "quota-claude", 1); err != nil {
		t.Fatal(err)
	}
	if !snapshot.catalogAccountAllows(account, "claude-opus-5-5") {
		t.Fatal("in-flight snapshot was mutated by revocation")
	}
	for _, row := range quotaHTTP(t, h, session) {
		if row.Provider == "claude" {
			t.Fatal("next request reused a revoked authorization snapshot")
		}
	}
	saveQuotaPolicy(t, h, id, ClientPolicy{Providers: PolicySelector{Deny: []string{"*"}}})
	if rows := quotaHTTP(t, h, session); len(rows) != 0 {
		t.Fatal("next request reused the previous effective policy")
	}
}

func TestMemberQuotaSnapshotCorruptAuthority(t *testing.T) {
	for _, kind := range []string{"resource", "grant", "grant_index", "missing_resource"} {
		t.Run(kind, func(t *testing.T) {
			h, id, session, _, _, _ := memberQuotaFixture(t)
			saveQuotaPolicy(t, h, id, ClientPolicy{})
			err := h.passport.db.Update(func(tx *bbolt.Tx) error {
				switch kind {
				case "resource":
					return tx.Bucket([]byte(bucketAccountResources)).Put([]byte(resourceKey(AccountTypeClaude, "private-claude-secret-id")), []byte(`{}`))
				case "missing_resource":
					return tx.Bucket([]byte(bucketAccountResources)).Delete([]byte(resourceKey(AccountTypeClaude, "private-claude-secret-id")))
				case "grant":
					return tx.Bucket([]byte(bucketAccountGrants)).Put([]byte("quota-claude"), []byte(`{}`))
				default:
					return tx.DeleteBucket([]byte(bucketAccountGrantIndex))
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas", session, "", nil))
			if w.Code != 503 || w.Body.String() != "{\"error\":\"quota authorization unavailable\"}\n" {
				t.Fatalf("corrupt authority did not fail closed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMemberQuotaSnapshotDiscoveredModels(t *testing.T) {
	h, id, session, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, id, ClientPolicy{})
	accounts := h.pool.allAccounts()
	accounts[0].Models = map[string]DiscoveredModel{
		"claude-discovered": {ID: "claude-discovered", DisplayName: "Discovered"},
		"claude-hidden":     {ID: "claude-hidden", DisplayName: "Hidden"},
	}
	grant := testGrant("discovered", id, accounts[0])
	grant.Models = []string{"claude-discovered"}
	if _, err := h.passport.createAccountGrant(memberQuotaOperator(t, h), 2, grant); err != nil {
		t.Fatal(err)
	}
	// A second accessible account does not support the discovered model. Its
	// lower quota must not dilute the model's actual supporting account quota.
	grant = testGrant("other-account", id, accounts[2])
	grant.Models = []string{"*"}
	if _, err := h.passport.createAccountGrant(memberQuotaOperator(t, h), 1, grant); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range quotaHTTP(t, h, session) {
		if row.Model == "claude-hidden" {
			t.Fatal("ungranted discovered model leaked")
		}
		if row.Model == "claude-discovered" {
			found = true
			if row.SupportingAccounts != 1 || row.Primary == nil || row.Primary.RemainingPercent != 75 {
				t.Fatalf("quota included an account that does not support the model: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("authorized discovered model missing")
	}
}

func TestMemberQuotaSnapshotAntigravityRequiresAccountDiscovery(t *testing.T) {
	antigravityModels.Reset()
	t.Cleanup(antigravityModels.Reset)
	h, id, session, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, id, ClientPolicy{})
	account := &Account{ID: "member-antigravity", Type: AccountTypeAntigravity, Usage: h.pool.allAccounts()[0].Usage}
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(account.Type, account.ID), accountResource{
			Version: 2, ID: account.ID, Provider: account.Type, OwnerID: "operator-managed", AddedBy: "operator-managed", OperatorManaged: true, Revision: 1, Status: "active",
		})
	}); err != nil {
		t.Fatal(err)
	}
	grant := testGrant("antigravity-grant", id, account)
	grant.Models = []string{"*"}
	if _, err := h.passport.createAccountGrant(memberQuotaOperator(t, h), 1, grant); err != nil {
		t.Fatal(err)
	}
	h.pool.replace([]*Account{account})
	antigravityModels.MarkAccount(account.ID)
	if rows := quotaHTTP(t, h, session); len(rows) != 0 {
		t.Fatal("cold-start catalog substitute established model access")
	}
	antigravityModels.ReplaceAccount("foreign-antigravity", AntigravityAccountSnapshot{
		Models: map[string]AntigravityModelInfo{"foreign-model": {ID: "foreign-model", DisplayName: "Foreign"}},
	})
	antigravityModels.ReplaceAccount(account.ID, AntigravityAccountSnapshot{
		Models: map[string]AntigravityModelInfo{"member-model": {ID: "member-model", DisplayName: "Member"}},
	})
	rows := quotaHTTP(t, h, session)
	if len(rows) != 1 || rows[0].Model != "antigravity/member-model" || rows[0].SupportingAccounts != 1 {
		t.Fatalf("per-account discovery isolation failed: %+v", rows)
	}
}
