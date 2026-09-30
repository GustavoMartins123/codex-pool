package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func zaiTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestZAILoginInitPollAndActor(t *testing.T) {
	var pollToken string
	h := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/oauth/cli/init":
			pollToken = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if len(pollToken) != 64 {
				t.Fatal("OAuth init did not send a 32-byte poll token")
			}
			authorize := "https://chat.z.ai/api/oauth/authorize?redirect_uri=https%3A%2F%2Fzcode.z.ai%2Fapi%2Fv1%2Foauth%2Fcli%2Fcallback%2Fzai&state=test-state"
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{
				"flow_id": "flow-123", "poll_token": pollToken, "authorize_url": authorize,
				"expires_at": time.Now().Add(5 * time.Minute).Unix(), "poll_interval_sec": 2,
			}})
			return zaiTestResponse(200, string(body)), nil
		case "/api/v1/oauth/cli/poll/flow-123":
			if req.Header.Get("Authorization") != "Bearer "+pollToken {
				t.Fatal("OAuth poll token changed")
			}
			return zaiTestResponse(200, `{"code":0,"data":{"status":"pending"}}`), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/zai/login/init", strings.NewReader("{}"))
	request = request.WithContext(context.WithValue(request.Context(), providerContributionActorKey{}, "member-a"))
	recorder := httptest.NewRecorder()
	h.handleZAILoginInit(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("init status %d: %s", recorder.Code, recorder.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &started); err != nil || started.SessionID == "" {
		t.Fatalf("missing OAuth session: %v", err)
	}
	t.Cleanup(func() {
		zaiLoginSessions.Lock()
		delete(zaiLoginSessions.byID, started.SessionID)
		zaiLoginSessions.Unlock()
	})
	pollBody := fmt.Sprintf(`{"session_id":%q}`, started.SessionID)
	wrong := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/zai/login/poll", strings.NewReader(pollBody))
	wrong = wrong.WithContext(context.WithValue(wrong.Context(), providerContributionActorKey{}, "member-b"))
	denied := httptest.NewRecorder()
	h.handleZAILoginPoll(denied, wrong)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("other member received status %d", denied.Code)
	}
	poll := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/zai/login/poll", strings.NewReader(pollBody))
	poll = poll.WithContext(context.WithValue(poll.Context(), providerContributionActorKey{}, "member-a"))
	status := httptest.NewRecorder()
	h.handleZAILoginPoll(status, poll)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status":"pending"`) {
		t.Fatalf("unexpected poll result: %d %s", status.Code, status.Body.String())
	}
}

func TestZAIOAuthRejectsAccountWithoutIndividualPlan(t *testing.T) {
	h := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/oauth/cli/poll/flow-123":
			return zaiTestResponse(200, `{"code":0,"data":{"status":"ready","token":"jwt","zai":{"access_token":"oauth-token"},"user":{"user_id":"user-1","email":"user@example.com"}}}`), nil
		case "/api/auth/z/login":
			return zaiTestResponse(200, `{"code":0,"data":{"access_token":"business-token"}}`), nil
		case "/api/biz/customer/getCustomerInfo":
			return zaiTestResponse(200, `{"code":0,"data":{"organizations":[{"organizationId":"org-1","projects":[{"projectId":"project-1","projectType":1}]}]}}`), nil
		case "/api/biz/v1/organization/org-1/projects/project-1/api_keys":
			return zaiTestResponse(200, `{"code":0,"data":[{"name":"codex-pool","apiKey":"key-id"}]}`), nil
		case "/api/biz/v1/organization/org-1/projects/project-1/api_keys/copy/key-id":
			return zaiTestResponse(200, `{"code":0,"data":{"secretKey":"secret"}}`), nil
		case "/api/biz/subscription/list":
			if req.Header.Get("Authorization") != "key-id.secret" {
				t.Fatal("subscription check did not use the resolved account key")
			}
			return zaiTestResponse(200, `{"code":0,"data":[{"productId":"payg","status":"VALID","inCurrentPeriod":true}]}`), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})}
	_, err := h.pollZAILogin(httptest.NewRequest(http.MethodPost, "/", nil), "flow-123", "poll-token")
	if err == nil || !strings.Contains(err.Error(), "no active Individual Coding Plan") {
		t.Fatalf("expected missing plan error, got %v", err)
	}
}

