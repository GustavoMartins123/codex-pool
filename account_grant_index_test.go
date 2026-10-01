package main

import (
	"fmt"
	"go.etcd.io/bbolt"
	"path/filepath"
	"testing"
	"time"
)

func indexedTestGrant(id, account, recipient string) accountGrant {
	return accountGrant{ID: id, Provider: AccountTypeCodex, AccountID: account, RecipientID: recipient, CreatedBy: "owner", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Revision: 1, Models: []string{"gpt-5.5"}, Budget: PolicyLimits{DailyRequests: 3}, Reason: "shared access"}
}

func TestGrantIndexMigratesAndRejectsCorruption(t *testing.T) {
	p, _ := testPolicyPassport(t)
	grant := indexedTestGrant("migrated", "a", "recipient")
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket([]byte(bucketAccountGrantIndex)); err != nil {
			return err
		}
		return putJSON(tx.Bucket([]byte(bucketAccountGrants)), grant.ID, grant)
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Update(initializeAccountGrantIndex); err != nil {
		t.Fatal(err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		grants, err := accountGrantsByResource(tx, grant.Provider, grant.AccountID, grant.RecipientID)
		if len(grants) != 1 {
			t.Fatalf("indexed grants=%d", len(grants))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketAccountGrantIndex)).Bucket([]byte(resourceKey(grant.Provider, grant.AccountID))).Bucket([]byte(grant.RecipientID)).Delete([]byte(grant.ID))
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Update(initializeAccountGrantIndex); err == nil {
		t.Fatal("incomplete index accepted")
	}
}

func TestIndexedGrantLookupIsScopedAndRevalidatesRevocationExpiryConsent(t *testing.T) {
	p, _ := testPolicyPassport(t)
	grant := indexedTestGrant("scoped", "a", "recipient")
	unrelated := indexedTestGrant("unrelated", "other", "somebody")
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		for _, g := range []accountGrant{grant, unrelated} {
			if err := putAccountGrant(tx, g); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte(bucketAccountGrants)).Put([]byte(unrelated.ID), []byte("corrupt"))
	}); err != nil {
		t.Fatal(err)
	}
	resource := &accountResource{ID: "a", Provider: AccountTypeCodex, OwnerID: "owner", Status: "active"}
	lookup := func() *accountGrant {
		var g *accountGrant
		if err := p.db.View(func(tx *bbolt.Tx) error {
			var err error
			g, err = findAccountGrant(tx, resource, "recipient", "gpt-5.5")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	if lookup() == nil {
		t.Fatal("valid scoped grant unavailable")
	}
	resource.OwnerID = "new-owner"
	if lookup() != nil {
		t.Fatal("consent change ignored")
	}
	resource.OperatorMayDelegate = true
	if lookup() == nil {
		t.Fatal("explicit consent unavailable")
	}
	grant.ExpiresAt = time.Now().Add(-time.Minute)
	grant.CreatedAt = grant.ExpiresAt.Add(-time.Hour)
	if err := p.db.Update(func(tx *bbolt.Tx) error { return putAccountGrant(tx, grant) }); err != nil {
		t.Fatal(err)
	}
	if lookup() != nil {
		t.Fatal("expired grant admitted")
	}
	grant.ExpiresAt = time.Now().Add(time.Hour)
	now := time.Now()
	grant.RevokedAt = &now
	if err := p.db.Update(func(tx *bbolt.Tx) error { return putAccountGrant(tx, grant) }); err != nil {
		t.Fatal(err)
	}
	if lookup() != nil {
		t.Fatal("revoked grant admitted")
	}
}

func BenchmarkGrantLookup(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			db, err := bbolt.Open(filepath.Join(b.TempDir(), "grants.db"), 0600, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			resource := &accountResource{ID: "a", Provider: AccountTypeCodex, OwnerID: "owner", Status: "active"}
			err = db.Update(func(tx *bbolt.Tx) error {
				if _, err := tx.CreateBucket([]byte(bucketAccountGrants)); err != nil {
					return err
				}
				if err := initializeAccountGrantIndex(tx); err != nil {
					return err
				}
				for i := 0; i < count; i++ {
					if err := putAccountGrant(tx, indexedTestGrant(fmt.Sprint(i), fmt.Sprint(i), "recipient")); err != nil {
						return err
					}
				}
				return putAccountGrant(tx, indexedTestGrant("target", "a", "recipient"))
			})
			if err != nil {
				b.Fatal(err)
			}
			b.Run("indexed", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := db.View(func(tx *bbolt.Tx) error { _, err := findAccountGrant(tx, resource, "recipient", "gpt-5.5"); return err }); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("scan", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := db.View(func(tx *bbolt.Tx) error {
						_, err := readAccountGrants(tx.Bucket([]byte(bucketAccountGrants)))
						return err
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
