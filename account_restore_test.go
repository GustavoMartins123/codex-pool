package main

import (
	"go.etcd.io/bbolt"
	"path/filepath"
	"testing"
	"time"
)

func mutateRestoreAuthority(t *testing.T, path string, mutate func(*bbolt.Tx) error) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(mutate); err != nil {
		t.Fatal(err)
	}
}

func TestRestorePreservesWithdrawalsOwnershipAndBudget(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "current.db")
	staged := filepath.Join(dir, "staged.db")
	resource := accountResource{Version: 2, ID: "private", Provider: AccountTypeKimi, OwnerID: "alice", AddedBy: "alice", Identity: "identity", SecretRef: "kimi/private.json", Status: "active", Revision: 2}
	mutateRestoreAuthority(t, current, func(tx *bbolt.Tx) error {
		for _, name := range []string{bucketAccountResources, bucketPrincipals, bucketPassportPolicyUsage, bucketAnalyticsState} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(resource.Provider, resource.ID), resource); err != nil {
			return err
		}
		return putJSON(tx.Bucket([]byte(bucketPrincipals)), "alice", Principal{ID: "alice", Kind: PrincipalMember, Status: PrincipalActive, CanContribute: true, Budget: PolicyLimits{DailyRequests: 10}})
	})
	if err := copyFile(current, staged, 0o600); err != nil {
		t.Fatal(err)
	}
	mutateRestoreAuthority(t, current, func(tx *bbolt.Tx) error {
		now := time.Now().UTC()
		resource.WithdrawnAt = &now
		resource.Revision++
		if err := putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(resource.Provider, resource.ID), resource); err != nil {
			return err
		}
		if err := writePolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), "day|principal:alice|20260930", policyUsageCounter{Requests: 10, Tokens: 100, ReservedTokens: 50}); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(bucketAnalyticsState)).Put([]byte("principal_policy_usage_migrated"), []byte{1}); err != nil {
			return err
		}
		return putJSON(tx.Bucket([]byte(bucketPrincipals)), "alice", Principal{ID: "alice", Kind: PrincipalMember, Status: PrincipalActive, Budget: PolicyLimits{DailyRequests: 10}})
	})
	if err := preserveAccountSecurityOnRestore(current, staged); err != nil {
		t.Fatal(err)
	}
	mutateRestoreAuthority(t, staged, func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), AccountTypeKimi, "private")
		if err != nil {
			return err
		}
		if r.WithdrawnAt == nil || r.OwnerID != "alice" || r.Revision != 3 {
			t.Fatal("restore lost withdrawal authority")
		}
		counter, err := readPolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), "day|principal:alice|20260930")
		if err != nil {
			return err
		}
		if counter.Requests != 10 || counter.Tokens != 100 || counter.ReservedTokens != 50 {
			t.Fatalf("restored budget: %+v", counter)
		}
		if tx.Bucket([]byte(bucketAnalyticsState)).Get([]byte("principal_policy_usage_migrated")) == nil {
			t.Fatal("budget migration state lost")
		}
		return nil
	})
}

func TestRestoreRejectsOwnershipConflicts(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "current.db")
	staged := filepath.Join(dir, "staged.db")
	for _, path := range []string{current, staged} {
		mutateRestoreAuthority(t, path, func(tx *bbolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte(bucketAccountResources))
			if err != nil {
				return err
			}
			owner := "alice"
			if path == staged {
				owner = "bob"
			}
			return putJSON(b, resourceKey(AccountTypeKimi, "private"), accountResource{Version: 2, ID: "private", Provider: AccountTypeKimi, OwnerID: owner, Status: "active", Revision: 2})
		})
	}
	if err := preserveAccountSecurityOnRestore(current, staged); err == nil {
		t.Fatal("conflicting account ownership restored")
	}
}
