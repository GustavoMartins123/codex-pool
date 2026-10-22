package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildWhamConsumeResetCreditURLKeepsBackendAPI(t *testing.T) {
	base, _ := url.Parse("https://chatgpt.com/backend-api")
	got := buildWhamConsumeResetCreditURL(base)
	expected := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	if got != expected {
		t.Fatalf("expected %s, got %s", expected, got)
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// resetCreditUpstream scripts the ChatGPT backend for a consume: the fresh
// credit list, the POST that spends one credit, then the credit and usage
// reads that follow it.
type resetCreditUpstream struct {
	t        *testing.T
	credits  string
	consume  func(body map[string]string) *http.Response
	requests []string
}

func (u *resetCreditUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	u.requests = append(u.requests, req.Method+" "+req.URL.Path)
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/rate-limit-reset-credits"):
		credits := u.credits
		if len(u.requests) > 2 {
			credits = `{"available_count":0,"credits":[]}`
		}
		return jsonResponse(credits), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/rate-limit-reset-credits/consume"):
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			u.t.Fatal(err)
		}
		return u.consume(body), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/wham/usage"):
		return jsonResponse(`{"rate_limit":{"primary_window":{"used_percent":0,"reset_at":1783828800,"limit_window_seconds":18000},"secondary_window":{"used_percent":0,"reset_at":1784433600,"limit_window_seconds":604800}}}`), nil
	}
	u.t.Fatalf("unexpected upstream request %s %s", req.Method, req.URL.Path)
	return nil, nil
}

func resetCreditHandler(upstream *resetCreditUpstream) *proxyHandler {
	base, _ := url.Parse("https://chatgpt.com/backend-api")
	return &proxyHandler{cfg: &config{whamBase: base}, transport: upstream}
}

func codexAccountWithResets() *Account {
	return &Account{ID: "pool-codex", Type: AccountTypeCodex, AccessToken: "access-token", AccountID: "account-id", Usage: UsageSnapshot{PrimaryUsedPercent: 1, SecondaryUsedPercent: 1, RetrievedAt: time.Now()}}
}

func TestConsumeEarliestResetCreditSpendsSoonestExpiringFreshCredit(t *testing.T) {
	soon := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	late := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339Nano)
	var spent []map[string]string
	upstream := &resetCreditUpstream{t: t, credits: `{"available_count":2,"credits":[
		{"id":"credit-late","status":"available","expires_at":"` + late + `"},
		{"id":"credit-soon","status":"available","expires_at":"` + soon + `"}]}`}
	upstream.consume = func(body map[string]string) *http.Response {
		spent = append(spent, body)
		return jsonResponse(`{"code":"reset","windows_reset":2}`)
	}
	h := resetCreditHandler(upstream)
	account := codexAccountWithResets()
	// A stale polled snapshot must not decide which credit is spent.
	account.RateLimitResetCredits = []RateLimitResetCredit{{ID: "credit-stale", ExpiresAt: time.Now().Add(time.Minute)}}

	result, err := h.consumeEarliestCodexResetCredit(account)
	if err != nil {
		t.Fatal(err)
	}
	if len(spent) != 1 || spent[0]["credit_id"] != "credit-soon" || spent[0]["redeem_request_id"] != "codex-pool:credit-soon" {
		t.Fatalf("spent = %+v", spent)
	}
	if result.CreditID != "credit-soon" || result.Code != "reset" || result.WindowsReset != 2 || result.RefreshError != "" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"GET /backend-api/wham/rate-limit-reset-credits", "POST /backend-api/wham/rate-limit-reset-credits/consume", "GET /backend-api/wham/rate-limit-reset-credits", "GET /backend-api/wham/usage"}
	if strings.Join(upstream.requests, "|") != strings.Join(want, "|") {
		t.Fatalf("upstream requests = %v", upstream.requests)
	}
	if account.ResetCreditsAvailable != 0 || len(account.RateLimitResetCredits) != 0 || account.ResetCreditRedeeming {
		t.Fatalf("account after consume: available=%d credits=%v redeeming=%v", account.ResetCreditsAvailable, account.RateLimitResetCredits, account.ResetCreditRedeeming)
	}
}

func TestConsumeEarliestResetCreditSkipsExpiredAndUnavailableCredits(t *testing.T) {
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	upstream := &resetCreditUpstream{t: t, credits: `{"available_count":2,"credits":[
		{"id":"credit-expired","status":"available","expires_at":"` + expired + `"},
		{"id":"credit-used","status":"redeemed","expires_at":"` + future + `"}]}`}
	upstream.consume = func(map[string]string) *http.Response {
		t.Fatal("a reset was spent without a usable credit")
		return nil
	}
	account := codexAccountWithResets()
	_, err := resetCreditHandler(upstream).consumeEarliestCodexResetCredit(account)
	if !errors.Is(err, errResetCreditUnavailable) {
		t.Fatalf("err = %v, want %v", err, errResetCreditUnavailable)
	}
	if account.ResetCreditRedeeming {
		t.Fatal("redeeming flag left set")
	}
}

func TestConsumeEarliestResetCreditRefusesConcurrentConsumeWithoutUpstreamCalls(t *testing.T) {
	upstream := &resetCreditUpstream{t: t}
	account := codexAccountWithResets()
	account.ResetCreditRedeeming = true
	_, err := resetCreditHandler(upstream).consumeEarliestCodexResetCredit(account)
	if !errors.Is(err, errResetCreditInProgress) {
		t.Fatalf("err = %v, want %v", err, errResetCreditInProgress)
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream requests = %v", upstream.requests)
	}
	if !account.ResetCreditRedeeming {
		t.Fatal("a concurrent consume released the first consume's guard")
	}
}

func TestConsumeEarliestResetCreditReportsUpstreamRejectionAndReleasesGuard(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	upstream := &resetCreditUpstream{t: t, credits: `{"available_count":1,"credits":[{"id":"credit","status":"available","expires_at":"` + future + `"}]}`}
	upstream.consume = func(map[string]string) *http.Response {
		response := jsonResponse(`{"error":"nope"}`)
		response.StatusCode, response.Status = http.StatusBadRequest, "400 Bad Request"
		return response
	}
	account := codexAccountWithResets()
	if _, err := resetCreditHandler(upstream).consumeEarliestCodexResetCredit(account); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if account.ResetCreditRedeeming {
		t.Fatal("redeeming flag left set after a rejection")
	}
	if len(upstream.requests) != 2 {
		t.Fatalf("a rejected consume must not be followed by refreshes: %v", upstream.requests)
	}
}

func TestConsumeEarliestResetCreditRejectsNonCodexAccounts(t *testing.T) {
	upstream := &resetCreditUpstream{t: t}
	account := codexAccountWithResets()
	account.Type = AccountTypeClaude
	if _, err := resetCreditHandler(upstream).consumeEarliestCodexResetCredit(account); err == nil {
		t.Fatal("consumed a reset on a non-Codex account")
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream requests = %v", upstream.requests)
	}
}
