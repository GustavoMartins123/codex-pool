package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func aistudioAdminHandler(t *testing.T, transport http.RoundTripper) *proxyHandler {
	t.Helper()
	h := qwenTestHandler(t, transport)
	other := mustParse("https://unused.example")
	h.registry = NewProviderRegistry(NewCodexProvider(other, other, nil), NewClaudeProvider(other), NewGeminiProvider(other, other), NewAIStudioProvider(mustParse("https://aistudio.example")))
	return h
}

func TestAIStudioContributionDiscoversModelsAndPersistsPrivateAccount(t *testing.T) {
	key := "AIza-synthetic-aistudio-account-key"
	calls := 0
	h := aistudioAdminHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://aistudio.example/v1beta/models?pageSize=1000" || req.Header.Get("x-goog-api-key") != key {
			t.Fatalf("unexpected discovery request: %s", req.URL)
		}
		return qwenJSONResponse(200, aistudioModelsFixture), nil
	}))
	attachContributionFixture(t, h, "aistudio-owner")
	session, csrf := addTestPassportOperator(t, h.passport, "aistudio-owner")
	req := passportContributionRequest(http.MethodPost, "/api/pool/accounts/aistudio/add", session, csrf, []byte(`{"api_key":"`+key+`"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || calls != 1 {
		t.Fatalf("contribution: status=%d calls=%d body=%s", rr.Code, calls, rr.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 || accounts[0].Type != AccountTypeAIStudio || accounts[0].HealthError != "" {
		t.Fatalf("AI Studio account not loaded: %+v", accounts)
	}
	account := accounts[0]
	if _, ok := account.Models["aistudio/gemini-3.8-flash"]; !ok || len(account.Models) != 1 {
		t.Fatalf("invalid discovered catalog: %+v", account.Models)
	}
	stored, err := os.ReadFile(account.File)
	if err != nil || strings.Contains(string(stored), key) {
		t.Fatal("account credentials were not encrypted")
	}
	if h.passport.authorizeAccount("aistudio-owner", account, "manage") != nil || h.passport.authorizeAccount("someone-else", account, "use") == nil {
		t.Fatal("AI Studio contribution did not retain private ownership")
	}
}

func TestAIStudioContributionRejectsInvalidDiscovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"invalid key", 400, `{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`, 400},
		{"denied", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`, 400},
		{"service", 503, `{"error":{"code":503}}`, 502},
		{"no text models", 200, `{"models":[{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}]}`, 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := aistudioAdminHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return qwenJSONResponse(test.status, test.body), nil }))
			attachContributionFixture(t, h, "owner")
			req := httptest.NewRequest("POST", "/api/pool/accounts/aistudio/add", strings.NewReader(`{"api_key":"AIza-synthetic-invalid"}`))
			rr := httptest.NewRecorder()
			h.handleAIStudioAdd(rr, contributionRequestActor(req, "owner"))
			if rr.Code != test.want || len(h.pool.allAccounts()) != 0 {
				t.Fatalf("status=%d accounts=%d body=%s", rr.Code, len(h.pool.allAccounts()), rr.Body.String())
			}
		})
	}
}
