package main

import (
	"encoding/json"
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

func TestRestorePreservesControlsPoliciesAndRevokedGrants(t *testing.T) {
	dir := t.TempDir()
	current, staged := filepath.Join(dir, "current.db"), filepath.Join(dir, "staged.db")
	resource := accountResource{Version: 2, ID: "private", Provider: AccountTypeCodex, OwnerID: "alice", AddedBy: "alice", Status: "active", Revision: 3, Controls: accountControls{State: accountMaintenance, MaxConcurrent: 2}}
	now := time.Now().UTC()
	grant := testGrant("restored", "bob", &Account{ID: resource.ID, Type: resource.Provider})
	grant.CreatedBy, grant.CreatedAt, grant.Revision, grant.RevokedAt = "alice", now.Add(-time.Minute), 2, &now
	for _, path := range []string{current, staged} {
		mutateRestoreAuthority(t, path, func(tx *bbolt.Tx) error {
			for _, name := range []string{bucketAccountResources, bucketAccountGrants, bucketPrincipals, bucketClientCredentials, bucketPassportPolicyUsage} {
				if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
					return err
				}
			}
			r, g := resource, grant
			principal := Principal{ID: "alice", Kind: PrincipalMember, Status: PrincipalActive, Policy: ClientPolicy{Models: PolicySelector{Deny: []string{"*"}}}, PolicyRevision: 3}
			client := ClientCredential{ID: "client", PrincipalID: "alice", Status: "active", Policy: principal.Policy}
			if path == staged {
				r.Controls = accountControls{}
				r.OperatorMayDelegate = true
				r.Revision = 10
				g.RevokedAt = nil
				g.Revision = 1
				principal.Policy = ClientPolicy{}
				principal.PolicyRevision = 0
				client.Policy = ClientPolicy{}
			}
			if err := putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(r.Provider, r.ID), r); err != nil {
				return err
			}
			if err := putJSON(tx.Bucket([]byte(bucketAccountGrants)), g.ID, g); err != nil {
				return err
			}
			if path == staged {
				phantom := g
				phantom.ID = "backup-only"
				if err := putJSON(tx.Bucket([]byte(bucketAccountGrants)), phantom.ID, phantom); err != nil {
					return err
				}
			}
			if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), principal.ID, principal); err != nil {
				return err
			}
			return putJSON(tx.Bucket([]byte(bucketClientCredentials)), client.ID, client)
		})
	}
	if err := preserveAccountSecurityOnRestore(current, staged); err != nil {
		t.Fatal(err)
	}
	mutateRestoreAuthority(t, staged, func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), resource.Provider, resource.ID)
		if err != nil {
			return err
		}
		if r.Controls.State != accountMaintenance || r.Controls.MaxConcurrent != 2 || r.OperatorMayDelegate || r.Revision != 3 {
			t.Fatal("restore replaced current controls or consent")
		}
		grants, err := readAccountGrants(tx.Bucket([]byte(bucketAccountGrants)))
		if err != nil {
			return err
		}
		if len(grants) != 1 || grants[0].RevokedAt == nil {
			t.Fatal("restore revived a grant")
		}
		var principal Principal
		if err := json.Unmarshal(tx.Bucket([]byte(bucketPrincipals)).Get([]byte("alice")), &principal); err != nil {
			return err
		}
		if principal.PolicyRevision != 3 || policyAllows(principal.Policy.Models, "gpt-5.5") {
			t.Fatal("restore widened principal policy")
		}
		var client ClientCredential
		if err := json.Unmarshal(tx.Bucket([]byte(bucketClientCredentials)).Get([]byte("client")), &client); err != nil {
			return err
		}
		if policyAllows(client.Policy.Models, "gpt-5.5") {
			t.Fatal("restore widened credential policy")
		}
		return nil
	})
}
