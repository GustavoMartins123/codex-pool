package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const antigravityVerifyFixture = `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Verify your account to continue.","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://accounts.google.com/verify"}}]}}`

const antigravityHealthyQuotaFixture = `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.4,"resetTime":"2099-01-01T00:00:00Z"},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.5,"resetTime":"2099-01-01T00:00:00Z"}]}]}`

func antigravityTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestAntigravitySyncTrustsDailyHostForVerification(t *testing.T) {
	antigravityModels.Reset()
	t.Cleanup(antigravityModels.Reset)
	daily, _ := url.Parse("https://daily.example.test")
	production, _ := url.Parse("https://prod.example.test")
	file := filepath.Join(t.TempDir(), "ag.json")
	account := &Account{Type: AccountTypeAntigravity, ID: "ag", File: file, AccessToken: "token", ProjectID: "project", ModelRateLimits: map[string]time.Time{},
		NeedsVerification: true, VerificationURL: "https://accounts.google.com/verify", HealthError: antigravityVerifyFixture}

	dailyQuotaStatus := http.StatusOK
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, ":fetchAvailableModels"):
			return antigravityTestResponse(http.StatusOK, `{"models":{"gemini-live":{"displayName":"Gemini Live"}}}`), nil
		case req.URL.Host == production.Host:
			return antigravityTestResponse(http.StatusForbidden, antigravityVerifyFixture), nil
		case dailyQuotaStatus == http.StatusOK:
			return antigravityTestResponse(http.StatusOK, antigravityHealthyQuotaFixture), nil
		default:
			return antigravityTestResponse(dailyQuotaStatus, antigravityVerifyFixture), nil
		}
	})

	if err := syncAntigravityModels(context.Background(), transport, account, daily, production); err != nil {
		t.Fatal(err)
	}
	if account.NeedsVerification || account.VerificationURL != "" || account.HealthError != "" {
		t.Fatalf("daily quota success must clear verification: %v %q %q", account.NeedsVerification, account.VerificationURL, account.HealthError)
	}
	snapshot, _ := antigravityModels.AccountSnapshot(account.ID)
	if snapshot.QuotaSummary == nil || len(snapshot.QuotaSummary.Groups) != 1 {
		t.Fatalf("quota summary not stored: %#v", snapshot.QuotaSummary)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("account was not persisted: %v", err)
	}

	dailyQuotaStatus = http.StatusForbidden
	if err := syncAntigravityModels(context.Background(), transport, account, daily, production); err == nil {
		t.Fatal("daily verification demand must be reported")
	}
	if !account.NeedsVerification || account.VerificationURL != "https://accounts.google.com/verify" {
		t.Fatalf("daily 403 must flag verification: %v %q", account.NeedsVerification, account.VerificationURL)
	}
	snapshot, _ = antigravityModels.AccountSnapshot(account.ID)
	if snapshot.QuotaSummary == nil {
		t.Fatal("previous quota summary must survive a failed quota poll")
	}
}

func TestAntigravitySyncIgnoresProductionOnlyVerificationDemand(t *testing.T) {
	antigravityModels.Reset()
	t.Cleanup(antigravityModels.Reset)
	daily, _ := url.Parse("https://daily.example.test")
	production, _ := url.Parse("https://prod.example.test")
	account := &Account{Type: AccountTypeAntigravity, ID: "ag", File: filepath.Join(t.TempDir(), "ag.json"), AccessToken: "token", ProjectID: "project", ModelRateLimits: map[string]time.Time{}}
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, ":fetchAvailableModels"):
			return antigravityTestResponse(http.StatusOK, `{"models":{"gemini-live":{"displayName":"Gemini Live"}}}`), nil
		case req.URL.Host == production.Host:
			return antigravityTestResponse(http.StatusForbidden, antigravityVerifyFixture), nil
		default:
			return antigravityTestResponse(http.StatusServiceUnavailable, "unavailable"), nil
		}
	})
	if err := syncAntigravityModels(context.Background(), transport, account, daily, production); err != nil {
		t.Fatalf("a transient quota failure must not fail the model sync: %v", err)
	}
	if account.NeedsVerification || account.HealthError != "" {
		t.Fatalf("production-only 403 flagged the account: %v %q", account.NeedsVerification, account.HealthError)
	}
}

func TestAntigravityProductionForbiddenDoesNotReplaceDailyAnswer(t *testing.T) {
	daily, _ := url.Parse("https://daily.example.test")
	production, _ := url.Parse("https://prod.example.test")
	provider := NewAntigravityProvider(daily, production)
	for _, dailyStatus := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusNotFound} {
		handler := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == production.Host {
				return antigravityTestResponse(http.StatusForbidden, antigravityVerifyFixture), nil
			}
			return antigravityTestResponse(dailyStatus, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`), nil
		})}
		response, err := handler.doAntigravityRequest(context.Background(), http.Header{}, &Account{AccessToken: "token"}, provider, antigravityPreparedRequest{Body: []byte(`{}`), Operation: "generateContent"})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != dailyStatus || !strings.Contains(string(body), "RESOURCE_EXHAUSTED") {
			t.Fatalf("daily %d: got %d %s, want the daily answer", dailyStatus, response.StatusCode, body)
		}
	}
}

func TestAntigravityProductionAnswerStillServesWhenDailyFails(t *testing.T) {
	daily, _ := url.Parse("https://daily.example.test")
	production, _ := url.Parse("https://prod.example.test")
	provider := NewAntigravityProvider(daily, production)
	handler := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == production.Host {
			return antigravityTestResponse(http.StatusOK, `{"response":{}}`), nil
		}
		return antigravityTestResponse(http.StatusServiceUnavailable, "unavailable"), nil
	})}
	response, err := handler.doAntigravityRequest(context.Background(), http.Header{}, &Account{AccessToken: "token"}, provider, antigravityPreparedRequest{Body: []byte(`{}`), Operation: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want production 200", response.StatusCode)
	}
}
