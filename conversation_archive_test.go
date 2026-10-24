package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func archiveFixture(t *testing.T) (*sessionBroker, *connectedRunner, *runnerproto.Session, string, func(time.Duration)) {
	t.Helper()
	_, b, login, principal := brokerFixture(t)
	clock := b.now()
	b.now = func() time.Time { return clock }
	r := brokerRunner(t, b, principal)
	r.hello.PersistentSessions = true
	r.out = make(chan runnerproto.Message, 4096)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "agent.checkpoint", func(m *runnerproto.Message) {
		m.Checkpoint = &runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: "native", NativeLogID: "log"}
	})
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	for i := range 650 {
		brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) {
			m.Kind = []string{"thinking", "tool_call", "tool_result", "text"}[i%4]
			m.Content = strings.Repeat("synthetic content ", 20)
		})
	}
	brokerEvent(t, b, r, s.ID, "turn.finished", nil)
	return b, r, s, principal, func(d time.Duration) { clock = clock.Add(d) }
}

func closeArchiveFixture(t *testing.T, b *sessionBroker, r *connectedRunner, s *runnerproto.Session, principal string) {
	t.Helper()
	if err := b.requestClose(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "process.exit", func(m *runnerproto.Message) {
		m.Checkpoint = &runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: "native", NativeLogID: "log", Ready: true, Digest: strings.Repeat("a", 64)}
	})
}

