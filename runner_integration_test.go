package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/runner"
	"codex-pool-proxy/internal/runnerproto"
)

func TestRunnerSessionEndToEnd(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "fake-agent")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command("go", "test", "-c", "-o", exe, "./internal/runner")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI fixture: %v %s", err, output)
	}
	t.Setenv("POOL_JWT_SECRET", "synthetic-integration-secret")
	h, principal, _, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.startSessionBroker(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := h.sessionBroker()
	var downloads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/config/") {
			downloads.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	defer server.Close()
	p, err := runner.BeginPairing(ctx, server.Client(), server.URL, runnerproto.PairingRequest{Name: "integration", OS: "test", Arch: "test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	approval, _ := json.Marshal(runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode})
	if response := appAuthRequest(h, "POST", "/api/me/runner-pairings/"+p.ID, login.Token, string(approval)); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	credential, done, err := runner.CompletePairing(ctx, server.Client(), server.URL, *p)
	if err != nil || !done {
		t.Fatal(err)
	}
	created := appAuthRequest(h, "POST", "/api/sessions", login.Token, `{"agent":"codex","model":"gpt-5.6-sol","workspace":{"mode":"ephemeral"},"permission_mode":"gated","prompt":"synthetic input"}`)
	if created.Code != 201 {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var session runnerproto.Session
	_ = json.Unmarshal(created.Body.Bytes(), &session)
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/conversations/"+session.ConversationID+"/events", nil)
	request.Header.Set("Authorization", "Bearer "+login.Token)
	stream, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	var deltas atomic.Int32
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: turn.delta" {
				deltas.Add(1)
			}
		}
	}()
	runnerCtx, stopRunner := context.WithCancel(ctx)
	defer stopRunner()
	root := t.TempDir()
	result := make(chan error, 1)
	go func() {
		result <- runner.Run(runnerCtx, runner.Options{Config: credential, HTTPClient: server.Client(), WorkspaceRoot: root, Executables: map[string]string{"codex": exe}})
	}()
	await := func(predicate func(*runnerproto.Session) bool) *runnerproto.Session {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			state, err := b.session(principal, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if predicate(state) {
				return state
			}
			select {
			case <-ctx.Done():
				t.Fatalf("session integration timeout, state=%s", state.State)
			case err := <-result:
				t.Fatalf("runner stopped: %v", err)
			case <-ticker.C:
			}
		}
	}
	state := await(func(s *runnerproto.Session) bool { return len(s.Approvals) == 1 })
	b.mu.Lock()
	connection := b.runners[credential.RunnerID]
	connection.stop()
	b.mu.Unlock()
	// The in-memory process and its approval survive a real server-side WS disconnect.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		b.mu.Lock()
		replacement := b.runners[credential.RunnerID]
		b.mu.Unlock()
		if replacement != nil && replacement != connection {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("runner failed to reconnect")
		case <-ticker.C:
		}
	}
	decision := appAuthRequest(h, "POST", "/api/sessions/"+session.ID+"/approvals/"+state.Approvals[0].ID, login.Token, `{"decision":"approve"}`)
	if decision.Code != 204 {
		t.Fatalf("approve=%d", decision.Code)
	}
	await(func(s *runnerproto.Session) bool { return s.State == "waiting_input" && s.Turns == 1 })
	for index, decision := range []string{"deny", "cancel"} {
		turn := appAuthRequest(h, "POST", "/api/sessions/"+session.ID+"/turns", login.Token, `{"input":"another synthetic turn"}`)
		if turn.Code != 200 {
			t.Fatal(turn.Code)
		}
		state = await(func(s *runnerproto.Session) bool { return len(s.Approvals) == 1 })
		if decision == "deny" {
			response := appAuthRequest(h, "POST", "/api/sessions/"+session.ID+"/approvals/"+state.Approvals[0].ID, login.Token, `{"decision":"deny"}`)
			if response.Code != 204 {
				t.Fatal(response.Code)
			}
		} else {
			response := appAuthRequest(h, "POST", "/api/sessions/"+session.ID+"/turns:cancel", login.Token, "")
			if response.Code != 204 {
				t.Fatal(response.Code)
			}
		}
		await(func(s *runnerproto.Session) bool { return s.State == "waiting_input" && s.Turns == index+2 })
	}
	response := appAuthRequest(h, "DELETE", "/api/sessions/"+session.ID, login.Token, "")
	if response.Code != 204 {
		t.Fatal(response.Code)
	}
	state = await(func(s *runnerproto.Session) bool { return s.State == "closed" })
	if state.ClosedReason != "cancelled" || state.Summaries[2].ExitCode != 130 {
		t.Fatalf("unexpected terminal state %s", state.ClosedReason)
	}
	if downloads.Load() != 1 {
		t.Fatalf("session credential fetched %d times", downloads.Load())
	}
	if _, err := os.Stat(filepath.Join(root, session.ID)); err != nil {
		t.Fatal("workspace required for resume was removed")
	}
	if state.Checkpoint == nil || !state.Checkpoint.Ready {
		t.Fatal("native checkpoint unavailable after close")
	}
	if deltas.Load() < 3 {
		t.Fatal("agent output did not reach SSE")
	}
	stopRunner()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	<-streamDone
}
