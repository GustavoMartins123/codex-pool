package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestResetWindowPolicyByProviderAndCodexTier(t *testing.T) {
	tests := []struct {
		name     string
		provider AccountType
		plan     string
		want     ResetWindowPolicy
	}{
		{"codex plus", AccountTypeCodex, "plus", ResetWindowPolicy{AccountTierPlus, ResetWindowFiveHour, ResetWindowWeekly}},
		{"codex team basic", AccountTypeCodex, "team_basic", ResetWindowPolicy{AccountTierTeamBasic, ResetWindowFiveHour, ResetWindowWeekly}},
		{"codex legacy team", AccountTypeCodex, "team", ResetWindowPolicy{AccountTierTeamBasic, ResetWindowFiveHour, ResetWindowWeekly}},
		{"codex pro", AccountTypeCodex, "pro", ResetWindowPolicy{AccountTierProOrHigher, ResetWindowNone, ResetWindowWeekly}},
		{"codex prolite", AccountTypeCodex, "prolite", ResetWindowPolicy{AccountTierProOrHigher, ResetWindowNone, ResetWindowWeekly}},
		{"codex business pro", AccountTypeCodex, "business_pro", ResetWindowPolicy{AccountTierProOrHigher, ResetWindowNone, ResetWindowWeekly}},
		{"codex enterprise", AccountTypeCodex, "enterprise", ResetWindowPolicy{AccountTierProOrHigher, ResetWindowNone, ResetWindowWeekly}},
		{"antigravity", AccountTypeAntigravity, "pro", ResetWindowPolicy{AccountTierUnknown, ResetWindowFiveHour, ResetWindowWeekly}},
		{"zai", AccountTypeZAI, "coding_plan", ResetWindowPolicy{AccountTierUnknown, ResetWindowRequests, ResetWindowTokens}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resetWindowPolicy(test.provider, test.plan); got != test.want {
				t.Fatalf("resetWindowPolicy(%q, %q) = %+v, want %+v", test.provider, test.plan, got, test.want)
			}
		})
	}
}

func TestPoolStatsExposesNormalizedResetWindows(t *testing.T) {
	accounts := []*Account{
		{ID: "plus", Type: AccountTypeCodex, PlanType: "plus"},
		{ID: "team", Type: AccountTypeCodex, PlanType: "team_basic"},
		{ID: "pro", Type: AccountTypeCodex, PlanType: "pro"},
		{ID: "antigravity", Type: AccountTypeAntigravity, PlanType: "pro"},
		{ID: "zai", Type: AccountTypeZAI, PlanType: "coding_plan"},
	}
	handler := &proxyHandler{pool: newPoolState(accounts, false)}
	recorder := httptest.NewRecorder()
	handler.handlePoolStats(recorder, httptest.NewRequest("GET", "/api/pool/stats", nil))
	var payload struct {
		Accounts []AccountStats `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != len(accounts) {
		t.Fatalf("account count = %d, want %d", len(payload.Accounts), len(accounts))
	}
	for i, account := range accounts {
		want := resetWindowPolicy(account.Type, account.PlanType)
		if got := payload.Accounts[i].ResetWindows; got != want {
			t.Errorf("%s reset windows = %+v, want %+v", account.ID, got, want)
		}
	}
}
