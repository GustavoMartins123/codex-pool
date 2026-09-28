package main

import (
	"testing"

	"go.etcd.io/bbolt"
)

// TestJoinLinkIndexBackfillCoversLegacyLinks proves the startup backfill
// repairs a missing join_links_by_token entry so a guest pass minted before
// the index (or left inconsistent by a crash) keeps resolving.
func TestJoinLinkIndexBackfillCoversLegacyLinks(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	principal, joinLink, _, token, err := passport.createGuest("operator", "legacy pass", "Legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if principal == nil || joinLink == nil || token == "" {
		t.Fatal("createGuest returned an incomplete pass")
	}

	// Simulate a pass written before the digest index existed.
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketJoinLinksByToken)).Delete([]byte(joinLink.TokenDigest))
	}); err != nil {
		t.Fatal(err)
	}

	// Reopening the store backfills the index; the pass must still redeem.
	reopened, err := newPassportStore(passport.db)
	if err != nil {
		t.Fatalf("reopen after index loss: %v", err)
	}
	joined, _, _, err := reopened.redeemJoin(token)
	if err != nil {
		t.Fatalf("legacy join token stopped resolving after reopen: %v", err)
	}
	if joined == nil || joined.ID != principal.ID {
		t.Fatalf("join token redeemed the wrong principal: %#v", joined)
	}
}
