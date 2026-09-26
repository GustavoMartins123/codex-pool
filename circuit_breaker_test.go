package main

import (
	"testing"
	"time"
)

func TestCircuitBreakerClosedToOpenToHalfOpenToClosed(t *testing.T) {
	cb := newCircuitBreakerManager()

	provider := "codex"
	accountID := "acc_test_1"
	model := "gpt-6-astra"

	// 1. Initial state is CLOSED
	allowed, reason, err := cb.AllowTarget(provider, accountID, model, nil)
	if !allowed || err != nil {
		t.Fatalf("expected allowed=true in CLOSED state, got %v (%s, %v)", allowed, reason, err)
	}

	// 2. Account+Model failure threshold is 2. First failure: remains CLOSED
	cb.RecordFailure(provider, accountID, model, nil, ErrorClassRateLimit)
	allowed, _, _ = cb.AllowTarget(provider, accountID, model, nil)
	if !allowed {
		t.Fatalf("expected still allowed after 1 failure")
	}

	// Second failure: Account+Model trips OPEN!
	cb.RecordFailure(provider, accountID, model, nil, ErrorClassRateLimit)
	allowed, reason, err = cb.AllowTarget(provider, accountID, model, nil)
	if allowed || err == nil {
		t.Fatalf("expected blocked after 2 failures, got allowed=%v", allowed)
	}
	if cb.State(accountModelKey(accountID, model)) != StateOpen {
		t.Fatalf("expected state OPEN, got %v", cb.State(accountModelKey(accountID, model)))
	}

	// 3. Crucial Criterion: Model failure does NOT trip other models on the same account!
	allowedOther, _, errOther := cb.AllowTarget(provider, accountID, "gpt-5.6-sol", nil)
	if !allowedOther || errOther != nil {
		t.Fatalf("expected other model gpt-5.6-sol to remain CLOSED on acc_test_1, got %v (%v)", allowedOther, errOther)
	}

	// 4. Force cooldown expiration to transition to HALF_OPEN
	entry := cb.getOrCreate(accountModelKey(accountID, model), "account_model", 2, 1, 10*time.Millisecond)
	entry.mu.Lock()
	entry.cooldownUntil = time.Now().Add(-1 * time.Second)
	entry.mu.Unlock()

	// 5. First request should be admitted as the single probe
	allowedProbe, _, errProbe := cb.AllowTarget(provider, accountID, model, nil)
	if !allowedProbe || errProbe != nil {
		t.Fatalf("expected probe to be admitted, got %v (%v)", allowedProbe, errProbe)
	}

	// A concurrent second request while probe is in flight should be rejected (controlled probe)
	allowedConcurrent, _, errConcurrent := cb.AllowTarget(provider, accountID, model, nil)
	if allowedConcurrent || errConcurrent != ErrCircuitHalfOpenProbing {
		t.Fatalf("expected concurrent request in HALF_OPEN to be rejected, got allowed=%v (%v)", allowedConcurrent, errConcurrent)
	}

	// 6. Probe succeeds -> transitions back to CLOSED!
	cb.RecordSuccess(provider, accountID, model, nil)
	if cb.State(accountModelKey(accountID, model)) != StateClosed {
		t.Fatalf("expected transition to CLOSED after probe success, got %v", cb.State(accountModelKey(accountID, model)))
	}

	// Requests now proceed normally again
	allowedAfter, _, _ := cb.AllowTarget(provider, accountID, model, nil)
	if !allowedAfter {
		t.Fatalf("expected allowed=true after recovery to CLOSED")
	}
}

func TestCircuitBreakerProbeFailureExtendsOpen(t *testing.T) {
	cb := newCircuitBreakerManager()
	accountID := "acc_test_probe_fail"
	model := "gpt-6-astra"

	// Trip to OPEN
	cb.RecordFailure("codex", accountID, model, nil, ErrorClassTransient)
	cb.RecordFailure("codex", accountID, model, nil, ErrorClassTransient)

	// Simulate cooldown expiry
	entry := cb.getOrCreate(accountModelKey(accountID, model), "account_model", 2, 1, 100*time.Millisecond)
	entry.mu.Lock()
	entry.cooldownUntil = time.Now().Add(-1 * time.Second)
	entry.mu.Unlock()

	// Probe enters HALF_OPEN
	allowed, _, _ := cb.AllowTarget("codex", accountID, model, nil)
	if !allowed {
		t.Fatalf("expected probe admitted")
	}

	// Probe fails -> trips immediately back to OPEN with extended cooldown
	cb.RecordFailure("codex", accountID, model, nil, ErrorClassTransient)
	if cb.State(accountModelKey(accountID, model)) != StateOpen {
		t.Fatalf("expected state OPEN after probe failure, got %v", cb.State(accountModelKey(accountID, model)))
	}
}

