package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func qwenBrowserTestHandler(t *testing.T) *proxyHandler {
	t.Helper()
	now := time.Now().UTC()
	end := now.Add(28 * 24 * time.Hour).UnixMilli()
	h := qwenTestHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "oauth.alibabacloud.com" {
			switch r.URL.Path {
			case "/v1/token":
				if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != qwenCloudOAuthClient || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "test-authorization-code" || r.Form.Get("redirect_uri") != qwenOAuthRedirectURI || len(r.Form.Get("code_verifier")) < 43 {
					t.Fatal("OAuth code exchange did not use the official international PKCE contract")
				}
				return qwenJSONResponse(200, `{"access_token":"synthetic-oauth-access","refresh_token":"synthetic-oauth-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
			case "/v1/exchange":
				if r.Header.Get("Authorization") != "Bearer synthetic-oauth-access" {
					t.Fatal("temporary credential exchange did not use OAuth")
				}
				return qwenJSONResponse(200, fmt.Sprintf(`{"accessKeyId":"temporary-oauth-id","accessKeySecret":"temporary-oauth-secret","securityToken":"temporary-oauth-sts","expiration":%q}`, now.Add(time.Hour).Format(time.RFC3339))), nil
			}
		}
		if r.URL.Path == "/modelstudio/cli/generateAccessToken" {
			if r.Header.Get("x-acs-security-token") != "temporary-oauth-sts" || !strings.Contains(r.Header.Get("Authorization"), "Credential=temporary-oauth-id,") {
				t.Fatal("Cloud authorization did not use OAuth-derived temporary credentials")
			}
			return qwenJSONResponse(200, `{"cliAccessToken":"synthetic-browser-console-token"}`), nil
		}
		if r.URL.Path == "/compatible-mode/v1/models" {
			if r.Header.Get("Authorization") != "Bearer sk-sp-browser-test-key" {
				t.Fatal("model discovery did not use the inference API key")
			}
			return qwenJSONResponse(200, `{"data":[{"id":"qwen3.8-max"}]}`), nil
		}
		if r.URL.Host != "bailian-singapore-cs.alibabacloud.com" || r.URL.Path != "/cli/api.json" || r.Header.Get("Authorization") != "Bearer synthetic-browser-console-token" {
			t.Fatalf("unexpected browser session request: %s", r.URL)
		}
		switch strings.TrimPrefix(r.URL.Query().Get("api"), qwenCloudPersonalAPI) {
		case "subscription":
			return qwenJSONResponse(200, qwenCloudEnvelope(fmt.Sprintf(`{"instanceCode":"personal-test","specCode":"essential","status":"VALID","startTime":%d,"endTime":%d}`, now.Add(-24*time.Hour).UnixMilli(), end))), nil
		case "quota-config":
			return qwenJSONResponse(200, qwenCloudEnvelope(`{"essential":{"monthly":45000}}`)), nil
		case "usage":
			return qwenJSONResponse(200, qwenCloudEnvelope(fmt.Sprintf(`{"per1MonthPercentage":0.25,"per1MonthResetTime":%d}`, end))), nil
		default:
			t.Fatalf("unexpected browser session operation: %s", r.URL)
			return nil, nil
		}
	}))
	attachContributionFixture(t, h, "browser-owner")
	return h
}

func startQwenBrowserTest(t *testing.T, h *proxyHandler, session, csrf, accountID string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"account_id": accountID})
	req := passportContributionRequest("POST", "https://pool.example/api/pool/accounts/qwen/login/init", session, csrf, body)
	req.Header.Set("Origin", "https://pool.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Header().Get("Strict-Transport-Security") == "" {
		t.Fatalf("HTTPS login init: %d %s", rr.Code, rr.Body.String())
	}
	var result struct {
		SessionID string `json:"session_id"`
		URL       string `json:"oauth_url"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &result) != nil || result.SessionID == "" {
		t.Fatal("missing browser login session")
	}
	authorize, err := url.Parse(result.URL)
	if err != nil || authorize.Scheme+"://"+authorize.Host != qwenCloudSignInOrigin || authorize.Path != "/oauth2/v1/auth" || authorize.Query().Get("redirect_uri") != qwenOAuthRedirectURI || authorize.Query().Has("notice") || authorize.Query().Get("client_id") != qwenCloudOAuthClient || authorize.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("login did not use the remote-compatible Alibaba Cloud OAuth flow")
	}
	verifier := h.qwenLogin.byID[result.SessionID].verifier
	challenge := sha256.Sum256([]byte(verifier))
	if authorize.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) {
		t.Fatal("OAuth challenge does not match the server-owned verifier")
	}
	return result.SessionID, qwenOAuthRedirectURI + "?" + url.Values{"state": {authorize.Query().Get("state")}, "code": {"test-authorization-code"}}.Encode()
}

