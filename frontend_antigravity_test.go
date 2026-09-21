package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestPoolStatsUsesEmptyAccountArrayForFirstAccountSignIn(t *testing.T) {
	handler := &proxyHandler{pool: newPoolState(nil, false)}
	recorder := httptest.NewRecorder()
	handler.handlePoolStats(recorder, httptest.NewRequest("GET", "/api/pool/stats", nil))
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["accounts"]) != "[]" {
		t.Fatalf("accounts must be [], got %s", payload["accounts"])
	}
}

func TestPoolStatsExposesBankedResetExpirationsToFriends(t *testing.T) {
	expiresAt := time.Date(2026, time.July, 18, 0, 35, 13, 709488000, time.UTC)
	account := &Account{
		Type:                    AccountTypeCodex,
		ID:                      "friend-visible-codex-account",
		ResetCreditsAvailable:   2,
		ResetCreditsRetrievedAt: time.Now(),
		RateLimitResetCredits:   []RateLimitResetCredit{{ID: "credit-1", ExpiresAt: expiresAt}},
	}
	handler := &proxyHandler{pool: newPoolState([]*Account{account}, false)}
	recorder := httptest.NewRecorder()
	handler.handlePoolStats(recorder, httptest.NewRequest("GET", "/api/pool/stats", nil))

	var payload struct {
		Accounts []AccountStats `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(payload.Accounts))
	}
	got := payload.Accounts[0]
	if !got.ResetCreditsKnown || got.ResetCreditsAvailable != 2 {
		t.Fatalf("reset credit summary = known %v, available %d", got.ResetCreditsKnown, got.ResetCreditsAvailable)
	}
	if len(got.ResetCreditExpirations) != 1 || got.ResetCreditExpirations[0] != expiresAt.Format(time.RFC3339Nano) {
		t.Fatalf("reset credit expirations = %v", got.ResetCreditExpirations)
	}
}

func TestPoolStatsUsesRecentRequestsForAccount24hBurn(t *testing.T) {
	store, err := newUsageStore(filepath.Join(t.TempDir(), "usage.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	account := &Account{
		Type: AccountTypeAntigravity,
		ID:   "antigravity-recent",
		Totals: AccountUsage{
			TotalInputTokens:  500000,
			TotalOutputTokens: 100000,
		},
	}
	if err := store.record(RequestUsage{
		Timestamp: time.Now().Add(-2 * time.Hour), AccountID: account.ID,
		AccountType: AccountTypeAntigravity, InputTokens: 500, OutputTokens: 120,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.record(RequestUsage{
		Timestamp: time.Now().Add(-48 * time.Hour), AccountID: account.ID,
		AccountType: AccountTypeAntigravity, InputTokens: 900, OutputTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}

	handler := &proxyHandler{pool: newPoolState([]*Account{account}, false), store: store}
	recorder := httptest.NewRecorder()
	handler.handlePoolStats(recorder, httptest.NewRequest("GET", "/api/pool/stats", nil))

	var payload struct {
		Accounts []AccountStats `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(payload.Accounts))
	}
	if got := payload.Accounts[0].Last24hTokens; got != 620 {
		t.Fatalf("last_24h_tokens = %d, want 620", got)
	}
}

func TestPreserveUsageSnapshotsCarriesBankedResetsAcrossHotReload(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 12, 17, 44, 43, 0, time.UTC)
	retrievedAt := time.Date(2026, time.July, 17, 16, 57, 33, 0, time.UTC)
	current := &Account{
		Type:                    AccountTypeCodex,
		ID:                      "account",
		ResetCreditsAvailable:   5,
		ResetCreditsRetrievedAt: retrievedAt,
		RateLimitResetCredits:   []RateLimitResetCredit{{ID: "credit", ExpiresAt: expiresAt}},
	}
	loaded := &Account{Type: AccountTypeCodex, ID: "account"}

	preserveUsageSnapshots([]*Account{current}, []*Account{loaded})

	if loaded.ResetCreditsAvailable != 5 || !loaded.ResetCreditsRetrievedAt.Equal(retrievedAt) {
		t.Fatalf("preserved reset summary = available %d, retrieved %s", loaded.ResetCreditsAvailable, loaded.ResetCreditsRetrievedAt)
	}
	if len(loaded.RateLimitResetCredits) != 1 || loaded.RateLimitResetCredits[0].ID != "credit" || !loaded.RateLimitResetCredits[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("preserved reset credits = %+v", loaded.RateLimitResetCredits)
	}

	current.RateLimitResetCredits[0].ID = "mutated"
	if loaded.RateLimitResetCredits[0].ID != "credit" {
		t.Fatal("hot-reloaded account shares reset credit backing storage")
	}
}
