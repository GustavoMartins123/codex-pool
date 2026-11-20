package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestQwenContributionDiscoversModelsAndPersistsPrivateAccount(t *testing.T) {
	key := "sk-sp-synthetic-qwen-account-key"
	calls := 0
	h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.String() != "https://qwen.example/compatible-mode/v1/models" || req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer "+key {
			t.Fatalf("unexpected discovery request: %s %s", req.Method, req.URL)
		}
		return qwenJSONResponse(200, `{"data":[{"id":"qwen3.8-max","context_window":1000000,"max_output_tokens":131072,"supports_reasoning":true},{"id":"qwen4-max-preview"},{"id":"MiniMax-M3"},{"id":"qwen-image-3.0-pro"}]}`), nil
	}))
	attachContributionFixture(t, h, "qwen-owner")
	now := time.Now().UTC()
	input := map[string]any{"api_key": key, "token_plan": &QwenTokenPlanConfig{Mode: qwenPersonalManual, TotalCredits: 25000, Remaining: 18750, CycleStart: now.Add(-24 * time.Hour), CycleEnd: now.Add(29 * 24 * time.Hour)}}
	body, _ := json.Marshal(input)
	session, csrf := addTestPassportOperator(t, h.passport, "qwen-owner")
	req := passportContributionRequest(http.MethodPost, "/api/pool/accounts/qwen/add", session, csrf, body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || calls != 1 {
		t.Fatalf("contribution: status=%d calls=%d body=%s", rr.Code, calls, rr.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 || accounts[0].Type != AccountTypeQwen {
		t.Fatalf("Qwen account not loaded: %d", len(accounts))
	}
	account := accounts[0]
	if len(account.Models) != 2 || account.HealthError != "" || account.Models["qwen3.8-max"].Protocol != "openai" {
		t.Fatalf("invalid discovered catalog: %+v", account.Models)
	}
	if account.qwen.Credits == nil || account.qwen.Credits.Used != 6250 || account.Usage.PrimaryUsedPercent != .25 {
		t.Fatalf("manual subscription credits not loaded: %+v", account.qwen.Credits)
	}
	encrypted, err := os.ReadFile(account.File)
	if err != nil || strings.Contains(string(encrypted), key) {
		t.Fatal("account credentials were not encrypted")
	}
	if h.passport.authorizeAccount("qwen-owner", account, "manage") != nil || h.passport.authorizeAccount("someone-else", account, "use") == nil {
		t.Fatal("Qwen contribution did not retain private ownership")
	}
	found := false
	for _, model := range poolModelDescriptors(h.pool) {
		if model.ID == "qwen4-max-preview" {
			found = model.Provider == "qwen" && model.Protocol == "openai" && model.SupportingAccounts == 1
		}
	}
	if !found {
		t.Fatal("new Qwen model is absent from the public catalog")
	}
}

func TestQwenContributionRejectsInvalidDiscovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"auth", 401, `{"error":"no access"}`, 400},
		{"service", 503, `{"error":"unavailable"}`, 502},
		{"no Qwen", 200, `{"data":[{"id":"MiniMax-M3"}]}`, 502},
		{"invalid JSON", 200, `{`, 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := qwenTestHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return qwenJSONResponse(test.status, test.body), nil }))
			attachContributionFixture(t, h, "owner")
			req := httptest.NewRequest("POST", "/api/pool/accounts/qwen/add", strings.NewReader(`{"api_key":"sk-sp-synthetic-invalid-key"}`))
			rr := httptest.NewRecorder()
			h.handleQwenAdd(rr, contributionRequestActor(req, "owner"))
			if rr.Code != test.want || len(h.pool.allAccounts()) != 0 {
				t.Fatalf("status=%d accounts=%d body=%s", rr.Code, len(h.pool.allAccounts()), rr.Body.String())
			}
		})
	}
}

