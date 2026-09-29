package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTrafficShadowForgedHeaderCannotBypassQuota(t *testing.T) {
	traffic := enabledTraffic()
	traffic.Principals = []string{"different-authorized-principal"}
	h, _, calls, _ := trafficShadowFixture(t, traffic, trafficShadowAccounts())
	h.experiments = nil
	h.cfg.setHotReloadable(0, RoutingConfigFile{}, map[string]ClientPolicy{
		"*": {Limits: PolicyLimits{DailyRequests: 1}},
	}, ExperimentsConfig{})
	owner := testPoolIdentity(t, h, "user")
	if h.trafficShadow.enabledFor("gpt-5.6-sol->gpt-5.6-sol", owner) {
		t.Fatal("caller must be outside the experiment principal allowlist")
	}
	if w := trafficShadowRequest(t, h); w.Code != http.StatusOK {
		t.Fatalf("real request failed: %d %s", w.Code, w.Body.String())
	}
	if w := trafficShadowRequest(t, h); w.Code != http.StatusTooManyRequests {
		t.Fatalf("quota control failed: %d %s", w.Code, w.Body.String())
	}
	before := atomic.LoadInt32(calls)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"probe","stream":false}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
	r.Header.Set("X-Pool-Shadow", "forged-by-client")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != http.StatusTooManyRequests || atomic.LoadInt32(calls) != before {
		t.Fatalf("forged header bypassed quota: status=%d calls=%d->%d", w.Code, before, atomic.LoadInt32(calls))
	}
}

func TestTrafficShadowPublicHeaderDoesNotChangeConversationNamespace(t *testing.T) {
	h, _, _, _ := trafficShadowFixture(t, TrafficShadowConfig{}, trafficShadowAccounts())
	h.experiments = nil
	body := `{"model":"gpt-5.6-sol","conversation_id":"real-conversation","input":"probe","stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
	r.Header.Set("X-Pool-Shadow", "forged-by-client")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("public header changed routing: %d %s", w.Code, w.Body.String())
	}
	owner := testPoolIdentity(t, h, "user")
	if _, ok := h.getContextHandoff().State(conversationScopedKey(owner, "real-conversation")); !ok {
		t.Fatal("real conversation state missing")
	}
	if _, ok := h.getContextHandoff().State(conversationScopedKey(owner, "shadow:forged-by-client\x00real-conversation")); ok {
		t.Fatal("public header created internal shadow state")
	}
}
