package main

import (
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
			t.Fatalf("unexpected browser session operation: %s", r.URL.Query().Get("api"))
			return nil, nil
		}
	}))
	attachContributionFixture(t, h, "browser-owner")
	return h
}

func startQwenBrowserTest(t *testing.T, h *proxyHandler, session, csrf, accountID string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"account_id": accountID})
	req := passportContributionRequest("POST", "http://localhost:8080/api/pool/accounts/qwen/login/init", session, csrf, body)
	req.Header.Set("Origin", "http://localhost:8080")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("login init: %d %s", rr.Code, rr.Body.String())
	}
	var result struct {
		SessionID string `json:"session_id"`
		URL       string `json:"oauth_url"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &result) != nil || result.SessionID == "" {
		t.Fatal("missing browser login session")
	}
	authorize, err := url.Parse(result.URL)
	if err != nil || authorize.Scheme+"://"+authorize.Host != qwenLoginOrigin || authorize.Path != "/console-login" || authorize.Query().Has("needapikey") {
		t.Fatal("login must authorize the console without creating a general Model Studio key")
	}
	callback := "http://" + authorize.Query().Get("notice")
	parsed, err := url.Parse(callback)
	if err != nil || parsed.Host != "localhost:8080" || parsed.Path != qwenLoginCallback || parsed.Query().Get("state") == "" || parsed.Query().Get("session_id") != result.SessionID {
		t.Fatal("login did not use the official local callback protocol")
	}
	return result.SessionID, callback
}

func postQwenBrowserCallback(h *proxyHandler, callback string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", callback, strings.NewReader(`{"access_token":"synthetic-browser-console-token","console_site":"international","console_region":"ap-southeast-1","workspace_id":"test-workspace"}`))
	req.Header.Set("Origin", qwenLoginOrigin)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func pollQwenBrowserTest(h *proxyHandler, session, csrf, id string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"session_id": id})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, passportContributionRequest("POST", "/api/pool/accounts/qwen/login/poll", session, csrf, body))
	return rr
}

func TestQwenBrowserLoginPersistsPrivateAccountAndAutomaticCredits(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	if rr := pollQwenBrowserTest(h, session, csrf, id); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Fatal("login did not wait for browser authorization")
	}
	if rr := postQwenBrowserCallback(h, callback); rr.Code != 200 || rr.Header().Get("Access-Control-Allow-Origin") != qwenLoginOrigin {
		t.Fatalf("unauthenticated provider callback failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := pollQwenBrowserTest(h, session, csrf, id); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"awaiting_key"`) || strings.Contains(rr.Body.String(), "synthetic-browser-console-token") {
		t.Fatal("browser authorization did not request the existing Token Plan key safely")
	}
	body, _ := json.Marshal(map[string]string{"session_id": id, "api_key": "sk-sp-browser-test-key"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, passportContributionRequest("POST", "/api/pool/accounts/qwen/login/complete", session, csrf, body))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"complete"`) {
		t.Fatalf("complete: %d %s", rr.Code, rr.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one private Qwen account, got %d", len(accounts))
	}
	account := accounts[0]
	if account.QwenTokenPlan.Mode != qwenPersonalBrowser || account.QwenCredits.Total != 45000 || account.QwenCredits.Used != 11250 || account.Usage.PrimaryUsedPercent != .25 || len(account.Models) != 1 {
		t.Fatal("browser account did not retain its subscription allowance and discovered models")
	}
	if h.passport.authorizeAccount("browser-owner", account, "manage") != nil || h.passport.authorizeAccount("other", account, "use") == nil {
		t.Fatal("browser account lost private ownership")
	}
	encrypted, err := os.ReadFile(account.File)
	if err != nil || strings.Contains(string(encrypted), "synthetic-browser-console-token") || strings.Contains(string(encrypted), "sk-sp-browser-test-key") {
		t.Fatal("browser credentials were not encrypted")
	}
	view := httptest.NewRecorder()
	h.ServeHTTP(view, passportContributionRequest("GET", "/api/accounts/qwen/"+account.ID+"/token-plan", session, csrf, nil))
	if view.Code != 200 || strings.Contains(view.Body.String(), "console_token") || strings.Contains(view.Body.String(), "synthetic-browser-console-token") {
		t.Fatal("credit management leaked the browser credential")
	}
	if h.qwenLogin.byID[id].token != "" {
		t.Fatal("completed login retained a temporary token")
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, passportContributionRequest("POST", "/api/pool/accounts/qwen/login/complete", session, csrf, body))
	if replay.Code != 400 || len(h.pool.allAccounts()) != 1 {
		t.Fatal("login completion was replayed")
	}
}

func TestQwenBrowserCallbackChecksStateOriginRegionExpiryAndReplay(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	for _, test := range []struct{ name, origin, target, body string }{
		{"origin", "https://attacker.example", callback, `{"access_token":"token"}`},
		{"state", qwenLoginOrigin, callback + "wrong", `{"access_token":"token"}`},
		{"host", qwenLoginOrigin, strings.Replace(callback, "localhost:8080", "localhost:8081", 1), `{"access_token":"token"}`},
		{"region", qwenLoginOrigin, callback, `{"access_token":"token","console_site":"international","console_region":"cn-beijing"}`},
		{"size", qwenLoginOrigin, callback, `{"access_token":"` + strings.Repeat("a", 65<<10) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", test.target, strings.NewReader(test.body))
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code < 400 || h.qwenLogin.byID[id].status != "pending" {
				t.Fatal("invalid callback changed the pending login")
			}
		})
	}
	preflight := httptest.NewRequest("OPTIONS", callback, nil)
	preflight.Header.Set("Origin", qwenLoginOrigin)
	preflight.Header.Set("Access-Control-Request-Private-Network", "true")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, preflight)
	if rr.Code != 204 || rr.Header().Get("Access-Control-Allow-Private-Network") != "true" || h.qwenLogin.byID[id].status != "pending" {
		t.Fatal("local network preflight consumed or rejected authorization")
	}
	if postQwenBrowserCallback(h, callback).Code != 200 || postQwenBrowserCallback(h, callback).Code != 409 {
		t.Fatal("callback did not enforce one-time authorization")
	}
	h.qwenLogin.byID[id].expiresAt = time.Now().Add(-time.Second)
	if postQwenBrowserCallback(h, callback).Code != 410 || h.qwenLogin.byID[id].token != "" {
		t.Fatal("expired authorization retained its token")
	}
}

