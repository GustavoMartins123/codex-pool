package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// P0-01 regression: two authenticated principals reusing the same external
// conversation id must never see each other's history on a provider switch.
func TestConversationHandoffIsolatedBetweenPrincipals(t *testing.T) {
	store := newConversationHandoffStore()
	alice := func() conversationKey { return conversationScopedKey("alice", "shared-conv") }
	bob := func() conversationKey { return conversationScopedKey("bob", "shared-conv") }

	step := func(key conversationKey, provider AccountType, question string) string {
		body := contextTestBody(transitionFormat(provider), question, false)
		out, _, err := store.Prepare(key, provider, contextTestPath(transitionFormat(provider)), body)
		if err != nil {
			t.Fatalf("%s -> %s: %v", key.owner, provider, err)
		}
		var root map[string]any
		if json.Unmarshal(out, &root) != nil {
			t.Fatalf("%s -> %s: invalid JSON", key.owner, provider)
		}
		return string(out)
	}

	step(alice(), AccountTypeCodex, "alice_secret_alpha")
	step(alice(), AccountTypeClaude, "alice_secret_beta")
	bobOut := step(bob(), AccountTypeClaude, "bob_secret_gamma")

	for _, leak := range []string{"alice_secret_alpha", "alice_secret_beta"} {
		if strings.Contains(bobOut, leak) {
			t.Fatalf("cross-principal leak: bob's switch output contains %q", leak)
		}
	}
	if !strings.Contains(bobOut, "bob_secret_gamma") {
		t.Fatalf("bob's own question missing from his switch output: %s", bobOut)
	}

	if _, ok := store.State(bob()); !ok {
		t.Fatal("bob's record missing after his switch")
	}
	if _, ok := store.State(conversationScopedKey("alice", "shared-conv")); !ok {
		t.Fatal("alice's record lost after bob's switch")
	}

	store.mu.Lock()
	records := len(store.records)
	store.mu.Unlock()
	if records != 2 {
		t.Fatalf("want 2 scoped records for the shared external id, got %d", records)
	}
}

func TestConversationScopedKeyCollisionSafety(t *testing.T) {
	a := conversationScopedKey("user|with|pipes", "conv")
	b := conversationScopedKey("user", "with|pipes|conv")
	if a == b {
		t.Fatal("field-content collision produced identical keys")
	}
	if (conversationScopedKey("u", "")) != (conversationKey{}) {
		t.Fatal("empty external id must yield the zero key")
	}
}

// Handoff retention is best-effort: memory limits must degrade to
// pass-through, never fail the user's request or kill a live session.
func TestHandoffMemoryLimitsDegradeInsteadOfFailing(t *testing.T) {
	store := newConversationHandoffStore()
	key := conversationScopedKey("user", "big-session")
	small := contextTestBody(contextFormatResponses, "turn one", false)
	if _, _, err := store.Prepare(key, AccountTypeCodex, "/v1/responses", small); err != nil {
		t.Fatal(err)
	}
	oversized := contextTestBody(contextFormatResponses, strings.Repeat("x", (maxContextBytes)+1), false)
	if len(oversized) <= maxContextBytes {
		t.Skipf("fixture cannot exceed the limit: %d", len(oversized))
	}
	out, result, err := store.Prepare(key, AccountTypeClaude, "/v1/messages", oversized)
	if err != nil {
		t.Fatalf("oversized input must pass through, got: %v", err)
	}
	if string(out) != string(oversized) {
		t.Fatal("oversized input must be returned unrewritten")
	}
	if len(result.Warnings) == 0 {
		t.Fatal("oversize degradation must be observable in warnings")
	}

	// A faulted retention record must self-heal instead of bricking the
	// conversation forever.
	store.mu.Lock()
	faulted := store.records[key]
	faulted.Fault = "conversation exceeds byte limit"
	store.records[key] = faulted
	store.mu.Unlock()
	out, _, err = store.Prepare(key, AccountTypeCodex, "/v1/responses", small)
	if err != nil {
		t.Fatalf("faulted retention must degrade, got: %v", err)
	}
	if string(out) != string(small) {
		t.Fatal("same-provider input must be returned unrewritten")
	}
	store.mu.Lock()
	_, healed := store.records[key]
	fault := store.records[key].Fault
	store.mu.Unlock()
	if !healed || fault != "" {
		t.Fatalf("faulted record must be dropped and rebuilt: healed=%v fault=%q", healed, fault)
	}

	// Retention quota overflow (record larger than the per-conversation
	// limit) must skip retention, not fail the request.
	bigTurn := contextTestBody(contextFormatResponses, strings.Repeat("y", maxContextBytes), false)
	if len(bigTurn) > maxContextBytes {
		out, result, err = store.Prepare(key, AccountTypeCodex, "/v1/responses", bigTurn)
		if err != nil {
			t.Fatalf("retention overflow must degrade, got: %v", err)
		}
		if string(out) != string(bigTurn) {
			t.Fatal("unrewritten body expected on retention overflow")
		}
		if len(result.Warnings) == 0 {
			t.Fatal("retention skip must be observable in warnings")
		}
	}
}