func TestProviderLevelCircuitBreaker(t *testing.T) {
	cb := newCircuitBreakerManager()
	provider := "degraded_provider"

	for i := 0; i < 5; i++ {
		cb.RecordFailure(provider, "", "", nil, ErrorClassTransient)
	}

	// Provider circuit should now be OPEN
	allowed, err := cb.AllowProvider(provider)
	if allowed || err != ErrCircuitOpen {
		t.Fatalf("expected provider to be blocked by OPEN circuit, got %v (%v)", allowed, err)
	}

	// Any account on this provider is blocked
	allowedTarget, reason, _ := cb.AllowTarget(provider, "acc_x", "some-model", nil)
	if allowedTarget {
		t.Fatalf("expected target on degraded provider to be blocked, reason: %s", reason)
	}
}

func TestAccountCapabilityCircuitBreaker(t *testing.T) {
	cb := newCircuitBreakerManager()
	accountID := "acc_cap_test"

	// Trip capability image_generation
	cb.RecordFailure("codex", accountID, "gpt-5.4", []string{"image_generation"}, ErrorClassTransient)
	cb.RecordFailure("codex", accountID, "gpt-5.4", []string{"image_generation"}, ErrorClassTransient)

	// image_generation request blocked
	allowed, reason, _ := cb.AllowTarget("codex", accountID, "gpt-5.4", []string{"image_generation"})
	if allowed {
		t.Fatalf("expected image_generation to be blocked on account %s, got reason=%s", accountID, reason)
	}

	// Plain text request without image_generation is allowed
	allowedText, _, errText := cb.AllowTarget("codex", accountID, "gpt-5.4", nil)
	if !allowedText || errText != nil {
		t.Fatalf("expected text request to still be allowed, got %v (%v)", allowedText, errText)
	}
}

func TestCircuitBreakerCandidateSelectionDoesNotConsumeUnselectedProbes(t *testing.T) {
	acc1 := &Account{ID: "acc_1", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.8}}
	acc2 := &Account{ID: "acc_2", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
	pool := newPoolState([]*Account{acc1, acc2}, false)

	// Trip both accounts and the provider to OPEN, then expire their cooldowns.
	for i := 0; i < 5; i++ {
		pool.circuitBreakers.RecordFailure("codex", "", "", nil, ErrorClassTransient)
	}
	for _, id := range []string{"acc_1", "acc_2"} {
		for i := 0; i < 3; i++ {
			pool.circuitBreakers.RecordFailure("codex", id, "gpt-5.5", nil, ErrorClassTransient)
		}
	}
	for _, key := range []string{
		providerKey("codex"),
		accountKey("acc_1"),
		accountKey("acc_2"),
		accountModelKey("acc_1", "gpt-5.5"),
		accountModelKey("acc_2", "gpt-5.5"),
	} {
		entry := pool.circuitBreakers.get(key)
		if entry == nil {
			t.Fatalf("missing circuit entry for %s", key)
		}
		entry.mu.Lock()
		entry.cooldownUntil = time.Now().Add(-time.Second)
		entry.mu.Unlock()
	}

	// Candidate selection evaluates both accounts, picks acc_2 (lower usage),
	// and must only claim the probe on acc_2 - not acc_1.
	decision := pool.smartCandidateForModelForUser("u1", "", nil, AccountTypeCodex, "", "127.0.0.1", "gpt-5.5", RoutingBalanced)
	if decision.Account == nil || decision.Account.ID != "acc_2" {
		t.Fatalf("expected acc_2 to be selected as probe, got %+v", decision.Account)
	}

	// Complete acc_2 probe -> provider and acc_2 recover to CLOSED.
	pool.circuitBreakers.RecordSuccess("codex", "acc_2", "gpt-5.5", nil)

	// acc_1 must NOT be stuck in HALF_OPEN with probesInFlight=1; it must still be eligible to probe.
	allowed, _, err := pool.circuitBreakers.AllowTarget("codex", "acc_1", "gpt-5.5", nil)
	if !allowed || err != nil {
		t.Fatalf("unselected account acc_1 should still be able to probe, got allowed=%v err=%v", allowed, err)
	}
}

