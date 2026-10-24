package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func TestConversationsPageBeyondTwoHundredAndIsolateOwnersAndCursors(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	start := b.now()
	for i := range 251 {
		id := fmt.Sprintf("conversation-%03d", i)
		at := start.Add(time.Duration(i) * time.Second)
		s := &sessionRecord{HistoryComplete: true, Session: runnerproto.Session{ID: id, ConversationID: id, PrincipalID: principal, ClientID: "synthetic-client", Agent: "codex", Model: "synthetic-model", State: "closed", CreatedAt: at}}
		if err := b.saveLocked(s, "", principal, newSessionEvent("turn.input", map[string]any{"turn_id": id, "input": id})); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	var cursor string
	for {
		page, err := b.conversations(principal, "active", cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Conversations {
			if seen[c.ID] {
				t.Fatal("conversation duplicated")
			}
			seen[c.ID] = true
		}
		if cursor == "" {
			if _, err := b.conversations("foreign", "active", page.NextCursor, 50); err == nil {
				t.Fatal("foreign cursor accepted")
			}
			if _, err := b.conversations(principal, "archived", page.NextCursor, 50); err == nil {
				t.Fatal("cursor crossed categories")
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 251 {
		t.Fatalf("visible conversations: %d", len(seen))
	}
	page, err := b.conversations("foreign", "active", "", 50)
	if err != nil || len(page.Conversations) != 0 {
		t.Fatal("foreign conversations exposed")
	}
	if _, err := b.conversation("foreign", "conversation-000"); err == nil {
		t.Fatal("foreign preview exposed")
	}
}

func TestConversationPreviewsAndActivitySurviveRestartWithoutHeartbeatActivity(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	clock := b.now()
	b.now = func() time.Time { return clock }
	r := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) {
		m.Kind = "thinking"
		m.Content = "analysis must stay inside the transcript"
	})
	brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) { m.Kind = "text"; m.Content = "first " })
	brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) { m.Kind = "text"; m.Content = strings.Repeat("界", 600) })
	brokerEvent(t, b, r, s.ID, "turn.finished", nil)
	c, err := b.conversation(principal, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.LastInteractionAt == nil || !c.LastInteractionAt.Equal(clock) || len(c.Preview) != 2 || c.Preview[1].Role != "assistant" || len([]rune(c.Preview[1].Text)) != 512 {
		t.Fatalf("invalid preview: %+v", c)
	}
	activity, expiry := *c.LastInteractionAt, c.ArchiveAfter
	clock = clock.Add(24 * time.Hour)
	if err := b.heartbeat(r, []string{s.ID}); err != nil {
		t.Fatal(err)
	}
	b.disconnectRunner(r)
	c, err = b.conversation(principal, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !c.LastInteractionAt.Equal(activity) || !c.ArchiveAfter.Equal(expiry) {
		t.Fatal("heartbeat or disconnect extended retention clock")
	}
	restored, err := newSessionBroker(h.passport)
	if err != nil {
		t.Fatal(err)
	}
	c, err = restored.conversation(principal, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !c.LastInteractionAt.Equal(activity) || c.PrincipalID != principal || len(c.Preview) != 2 {
		t.Fatal("conversation metadata lost on restart")
	}
}

func legacyConversationFixture(t *testing.T, corrupt bool) (*PassportStore, string, []byte) {
	t.Helper()
	_, b, login, principal := brokerFixture(t)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.requestClose(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range []string{bucketConversations, bucketConversationIndex, bucketConversationDue, bucketConversationSchema} {
			if err := tx.DeleteBucket([]byte(bucket)); err != nil {
				return err
			}
		}
		raw := tx.Bucket([]byte(bucketRunnerSessions)).Get([]byte(s.ID))
		var record sessionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		record.ConversationID = ""
		if err := putJSON(tx.Bucket([]byte(bucketRunnerSessions)), s.ID, record); err != nil {
			return err
		}
		journal := tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID))
		if err := journal.ForEach(func(key, raw []byte) error {
			var e sessionEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			e.At = time.Time{}
			return putJSON(journal, string(key), e)
		}); err != nil {
			return err
		}
		if corrupt {
			if err := journal.Delete(eventKey(2)); err != nil {
				return err
			}
		}
		return journal.ForEach(func(_, raw []byte) error { before = append(before, raw...); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return b.store, s.ID, before
}

func TestConversationMigrationPreservesJournalAndUnknownInteractionDates(t *testing.T) {
	store, id, before := legacyConversationFixture(t, false)
	b, err := newSessionBroker(store)
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.conversation(b.sessions[id].PrincipalID, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.LastInteractionAt != nil || c.MigratedAt == nil || c.ArchiveAfter.Sub(*c.MigratedAt) != conversationIdleTTL {
		t.Fatal("migration invented interaction dates or skipped the grace period")
	}
	var after []byte
	if err := store.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(id)).ForEach(func(_, raw []byte) error { after = append(after, raw...); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("migration rewrote transcript bytes")
	}
	if _, err := newSessionBroker(store); err != nil {
		t.Fatal("migration is not idempotent:", err)
	}
}

func TestConversationMigrationRollsBackOnMissingJournalEvent(t *testing.T) {
	store, id, _ := legacyConversationFixture(t, true)
	if _, err := newSessionBroker(store); err == nil {
		t.Fatal("incomplete migration accepted")
	}
	if err := store.db.View(func(tx *bbolt.Tx) error {
		var s sessionRecord
		if err := json.Unmarshal(tx.Bucket([]byte(bucketRunnerSessions)).Get([]byte(id)), &s); err != nil {
			return err
		}
		if s.ConversationID != "" || tx.Bucket([]byte(bucketConversationSchema)) != nil {
			t.Fatal("failed migration published partial metadata")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