func TestQwenBrowserLoginIsActorScopedAndRequiresCSRF(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	other, otherCSRF := addTestPassportOperator(t, h.passport, "other")
	if pollQwenBrowserTest(h, other, otherCSRF, id).Code != 403 || pollQwenBrowserTest(h, session, "invalid", id).Code != 403 {
		t.Fatal("login polling accepted another actor or invalid CSRF")
	}
	if postQwenBrowserCallback(h, callback).Code != 200 {
		t.Fatal("valid callback failed")
	}
	body, _ := json.Marshal(map[string]string{"session_id": id, "api_key": "sk-sp-browser-test-key"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, passportContributionRequest("POST", "/api/pool/accounts/qwen/login/complete", other, otherCSRF, body))
	if rr.Code != 403 || len(h.pool.allAccounts()) != 0 {
		t.Fatal("another actor finalized the browser account")
	}
	h.qwenLogin.byID[id].expiresAt = time.Now().Add(-time.Second)
	if rr := pollQwenBrowserTest(h, session, csrf, id); !strings.Contains(rr.Body.String(), `"status":"error"`) || h.qwenLogin.byID[id].token != "" {
		t.Fatal("expired login remained usable")
	}
}

func TestQwenBrowserLoginRejectsUnsupportedCallbackAddresses(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	for _, origin := range []string{"", "https://localhost:8080", "https://pool.example", "http://192.168.1.20:8080", "http://localhost:8080/invalid"} {
		req := passportContributionRequest("POST", "/api/pool/accounts/qwen/login/init", session, csrf, []byte(`{}`))
		req.Header.Set("Origin", origin)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 400 || len(h.qwenLogin.byID) != 0 {
			t.Fatalf("unsupported browser callback address was accepted: %q", origin)
		}
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
	if postQwenBrowserCallback(h, callback).Code != 200 {
		t.Fatal("existing account callback failed")
	}
	rr := pollQwenBrowserTest(h, session, csrf, id)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"complete"`) {
		t.Fatalf("existing account link: %d %s", rr.Code, rr.Body.String())
	}
	accounts := h.pool.allAccounts()
	if len(accounts) != 1 || accounts[0].ID != accountID || accounts[0].AccessToken != "sk-sp-browser-test-key" || accounts[0].QwenTokenPlan.Mode != qwenPersonalBrowser || accounts[0].QwenCredits.Remaining != 33750 {
		t.Fatal("Cloud linking replaced the existing account or failed to load its balance")
	}
	other, otherCSRF := addTestPassportOperator(t, h.passport, "other")
	body, _ := json.Marshal(map[string]string{"account_id": accountID})
	req := passportContributionRequest("POST", "/api/pool/accounts/qwen/login/init", other, otherCSRF, body)
	req.Header.Set("Origin", "http://localhost:8080")
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, req)
	if denied.Code != 403 {
		t.Fatal("another actor linked the private account")
	}
}

func TestQwenBrowserLoginFailsClosedWhenCloudRejectsSession(t *testing.T) {
	h := qwenBrowserTestHandler(t)
	calls := 0
	h.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return qwenJSONResponse(401, `{"error":"synthetic-private-upstream-response"}`), nil
	})
	session, csrf := addTestPassportOperator(t, h.passport, "browser-owner")
	id, callback := startQwenBrowserTest(t, h, session, csrf, "")
	postQwenBrowserCallback(h, callback)
	body, _ := json.Marshal(map[string]string{"session_id": id, "api_key": "sk-sp-browser-test-key"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, passportContributionRequest("POST", "/api/pool/accounts/qwen/login/complete", session, csrf, body))
	if !strings.Contains(rr.Body.String(), `"status":"error"`) || strings.Contains(rr.Body.String(), "synthetic-private") || calls != 1 || len(h.pool.allAccounts()) != 0 || h.qwenLogin.byID[id].token != "" {
		t.Fatal("rejected browser session was reused, leaked or produced an account")
	}
}
