package main

import (
	"testing"
)

func TestPoolAutoOrchestrationProfiles(t *testing.T) {
	orchestrator := newPoolAutoOrchestrator(nil)

	pool := newPoolState([]*Account{
		{ID: "codex_1", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude_1", Type: AccountTypeClaude, PlanType: "pro"},
		{ID: "gemini_1", Type: AccountTypeGemini, PlanType: "api"},
	}, false)

	cb := newCircuitBreakerManager()
	pricing := newPricingData()

	capsText := RequestCapabilities{
		ContextTokens: 1500,
		Modalities:    []string{"text"},
	}

	// 1. Balanced profile (pool/auto)
	decBalanced, err := orchestrator.Orchestrate("balanced", capsText, "", pool, cb, pricing, nil)
	if err != nil {
		t.Fatalf("balanced orchestration failed: %v", err)
	}
	if decBalanced.SelectedModel == "" {
		t.Fatalf("expected selected model, got empty")
	}
	if !decBalanced.GatePassed {
		t.Fatalf("expected gate_passed to be true")
	}
	if len(decBalanced.Explanation) == 0 {
		t.Fatalf("expected explanations")
	}

	// 2. Fast profile (pool/auto-fast)
	decFast, err := orchestrator.Orchestrate("fast", capsText, "", pool, cb, pricing, nil)
	if err != nil {
		t.Fatalf("fast orchestration failed: %v", err)
	}
	fastMeta, _ := lookupModelMetadata(decFast.SelectedModel, pool)
	if fastMeta.ID == "gpt-6-astra" || fastMeta.ID == "claude-opus-5" {
		t.Errorf("fast profile should not select heavy frontier model %s", decFast.SelectedModel)
	}

	// 3. Quality profile (pool/auto-quality)
	decQuality, err := orchestrator.Orchestrate("quality", capsText, "", pool, cb, pricing, nil)
	if err != nil {
		t.Fatalf("quality orchestration failed: %v", err)
	}
	if decQuality.Scores.AdequacyScore < 0.8 {
		t.Errorf("quality profile adequacy score should be >= 0.8, got %v", decQuality.Scores.AdequacyScore)
	}

	// 4. Long context profile (pool/auto-long-context)
	decLong, err := orchestrator.Orchestrate("long-context", capsText, "", pool, cb, pricing, nil)
	if err != nil {
		t.Fatalf("long-context orchestration failed: %v", err)
	}
	longMeta, _ := lookupModelMetadata(decLong.SelectedModel, pool)
	if longMeta.ContextWindow < 1000000 {
		t.Errorf("long-context profile should select model with >=1M context, got %s (%d tokens)",
			decLong.SelectedModel, longMeta.ContextWindow)
	}
}

func TestPoolAutoGateRejectsIncompatibleModels(t *testing.T) {
	orchestrator := newPoolAutoOrchestrator(nil)

	pool := newPoolState([]*Account{
		{ID: "codex_1", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude_1", Type: AccountTypeClaude, PlanType: "pro"},
		{ID: "minimax_1", Type: AccountTypeMinimax, PlanType: "pro"},
	}, false)

	cb := newCircuitBreakerManager()
	pricing := newPricingData()

	// Multimodal image request
	capsImage := RequestCapabilities{
		ContextTokens: 2000,
		Modalities:    []string{"text", "image"},
	}

	dec, err := orchestrator.Orchestrate("balanced", capsImage, "", pool, cb, pricing, nil)
	if err != nil {
		t.Fatalf("orchestration with image failed: %v", err)
	}

	meta, ok := lookupModelMetadata(dec.SelectedModel, pool)
	if !ok || !meta.SupportsImage {
		t.Fatalf("selected model %s must support image modality", dec.SelectedModel)
	}

	// Check that text-only models (like MiniMax-M2.7) scored ZERO
	if dec.CandidateScores["MiniMax-M2.7"] != 0.0 {
		t.Errorf("incompatible model MiniMax-M2.7 should receive score 0, got %v",
			dec.CandidateScores["MiniMax-M2.7"])
	}
}

func TestPoolAutoConversationAffinity(t *testing.T) {
	orchestrator := newPoolAutoOrchestrator(nil)

	claudeAccount := &Account{ID: "claude_acc_pinned", Type: AccountTypeClaude, PlanType: "pro"}
	codexAccount := &Account{ID: "codex_acc_1", Type: AccountTypeCodex, PlanType: "pro"}
	pool := newPoolState([]*Account{claudeAccount, codexAccount}, false)

	// Pin conversation to Claude account
	convID := "pinned-conversation-123"
	pool.pin(convID, claudeAccount.ID)

	cb := newCircuitBreakerManager()
	pricing := newPricingData()

	caps := RequestCapabilities{ContextTokens: 1000, Modalities: []string{"text"}}

	dec, err := orchestrator.Orchestrate("balanced", caps, convID, pool, cb, pricing, nil)
	if err != nil {
		t.Fatalf("orchestration failed: %v", err)
	}

	if dec.SelectedProvider != AccountTypeClaude {
		t.Errorf("affinity should prefer Claude provider for pinned conversation, got provider=%s model=%s",
			dec.SelectedProvider, dec.SelectedModel)
	}
	if dec.Scores.AffinityScore != 1.0 {
		t.Errorf("expected affinity score 1.0, got %v", dec.Scores.AffinityScore)
	}
}
