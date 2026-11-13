package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQwenCloudOAuthRefreshesRotatingCredentialsAndCachesConsoleToken(t *testing.T) {
	now := time.Now().UTC()
	refreshes, exchanges, issued := 0, 0, 0
	h := qwenTestHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/token":
			refreshes++
			if r.ParseForm() != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "original-refresh" || r.Form.Get("client_id") != qwenCloudOAuthClient {
				t.Fatal("invalid OAuth refresh request")
			}
			return qwenJSONResponse(200, `{"access_token":"renewed-access","refresh_token":"rotated-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
		case "/v1/exchange":
			exchanges++
			if r.Header.Get("Authorization") != "Bearer renewed-access" {
				t.Fatal("expired access token was used after refresh")
			}
			return qwenJSONResponse(200, fmt.Sprintf(`{"accessKeyId":"temporary-id","accessKeySecret":"temporary-secret","securityToken":"temporary-sts","expiration":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
		case "/modelstudio/cli/generateAccessToken":
			issued++
			if r.Header.Get("x-acs-security-token") != "temporary-sts" || !strings.Contains(r.Header.Get("Authorization"), "Credential=temporary-id,") {
				t.Fatal("OAuth-derived temporary credentials did not sign the Cloud request")
			}
			return qwenJSONResponse(200, `{"cliAccessToken":"cached-oauth-console-token"}`), nil
		default:
			t.Fatalf("unexpected OAuth request: %s", r.URL)
			return nil, nil
		}
	}))
	oauth := &QwenCloudOAuth{tokens: qwenOAuthTokens{AccessToken: "expired-access", RefreshToken: "original-refresh", ExpiresAt: now.Add(-time.Minute)}}
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalBrowser, CloudOAuth: oauth}
	for _, at := range []time.Time{now, now.Add(time.Minute)} {
		if token, err := h.qwenCloudAccessToken(context.Background(), plan, at); err != nil || token != "cached-oauth-console-token" {
			t.Fatalf("OAuth authorization failed: %v", err)
		}
	}
	if refreshes != 1 || exchanges != 1 || issued != 1 {
		t.Fatalf("OAuth credentials or console token were not cached: %d/%d/%d", refreshes, exchanges, issued)
	}
	data, _ := json.Marshal(plan)
	var restored QwenTokenPlanConfig
	if json.Unmarshal(data, &restored) != nil || restored.CloudOAuth.snapshot().RefreshToken != "rotated-refresh" || restored.CloudOAuth.snapshot().AccessToken != "renewed-access" {
		t.Fatal("refreshed OAuth credentials would be lost after restart")
	}
	if _, err := h.qwenCloudAccessToken(context.Background(), &restored, now.Add(qwenCloudTokenTTL+time.Second)); err != nil || refreshes != 1 || exchanges != 2 || issued != 2 {
		t.Fatal("expired console cache did not use the current OAuth credentials")
	}
}

func TestQwenCloudOAuthDeduplicatesConcurrentTokenIssuance(t *testing.T) {
	now := time.Now().UTC()
	var exchanges, issued atomic.Int32
	h := qwenTestHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/exchange" {
			exchanges.Add(1)
			return qwenJSONResponse(200, fmt.Sprintf(`{"accessKeyId":"temporary-id","accessKeySecret":"temporary-secret","securityToken":"temporary-sts","expiration":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
		}
		if r.URL.Path == "/modelstudio/cli/generateAccessToken" {
			issued.Add(1)
			return qwenJSONResponse(200, `{"cliAccessToken":"shared-console-token"}`), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return qwenJSONResponse(500, `{}`), nil
	}))
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalBrowser, CloudOAuth: &QwenCloudOAuth{tokens: qwenOAuthTokens{AccessToken: "valid-access", RefreshToken: "valid-refresh", ExpiresAt: now.Add(time.Hour)}}}
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if token, err := h.qwenCloudAccessToken(context.Background(), plan, now); err != nil || token != "shared-console-token" {
				t.Errorf("concurrent authorization failed: %v", err)
			}
		}()
	}
	group.Wait()
	if exchanges.Load() != 1 || issued.Load() != 1 {
		t.Fatal("concurrent usage refreshes duplicated OAuth token issuance")
	}
}

func TestQwenCloudOAuthFailsClosedOnRefreshRejection(t *testing.T) {
	now := time.Now()
	calls := 0
	h := qwenTestHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/v1/token" {
			t.Fatal("rejected refresh reused an expired credential")
		}
		return qwenJSONResponse(401, `{"error_description":"private-upstream-response"}`), nil
	}))
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalBrowser, CloudOAuth: &QwenCloudOAuth{tokens: qwenOAuthTokens{AccessToken: "expired-access", RefreshToken: "revoked-refresh", ExpiresAt: now.Add(-time.Hour)}}}
	token, err := h.qwenCloudAccessToken(context.Background(), plan, now)
	if err == nil || token != "" || calls != 1 || strings.Contains(err.Error(), "private-upstream-response") {
		t.Fatal("rejected OAuth refresh did not fail closed")
	}
}
