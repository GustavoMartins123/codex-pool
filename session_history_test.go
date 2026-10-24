package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func TestSessionHistorySurvivesRestartAndReplaysPastMemoryRing(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	for i := 0; i < 530; i++ {
		brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) { m.Kind = "text"; m.Content = fmt.Sprint(i) })
	}
	brokerEvent(t, b, r, s.ID, "turn.finished", func(m *runnerproto.Message) { m.Tokens = 42 })
	if _, err := b.startTurn(principal, s.ID, "second input"); err != nil {
		t.Fatal(err)
	}
	if err := b.requestClose(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "process.exit", nil)
	restored, err := newSessionBroker(h.passport)
	if err != nil {
		t.Fatal(err)
	}
	var after uint64
	var through *uint64
	counts := map[string]int{}
	for {
		page, err := restored.history(principal, s.ID, after, through)
		if err != nil {
			t.Fatal(err)
		}
		if page.Session.State != "closed" {
			t.Fatal("closed snapshot lost")
		}
		through = &page.Through
		for _, event := range page.Events {
			if event.ID != after+1 {
				t.Fatal("non-contiguous journal")
			}
			after = event.ID
			counts[event.Type]++
		}
		if page.Cursor != after {
			t.Fatal("invalid page cursor")
		}
		if !page.HasMore {
			break
		}
	}
	if counts["turn.input"] != 2 || counts["turn.delta"] != 530 || counts["turn.finished"] != 1 || counts["session.closed"] != 1 {
		t.Fatal(counts)
	}
	if _, err := restored.history("foreign", s.ID, 0, nil); err == nil {
		t.Fatal("foreign transcript exposed")
	}
	b.mu.Lock()
	replay, err := b.readEventsLocked(b.sessions[s.ID], 1, b.sessions[s.ID].EventCursor, 0)
	b.mu.Unlock()
	if err != nil || len(replay) <= 512 {
		t.Fatalf("durable replay: %d %v", len(replay), err)
	}
}

func TestSessionHistoryRejectsMissingRangesAndOldUnrecordedSessions(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	s := brokerSession(t, b, principal, login.ClientID)
	response := appAuthRequest(h, "GET", "/api/conversations/"+s.ConversationID+"/history?limit=256", login.Token, "")
	var page conversationHistoryPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Events) != 2 {
		t.Fatalf("history route: %d %s", response.Code, response.Body.String())
	}
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID)).Delete(eventKey(1))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.history(principal, s.ID, 0, nil); err == nil {
		t.Fatal("partial history accepted")
	}
	b.sessions[s.ID].HistoryComplete = false
	if _, err := b.history(principal, s.ID, 0, nil); err == nil {
		t.Fatal("unrecorded history accepted")
	}
}

func TestSessionHistoryBoundaryExcludesConcurrentEvents(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	s := brokerSession(t, b, principal, login.ClientID)
	page, err := b.history(principal, s.ID, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.requestClose(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	next, err := b.history(principal, s.ID, 0, &page.Through)
	if err != nil || next.Cursor != page.Through || len(next.Events) != len(page.Events) {
		t.Fatalf("history boundary changed: %+v %v", next, err)
	}
}
