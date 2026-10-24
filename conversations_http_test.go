package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
)

func TestConversationHistoryLoadsRecentPagesWithStableBoundaryAndNoGaps(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	for i := range 530 {
		brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) { m.Kind = "text"; m.Content = fmt.Sprint(i) })
	}
	response := appAuthRequest(h, "GET", "/api/conversations/"+s.ConversationID+"/history?limit=100", login.Token, "")
	var page conversationHistoryPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatalf("history: %d %s", response.Code, response.Body.String())
	}
	if len(page.Events) != 100 || !page.HasOlder || page.Events[99].ID != page.Through || page.Events[0].ID != page.Before {
		t.Fatal("initial page did not start with recent events")
	}
	boundary := page.Through
	brokerEvent(t, b, r, s.ID, "turn.event", func(m *runnerproto.Message) { m.Kind = "text"; m.Content = "new concurrent output" })
	seen := map[uint64]bool{}
	for {
		for i, event := range page.Events {
			if seen[event.ID] {
				t.Fatal("duplicate history event")
			}
			seen[event.ID] = true
			if i > 0 && page.Events[i-1].ID+1 != event.ID {
				t.Fatal("page event order is invalid")
			}
		}
		if !page.HasOlder {
			break
		}
		before := page.Before
		var err error
		pageValue, err := b.recentHistory(principal, s.ConversationID, &before, &boundary, 100)
		if err != nil {
			t.Fatal(err)
		}
		page = *pageValue
		if page.Through != boundary {
			t.Fatal("history boundary changed")
		}
	}
	if uint64(len(seen)) != boundary || seen[boundary+1] {
		t.Fatal("history dropped events or crossed the snapshot")
	}
	if _, err := b.recentHistory("foreign", s.ConversationID, nil, nil, 100); err == nil {
		t.Fatal("foreign conversation read accepted")
	}
}

func TestConversationHTTPContractIsCanonicalAndAuthenticated(t *testing.T) {
	h, _, login, _ := brokerFixture(t)
	if result := appAuthRequest(h, "GET", "/api/conversations?storage=active&limit=50", "", ""); result.Code != 401 {
		t.Fatal("unauthenticated list accepted")
	}
	for _, path := range []string{"/api/conversations", "/api/conversations?storage=all&limit=50", "/api/conversations?storage=active&limit=999"} {
		if result := appAuthRequest(h, "GET", path, login.Token, ""); result.Code != 400 {
			t.Fatalf("invalid parameters accepted: %s %d", path, result.Code)
		}
	}
	if result := appAuthRequest(h, "GET", "/api/sessions", login.Token, ""); result.Code != 405 {
		t.Fatal("retired session list remained available")
	}
	result := appAuthRequest(h, "GET", "/api/conversations?storage=active&limit=50", login.Token, "")
	if result.Code != 200 || result.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("canonical conversation list failed: %d %s", result.Code, result.Body.String())
	}
}
