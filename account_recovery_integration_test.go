package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestPairedRestorePreservesAccountAndCredentialSecurity(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "recovery-test-key")
	dir := t.TempDir()
	boltPath := filepath.Join(dir, "proxy.db")
	duckPath := filepath.Join(dir, "usage.duckdb")
	store, err := newUsageStore(boltPath, 90)
	if err != nil {
		t.Fatal(err)
	}
	db := store.db
	p, err := newPassportStore(db)
	if err != nil {
		t.Fatal(err)
	}
	addTestPassportOperator(t, p, "alice")
	addTestPassportOperator(t, p, "bob")
	client, err := p.createClient("alice", "client", nil)
	if err != nil {
		t.Fatal(err)
	}
	resource := accountResource{Version: 2, ID: "private", Provider: AccountTypeKimi, OwnerID: "alice", AddedBy: "alice", Identity: "identity", SecretRef: "kimi/private.json", Status: "active", Revision: 2}
	if err := db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(resource.Provider, resource.ID), resource)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	duck, err := sql.Open("duckdb", duckPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duck.Exec("CREATE TABLE proof(value VARCHAR); INSERT INTO proof VALUES ('before')"); err != nil {
		t.Fatal(err)
	}
	if err := duck.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := createPairedBackup(boltPath, duckPath, filepath.Join(dir, "backup"))
	if err != nil {
		t.Fatal(err)
	}
	db, err = bbolt.Open(boltPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err = newPassportStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.withdrawAccount("alice", AccountTypeKimi, "private", 2); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cutoff := now.Add(time.Second)
	if err := db.Update(func(tx *bbolt.Tx) error {
		client.Status = "revoked"
		client.ValidAfter = cutoff
		if err := putJSON(tx.Bucket([]byte(bucketClientCredentials)), client.ID, client); err != nil {
			return err
		}
		if err := writePolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), "day|principal:alice|"+now.Format("20060102"), policyUsageCounter{Requests: 20, Tokens: 100, ReservedTokens: 50}); err != nil {
			return err
		}
		return putJSON(tx.Bucket([]byte(bucketContributionAttempts)), "alice", contributionAttemptWindow{StartedAt: now, Attempts: contributionAttemptsPerHour})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restorePairedBackup(manifest, boltPath, duckPath); err != nil {
		t.Fatal(err)
	}
	db, err = bbolt.Open(boltPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err = newPassportStore(db)
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{ID: "private", Type: AccountTypeKimi, AccessToken: "key"}
	for _, actor := range []string{"alice", "bob"} {
		if p.authorizeAccount(actor, account, "use") == nil {
			t.Fatal("restore resurrected account", actor)
		}
	}
	if got := p.clients[client.ID]; got.Status != "revoked" || !got.ValidAfter.Equal(cutoff) {
		t.Fatalf("restore resurrected client: %+v", got)
	}
	if p.admitContributionAttempt("alice", now) == nil {
		t.Fatal("restore reset abuse limit")
	}
	entries, err := p.recentAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		found = found || e.Action == "account.withdrawn"
	}
	if !found {
		t.Fatal("restore erased withdrawal audit")
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		var counter policyUsageCounter
		if err := json.Unmarshal(tx.Bucket([]byte(bucketPassportPolicyUsage)).Get([]byte("day|principal:alice|"+now.Format("20060102"))), &counter); err != nil {
			return err
		}
		if counter.Requests != 20 || counter.Tokens != 100 || counter.ReservedTokens != 50 {
			t.Fatal(counter)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