func authorizeQwenBrowserTest(h *proxyHandler, session, csrf, id, callback string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"session_id": id, "callback_url": callback})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, passportContributionRequest("POST", "https://pool.example/api/pool/accounts/qwen/login/authorize", session, csrf, body))
	return rr
}

func completeQwenBrowserTest(h *proxyHandler, session, csrf, id string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"session_id": id, "api_key": "sk-sp-browser-test-key"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, passportContributionRequest("POST", "https://pool.example/api/pool/accounts/qwen/login/complete", session, csrf, body))
	return rr
}

func TestQwenBrowserLoginPersistsPrivateAccountAndAutomaticCredits(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	if rr := completeQwenBrowserTest(h, session, csrf, id); rr.Code != 409 {
		t.Fatal("inference key was accepted before Cloud authorization")
	}
	if rr := authorizeQwenBrowserTest(h, session, csrf, id, callback); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"awaiting_key"`) || strings.Contains(rr.Body.String(), "synthetic-oauth") {
		t.Fatalf("remote authorization: %d %s", rr.Code, rr.Body.String())
	}
	rr := completeQwenBrowserTest(h, session, csrf, id)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"complete"`) {
		t.Fatalf("complete: %d %s", rr.Code, rr.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one private Qwen account, got %d", len(accounts))
	}
	account := accounts[0]
	if account.qwen.TokenPlan.Mode != qwenPersonalBrowser || account.qwen.Credits.Total != 45000 || account.qwen.Credits.Used != 11250 || account.Usage.PrimaryUsedPercent != .25 || len(account.Models) != 1 {
		t.Fatal("browser account did not retain its subscription allowance and discovered models")
	}
	if h.passport.authorizeAccount("browser-owner", account, "manage") != nil || h.passport.authorizeAccount("other", account, "use") == nil {
		t.Fatal("browser account lost private ownership")
	}
	encrypted, err := os.ReadFile(account.File)
	if err != nil || strings.Contains(string(encrypted), "synthetic-oauth") || strings.Contains(string(encrypted), "sk-sp-browser-test-key") {
		t.Fatal("browser credentials were not encrypted")
	}
	data, err := account.vault.readAccountFile(account.File)
	var stored QwenAuthJSON
	if err != nil || json.Unmarshal(data, &stored) != nil || stored.TokenPlan.CloudOAuth.snapshot().RefreshToken != "synthetic-oauth-refresh" {
		t.Fatal("OAuth renewal credentials were not persisted")
	}
	view := httptest.NewRecorder()
	h.ServeHTTP(view, passportContributionRequest("GET", "/api/accounts/qwen/"+account.ID+"/token-plan", session, csrf, nil))
	if view.Code != 200 || strings.Contains(view.Body.String(), "cloud_oauth") || strings.Contains(view.Body.String(), "synthetic-oauth") {
		t.Fatal("credit management leaked OAuth credentials")
	}
	if h.qwenLogin.byID[id].oauth != nil || h.qwenLogin.byID[id].verifier != "" || completeQwenBrowserTest(h, session, csrf, id).Code != 409 {
		t.Fatal("completed authorization retained temporary credentials or accepted replay")
	}
}

