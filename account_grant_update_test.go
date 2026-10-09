package main

import (
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestUpdateAccountGrantPreservesUsageAndReplacesPolicy(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	created, err := p.createAccountGrant("alice", 1, testGrant("editable", "bob", a))
	if err != nil {
		t.Fatal(err)
	}
	admission, err := p.reserveGrantRequest(created, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	admission.Release()
	draft := *created
	draft.Models = []string{"gpt-5.5", "gpt-6-astra"}
	draft.Budget.DailyRequests = 1
	draft.Reason = "Add a model and reduce the budget"
	draft.ExpiresAt = time.Now().UTC().Add(2 * time.Hour)
	updated, err := p.updateAccountGrant("alice", 2, 1, draft)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || !updated.CreatedAt.Equal(created.CreatedAt) || updated.Revision != 2 || accountRevision(t, p, a) != 3 {
		t.Fatalf("grant identity/revisions changed unexpectedly: %+v", updated)
	}
	if _, err := p.accountGrantForUse("bob", a, "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	if grantUsage(t, p, updated.ID).Requests != 1 {
		t.Fatal("update reset the consumed budget")
	}
	if _, err := p.reserveGrantRequest(updated, time.Now()); err == nil {
		t.Fatal("reduced budget ignored existing consumption")
	}
	draft = *updated
	draft.Models = []string{"gpt-6-astra"}
	draft.Budget.DailyRequests = 5
	if _, err := p.updateAccountGrant("alice", 3, 2, draft); err != nil {
		t.Fatal(err)
	}
	if _, err := p.accountGrantForUse("bob", a, "gpt-5.5"); err == nil {
		t.Fatal("removed model still authorized")
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reloaded.accountGrantForUse("bob", a, "gpt-6-astra")
	if err != nil || persisted == nil || persisted.Revision != 3 || persisted.Budget.DailyRequests != 5 || grantUsage(t, reloaded, draft.ID).Requests != 1 {
		t.Fatalf("updated grant did not survive reload: %+v %v", persisted, err)
	}
}

func TestUpdateAccountGrantRejectsConflictsWithoutChangingAccess(t *testing.T) {
	for _, tc := range []struct {
		name, code              string
		change                  func(*accountGrant)
		revision, grantRevision uint64
	}{
		{"stale account", "account_revision_conflict", func(*accountGrant) {}, 1, 1},
		{"stale grant", "grant_revision_conflict", func(*accountGrant) {}, 3, 2},
		{"different recipient", "grant_not_found", func(g *accountGrant) { g.RecipientID = "charlie" }, 3, 1},
		{"missing grant", "grant_not_found", func(g *accountGrant) { g.ID = "missing" }, 3, 1},
		{"overlap", "grant_models_overlap", func(g *accountGrant) { g.Models = []string{"gpt-6-astra"} }, 3, 1},
		{"wildcard overlap", "grant_models_overlap", func(g *accountGrant) { g.Models = []string{"*"} }, 3, 1},
		{"empty models", "grant_invalid", func(g *accountGrant) { g.Models = nil }, 3, 1},
		{"empty budget", "grant_invalid", func(g *accountGrant) { g.Budget = PolicyLimits{} }, 3, 1},
		{"past expiry", "grant_expiry_invalid", func(g *accountGrant) { g.ExpiresAt = g.CreatedAt.Add(time.Nanosecond) }, 3, 1},
		{"long expiry", "grant_expiry_invalid", func(g *accountGrant) { g.ExpiresAt = time.Now().Add(366 * 24 * time.Hour) }, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, pool := ownershipFixture(t)
			a := pool.allAccounts()[1]
			addTestPassportOperator(t, p, "charlie")
			created, err := p.createAccountGrant("alice", 1, testGrant("editable", "bob", a))
			if err != nil {
				t.Fatal(err)
			}
			other := testGrant("other", "bob", a)
			other.Models = []string{"gpt-6-astra"}
			if _, err := p.createAccountGrant("alice", 2, other); err != nil {
				t.Fatal(err)
			}
			draft := *created
			tc.change(&draft)
			_, err = p.updateAccountGrant("alice", tc.revision, tc.grantRevision, draft)
			requirePolicyCode(t, err, tc.code)
			stored, err := p.accountGrantForUse("bob", a, "gpt-5.5")
			if err != nil || stored == nil || stored.Revision != 1 || accountRevision(t, p, a) != 3 {
				t.Fatalf("failed update mutated access: %+v %v", stored, err)
			}
		})
	}
}

func TestUpdateAccountGrantRequiresConsentAndCannotReviveInactiveGrants(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	addTestPassportOperator(t, p, "charlie")
	created, err := p.createAccountGrant("alice", 1, testGrant("editable", "charlie", a))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.updateAccountGrant("bob", 2, 1, *created)
	requirePolicyCode(t, err, "account_delegation_denied")
	if err := p.setAccountDelegation("alice", a.Type, a.ID, 2, true); err != nil {
		t.Fatal(err)
	}
	updated, err := p.updateAccountGrant("bob", 3, 1, *created)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CreatedBy != "bob" {
		t.Fatal("update bypasses delegated consent")
	}
	if err := p.setAccountDelegation("alice", a.Type, a.ID, 4, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.accountGrantForUse("charlie", a, "gpt-5.5"); err == nil {
		t.Fatal("removing consent retained operator changes")
	}
	_, err = p.updateAccountGrant("alice", 5, 3, *updated)
	requirePolicyCode(t, err, "grant_inactive")
	// Expired grants also require a new grant rather than reviving old usage.
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		g := *created
		g.ID = "expired"
		g.CreatedAt = time.Now().Add(-2 * time.Hour)
		g.ExpiresAt = time.Now().Add(-time.Hour)
		return putAccountGrant(tx, g)
	}); err != nil {
		t.Fatal(err)
	}
	draft := *created
	draft.ID = "expired"
	_, err = p.updateAccountGrant("alice", 5, 1, draft)
	requirePolicyCode(t, err, "grant_inactive")
}
