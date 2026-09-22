package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCanTransitionAccountsForHistoryAndCurrentContent(t *testing.T) {
	state := ConversationState{ActiveProvider: AccountTypeCodex}
	state.IR = conversationIRFromMessages([]Message{transitionText("user", "hello")}, "conv", 0)
	if plan := CanTransition(state, AccountTypeCodex, AccountTypeCodex); !plan.Allowed || plan.Cost != 0 || plan.Mode != TransitionNative {
		t.Fatalf("native plan=%+v", plan)
	}
	if plan := CanTransition(state, AccountTypeCodex, AccountTypeAntigravity); !plan.Allowed || plan.Cost != 0.3 || plan.Mode != TransitionSafeHistory {
		t.Fatalf("safe plan=%+v", plan)
	}
	if plan := CanTransition(state, AccountTypeClaude, AccountTypeZAI); !plan.Allowed || plan.Cost != 0.1 || plan.Mode != TransitionFullHistory {
		t.Fatalf("full plan=%+v", plan)
	}
	state.IR = conversationIRFromMessages([]Message{{Role: "user", Parts: []MessagePart{{Type: "image", ImageURL: "data:image/png;base64,aGVsbG8="}}}}, "conv", 0)
	if plan := CanTransition(state, AccountTypeCodex, AccountTypeZAI); plan.Allowed || plan.Mode != TransitionSummary {
		t.Fatalf("unsupported current image allowed: %+v", plan)
	}
	state.IR = conversationIRFromMessages([]Message{{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call", Text: "done"}}}}, "conv", 0)
	if plan := CanTransition(state, AccountTypeCodex, AccountType("unknown")); plan.Allowed {
		t.Fatalf("orphan current tool result allowed: %+v", plan)
	}
}

func TestFallbackGraphPrefersCompatibleNativeContinuation(t *testing.T) {
	graph := newFallbackGraph()
	graph.SetRoute("test-transition-model", FallbackRule{OnUnavailable: []string{"claude-sonnet-5", "gpt-5.6-sol"}})
	pool := newPoolState([]*Account{
		{ID: "codex", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude", Type: AccountTypeClaude, PlanType: "pro"},
	}, false)
	caps := RequestCapabilities{ContextTokens: 2000, Modalities: []string{"text"}}
	legacy, _, ok := graph.ResolveFallback("test-transition-model", TriggerUnavailable, caps, pool, nil)
	if !ok || legacy != "claude-sonnet-5" {
		t.Fatalf("legacy fallback=%q ok=%v", legacy, ok)
	}
	state := ConversationState{ActiveProvider: AccountTypeCodex, IR: conversationIRFromMessages([]Message{transitionText("user", "hello")}, "conv", 0)}
	selected, reason, ok := graph.ResolveFallbackWithTransition("test-transition-model", TriggerUnavailable, caps, pool, nil, &state)
	if !ok || selected != "gpt-5.6-sol" || !strings.Contains(reason, ":native") {
		t.Fatalf("compatibility-aware fallback=%q reason=%q ok=%v", selected, reason, ok)
	}
}

func TestFallbackGraphSkipsPreviouslyVisitedModel(t *testing.T) {
	graph := newFallbackGraph()
	graph.SetRoute("model-a", FallbackRule{On429: []string{"gpt-6-astra", "claude-sonnet-5"}})
	pool := newPoolState([]*Account{
		{ID: "codex", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "claude", Type: AccountTypeClaude, PlanType: "pro"},
	}, false)
	caps := RequestCapabilities{ContextTokens: 1000, Modalities: []string{"text"}}
	state := ConversationState{ActiveProvider: AccountTypeCodex, IR: conversationIRFromMessages([]Message{transitionText("user", "hello")}, "conv", 0)}
	selected, _, ok := graph.ResolveFallbackWithTransitionExcluding("model-a", Trigger429, caps, pool, nil, &state, map[string]bool{"gpt-6-astra": true})
	if !ok || selected != "claude-sonnet-5" {
		t.Fatalf("visited model selected again: %q ok=%v", selected, ok)
	}
}

func TestFallbackPayloadRebuiltForTargetProtocol(t *testing.T) {
	store := newConversationHandoffStore()
	initial := contextTestBody(contextFormatResponses, "prior turn", false)
	if _, _, err := store.Prepare("fallback-conversation", AccountTypeCodex, "/v1/responses", initial); err != nil {
		t.Fatal(err)
	}
	store.RecordAssistantText("fallback-conversation", AccountTypeCodex, "prior answer")
	source := addContextOpaqueState(t, []byte(`{"model":"claude-sonnet-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"current request"}]}]}`))
	clean, result, err := store.Prepare("fallback-conversation", AccountTypeClaude, "/v1/responses", source)
	if err != nil || !result.Switched {
		t.Fatalf("handoff=%+v err=%v", result, err)
	}
	translated, path, direction, err := translateFallbackPayload(clean, "/v1/responses", AccountTypeClaude)
	if err != nil || path != "/v1/messages" || direction != TranslateResponsesToClaude {
		t.Fatalf("translation path=%s direction=%d err=%v", path, direction, err)
	}
	if strings.Contains(string(translated), "resp_previous_provider") || strings.Contains(string(translated), "opaque-reasoning") {
		t.Fatalf("foreign state reached fallback: %s", translated)
	}
	var root map[string]any
	if err := json.Unmarshal(translated, &root); err != nil {
		t.Fatal(err)
	}
	visible := normalizedContextText(t, path, translated)
	for _, want := range []string{"prior turn", "prior answer", "current request"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("fallback lost %q: %s", want, translated)
		}
	}
}

func TestFailedCodexRequestFallsBackWithSafeHistory(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "transition-fallback-secret")
	codexBase, _ := url.Parse("https://codex.mock")
	claudeBase, _ := url.Parse("https://claude.mock")
	graph := newFallbackGraph()
	graph.SetRoute("gpt-5.6-sol", FallbackRule{On429: []string{"claude-sonnet-5"}})
	pool := newPoolState([]*Account{
		{ID: "codex", Type: AccountTypeCodex, AccessToken: "codex-token", PlanType: "pro"},
		{ID: "claude", Type: AccountTypeClaude, AccessToken: "claude-token", PlanType: "pro"},
	}, false)
	pool.fallbackGraph = graph
	var claudePayload []byte
	h := &proxyHandler{
		cfg:           &config{maxAttempts: 2, maxInMemoryBodyBytes: 1 << 20, requestTimeout: time.Second, streamTimeout: time.Second},
		pool:          pool,
		registry:      NewProviderRegistry(NewCodexProvider(codexBase, codexBase, nil), NewClaudeProvider(claudeBase), nil),
		fallbackGraph: graph,
		metrics:       newMetrics(),
		recent:        newRecentErrors(10),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			if req.URL.Host == "codex.mock" {
				return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit exceeded"}}`)), Request: req}, nil
			}
			claudePayload = append([]byte(nil), body...)
			answer := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"fallback answer"}],"model":"claude-sonnet-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(answer)), Request: req}, nil
		}),
	}
	store := h.getContextHandoff()
	if _, _, err := store.Prepare("fallback-live", AccountTypeCodex, "/v1/responses", contextTestBody(contextFormatResponses, "prior request", false)); err != nil {
		t.Fatal(err)
	}
	store.RecordAssistantText("fallback-live", AccountTypeCodex, "prior answer")
	body := `{"model":"gpt-5.6-sol","conversation_id":"fallback-live","previous_response_id":"resp_codex_foreign","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"current request"}]}],"stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("transition-fallback-secret", "user"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || len(claudePayload) == 0 {
		t.Fatalf("fallback status=%d payload=%s response=%s", w.Code, claudePayload, w.Body.String())
	}
	if strings.Contains(string(claudePayload), "resp_codex_foreign") {
		t.Fatalf("Codex response ID reached Claude: %s", claudePayload)
	}
	visible := normalizedContextText(t, "/v1/messages", claudePayload)
	for _, want := range []string{"prior request", "prior answer", "current request"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("fallback omitted %q: %s", want, claudePayload)
		}
	}
}
