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
