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
