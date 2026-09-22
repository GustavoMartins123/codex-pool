package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransitionDiagnosticsRedactNativeState(t *testing.T) {
	h := &proxyHandler{cfg: &config{adminToken: "operator-token"}}
	store := h.getContextHandoff()
	if _, _, err := store.Prepare("conversation-1", AccountTypeCodex, "/v1/responses", contextTestBody(contextFormatResponses, "first", false)); err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"model":"gemini","previous_response_id":"secret-response","prompt_cache_key":"secret-cache","input":[{"role":"user","content":"second"}]}`)
	_, result, err := store.Prepare("conversation-1", AccountTypeAntigravity, "/v1/responses", request)
	if err != nil || !result.Switched {
		t.Fatalf("handoff: %+v %v", result, err)
	}
	if result.Epoch != 1 || len(result.RemovedState) != 2 {
		t.Fatalf("result: %+v", result)
	}

	for _, path := range []string{"/admin/transitions", "/admin/debug/conversations/conversation-1"} {
		unauth := httptest.NewRecorder()
		h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, path, nil))
		if unauth.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status=%d", path, unauth.Code)
		}
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Admin-Token", "operator-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret-response") || strings.Contains(w.Body.String(), "secret-cache") {
			t.Fatalf("native state leaked: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"handoff_mode":"safe-history"`) || !strings.Contains(w.Body.String(), `"transition_epoch":1`) {
			t.Fatalf("missing diagnostics: %s", w.Body.String())
		}
	}
	trace := h.transitionForTrace("conversation-1", AccountTypeAntigravity, make(http.Header))
	if trace == nil || !trace.FreshSession || trace.Epoch != 1 {
		t.Fatalf("trace=%+v", trace)
	}
	store.MarkTransitionOutcome("conversation-1", 1, 429, ProviderErrorContext, false)
	events := store.TransitionDiagnostics("conversation-1")
	if len(events) != 1 || events[0].ErrorClass != ProviderErrorContext || events[0].StatusCode != 429 {
		t.Fatalf("events=%+v", events)
	}
}