func TestQwenCreditManagementIsOwnerScopedAndHidesCloudCredentials(t *testing.T) {
	h := qwenTestHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return qwenJSONResponse(200, `{"data":[{"id":"qwen3.8-max"}]}`), nil
	}))
	attachContributionFixture(t, h, "owner")
	ownerSession, ownerCSRF := addTestPassportOperator(t, h.passport, "owner")
	_, err := h.saveContribution(contributionRequestActor(httptest.NewRequest("POST", "/", nil), "owner"), AccountTypeQwen, "synthetic-identity", map[string]any{"api_key": "sk-sp-test-management-key", "provider_model_snapshot": providerModelSnapshot{FetchedAt: time.Now(), Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max"}}}})
	if err != nil {
		t.Fatal(err)
	}
	account := h.pool.allAccounts()[0]
	path := "/api/accounts/qwen/" + account.ID + "/token-plan"
	now := time.Now().UTC()
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalManual, TotalCredits: 10000, Remaining: 4000, CycleStart: now.Add(-time.Hour), CycleEnd: now.Add(29 * 24 * time.Hour)}
	body, _ := json.Marshal(map[string]any{"token_plan": plan})
	saved := httptest.NewRecorder()
	h.ServeHTTP(saved, passportContributionRequest("PUT", path, ownerSession, ownerCSRF, body))
	if saved.Code != 200 || account.qwen.Credits == nil || account.qwen.Credits.Used != 6000 || account.Usage.PrimaryUsedPercent != .6 {
		t.Fatalf("Personal balance update failed: %d %s", saved.Code, saved.Body.String())
	}
	noCSRF := httptest.NewRecorder()
	h.ServeHTTP(noCSRF, passportContributionRequest("PUT", path, ownerSession, "wrong-csrf", body))
	if noCSRF.Code != 403 {
		t.Fatalf("credit mutation accepted invalid CSRF: %d", noCSRF.Code)
	}
	otherSession, otherCSRF := addTestPassportOperator(t, h.passport, "other")
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, passportContributionRequest("PUT", path, otherSession, otherCSRF, body))
	if denied.Code != 403 {
		t.Fatalf("another principal changed credits: %d %s", denied.Code, denied.Body.String())
	}
	account.mu.Lock()
	account.qwen.TokenPlan = &QwenTokenPlanConfig{Mode: qwenTeamAPI, AccessKeyID: "cloud-access-id", AccessKeySecret: "cloud-access-secret", SecurityToken: "cloud-session-secret", SeatID: "seat-test"}
	account.mu.Unlock()
	view := httptest.NewRecorder()
	h.ServeHTTP(view, passportContributionRequest("GET", path, ownerSession, ownerCSRF, nil))
	if view.Code != 200 || strings.Contains(view.Body.String(), "cloud-access") || strings.Contains(view.Body.String(), "cloud-session-secret") {
		t.Fatalf("cloud credentials leaked: %d %s", view.Code, view.Body.String())
	}
	h.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return qwenJSONResponse(403, `{"Message":"do not expose upstream secrets"}`), nil
	})
	refresh := httptest.NewRecorder()
	h.ServeHTTP(refresh, passportContributionRequest("POST", path, ownerSession, ownerCSRF, nil))
	if refresh.Code != 502 || account.qwen.Credits != nil || account.Usage.PrimaryUsageReported || account.qwen.CreditsError == "" {
		t.Fatalf("failed refresh retained a credit balance: %d %s", refresh.Code, refresh.Body.String())
	}
}

func TestQwenCreditSnapshotsExpireWithoutInventingFreshUsage(t *testing.T) {
	now := time.Now().UTC()
	credits := &QwenCreditSnapshot{Edition: "personal", Total: 45000, Used: 11250, Remaining: 33750, CycleStart: now.Add(-time.Hour), CycleEnd: now.Add(time.Hour), UpdatedAt: now.Add(-qwenCreditPollInterval - time.Second), Source: "alibaba-console"}
	if qwenCreditsCurrent(credits, now) {
		t.Fatal("stale automatic credits treated as current")
	}
	account := &Account{ID: "qwen", Type: AccountTypeQwen, qwen: qwenAccountState{Credits: credits}, Usage: qwenCreditUsage(credits, now)}
	h := qwenTestHandler(t, nil, account)
	rr := httptest.NewRecorder()
	h.handlePoolStats(rr, httptest.NewRequest("GET", "/api/pool/stats", nil))
	var payload struct {
		Accounts []AccountStats `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].TokenPlanCredits != nil || payload.Accounts[0].PrimaryUsageReported || payload.Accounts[0].TokenPlanCreditsError == "" {
		t.Fatalf("stale credits reached account stats: %+v", payload.Accounts)
	}
}
