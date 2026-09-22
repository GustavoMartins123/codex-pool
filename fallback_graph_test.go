package main

import (
	"testing"
)

func TestFallbackGraphOn429AndUnavailable(t *testing.T) {
	fg := newFallbackGraph()

	// Text-only request
	caps := RequestCapabilities{
		ContextTokens: 2000,
		Modalities:    []string{"text"},
	}

	pool := newPoolState([]*Account{
		{ID: "codex_1", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude_1", Type: AccountTypeClaude, PlanType: "pro"},
	}, false)

	cb := newCircuitBreakerManager()

	// 1. Fallback on 429 for gpt-6-astra -> gpt-5.6-sol
	target, reason, ok := fg.ResolveFallback("gpt-6-astra", Trigger429, caps, pool, cb)
	if !ok || target != "gpt-5.6-sol" {
		t.Fatalf("expected fallback to gpt-5.6-sol on 429, got %q (ok=%v, reason=%s)", target, ok, reason)
	}

	// 2. Fallback on unavailable for gpt-5.6-sol -> claude-sonnet-5
	target, reason, ok = fg.ResolveFallback("gpt-5.6-sol", TriggerUnavailable, caps, pool, cb)
	if !ok || target != "claude-opus-5" && target != "claude-sonnet-5" {
		t.Fatalf("expected fallback to claude-sonnet-5 on unavailable, got %q (ok=%v, reason=%s)", target, ok, reason)
	}
}

func TestFallbackGraphCompatibilityFilterRejectsIncompatible(t *testing.T) {
	fg := newFallbackGraph()

	// Request with image input
	capsWithImage := RequestCapabilities{
		ContextTokens: 2000,
		Modalities:    []string{"text", "image"},
	}

	pool := newPoolState([]*Account{
		{ID: "codex_1", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude_1", Type: AccountTypeClaude, PlanType: "pro"},
	}, false)

	cb := newCircuitBreakerManager()

	// Configure a test route where first fallback does NOT support image (e.g. MiniMax-M2.7 text-only),
	// second fallback supports image (e.g. claude-sonnet-5).
	fg.SetRoute("test-model", FallbackRule{
		On429: []string{"MiniMax-M2.7", "claude-sonnet-5"},
	})

	target, _, ok := fg.ResolveFallback("test-model", Trigger429, capsWithImage, pool, cb)
	if !ok || target != "claude-opus-5" && target != "claude-sonnet-5" {
		t.Fatalf("expected compatibility filter to skip MiniMax-M2.7 (no image) and choose claude-sonnet-5, got %q (ok=%v)", target, ok)
	}
}

func TestFallbackGraphBypassesCircuitOpenProvider(t *testing.T) {
	fg := newFallbackGraph()

	caps := RequestCapabilities{
		ContextTokens: 1000,
		Modalities:    []string{"text"},
	}

	pool := newPoolState([]*Account{
		{ID: "codex_1", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude_1", Type: AccountTypeClaude, PlanType: "pro"},
	}, false)

	cb := newCircuitBreakerManager()

	// Trip Codex provider circuit
	for i := 0; i < 5; i++ {
		cb.RecordFailure("codex", "", "", nil, ErrorClassTransient)
	}

	// gpt-6-astra fallback on 429 has gpt-5.6-sol (codex) first, then claude-sonnet-5
	target, _, ok := fg.ResolveFallback("gpt-6-astra", Trigger429, caps, pool, cb)
	if !ok || target != "claude-opus-5" && target != "claude-sonnet-5" {
		t.Fatalf("expected codex to be skipped due to OPEN circuit and choose claude-sonnet-5, got %q (ok=%v)", target, ok)
	}
}
