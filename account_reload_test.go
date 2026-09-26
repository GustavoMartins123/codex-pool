package main

import (
	"sync"
	"testing"
	"time"
)

func TestMergeReloadedAccountsPreservesRuntimeState(t *testing.T) {
	cooldown := time.Now().Add(30 * time.Minute).UTC()
	current := &Account{
		Type:           AccountTypeCodex,
		ID:             "codex-one",
		AccessToken:    "old-token",
		RateLimitUntil: cooldown,
		Penalty:        12.5,
		BackoffLevel:   3,
		Usage: UsageSnapshot{
			SecondaryUsed:        0.18,
			SecondaryUsedPercent: 18,
			SecondaryResetAt:     time.Date(2026, 7, 21, 20, 40, 0, 0, time.UTC),
			RetrievedAt:          time.Now(),
			Source:               "wham",
			secondarySet:         true,
		},
	}
	loaded := &Account{Type: AccountTypeCodex, ID: "codex-one", AccessToken: "new-token", Disabled: true}

	merged := mergeReloadedAccounts([]*Account{current}, []*Account{loaded})

	if len(merged) != 1 || merged[0] != current {
		t.Fatal("surviving account must keep its pointer identity")
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.AccessToken != "new-token" {
		t.Fatalf("file-derived access token = %q", current.AccessToken)
	}
	if !current.Disabled {
		t.Fatal("file-derived disabled flag not applied")
	}
	if !current.RateLimitUntil.Equal(cooldown) {
		t.Fatalf("runtime cooldown lost: %v", current.RateLimitUntil)
	}
	if current.Penalty != 12.5 || current.BackoffLevel != 3 {
		t.Fatalf("runtime penalty/backoff lost: %v %v", current.Penalty, current.BackoffLevel)
	}
	if !current.Usage.secondarySet || current.Usage.SecondaryUsedPercent != 18 {
		t.Fatalf("usage telemetry lost: %#v", current.Usage)
	}
}

func TestMergeReloadedAccountsDoesNotCrossAccountTypes(t *testing.T) {
	current := &Account{Type: AccountTypeCodex, ID: "shared", Penalty: 4}
	loaded := &Account{Type: AccountTypeClaude, ID: "shared"}

	merged := mergeReloadedAccounts([]*Account{current}, []*Account{loaded})

	if len(merged) != 1 || merged[0] != loaded {
		t.Fatalf("merge crossed provider boundary: %d accounts", len(merged))
	}
	if loaded.Penalty != 0 {
		t.Fatal("runtime state crossed provider boundary")
	}
}

func TestMergeReloadedAccountsAddsAndDropsAccounts(t *testing.T) {
	stale := &Account{Type: AccountTypeCodex, ID: "codex-gone", Penalty: 9}
	keep := &Account{Type: AccountTypeCodex, ID: "codex-keep", Penalty: 9}
	fresh := &Account{Type: AccountTypeClaude, ID: "claude-new"}

	merged := mergeReloadedAccounts([]*Account{stale, keep}, []*Account{keep, fresh})

	if len(merged) != 2 {
		t.Fatalf("merged count = %d, want 2", len(merged))
	}
	if merged[0] != keep || merged[1] != fresh {
		t.Fatal("merged identities wrong")
	}
	if keep.Penalty != 9 {
		t.Fatal("survivor lost runtime state")
	}
}

// Regression test for the reload race: a response still being processed when
// the pool hot-reloads must land its cooldown on the account the pool now
// serves, not on a silently-discarded object.
func TestMergeReloadedAccountsLateMutationSurvivesReload(t *testing.T) {
	existing := &Account{Type: AccountTypeCodex, ID: "codex-one", AccessToken: "old"}
	first := mergeReloadedAccounts(
		[]*Account{existing},
		[]*Account{{Type: AccountTypeCodex, ID: "codex-one", AccessToken: "new"}},
	)

	// A 429 response processed after the reload writes to the pointer the
	// request captured before the reload happened.
	lateCooldown := time.Now().Add(time.Hour).UTC()
	first[0].mu.Lock()
	first[0].RateLimitUntil = lateCooldown
	first[0].mu.Unlock()

	// A second reload must not drop the late cooldown.
	second := mergeReloadedAccounts(
		first,
		[]*Account{{Type: AccountTypeCodex, ID: "codex-one", AccessToken: "newer"}},
	)

	if second[0] != existing || first[0] != existing {
		t.Fatal("pointer identity lost across reloads")
	}
	existing.mu.Lock()
	defer existing.mu.Unlock()
	if existing.AccessToken != "newer" {
		t.Fatalf("file token = %q, want newer", existing.AccessToken)
	}
	if !existing.RateLimitUntil.Equal(lateCooldown) {
		t.Fatalf("late cooldown lost: %v", existing.RateLimitUntil)
	}
}

// Mutations racing with the reload itself must serialize under the account
// mutex and none may be lost.
func TestMergeReloadedAccountsConcurrentMutationsAllLand(t *testing.T) {
	existing := &Account{Type: AccountTypeGemini, ID: "gemini-one", AccessToken: "old"}
	const writers = 32
	start := time.Now()
	var writersDone sync.WaitGroup
	var mergeDone sync.WaitGroup
	release := make(chan struct{})

	writersDone.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer writersDone.Done()
			<-release
			existing.mu.Lock()
			until := time.Now().Add(time.Duration(i+1) * time.Minute)
			if until.After(existing.RateLimitUntil) {
				existing.RateLimitUntil = until
			}
			existing.mu.Unlock()
		}(i)
	}
	mergeDone.Add(1)
	go func() {
		defer mergeDone.Done()
		<-release
		merged := mergeReloadedAccounts(
			[]*Account{existing},
			[]*Account{{Type: AccountTypeGemini, ID: "gemini-one", AccessToken: "new"}},
		)
		if len(merged) != 1 || merged[0] != existing {
			t.Error("merge lost pointer identity")
		}
	}()

	close(release)
	writersDone.Wait()
	mergeDone.Wait()

	existing.mu.Lock()
	cooldown := existing.RateLimitUntil
	token := existing.AccessToken
	existing.mu.Unlock()
	if token != "new" {
		t.Fatalf("file token = %q, want new", token)
	}
	// Every writer proposed now_i + (i+1)m; the max-fold guarantees the final
	// cooldown is at least testStart + writers minutes.
	if cooldown.Before(start.Add(time.Duration(writers) * time.Minute)) {
		t.Fatalf("a concurrent cooldown mutation was lost: %v", cooldown)
	}
}
