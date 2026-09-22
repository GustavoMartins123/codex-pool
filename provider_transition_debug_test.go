package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransitionDryRunDoesNotMutateConversation(t *testing.T) {
	h := &proxyHandler{cfg: &config{adminToken: "operator-token"}}
	store := h.getContextHandoff()
	if _, _, err := store.Prepare("dry-run", AccountTypeCodex, "/v1/responses", contextTestBody(contextFormatResponses, "first", false)); err != nil {
		t.Fatal(err)
	}
	before, _ := store.State("dry-run")
	request := `{"conversation_id":"dry-run","from":"codex","to":"antigravity","model":"gemini","request":{"model":"gemini","previous_response_id":"private-response-id","input":[{"role":"user","content":"next"}]}}`
	r := httptest.NewRequest(http.MethodPost, "/admin/debug/transition", strings.NewReader(request))
	r.Header.Set("X-Admin-Token", "operator-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var result transitionDryRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Mode != TransitionSafeHistory || !result.FreshSession || result.MessagesBefore != 1 || result.MessagesAfter < 2 {
		t.Fatalf("dry run=%+v", result)
	}
	if !strings.Contains(w.Body.String(), "previous_response_id") || strings.Contains(w.Body.String(), "private-response-id") {
		t.Fatalf("redaction failed: %s", w.Body.String())
	}
	after, _ := store.State("dry-run")
	if after.TransitionEpoch != before.TransitionEpoch || after.ActiveProvider != before.ActiveProvider || len(after.Messages) != len(before.Messages) || len(store.TransitionDiagnostics("dry-run")) != 0 {
		t.Fatalf("dry run mutated conversation: before=%+v after=%+v", before, after)
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/admin/debug/transition", strings.NewReader(request)))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauth.Code)
	}
}

func TestTransitionDryRunToolPairCounts(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Parts: []MessagePart{{Type: "tool_call", ToolID: "a"}, {Type: "tool_call", ToolID: "b"}}},
		{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "a"}}},
	}
	if total, valid := countTransitionToolPairs(messages); total != 2 || valid != 1 {
		t.Fatalf("pairs=%d valid=%d", total, valid)
	}
}