func TestConversationArchivalPreservesEveryEventAndReadsAfterRestart(t *testing.T) {
	b, r, s, principal, advance := archiveFixture(t)
	closeArchiveFixture(t, b, r, s, principal)
	var original [][]byte
	if err := b.store.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID)).ForEach(func(_, raw []byte) error { original = append(original, bytes.Clone(raw)); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	advance(conversationIdleTTL)
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	c, err := b.conversation(principal, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage != "archived" || c.ArchiveID == "" || c.ArchivedThrough != uint64(len(original)) || !c.ResumeAvailable {
		t.Fatalf("archive state invalid: %+v", c)
	}
	if err := b.store.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID)) != nil {
			t.Fatal("uncompressed duplicate retained")
		}
		reader, err := newConversationEventReader(tx, c)
		if err != nil {
			return err
		}
		for i, raw := range original {
			restored, err := reader.raw(uint64(i + 1))
			if err != nil {
				return err
			}
			if !bytes.Equal(raw, restored) {
				t.Fatal("archive changed original journal bytes")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if page, err := b.conversations(principal, "active", "", 50); err != nil || len(page.Conversations) != 0 {
		t.Fatal("archive remained in active list")
	}
	if page, err := b.conversations(principal, "archived", "", 50); err != nil || len(page.Conversations) != 1 || len(page.Conversations[0].Preview) == 0 {
		t.Fatal("archive or preview missing")
	}
	restored, err := newSessionBroker(b.store)
	if err != nil {
		t.Fatal(err)
	}
	page, err := restored.recentHistory(principal, s.ID, nil, nil, 100)
	if err != nil || len(page.Events) != 100 || !page.HasOlder {
		t.Fatalf("read archived after restart: %+v %v", page, err)
	}
	if err := restored.archiveConversation(principal, s.ID); err != nil {
		t.Fatal("archive is not idempotent:", err)
	}
	if _, err := restored.recentHistory("foreign", s.ID, nil, nil, 100); err == nil {
		t.Fatal("foreign archive exposed")
	}
}

func TestSevenDayArchivalSuspendsOnlyIdleExecutionAndWaitsForCheckpoint(t *testing.T) {
	b, r, s, principal, advance := archiveFixture(t)
	advance(conversationIdleTTL - time.Nanosecond)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	if b.sessions[s.ID].State != "waiting_input" {
		t.Fatal("archived before 168 hours")
	}
	advance(time.Nanosecond)
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	if b.sessions[s.ID].State != "closing" || b.sessions[s.ID].ClosedReason != "archived" {
		t.Fatal("idle execution was not suspended")
	}
	c, err := b.conversation(principal, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage != "active" {
		t.Fatal("archive published before native checkpoint")
	}
	brokerEvent(t, b, r, s.ID, "process.exit", func(m *runnerproto.Message) {
		m.Checkpoint = &runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: "native", NativeLogID: "log", Ready: true, Digest: strings.Repeat("b", 64)}
	})
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	c, err = b.conversation(principal, s.ID)
	if err != nil || c.Storage != "archived" {
		t.Fatal("checkpointed idle conversation not archived")
	}
}

func TestArchiveCorruptionFailsClosedAndRetainsTheManifest(t *testing.T) {
	b, r, s, principal, advance := archiveFixture(t)
	closeArchiveFixture(t, b, r, s, principal)
	advance(conversationIdleTTL)
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	c, _ := b.conversation(principal, s.ID)
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte(c.ArchiveID))
		var manifest conversationArchive
		if err := json.Unmarshal(bucket.Get([]byte("manifest")), &manifest); err != nil {
			return err
		}
		last := manifest.Blocks[len(manifest.Blocks)-1]
		raw := bytes.Clone(bucket.Get(eventKey(last.First)))
		raw[0] ^= 1
		return bucket.Put(eventKey(last.First), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.recentHistory(principal, s.ID, nil, nil, 100); err == nil {
		t.Fatal("corrupt archive returned partial history")
	}
	if _, err := b.resumeConversation(principal, s.ClientID, s.ID); err == nil {
		t.Fatal("corrupt archive resumed")
	}
	c2, _ := b.conversation(principal, s.ID)
	if c2.Storage != "archived" || c2.ArchiveID != c.ArchiveID {
		t.Fatal("failure changed canonical archive")
	}
}

func TestResumeUsesSameConversationAndPreservesArchivePrefix(t *testing.T) {
	b, r, s, principal, advance := archiveFixture(t)
	closeArchiveFixture(t, b, r, s, principal)
	advance(conversationIdleTTL)
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	c, _ := b.conversation(principal, s.ID)
	prefix := c.ArchivedThrough
	resumed, err := b.resumeConversation(principal, s.ClientID, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != s.ID || resumed.ConversationID != s.ConversationID || !resumed.ResumeRequested || resumed.State != "pending_runner" {
		t.Fatal("resume changed conversation or native identity")
	}
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "agent.checkpoint", func(m *runnerproto.Message) {
		m.Checkpoint = &runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: "native", NativeLogID: "log"}
	})
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	if _, err := b.startTurn(principal, s.ID, "continued question"); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) { m.Kind = "text"; m.Content = "continued answer" })
	brokerEvent(t, b, r, s.ID, "turn.finished", nil)
	c, _ = b.conversation(principal, s.ID)
	if c.Storage != "active" || c.ArchivedThrough != prefix || c.EventCursor <= prefix {
		t.Fatal("resume discarded compressed prefix")
	}
	page, err := b.recentHistory(principal, s.ID, nil, nil, 256)
	if err != nil {
		t.Fatal(err)
	}
	if page.Events[len(page.Events)-1].ID != c.EventCursor {
		t.Fatal("new events unavailable")
	}
	closeArchiveFixture(t, b, r, s, principal)
	advance(conversationIdleTTL)
	if err := b.archiveDueConversations(); err != nil {
		t.Fatal(err)
	}
	c, _ = b.conversation(principal, s.ID)
	if c.Storage != "archived" || c.ArchivedThrough <= prefix {
		t.Fatal("re-archival lost newer events")
	}
}

func TestArchiveBuildFailureRetainsOriginalAndRestartRemovesUnpublishedBlocks(t *testing.T) {
	b, r, s, principal, advance := archiveFixture(t)
	closeArchiveFixture(t, b, r, s, principal)
	advance(conversationIdleTTL)
	var original []byte
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		journal := tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID))
		original = bytes.Clone(journal.Get(eventKey(2)))
		return journal.Put(eventKey(2), []byte("corrupt"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.archiveConversation(principal, s.ID); err == nil {
		t.Fatal("corrupt source was archived")
	}
	c, _ := b.conversation(principal, s.ID)
	if c.Storage != "active" || c.ArchiveID != "" {
		t.Fatal("failed build changed canonical metadata")
	}
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID)).Put(eventKey(2), original); err != nil {
			return err
		}
		staging, err := tx.Bucket([]byte(bucketConversationArchives)).CreateBucket([]byte("unpublished-build"))
		if err != nil {
			return err
		}
		return staging.Put(eventKey(1), []byte("partial compressed data"))
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := newSessionBroker(b.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.store.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte("unpublished-build")) != nil {
			t.Fatal("unpublished build survived startup")
		}
		if tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID)) == nil {
			t.Fatal("cleanup removed original journal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.recentHistory(principal, s.ID, nil, nil, 100); err != nil {
		t.Fatal(err)
	}
}
