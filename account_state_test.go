package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"codex-pool-proxy/internal/accountstate"
)

// The unified state must never disagree with the routing gates: this is the
// parity invariant that makes the projection trustworthy.
func TestUnifiedStateMatchesRoutingAvailability(t *testing.T) {
	now := time.Now()
	accounts := []*Account{
		{},
		{Dead: true},
		{Disabled: true},
		{NeedsVerification: true},
		{VerificationURL: "https://verify.example/x"},
		{HealthError: "probe failed"},
		{RateLimitUntil: now.Add(time.Minute)},
		{RateLimitUntil: now.Add(-time.Minute)},
		{Usage: UsageSnapshot{PrimaryUsedPercent: 0.96, PrimaryResetAt: now.Add(time.Hour)}},
		{Usage: UsageSnapshot{SecondaryUsedPercent: 0.995, SecondaryResetAt: now.Add(40 * time.Hour)}},
		{Usage: UsageSnapshot{PrimaryUsedPercent: 0.94}},
		{ExpiresAt: now.Add(-time.Hour)},
		{Dead: true, Disabled: true},
		{Dead: true, RateLimitUntil: now.Add(time.Minute)},
		{Disabled: true, HealthError: "x"},
	}
	for i, a := range accounts {
		state := accountLifecycleStateLocked(a, now)
		wantRoutable := accountAvailableForRoutingLocked(a, now)
		if got := accountstate.Routable(state); got != wantRoutable {
			t.Fatalf("account[%d] state=%s routable=%v, routing gate says %v", i, state, got, wantRoutable)
		}
	}
}

func TestObserveAccountStatesRecordsTransitions(t *testing.T) {
	now := time.Now()
	acc := &Account{ID: "codex-1", Usage: UsageSnapshot{PrimaryUsedPercent: 0.96, PrimaryResetAt: now.Add(time.Hour)}}
	p := newPoolState([]*Account{acc}, false)

	p.observeAccountStates(now)
	if acc.LifecycleState != accountstate.StateCooldown {
		t.Fatalf("initial state = %s, want cooldown", acc.LifecycleState)
	}
	if len(acc.LifecycleHistory) != 0 {
		t.Fatalf("seeding must not record a transition, got %+v", acc.LifecycleHistory)
	}

	// Window resets: usage drops below thresholds.
	acc.mu.Lock()
	acc.Usage.PrimaryUsedPercent = 0.1
	acc.mu.Unlock()
	p.observeAccountStates(now.Add(2 * time.Hour))
	if acc.LifecycleState != accountstate.StateHealthy {
		t.Fatalf("state after reset = %s, want healthy", acc.LifecycleState)
	}
	if len(acc.LifecycleHistory) != 1 {
		t.Fatalf("expected 1 recorded transition, got %+v", acc.LifecycleHistory)
	}
	tr := acc.LifecycleHistory[0]
	if tr.From != accountstate.StateCooldown || tr.To != accountstate.StateHealthy {
		t.Fatalf("unexpected transition %+v", tr)
	}

	// Sweep again with no fact change: no duplicate transitions.
	p.observeAccountStates(now.Add(3 * time.Hour))
	if len(acc.LifecycleHistory) != 1 {
		t.Fatalf("expected stable history, got %+v", acc.LifecycleHistory)
	}
}

func TestAccountStateSnapshotFeedsAdmin(t *testing.T) {
	now := time.Now()
	acc := &Account{ID: "codex-2", Dead: true}
	snap := accountStateSnapshotLocked(acc, now)
	if snap.State != accountstate.StateDead || snap.Reason != "permanent_failure" || snap.Routable {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	healthy := &Account{ID: "codex-3"}
	snap = accountStateSnapshotLocked(healthy, now)
	if snap.State != accountstate.StateHealthy || !snap.Routable {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestTransitionHistoryIsBounded(t *testing.T) {
	var history []accountstate.Transition
	for i := 0; i < maxAccountStateHistory+10; i++ {
		history = appendAccountTransition(history, accountstate.Transition{
			From: accountstate.StateHealthy, To: accountstate.StateCooldown, Reason: "x", At: time.Now(),
		})
	}
	if len(history) != maxAccountStateHistory {
		t.Fatalf("history length = %d, want %d", len(history), maxAccountStateHistory)
	}
}

func TestServeAccountsExposesUnifiedState(t *testing.T) {
	dead := &Account{ID: "dead", Type: AccountTypeCodex, Dead: true}
	healthy := &Account{ID: "ok", Type: AccountTypeCodex}
	p := newPoolState([]*Account{dead, healthy}, false)
	p.observeAccountStates(time.Now())

	h := &proxyHandler{pool: p}
	rec := httptest.NewRecorder()
	h.serveAccounts(rec, httptest.NewRequest("GET", "/admin/accounts", nil))

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("bad response: %v (%s)", err, rec.Body.String())
	}
	states := map[string]string{}
	for _, row := range rows {
		id, _ := row["id"].(string)
		state, _ := row["state"].(string)
		states[id] = state
	}
	if states["dead"] != string(accountstate.StateDead) {
		t.Fatalf("dead account state = %q, want %q", states["dead"], accountstate.StateDead)
	}
	if states["ok"] != string(accountstate.StateHealthy) {
		t.Fatalf("healthy account state = %q, want %q", states["ok"], accountstate.StateHealthy)
	}
}