func TestQwenBrowserOAuthCallbackChecksStateAddressExpiryAndReplay(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	for _, invalid := range []string{
		callback + "wrong", callback + "&state=duplicate", callback + "&code=duplicate", callback + "#fragment",
		strings.Replace(callback, "127.0.0.1:12345", "attacker.example", 1), strings.Replace(callback, "http:", "https:", 1),
		strings.Replace(callback, "/cli/callback", "/another-path", 1), strings.Replace(callback, "test-authorization-code", "", 1),
	} {
		if rr := authorizeQwenBrowserTest(h, session, csrf, id, invalid); rr.Code != 400 || h.qwenLogin.byID[id].status != "pending" {
			t.Fatalf("invalid callback consumed authorization: %d", rr.Code)
		}
	}
	if authorizeQwenBrowserTest(h, session, csrf, id, callback).Code != 200 || authorizeQwenBrowserTest(h, session, csrf, id, callback).Code != 409 {
		t.Fatal("authorization did not enforce one-time use")
	}
	h.qwenLogin.byID[id].expiresAt = time.Now().Add(-time.Second)
	if completeQwenBrowserTest(h, session, csrf, id).Code != 410 || h.qwenLogin.byID[id].oauth != nil {
		t.Fatal("expired authorization retained its credentials")
	}
}

func TestQwenBrowserLoginIsActorScopedAndRequiresCSRF(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	other, otherCSRF := addTestPassportOperator(t, h.passport, "other")
	if authorizeQwenBrowserTest(h, other, otherCSRF, id, callback).Code != 403 || authorizeQwenBrowserTest(h, session, "invalid", id, callback).Code != 403 || authorizeQwenBrowserTest(h, "", "", id, callback).Code < 400 {
		t.Fatal("OAuth exchange accepted another actor, missing authentication or invalid CSRF")
	}
	if authorizeQwenBrowserTest(h, session, csrf, id, callback).Code != 200 || completeQwenBrowserTest(h, other, otherCSRF, id).Code != 403 {
		t.Fatal("OAuth completion was not scoped to its authenticated actor")
	}
}

func TestQwenBrowserLoginLinksExistingKeyAccountWithoutReplacingIt(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	accountID, err := h.saveContribution(contributionRequestActor(httptest.NewRequest("POST", "/", nil), "browser-owner"), AccountTypeQwen, "sk-sp-browser-test-key", map[string]any{
		"api_key": "sk-sp-browser-test-key", "provider_model_snapshot": providerModelSnapshot{FetchedAt: time.Now(), Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, callback := startQwenBrowserTest(t, h, session, csrf, accountID)
	rr := authorizeQwenBrowserTest(h, session, csrf, id, callback)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"complete"`) {
		t.Fatalf("existing account link: %d %s", rr.Code, rr.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 || accounts[0].ID != accountID || accounts[0].AccessToken != "sk-sp-browser-test-key" || accounts[0].qwen.TokenPlan.CloudOAuth == nil || accounts[0].qwen.Credits.Remaining != 33750 {
		t.Fatal("Cloud linking replaced the existing account or failed to load its balance")
	}
}

func TestQwenBrowserLoginFailsClosedWhenCloudRejectsSession(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	original := h.transport
	calls := 0
	h.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/modelstudio/cli/generateAccessToken" {
			calls++
			return qwenJSONResponse(401, `{"error":"synthetic-private-upstream-response"}`), nil
		}
		return original.RoundTrip(r)
	})
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	authorizeQwenBrowserTest(h, session, csrf, id, callback)
	rr := completeQwenBrowserTest(h, session, csrf, id)
	if !strings.Contains(rr.Body.String(), `"status":"error"`) || strings.Contains(rr.Body.String(), "synthetic-private") || calls != 1 || len(h.pool.allAccounts()) != 0 || h.qwenLogin.byID[id].oauth != nil {
		t.Fatal("rejected Cloud authorization was reused, leaked or produced an account")
	}
}
