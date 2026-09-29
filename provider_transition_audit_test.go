package main

// Randomized long-hop provider transition property test with a fixed seed.
// Exercises the conversation handoff store across dozens of provider hops
// mixing Codex, Claude, Antigravity, and Z.ai, alternating streaming modes,
// and asserting the invariants after every hop.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestAuditProviderTransitionRandomizedLongHops(t *testing.T) {
	rng := rand.New(rand.NewSource(20260928))
	providers := []AccountType{AccountTypeCodex, AccountTypeClaude, AccountTypeAntigravity, AccountTypeZAI}
	const hops = 60

	store := newConversationHandoffStore()
	seenEpoch := uint64(0)
	switches := 0
	var previous AccountType

	for step := 0; step < hops; step++ {
		provider := providers[rng.Intn(len(providers))]
		streaming := rng.Intn(2) == 1
		format := transitionFormat(provider)
		question := fmt.Sprintf("question_%d", step)
		body := contextTestBody(format, question, streaming)
		if step > 0 {
			body = addContextOpaqueState(t, body)
		}
		out, result, err := store.Prepare(conversationScopedKey("user", "audit-random-walk"), provider, contextTestPath(format), body)
		if err != nil {
			t.Fatalf("step %d (%s): %v", step, provider, err)
		}
		wantSwitch := step > 0 && provider != previous
		if result.Switched != wantSwitch {
			t.Fatalf("step %d (%s after %s): switch=%v want %v", step, provider, previous, result.Switched, wantSwitch)
		}
		if wantSwitch {
			switches++
		}

		var root map[string]any
		if err := json.Unmarshal(out, &root); err != nil {
			t.Fatalf("step %d: invalid JSON: %v", step, err)
		}
		if root["stream"] != streaming {
			t.Fatalf("step %d: streaming flag mutated: want %v got %v", step, streaming, root["stream"])
		}
		if wantSwitch {
			if raw := string(out); strings.Contains(raw, "resp_previous_provider") || strings.Contains(raw, "session_previous_provider") {
				t.Fatalf("step %d: foreign provider state leaked after switch: %s", step, raw)
			}
		}

		if wantSwitch {
			visible := normalizedContextText(t, contextTestPath(format), out)
			for prior := 0; prior <= step; prior++ {
				if !strings.Contains(visible, fmt.Sprintf("question_%d", prior)) {
					t.Fatalf("step %d: context lost question_%d (visible=%.200s)", step, prior, visible)
				}
				if prior < step && !strings.Contains(visible, fmt.Sprintf("answer_%d", prior)) {
					t.Fatalf("step %d: context lost answer_%d (visible=%.200s)", step, prior, visible)
				}
			}
		}

		messages := normalizeConversationMessages(format, contextRequestObject(mustJSONMap(t, out)))
		calls, results, valid := transitionToolPairs(messages)
		if !valid || calls != results {
			t.Fatalf("step %d: orphan tool state calls=%d results=%d valid=%v", step, calls, results, valid)
		}

		state, _ := store.State(conversationScopedKey("user", "audit-random-walk"))
		if state.TransitionEpoch != uint64(switches) {
			t.Fatalf("step %d: transition epoch %d want %d", step, state.TransitionEpoch, switches)
		}
		if state.TransitionEpoch < seenEpoch {
			t.Fatalf("step %d: epoch %d below previously observed %d", step, state.TransitionEpoch, seenEpoch)
		}
		seenEpoch = state.TransitionEpoch
		if state.ActiveProvider != provider {
			t.Fatalf("step %d: active provider %v want %v", step, state.ActiveProvider, provider)
		}

		if provider == AccountTypeAntigravity {
			if _, fresh := store.NativeSessionSeed(conversationScopedKey("user", "audit-random-walk"), provider); !fresh {
				t.Fatalf("step %d: Antigravity session seed not fresh on reentry", step)
			}
		}

		store.RecordAssistantText(conversationScopedKey("user", "audit-random-walk"), provider, fmt.Sprintf("answer_%d", step))
		previous = provider
	}
}