func TestZAIOAuthRejectsPlanKeyWithoutModelAccess(t *testing.T) {
	base, err := url.Parse("https://api.z.ai/api/anthropic")
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{cfg: &config{zaiBase: base}, transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/biz/subscription/list" {
			return zaiTestResponse(200, `{"code":0,"data":[{"productId":"coding-plan","status":"VALID","inCurrentPeriod":true}]}`), nil
		}
		if req.URL.Path == "/api/anthropic/v1/messages" {
			return zaiTestResponse(429, `{"error":{"code":1113}}`), nil
		}
		t.Fatalf("unexpected request: %s", req.URL)
		return nil, nil
	})}
	if err := h.validateZAICodingPlan(context.Background(), "key-id.secret"); err != nil {
		t.Fatalf("active plan was rejected: %v", err)
	}
	if err := h.validateZAICodingPlanKey(context.Background(), "key-id.secret"); err == nil {
		t.Fatal("PAYG-only key was accepted for Coding Plan traffic")
	}
}

func TestZAIOAuthPersistsLinkedCodingPlanAccount(t *testing.T) {
	base, err := url.Parse("https://api.z.ai/api/anthropic")
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{
		cfg:      &config{poolDir: t.TempDir(), zaiBase: base},
		registry: NewProviderRegistry(nil, nil, nil, NewZAIProvider(base)),
		pool:     newPoolState(nil, false),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/v1/oauth/cli/poll/flow-123":
				return zaiTestResponse(200, `{"code":0,"data":{"status":"ready","token":"jwt","zai":{"access_token":"oauth-token"},"user":{"user_id":"user-1","email":"user@example.com"}}}`), nil
			case "/api/auth/z/login":
				return zaiTestResponse(200, `{"code":0,"data":{"access_token":"business-token"}}`), nil
			case "/api/biz/customer/getCustomerInfo":
				return zaiTestResponse(200, `{"code":0,"data":{"organizations":[{"organizationId":"org-1","projects":[{"projectId":"project-1","projectType":1}]}]}}`), nil
			case "/api/biz/v1/organization/org-1/projects/project-1/api_keys":
				return zaiTestResponse(200, `{"code":0,"data":[{"name":"codex-pool","apiKey":"key-id"}]}`), nil
			case "/api/biz/v1/organization/org-1/projects/project-1/api_keys/copy/key-id":
				return zaiTestResponse(200, `{"code":0,"data":{"secretKey":"secret"}}`), nil
			case "/api/biz/subscription/list":
				return zaiTestResponse(200, `{"code":0,"data":[{"productId":"coding-plan","status":"VALID","inCurrentPeriod":true}]}`), nil
			case "/api/anthropic/v1/messages":
				if req.Header.Get("X-Api-Key") != "key-id.secret" {
					t.Fatal("model request did not use the linked account key")
				}
				return zaiTestResponse(200, `{"content":[]}`), nil
			default:
				t.Fatalf("unexpected request: %s", req.URL)
				return nil, nil
			}
		}),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/zai/login/poll", nil)
	request = request.WithContext(context.WithValue(request.Context(), providerContributionActorKey{}, "member-a"))
	attachContributionFixture(t, h, "member-a")
	result, err := h.pollZAILogin(request, "flow-123", "poll-token")
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.accountID == "" || result.email != "user@example.com" {
		t.Fatalf("unexpected login result: %+v", result)
	}
	data, err := readAccountFile(filepath.Join(h.cfg.poolDir, "zai", result.accountID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var account ZAIAuthJSON
	if err := json.Unmarshal(data, &account); err != nil {
		t.Fatal(err)
	}
	if account.AuthType != "oauth" || account.UserID != "user-1" || account.BusinessToken != "business-token" || account.ZCodeJWT != "jwt" {
		t.Fatal("OAuth account identity or session was not persisted")
	}
	if h.pool.count() != 1 {
		t.Fatalf("expected linked account in pool, got %d", h.pool.count())
	}
}
