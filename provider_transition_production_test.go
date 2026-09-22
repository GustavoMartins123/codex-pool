package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProviderTransitionStreamingModes(t *testing.T) {
	for _, pair := range transitionFixtures {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", pair.name, streaming), func(t *testing.T) {
				store := newConversationHandoffStore()
				fromFormat, toFormat := transitionFormat(pair.from), transitionFormat(pair.to)
				if _, _, err := store.Prepare("stream-matrix", pair.from, contextTestPath(fromFormat), contextTestBody(fromFormat, "prior", streaming)); err != nil {
					t.Fatal(err)
				}
				store.RecordAssistantText("stream-matrix", pair.from, "answer")
				out, result, err := store.Prepare("stream-matrix", pair.to, contextTestPath(toFormat), addContextOpaqueState(t, contextTestBody(toFormat, "current", streaming)))
				if err != nil || !result.Switched {
					t.Fatalf("handoff=%+v err=%v", result, err)
				}
				var root map[string]any
				if err := json.Unmarshal(out, &root); err != nil {
					t.Fatal(err)
				}
				if root["stream"] != streaming {
					t.Fatalf("stream flag changed: %s", out)
				}
				if strings.Contains(string(out), "resp_previous_provider") {
					t.Fatalf("foreign response ID: %s", out)
				}
				visible := normalizedContextText(t, contextTestPath(toFormat), out)
				if !strings.Contains(visible, "prior") || !strings.Contains(visible, "answer") || !strings.Contains(visible, "current") {
					t.Fatalf("lost context: %s", out)
				}
			})
		}
	}
}

func TestProviderTransitionProductionSequences(t *testing.T) {
	for _, sequence := range []struct {
		name      string
		providers []AccountType
	}{
		{"five_provider_hops", []AccountType{AccountTypeCodex, AccountTypeAntigravity, AccountTypeZAI, AccountTypeClaude, AccountTypeCodex}},
		{"antigravity_reentry", []AccountType{AccountTypeAntigravity, AccountTypeCodex, AccountTypeAntigravity}},
	} {
		t.Run(sequence.name, func(t *testing.T) {
			store := newConversationHandoffStore()
			var previousSeed string
			for index, provider := range sequence.providers {
				format := transitionFormat(provider)
				body := contextTestBody(format, fmt.Sprintf("question_%d", index), false)
				if index > 0 {
					body = addContextOpaqueState(t, body)
				}
				out, result, err := store.Prepare("sequence", provider, contextTestPath(format), body)
				if err != nil || result.Switched != (index > 0) {
					t.Fatalf("step=%d result=%+v err=%v", index, result, err)
				}
				if index > 0 {
					if strings.Contains(string(out), "resp_previous_provider") || strings.Contains(string(out), "session_previous_provider") {
						t.Fatalf("foreign state at step %d: %s", index, out)
					}
					visible := normalizedContextText(t, contextTestPath(format), out)
					for prior := 0; prior <= index; prior++ {
						if !strings.Contains(visible, fmt.Sprintf("question_%d", prior)) {
							t.Fatalf("step %d lost question_%d", index, prior)
						}
					}
					messages := normalizeConversationMessages(format, contextRequestObject(mustJSONMap(t, out)))
					calls, results, valid := transitionToolPairs(messages)
					if !valid || calls != results {
						t.Fatalf("step %d orphan tools: calls=%d results=%d", index, calls, results)
					}
				}
				state, _ := store.State("sequence")
				if state.TransitionEpoch != uint64(index) || state.ActiveProvider != provider {
					t.Fatalf("step %d state=%+v", index, state)
				}
				if provider == AccountTypeAntigravity {
					seed, fresh := store.NativeSessionSeed("sequence", provider)
					if !fresh || (previousSeed != "" && seed == previousSeed) {
						t.Fatalf("step %d reused Antigravity seed", index)
					}
					previousSeed = seed
				}
				store.RecordAssistantText("sequence", provider, fmt.Sprintf("answer_%d", index))
			}
		})
	}
}

func mustJSONMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAntigravityTransitionHTTPErrorMatrix(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		wantStatus    int
		wantCalls     int
		quotaAffected bool
	}{
		{"signature", 429, 1, false},
		{"capacity", 503, 4, false},
		{"provider", 500, 2, false},
		{"auth", 401, 1, false},
		{"quota", 429, 2, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			daily, _ := url.Parse("https://daily.example")
			prod, _ := url.Parse("https://prod.example")
			account := &Account{Type: AccountTypeAntigravity, ID: "anti", AccessToken: "token", ProjectID: "project", PlanType: "pro"}
			calls := 0
			h := &proxyHandler{
				cfg:      &config{maxAttempts: 1, requestTimeout: time.Second, streamTimeout: time.Second},
				pool:     newPoolState([]*Account{account}, false),
				registry: NewProviderRegistry(nil, nil, nil, NewAntigravityProvider(daily, prod)),
				transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					body, _ := io.ReadAll(req.Body)
					status, payload := strictAntigravityTransitionStatus(body, scenario.name)
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
				}),
			}
			store := h.getContextHandoff()
			if _, _, err := store.Prepare("error-matrix", AccountTypeCodex, "/v1/responses", contextTestBody(contextFormatResponses, "prior", false)); err != nil {
				t.Fatal(err)
			}
			body, result, err := store.Prepare("error-matrix", AccountTypeAntigravity, "/v1/responses", addContextOpaqueState(t, contextTestBody(contextFormatResponses, "current", false)))
			if err != nil || !result.Switched {
				t.Fatalf("handoff=%+v err=%v", result, err)
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
			if !h.handleAntigravityProxy(w, r, body, "antigravity/gemini-3.8-flash-high", "error-matrix", "user", "origin", "127.0.0.1", "req", "") {
				t.Fatal("not handled")
			}
			if w.Code != scenario.wantStatus || calls != scenario.wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			account.mu.Lock()
			cooldowns := len(account.ModelRateLimits)
			account.mu.Unlock()
			if (cooldowns > 0) != scenario.quotaAffected {
				t.Fatalf("cooldowns=%d quotaAffected=%t", cooldowns, scenario.quotaAffected)
			}
		})
	}
}
