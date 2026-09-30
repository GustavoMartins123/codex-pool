package main

import (
	"go.etcd.io/bbolt"
	"testing"
	"time"
)

func ownershipFixture(t *testing.T) (*PassportStore, *poolState) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "ownership-test-key")
	store := testUsageStore(t)
	p, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	addTestPassportOperator(t, p, "alice")
	addTestPassportOperator(t, p, "bob")
	accounts := []*Account{{ID: "shared", Type: AccountTypeKimi}, {ID: "private", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true}}
	if err := p.initializeAccountAuthority(accounts[:1]); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(AccountTypeCodex, "private"), &accountResource{Version: 2, ID: "private", Provider: AccountTypeCodex, OwnerID: "alice", AddedBy: "alice", Revision: 1, Status: "active"})
	}); err != nil {
		t.Fatal(err)
	}
	pool := newPoolState(accounts, false)
	pool.accountAuthority = p
	return p, pool
}

func TestAccountOwnershipSelectionAndPins(t *testing.T) {
	p, pool := ownershipFixture(t)
	client, err := p.createClient("alice", "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := "alice-c-" + client.ID
	pool.pin("session", "private")
	for _, profile := range []RoutingProfile{RoutingBalanced, RoutingLegacy} {
		a, _, _, _, alternatives, _ := pool.candidateWithRoutingTraceForUser("bob", "session", nil, AccountTypeCodex, "", "", "", profile)
		if a != nil || len(alternatives) != 0 {
			t.Fatalf("foreign private account selected (%s)", profile)
		}
		a, _, _, _, _, _ = pool.candidateWithRoutingTraceForUser(identity, "session", nil, AccountTypeCodex, "", "", "", profile)
		if a == nil || a.ID != "private" {
			t.Fatalf("owner denied (%s)", profile)
		}
	}
	if pool.candidateByIDForUser("bob", "private", AccountTypeCodex, "", "") != nil || pool.candidateWithCyberAccessForUser("bob", nil, AccountTypeCodex, "", "") != nil {
		t.Fatal("private account escaped selector")
	}
	if len(pool.visiblePool("bob").allAccounts()) != 1 || len(pool.visiblePool(identity).allAccounts()) != 2 {
		t.Fatal("catalog membership incorrect")
	}
	if err := p.authorizeAccount("bob", pool.allAccounts()[1], "manage"); err == nil {
		t.Fatal("operator managed foreign private account")
	}
}

func TestAccountOwnershipMissingMetadataAndRestart(t *testing.T) {
	p, pool := ownershipFixture(t)
	unknown := &Account{ID: "unknown", Type: AccountTypeCodex, PlanType: "pro"}
	pool.replace(append(pool.allAccounts(), unknown))
	if err := p.initializeAccountAuthority(pool.allAccounts()); err != nil {
		t.Fatal(err)
	}
	if p.authorizeAccount("alice", unknown, "use") == nil {
		t.Fatal("unknown account was implicitly migrated")
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.authorizeAccount("bob", pool.allAccounts()[1], "use") == nil {
		t.Fatal("restart lost ownership")
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		r, err := readAccountResource(b, AccountTypeCodex, "private")
		if err != nil {
			return err
		}
		now := time.Now()
		r.WithdrawnAt = &now
		r.Revision++
		return putJSON(b, resourceKey(r.Provider, r.ID), r)
	}); err != nil {
		t.Fatal(err)
	}
	if reloaded.authorizeAccount("alice", pool.allAccounts()[1], "use") == nil {
		t.Fatal("stale authority admitted withdrawn account")
	}
}