func TestCircuitBreakerAllowTargetDoesNotLeakParentProbeWhenChildOpen(t *testing.T) {
	cb := newCircuitBreakerManager()
	for i := 0; i < 5; i++ {
		cb.RecordFailure("codex", "", "", nil, ErrorClassTransient)
	}
	cb.RecordFailure("codex", "acc_1", "gpt-6-astra", nil, ErrorClassTransient)
	cb.RecordFailure("codex", "acc_1", "gpt-6-astra", nil, ErrorClassTransient)

	// Provider cooldown expires, but account+model is still in cooldown.
	provEntry := cb.get(providerKey("codex"))
	provEntry.mu.Lock()
	provEntry.cooldownUntil = time.Now().Add(-time.Second)
	provEntry.mu.Unlock()

	// Request to acc_1/gpt-6-astra must be rejected by account_model circuit.
	allowed, _, err := cb.AllowTarget("codex", "acc_1", "gpt-6-astra", nil)
	if allowed || err != ErrCircuitOpen {
		t.Fatalf("expected rejection by open model circuit, got allowed=%v err=%v", allowed, err)
	}

	// Provider probe slot must NOT have been consumed by the rejected target check.
	allowedOther, _, errOther := cb.AllowTarget("codex", "acc_2", "gpt-5.5", nil)
	if !allowedOther || errOther != nil {
		t.Fatalf("expected provider probe to remain available for acc_2, got allowed=%v err=%v", allowedOther, errOther)
	}
}

func TestCircuitBreakerNonTrippingFailureReleasesHalfOpenProbe(t *testing.T) {
	cb := newCircuitBreakerManager()
	cb.RecordFailure("codex", "acc_1", "gpt-5.5", nil, ErrorClassTransient)
	cb.RecordFailure("codex", "acc_1", "gpt-5.5", nil, ErrorClassTransient)

	for _, key := range []string{accountKey("acc_1"), accountModelKey("acc_1", "gpt-5.5")} {
		if entry := cb.get(key); entry != nil {
			entry.mu.Lock()
			entry.cooldownUntil = time.Now().Add(-time.Second)
			entry.mu.Unlock()
		}
	}

	allowed, _, err := cb.AllowTarget("codex", "acc_1", "gpt-5.5", nil)
	if !allowed || err != nil {
		t.Fatalf("expected probe admitted, got %v (%v)", allowed, err)
	}

	// Probe finishes with a client 400 (ErrorClassInvalid) or context cancel (ErrorClassNone).
	cb.RecordFailure("codex", "acc_1", "gpt-5.5", nil, ErrorClassInvalid)

	// Probe slot must be released so the next request is not deadlocked in ErrCircuitHalfOpenProbing.
	allowedNext, _, errNext := cb.AllowTarget("codex", "acc_1", "gpt-5.5", nil)
	if !allowedNext || errNext != nil {
		t.Fatalf("expected probe slot released after non-tripping error, got allowed=%v err=%v", allowedNext, errNext)
	}
}

func TestCircuitBreakerStaleProbeLeaseExpires(t *testing.T) {
	cb := newCircuitBreakerManager()
	cb.RecordFailure("codex", "acc_1", "gpt-5.5", nil, ErrorClassTransient)
	cb.RecordFailure("codex", "acc_1", "gpt-5.5", nil, ErrorClassTransient)

	entry := cb.get(accountModelKey("acc_1", "gpt-5.5"))
	entry.mu.Lock()
	entry.cooldownUntil = time.Now().Add(-time.Second)
	entry.cooldownDuration = 10 * time.Millisecond
	entry.mu.Unlock()

	if ok, _, _ := cb.AllowTarget("codex", "acc_1", "gpt-5.5", nil); !ok {
		t.Fatal("expected initial probe to be admitted")
	}

	// Simulate an abandoned probe whose lease expired > 5s ago.
	entry.mu.Lock()
	entry.probeStartedAt = time.Now().Add(-10 * time.Second)
	entry.mu.Unlock()

	if ok, _, err := cb.AllowTarget("codex", "acc_1", "gpt-5.5", nil); !ok || err != nil {
		t.Fatalf("expected expired probe lease to admit new probe, got ok=%v err=%v", ok, err)
	}
}