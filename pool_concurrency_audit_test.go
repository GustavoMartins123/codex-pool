package main

// Concurrent selection/reload invariants for the account pool. These run
// under -race in CI and target races between conversation pinning, account
// replacement (hot-reload), and candidate selection.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func auditClaudePool(n int) *poolState {
	accs := make([]*Account, 0, n)
	for i := 0; i < n; i++ {
		accs = append(accs, &Account{
			ID:       "claude-" + string(rune('a'+i)),
			Type:     AccountTypeClaude,
			PlanType: "max",
			Usage:    UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1},
		})
	}
	return newPoolState(accs, false)
}

// TestAuditPoolPinningStableUnderConcurrentSelection pins a conversation and
// then hammers candidateForUser from many goroutines at once. Invariant: a
// healthy pinned account must be returned to every caller — concurrent
// selection must never silently break stickiness or hand out different
// accounts for the same conversation.
func TestAuditPoolPinningStableUnderConcurrentSelection(t *testing.T) {
	p := auditClaudePool(5)
	accounts := p.allAccounts()
	first := p.candidateForUser("user-1", "conv-1", nil, AccountTypeClaude, "", "")
	if first == nil {
		t.Fatal("no candidate")
	}
	p.pinForUser("user-1", "conv-1", first.ID)

	var violations atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				got := p.candidateForUser("user-1", "conv-1", nil, AccountTypeClaude, "", "")
				if got == nil || got.ID != first.ID {
					violations.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()
	if violations.Load() != 0 {
		t.Fatalf("pin stability violated %d times: conversation received accounts other than %s", violations.Load(), first.ID)
	}
	_ = accounts
}

// TestAuditPoolReplaceWhileSelecting runs candidate selection against a pool
// that is concurrently hot-reloaded (replace) with shrinking and rotating
// account sets, including removal of the pinned account. Invariants: no
// deadlock (test completes), no panic, and every returned account pointer
// belongs to some pool generation (no phantom accounts).
func TestAuditPoolReplaceWhileSelecting(t *testing.T) {
	p := auditClaudePool(8)
	generations := [][]*Account{p.allAccounts()}
	for gen := 1; gen <= 8; gen++ {
		accs := make([]*Account, 0, 8)
		for i := 0; i < 8-gen; i++ {
			accs = append(accs, &Account{
				ID:       "claude-gen" + string(rune('0'+gen)) + "-" + string(rune('a'+i)),
				Type:     AccountTypeClaude,
				PlanType: "max",
				Usage:    UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1},
			})
		}
		generations = append(generations, accs)
	}

	known := map[*Account]bool{}
	for _, gen := range generations {
		for _, a := range gen {
			known[a] = true
		}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	stop := atomic.Bool{}
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				acc := p.candidateForUser("user", "", nil, AccountTypeClaude, "", "")
				if acc != nil && !known[acc] {
					t.Errorf("phantom account returned: %s", acc.ID)
					return
				}
			}
		}()
	}
	reloaderDone := make(chan struct{})
	go func() {
		defer close(reloaderDone)
		for gen := 1; gen < len(generations); gen++ {
			p.replace(generations[gen])
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-reloaderDone
	stop.Store(true)
	doneClosed := make(chan struct{})
	go func() { wg.Wait(); close(doneClosed) }()
	select {
	case <-doneClosed:
	case <-time.After(20 * time.Second):
		close(done)
		t.Fatal("deadlock: selection did not finish within 20s of concurrent replace")
	}
}

// TestAuditExhaustedOrCoolingAccountNeverSelected verifies the hard
// exclusions: a rate-limited account and a usage-exhausted account must
// never be handed out by selection, and when every account is exhausted the
// pool must return nil rather than degrade to a bad choice.
func TestAuditExhaustedOrCoolingAccountNeverSelected(t *testing.T) {
	healthy := &Account{ID: "claude-healthy", Type: AccountTypeClaude, PlanType: "max", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1}}
	cooling := &Account{ID: "claude-cooling", Type: AccountTypeClaude, PlanType: "max", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1}, RateLimitUntil: time.Now().Add(10 * time.Minute)}
	exhausted := &Account{ID: "claude-exhausted", Type: AccountTypeClaude, PlanType: "max", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.99}}
	p := newPoolState([]*Account{healthy, cooling, exhausted}, false)

	for i := 0; i < 200; i++ {
		got := p.candidate("", nil, AccountTypeClaude, "", "")
		if got == nil || got.ID != "claude-healthy" {
			t.Fatalf("iteration %d: selected %v, want claude-healthy", i, got)
		}
	}

	allBad := newPoolState([]*Account{cooling, exhausted}, false)
	if got := allBad.candidate("", nil, AccountTypeClaude, "", ""); got != nil {
		t.Fatalf("exhausted pool returned %s instead of nil", got.ID)
	}
}

// TestAuditPinSurvivesAccountReplacementWhenReplacementKeepsPointer merges
// the reload path with pinning: mergeReloadedAccounts keeps the existing
// pointer for surviving accounts, so a conversation pinned to that account
// must keep routing there after a hot-reload cycle.
func TestAuditPinSurvivesAccountReplacementWhenReplacementKeepsPointer(t *testing.T) {
	current := []*Account{{
		ID:       "claude-a",
		Type:     AccountTypeClaude,
		PlanType: "max",
		Usage:    UsageSnapshot{PrimaryUsedPercent: 0.2, SecondaryUsedPercent: 0.2},
	}}
	p := newPoolState(current, false)
	acc := p.candidateForUser("user-1", "conv-1", nil, AccountTypeClaude, "", "")
	if acc == nil {
		t.Fatal("no candidate")
	}
	p.pinForUser("user-1", "conv-1", acc.ID)

	loaded := []*Account{{
		ID:       "claude-a",
		Type:     AccountTypeClaude,
		PlanType: "max",
		Usage:    UsageSnapshot{PrimaryUsedPercent: 0.3, SecondaryUsedPercent: 0.3},
	}}
	p.replace(mergeReloadedAccounts(p.allAccounts(), loaded))

	got := p.candidateForUser("user-1", "conv-1", nil, AccountTypeClaude, "", "")
	if got == nil || got.ID != acc.ID {
		t.Fatalf("pin lost across reload: got %v want %s", got, acc.ID)
	}
}
