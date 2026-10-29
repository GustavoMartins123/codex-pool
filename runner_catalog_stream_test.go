package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func catalogStreamFixture(t *testing.T) (*proxyHandler, *sessionBroker, string, string) {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "synthetic-catalog-stream-secret")
	h, principal, _, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.sessionBroker()
	if err != nil {
		t.Fatal(err)
	}
	return h, b, principal, login.Token
}

func readCatalogSnapshot(t *testing.T, reader *bufio.Scanner) runnerCatalogSnapshot {
	t.Helper()
	for reader.Scan() {
		if data, ok := strings.CutPrefix(reader.Text(), "data: "); ok {
			var snapshot runnerCatalogSnapshot
			if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision == "" {
				t.Fatal("missing revision")
			}
			return snapshot
		}
	}
	t.Fatalf("catalog stream ended: %v", reader.Err())
	return runnerCatalogSnapshot{}
}

func TestRunnerCatalogStreamResynchronizesAndPublishesRunnerChanges(t *testing.T) {
	h, b, principal, token := catalogStreamFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	open := func(cursor string) (*http.Response, *bufio.Scanner) {
		r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/me/runner-catalog/events", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Last-Event-ID", cursor)
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			t.Fatalf("events = %d", response.StatusCode)
		}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		return response, scanner
	}
	response, reader := open("lost-cursor")
	defer response.Body.Close()
	initial := readCatalogSnapshot(t, reader)
	if len(initial.Runners) != 0 {
		t.Fatal("unexpected runners")
	}
	runner := brokerRunner(t, b, principal)
	connected := readCatalogSnapshot(t, reader)
	if len(connected.Runners) != 1 || connected.Runners[0].ID != runner.id || !connected.Runners[0].Online || connected.Revision == initial.Revision {
		t.Fatal("connection was not published")
	}
	models := []runnerproto.AgentModel{{Agent: "codex", Model: "gpt-5.6-sol", ReasoningEfforts: []string{"high"}}}
	capabilities := &runnerproto.ModelCapabilities{Revision: runnerproto.ModelsRevision(models), Models: models}
	if err := b.updateRunnerModels(runner, capabilities); err != nil {
		t.Fatal(err)
	}
	updated := readCatalogSnapshot(t, reader)
	if updated.Runners[0].CatalogRevision != capabilities.Revision || updated.Revision == connected.Revision {
		t.Fatal("new capabilities were not published")
	}
	change := b.catalogChanges()
	if err := b.updateRunnerModels(runner, capabilities); err != nil {
		t.Fatal(err)
	}
	select {
	case <-change:
		t.Fatal("identical capabilities triggered another update")
	default:
	}
	b.disconnectRunner(runner)
	offline := readCatalogSnapshot(t, reader)
	if offline.Runners[0].Online {
		t.Fatal("offline runner is still advertised online")
	}
	response.Body.Close()
	resumed, resumedReader := open(initial.Revision)
	defer resumed.Body.Close()
	latest := readCatalogSnapshot(t, resumedReader)
	if latest.Revision != offline.Revision || latest.Runners[0].Online {
		t.Fatal("reconnect did not send the latest full snapshot")
	}
}

func TestAppRunnerCatalogConditionalGetAndAccountScope(t *testing.T) {
	h, b, principal, token := catalogStreamFixture(t)
	runner := brokerRunner(t, b, principal)
	request := func(token, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/me/runner-catalog", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	first := request(token, "")
	if first.Code != 200 || first.Header().Get("ETag") == "" {
		t.Fatalf("snapshot = %d %s", first.Code, first.Body.String())
	}
	if next := request(token, first.Header().Get("ETag")); next.Code != 304 || next.Body.Len() != 0 {
		t.Fatal("unchanged snapshot was resent")
	}
	if unauthorized := request("", ""); unauthorized.Code != 401 {
		t.Fatal("unauthenticated catalog accepted")
	}
	if err := b.revokeRunner(principal, runner.id); err != nil {
		t.Fatal(err)
	}
	if next := request(token, first.Header().Get("ETag")); next.Code != 200 || next.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatal("revocation did not change the catalog")
	}
}

func TestRunnerCatalogUpdatesDoNotExpandAnExistingSession(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	oldModels := []runnerproto.AgentModel{{Agent: "claude", Model: "synthetic-model", ReasoningEfforts: []string{"high"}}}
	if err := b.updateRunnerModels(r, &runnerproto.ModelCapabilities{Revision: runnerproto.ModelsRevision(oldModels), Models: oldModels}); err != nil {
		t.Fatal(err)
	}
	s, err := b.create(principal, login.ClientID, runnerproto.CreateSession{Agent: "claude", Model: "synthetic-model", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, PermissionMode: "gated", RunnerID: r.id})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	newModels := []runnerproto.AgentModel{{Agent: "claude", Model: "new-model"}}
	if err := b.updateRunnerModels(r, &runnerproto.ModelCapabilities{Revision: runnerproto.ModelsRevision(newModels), Models: newModels}); err != nil {
		t.Fatal(err)
	}
	stored, _ := b.session(principal, s.ID)
	if stored.Capabilities == nil || stored.Capabilities.Revision != runnerproto.ModelsRevision(oldModels) {
		t.Fatal("session capabilities changed underneath the running agent")
	}
	model := "new-model"
	if _, err := b.startTurn(principal, s.ID, runnerproto.TurnInput{Input: "continue", Model: &model}, authorizeSyntheticSession); err == nil {
		t.Fatal("new runner model leaked into the old session")
	}
	model = "synthetic-model"
	if _, err := b.startTurn(principal, s.ID, runnerproto.TurnInput{Input: "continue", Model: &model}, authorizeSyntheticSession); err != nil {
		t.Fatalf("running session lost its original model: %v", err)
	}
}

func TestCatalogStreamRechecksPolicyAndStopsForRevokedAppCredentials(t *testing.T) {
	h, _, principal, token := catalogStreamFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/me/runner-catalog/events", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	_, client := h.authenticateAppRequest(r)
	if client == nil {
		t.Fatal("missing app credential")
	}
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewScanner(response.Body)
	reader.Buffer(make([]byte, 4096), 8<<20)
	initial := readCatalogSnapshot(t, reader)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"claude"}}})
	updated := readCatalogSnapshot(t, reader)
	if updated.Revision == initial.Revision {
		t.Fatal("account policy change did not update the snapshot")
	}
	for _, model := range updated.Models {
		if model.ID != "claude-opus-5-5" {
			t.Fatalf("policy denied model remained visible: %s", model.ID)
		}
	}
	if err := h.passport.revokeClient(principal, principal, client.ID); err != nil {
		t.Fatal(err)
	}
	for reader.Scan() {
		if strings.HasPrefix(reader.Text(), "data:") {
			t.Fatal("revoked app received another snapshot")
		}
	}
	if ctx.Err() != nil {
		t.Fatal("revoked app stream did not close on the next authorization check")
	}
}
