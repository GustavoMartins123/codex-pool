package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAccountControlsPersistAndEnforcePinnedDraining(t *testing.T) {
	p, pool := ownershipFixture(t)
	h := &proxyHandler{passport: p, pool: pool}
	a := pool.allAccounts()[1]
	key := conversationPinKey("alice", "existing")
	pool.pinForUser("alice", key, a.ID)
	if err := p.updateAccountControls("alice", a.Type, a.ID, 1, accountControls{State: accountDraining, MaxConcurrent: 1}, "finish conversations"); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []RoutingProfile{RoutingLegacy, RoutingBalanced} {
		if got, _, _, _, _, _ := pool.candidateWithRoutingTraceForUser("alice", "new", nil, a.Type, "", "", "gpt-5.5", profile); got != nil {
			t.Fatal("draining admitted a new conversation")
		}
		if got, _, _, _, _, _ := pool.candidateWithRoutingTraceForUser("alice", key, nil, a.Type, "", "", "gpt-5.5", profile); got != a {
			t.Fatal("draining lost an existing pin")
		}
	}
	release, err := h.acquireAccountSlot("alice", key, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.acquireAccountSlot("alice", key, a); err == nil {
		t.Fatal("concurrency limit bypassed by a pin")
	}
	body := &accountLeaseBody{ReadCloser: io.NopCloser(strings.NewReader("stream")), release: release}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acquireAccountSlot("alice", key, a); err == nil {
		t.Fatal("slot released before response closed")
	}
	body.Close()
	body.Close()
	next, err := h.acquireAccountSlot("alice", key, a)
	if err != nil {
		t.Fatal(err)
	}
	next()
	if err := p.updateAccountControls("alice", a.Type, a.ID, 2, accountControls{State: accountMaintenance, MaxConcurrent: 1}, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acquireAccountSlot("alice", key, a); err == nil {
		t.Fatal("maintenance admitted a pin")
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	pool.accountAuthority = reloaded
	if got := pool.candidateForUser("alice", key, nil, a.Type, "", ""); got != nil {
		t.Fatal("restart lost maintenance")
	}
	restoreValidatedAccount(a, "test")
	if got := pool.candidateForUser("alice", key, nil, a.Type, "", ""); got != nil {
		t.Fatal("refresh bypassed maintenance")
	}
}

func TestAccountConcurrencySurvivesReloadAndRaces(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	if err := p.updateAccountControls("alice", a.Type, a.ID, 1, accountControls{MaxConcurrent: 1}, "limit concurrency"); err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var ready, finished sync.WaitGroup
	start, end := make(chan struct{}), make(chan struct{})
	ready.Add(24)
	finished.Add(24)
	for i := 0; i < 24; i++ {
		go func() {
			defer finished.Done()
			<-start
			release, err := p.controlAdmission(a.Type, a.ID, false, true)
			if err == nil {
				admitted.Add(1)
			}
			ready.Done()
			<-end
			if release != nil {
				release()
			}
		}()
	}
	close(start)
	ready.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted %d", admitted.Load())
	}
	pool.replace([]*Account{{ID: a.ID, Type: a.Type, PlanType: "pro"}})
	if _, err := p.controlAdmission(a.Type, a.ID, false, true); err == nil {
		t.Fatal("reload reset concurrency")
	}
	close(end)
	finished.Wait()
	release, err := p.controlAdmission(a.Type, a.ID, false, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAccountControlsAPIRequiresOwnershipRevisionAndCSRF(t *testing.T) {
	p, pool := ownershipFixture(t)
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	alice, csrf := addTestPassportOperator(t, p, "alice")
	bob, bobCSRF := addTestPassportOperator(t, p, "bob")
	path := "/api/accounts/codex/private/controls"
	for _, tc := range []struct {
		cookie, csrf, body string
		code               int
	}{
		{bob, bobCSRF, `{"revision":1,"controls":{"state":"maintenance","max_concurrent":2},"reason":"work"}`, 403},
		{alice, "", `{"revision":1,"controls":{"state":"maintenance","max_concurrent":2},"reason":"work"}`, 403},
		{alice, csrf, `{"revision":2,"controls":{"state":"maintenance","max_concurrent":2},"reason":"work"}`, 409},
		{alice, csrf, `{"revision":1,"controls":{"state":"maintenance"},"reason":"work"}`, 400},
		{alice, csrf, `{"revision":1,"controls":{"state":"unknown","max_concurrent":2},"reason":"work"}`, 400},
		{alice, csrf, `{"revision":1,"reason":"work"}`, 400},
		{alice, csrf, `{"revision":1,"controls":{"state":"maintenance","max_concurrent":2},"reason":"work"}`, 200},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, passportContributionRequest("PUT", path, tc.cookie, tc.csrf, []byte(tc.body)))
		if w.Code != tc.code {
			t.Fatalf("status %d, want %d: %s", w.Code, tc.code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", path, alice, "", nil))
	var view accountControlView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || view.Revision != 2 || view.Controls.State != accountMaintenance {
		t.Fatalf("view: %s %v", w.Body.String(), err)
	}
	entries, err := p.recentAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Action == "account.controls_updated" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("audit count %d", count)
	}
	if _, err := p.controlAdmission(AccountTypeCodex, "private", true, true); err == nil {
		t.Fatal("API did not enforce maintenance")
	}
}
