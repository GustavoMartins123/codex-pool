package main

import (
	"encoding/json"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestOperatorAccountAccessIsPersonalAndDoesNotTransferManagement(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	addTestPassportOperator(t, p, "charlie")
	if p.authorizeAccount("bob", a, "use") == nil {
		t.Fatal("private account initially accessible")
	}
	if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 1, true, "Support investigation"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"read", "use"} {
		if err := p.authorizeAccount("bob", a, action); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"manage", "withdraw"} {
		if p.authorizeAccount("bob", a, action) == nil {
			t.Fatal("activation transferred management")
		}
	}
	if p.authorizeAccount("charlie", a, "read") == nil || p.authorizeAccount("break-glass", a, "use") == nil {
		t.Fatal("activation escaped the operator")
	}
	if _, err := p.createAccountGrant("bob", 2, testGrant("forbidden", "charlie", a)); err == nil {
		t.Fatal("activation allowed delegation")
	}
	client, err := p.createClient("bob", "Laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := "bob-c-" + client.ID
	if _, err := p.accountGrantForUse(identity, a, "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if pool.candidateByIDForUser(identity, a.ID, a.Type, "", "") == nil || len(pool.visiblePool("bob").allAccounts()) != 2 {
		t.Fatal("activation was not applied to selectors and catalog")
	}
	if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 2, false, "Investigation completed"); err != nil {
		t.Fatal(err)
	}
	if p.authorizeAccount(identity, a, "use") == nil || pool.candidateByIDForUser(identity, a.ID, a.Type, "", "") != nil {
		t.Fatal("deactivation retained access")
	}
	if _, err := p.accountGrantForUse(identity, a, "gpt-5.5"); err == nil {
		t.Fatal("deactivation retained admission")
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil || reloaded.authorizeAccount("bob", a, "read") == nil {
		t.Fatalf("restart revived access: %v", err)
	}
}

func TestOperatorAccountAccessRevokedOnSuspensionAndDemotion(t *testing.T) {
	for _, change := range []string{"suspend", "demote"} {
		t.Run(change, func(t *testing.T) {
			p, pool := ownershipFixture(t)
			a := pool.allAccounts()[1]
			if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 1, true, "Support"); err != nil {
				t.Fatal(err)
			}
			if change == "suspend" {
				if _, err := p.setPrincipalStatus("alice", "bob", PrincipalSuspended); err != nil {
					t.Fatal(err)
				}
				if _, err := p.setPrincipalStatus("alice", "bob", PrincipalActive); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := p.setPrincipalKind("alice", "bob", PrincipalMember); err != nil {
					t.Fatal(err)
				}
				if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 3, true, "Member attack"); err == nil {
					t.Fatal("member activated operator access")
				}
				if _, err := p.setPrincipalKind("alice", "bob", PrincipalOperator); err != nil {
					t.Fatal(err)
				}
			}
			if p.authorizeAccount("bob", a, "use") == nil {
				t.Fatal("restoring principal revived activation")
			}
		})
	}
}

func TestOperatorAccountAccessAuditRevisionAndStorageFailClosed(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	for _, actor := range []string{"alice", "break-glass", "unknown"} {
		if p.setOperatorAccountAccess(actor, a.Type, a.ID, 1, true, "Attack") == nil {
			t.Fatalf("unauthorized activation: %s", actor)
		}
	}
	if p.setOperatorAccountAccess("bob", a.Type, a.ID, 1, true, " ") == nil || p.setOperatorAccountAccess("bob", a.Type, a.ID, 0, true, "Support") == nil {
		t.Fatal("invalid request admitted")
	}
	if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 1, true, "Support"); err != nil {
		t.Fatal(err)
	}
	if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 1, false, "Stale"); err == nil {
		t.Fatal("stale revision accepted")
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte(bucketPassportAudit)) }); err != nil {
		t.Fatal(err)
	}
	if err := p.setOperatorAccountAccess("bob", a.Type, a.ID, 2, false, "No audit"); err == nil {
		t.Fatal("mutation succeeded without audit")
	}
	if err := p.authorizeAccount("bob", a, "use"); err != nil || accountRevision(t, p, a) != 2 {
		t.Fatalf("audit failure did not roll back: %v", err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		r, err := readAccountResource(b, a.Type, a.ID)
		if err != nil {
			return err
		}
		r.OperatorAccess["bob"] = operatorAccountAccess{Enabled: true, Reason: "Malformed", UpdatedAt: time.Time{}}
		return putJSON(b, resourceKey(a.Type, a.ID), r)
	}); err != nil {
		t.Fatal(err)
	}
	if p.authorizeAccount("bob", a, "use") == nil {
		t.Fatal("malformed activation admitted")
	}
}

func TestOperatorAccountAccessMigrationDoesNotActivateAccounts(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b.Get([]byte(resourceKey(a.Type, a.ID))), &fields); err != nil {
			return err
		}
		delete(fields, "operator_access")
		return putJSON(b, resourceKey(a.Type, a.ID), fields)
	}); err != nil {
		t.Fatal(err)
	}
	if p.authorizeAccount("alice", a, "use") == nil {
		t.Fatal("unmigrated resource was accepted")
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.authorizeAccount("alice", a, "use"); err != nil {
		t.Fatal(err)
	}
	if reloaded.authorizeAccount("bob", a, "use") == nil {
		t.Fatal("migration activated operator access")
	}
}
