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

func TestAntigravityStartsFreshNativeSessionOnEachEntry(t *testing.T) {
	daily, _ := url.Parse("https://daily.example")
	prod, _ := url.Parse("https://prod.example")
	provider := NewAntigravityProvider(daily, prod)
	account := &Account{Type: AccountTypeAntigravity, ID: "anti", AccessToken: "token", ProjectID: "project", PlanType: "pro"}
	var upstream []map[string]any
	h := &proxyHandler{
		cfg:      &config{maxAttempts: 1, requestTimeout: time.Second, streamTimeout: time.Second},
		pool:     newPoolState([]*Account{account}, false),
		registry: NewProviderRegistry(nil, nil, nil, provider),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			var envelope map[string]any
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatalf("upstream payload: %v", err)
			}
			upstream = append(upstream, envelope)
			status, errorBody := strictAntigravityTransitionStatus(body, "")
			if status != 200 {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(errorBody)), Request: req}, nil
			}
			response := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"valid answer","thoughtSignature":"native-signature"}]},"finishReason":"STOP"}]}}` + "\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
		}),
	}
	store := h.getContextHandoff()
	conversationID := "fresh-entry-conversation"
	sendAnti := func(text string, foreign bool) {
		t.Helper()
		body := providerSessionRequest(t, contextFormatResponses, text, nil)
		if foreign {
			body = addContextOpaqueState(t, body)
		}
		prepared, _, err := store.Prepare(conversationScopedKey("user", conversationID), AccountTypeAntigravity, "/v1/responses", body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(prepared)))
		w := httptest.NewRecorder()
		if !h.handleAntigravityProxy(w, r, prepared, "antigravity/gemini-3.8-flash-high", conversationID, "user", "origin", "127.0.0.1", "req", "") || w.Code != 200 {
			t.Fatalf("Antigravity status=%d body=%s", w.Code, w.Body.String())
		}
	}
	sendAnti("first direct turn", false)
	firstState, _ := store.State(conversationScopedKey("user", conversationID))
	if !firstState.ProviderSessions[AccountTypeAntigravity].Established {
		t.Fatal("valid first response did not establish native session")
	}
	codex := providerSessionRequest(t, contextFormatResponses, "Codex turn", nil)
	if _, result, err := store.Prepare(conversationScopedKey("user", conversationID), AccountTypeCodex, "/v1/responses", codex); err != nil || !result.Switched {
		t.Fatalf("Antigravity -> Codex: %+v %v", result, err)
	}
	sendAnti("return to Antigravity", true)
	sendAnti("native continuation", false)
	if len(upstream) != 3 {
		t.Fatalf("upstream request count=%d", len(upstream))
	}
	session := func(index int) string {
		request, _ := upstream[index]["request"].(map[string]any)
		return stringValue(request["sessionId"])
	}
	if session(0) == session(1) || session(1) != session(2) {
		t.Fatalf("native sessions reused across epochs or changed within epoch: %q %q %q", session(0), session(1), session(2))
	}
	for _, index := range []int{1, 2} {
		encoded, _ := json.Marshal(upstream[index]["request"])
		if strings.Contains(string(encoded), "resp_previous_provider") || strings.Contains(string(encoded), "opaque-reasoning") {
			t.Fatalf("foreign state reached Antigravity: %s", encoded)
		}
		if index == 1 && strings.Contains(string(encoded), "native-signature") {
			t.Fatalf("previous epoch replay reached fresh session: %s", encoded)
		}
	}
	state, _ := store.State(conversationScopedKey("user", conversationID))
	if state.TransitionEpoch != 2 || state.ProviderSessions[AccountTypeAntigravity].Epoch != 2 || !state.ProviderSessions[AccountTypeAntigravity].Established {
		t.Fatalf("new native epoch not established: %+v", state.ProviderSessions[AccountTypeAntigravity])
	}
}
