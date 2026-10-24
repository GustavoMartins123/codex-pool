package main

import (
	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
	"testing"
)

func TestExplicitDeletionWaitsForOriginalRunnerAndRemovesHistoryOnlyAfterAcknowledgement(t *testing.T) {
	b, r, s, principal, _ := archiveFixture(t)
	if err := b.deleteConversation(principal, s.ID); err == nil {
		t.Fatal("running execution deleted")
	}
	closeArchiveFixture(t, b, r, s, principal)
	if err := b.deleteConversation("foreign", s.ID); err == nil {
		t.Fatal("foreign deletion accepted")
	}
	delete(b.runners, r.id)
	if err := b.deleteConversation(principal, s.ID); err == nil {
		t.Fatal("deletion silently skipped offline runner")
	}
	b.runners[r.id] = r
	if err := b.deleteConversation(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	c, err := b.conversation(principal, s.ID)
	if err != nil || !c.Deleting {
		t.Fatal("deletion status missing")
	}
	if _, err := b.resumeConversation(principal, s.ClientID, s.ID); err == nil {
		t.Fatal("conversation resumed during deletion")
	}
	if _, err := b.recentHistory(principal, s.ID, nil, nil, 100); err != nil {
		t.Fatal("history removed before file acknowledgement")
	}
	result := runnerproto.Frame("session.purged")
	result.SessionID = s.ID
	result.Status = "deleted"
	if err := b.erasedByRunner(r, result); err != nil {
		t.Fatal(err)
	}
	if _, err := b.conversation(principal, s.ID); err == nil {
		t.Fatal("deleted conversation still accessible")
	}
	if _, err := b.recentHistory(principal, s.ID, nil, nil, 100); err == nil {
		t.Fatal("deleted transcript still accessible")
	}
	if b.sessions[s.ID] != nil {
		t.Fatal("deleted execution remains in memory")
	}
	if err := b.store.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID)) != nil || tx.Bucket([]byte(bucketRunnerSessions)).Get([]byte(s.ID)) != nil {
			t.Fatal("deleted data retained in canonical buckets")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.erasedByRunner(r, result); err != nil {
		t.Fatal("repeated purge acknowledgement was not idempotent")
	}
}

func TestFailedDeletionPreservesHistoryAndCanBeRetried(t *testing.T) {
	b, r, s, principal, advance := archiveFixture(t)
	closeArchiveFixture(t, b, r, s, principal)
	advance(conversationIdleTTL)
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	c, _ := b.conversation(principal, s.ID)
	archiveID := c.ArchiveID
	if err := b.deleteConversation(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	failed := runnerproto.Frame("session.purged")
	failed.SessionID = s.ID
	failed.Status = "failed"
	if err := b.erasedByRunner(r, failed); err != nil {
		t.Fatal(err)
	}
	c, err := b.conversation(principal, s.ID)
	if err != nil || c.Deleting || c.DeleteError == "" || c.ArchiveID != archiveID {
		t.Fatal("failed deletion destroyed archive or hid the error")
	}
	if err := b.deleteConversation(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	done := runnerproto.Frame("session.purged")
	done.SessionID = s.ID
	done.Status = "deleted"
	if err := b.erasedByRunner(r, done); err != nil {
		t.Fatal(err)
	}
	if err := b.store.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte(archiveID)) != nil {
			t.Fatal("explicit deletion retained compressed transcript")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
