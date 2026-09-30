package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	zaiOAuthOrigin = "https://zcode.z.ai"
	zaiAPIOrigin   = "https://api.z.ai"
	zaiKeyName     = "codex-pool"
)

type zaiLoginSession struct {
	actor     string
	pollToken string
	flowID    string
	expiresAt time.Time
	status    string
	accountID string
	email     string
	err       string
}

var zaiLoginSessions = struct {
	sync.Mutex
	byID map[string]*zaiLoginSession
}{byID: make(map[string]*zaiLoginSession)}

type zaiEnvelope struct {
	Code    *int            `json:"code"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

func zaiRandomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (h *proxyHandler) zaiHTTPClient() (*http.Client, error) {
	if h.transport == nil {
		return nil, errors.New("Z.ai transport is not configured")
	}
	return &http.Client{
		Transport: h.transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func (h *proxyHandler) zaiRequest(ctx context.Context, method, target, authorization string, body any, output any) error {
	var content io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		content = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, content)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	client, err := h.zaiHTTPClient()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Z.ai %s returned HTTP %d", req.URL.Path, resp.StatusCode)
	}
	var envelope zaiEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("decode Z.ai %s: %w", req.URL.Path, err)
	}
	if envelope.Code == nil || (*envelope.Code != 0 && *envelope.Code != 200) || (envelope.Success != nil && !*envelope.Success) {
		return fmt.Errorf("Z.ai %s rejected request: %s", req.URL.Path, strings.TrimSpace(envelope.Msg))
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("Z.ai %s returned no data", req.URL.Path)
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return fmt.Errorf("decode Z.ai %s data: %w", req.URL.Path, err)
	}
	return nil
}

func (h *proxyHandler) handleZAILoginInit(w http.ResponseWriter, r *http.Request) {
	pollToken, err := zaiRandomHex(32)
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "could not create OAuth token")
		return
	}
	sessionID, err := zaiRandomHex(24)
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "could not create OAuth session")
		return
	}
	var data struct {
		FlowID          string `json:"flow_id"`
		PollToken       string `json:"poll_token"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"`
		PollIntervalSec int    `json:"poll_interval_sec"`
	}
	err = h.zaiRequest(r.Context(), http.MethodPost, zaiOAuthOrigin+"/api/v1/oauth/cli/init", "Bearer "+pollToken, map[string]string{"provider": "zai"}, &data)
	if err != nil {
		respondJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	authorize, parseErr := url.Parse(data.AuthorizeURL)
	redirect := "https://zcode.z.ai/api/v1/oauth/cli/callback/zai"
	if parseErr != nil || authorize.Scheme != "https" || authorize.Host != "chat.z.ai" || authorize.Path != "/api/oauth/authorize" || authorize.Query().Get("redirect_uri") != redirect || authorize.Query().Get("state") == "" || data.FlowID == "" || data.PollToken != pollToken || data.PollIntervalSec < 1 || data.PollIntervalSec > 10 || data.ExpiresAt <= time.Now().Unix() || data.ExpiresAt > time.Now().Add(10*time.Minute).Unix() {
		respondJSONError(w, http.StatusBadGateway, "Z.ai returned an invalid OAuth flow")
		return
	}
	actor := providerContributionActor(r)
	zaiLoginSessions.Lock()
	for id, session := range zaiLoginSessions.byID {
		if session.actor == actor && session.status == "exchanging" {
			zaiLoginSessions.Unlock()
			respondJSONError(w, http.StatusConflict, "Z.ai login is still being finalized")
			return
		}
		if session.status != "exchanging" && (session.actor == actor || time.Now().After(session.expiresAt.Add(time.Minute))) {
			delete(zaiLoginSessions.byID, id)
		}
	}
	zaiLoginSessions.byID[sessionID] = &zaiLoginSession{
		actor: actor, pollToken: pollToken, flowID: data.FlowID,
		expiresAt: time.Unix(data.ExpiresAt, 0), status: "pending",
	}
	zaiLoginSessions.Unlock()
	respondJSON(w, map[string]any{"session_id": sessionID, "oauth_url": data.AuthorizeURL, "poll_period": data.PollIntervalSec})
}

func (h *proxyHandler) handleZAILoginPoll(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SessionID == "" {
		respondJSONError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	zaiLoginSessions.Lock()
	session := zaiLoginSessions.byID[input.SessionID]
	if session == nil {
		zaiLoginSessions.Unlock()
		respondJSONError(w, http.StatusNotFound, "OAuth session not found")
		return
	}
	if session.actor != providerContributionActor(r) {
		zaiLoginSessions.Unlock()
		respondJSONError(w, http.StatusForbidden, "OAuth session belongs to another principal")
		return
	}
	if time.Now().After(session.expiresAt) && session.status == "pending" {
		session.status, session.err = "error", "Z.ai login expired"
	}
	if session.status != "pending" {
		result := zaiLoginStatus(session)
		zaiLoginSessions.Unlock()
		respondJSON(w, result)
		return
	}
	session.status = "exchanging"
	flowID, pollToken := session.flowID, session.pollToken
	zaiLoginSessions.Unlock()

	result, err := h.pollZAILogin(r, flowID, pollToken)
	zaiLoginSessions.Lock()
	if err != nil {
		session.status, session.err = "error", err.Error()
	} else if result != nil {
		session.status, session.accountID, session.email = "complete", result.accountID, result.email
	} else {
		session.status = "pending"
	}
	response := zaiLoginStatus(session)
	zaiLoginSessions.Unlock()
	respondJSON(w, response)
}

func zaiLoginStatus(session *zaiLoginSession) map[string]any {
	return map[string]any{"status": session.status, "account_id": session.accountID, "email": session.email, "error": session.err}
}

type zaiLoginResult struct{ accountID, email string }

func (h *proxyHandler) pollZAILogin(r *http.Request, flowID, pollToken string) (*zaiLoginResult, error) {
	ctx := r.Context()
	var data struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		Zai    struct {
			AccessToken string `json:"access_token"`
		} `json:"zai"`
		User struct {
			ID    string `json:"user_id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	target := zaiOAuthOrigin + "/api/v1/oauth/cli/poll/" + url.PathEscape(flowID)
	if err := h.zaiRequest(ctx, http.MethodGet, target, "Bearer "+pollToken, nil, &data); err != nil {
		return nil, err
	}
	switch data.Status {
	case "pending":
		return nil, nil
	case "failed":
		return nil, errors.New("Z.ai authorization was declined")
	case "ready":
	default:
		return nil, fmt.Errorf("invalid Z.ai OAuth status %q", data.Status)
	}
	if data.User.ID == "" || data.Token == "" || data.Zai.AccessToken == "" {
		return nil, errors.New("Z.ai OAuth response is missing account credentials")
	}
	var business struct {
		AccessToken string `json:"access_token"`
	}
	if err := h.zaiRequest(ctx, http.MethodPost, zaiAPIOrigin+"/api/auth/z/login", "", map[string]string{"token": data.Zai.AccessToken}, &business); err != nil {
		return nil, err
	}
	if business.AccessToken == "" {
		return nil, errors.New("Z.ai business login returned no access token")
	}
	apiKey, err := h.zaiAccountAPIKey(ctx, business.AccessToken)
	if err != nil {
		return nil, err
	}
	if err := h.validateZAICodingPlan(ctx, apiKey); err != nil {
		return nil, err
	}
	if err := h.validateZAICodingPlanKey(ctx, apiKey); err != nil {
		return nil, err
	}
	email := strings.TrimSpace(data.User.Email)
	label := "Z.ai Coding Plan"
	if email != "" {
		label = "Z.ai (" + email + ")"
	}
	linkedAt := time.Now().UTC().Format(time.RFC3339Nano)
	accountID, err := h.saveContribution(r, AccountTypeZAI, data.User.ID, ZAIAuthJSON{
		APIKey: apiKey, AuthType: "oauth", UserID: data.User.ID, Email: email,
		Label: label, PlanType: "coding_plan", BusinessToken: business.AccessToken,
		ZCodeJWT: data.Token, LinkedAt: linkedAt, AddedAt: linkedAt,
	})
	if err != nil {
		return nil, err
	}
	return &zaiLoginResult{accountID: accountID, email: email}, nil
}

func (h *proxyHandler) zaiAccountAPIKey(ctx context.Context, businessToken string) (string, error) {
	var customer struct {
		Organizations []struct {
			ID       string `json:"organizationId"`
			Projects []struct {
				ID   string `json:"projectId"`
				Type any    `json:"projectType"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	if err := h.zaiRequest(ctx, http.MethodGet, zaiAPIOrigin+"/api/biz/customer/getCustomerInfo", "Bearer "+businessToken, nil, &customer); err != nil {
		return "", err
	}
	var projects []string
	for _, org := range customer.Organizations {
		for _, project := range org.Projects {
			switch fmt.Sprint(project.Type) {
			case "2":
				continue
			case "1":
			default:
				return "", fmt.Errorf("Z.ai returned unknown project type for project %q", project.ID)
			}
			if org.ID == "" || project.ID == "" {
				return "", errors.New("Z.ai returned a personal project without an ID")
			}
			projects = append(projects, zaiAPIOrigin+"/api/biz/v1/organization/"+url.PathEscape(org.ID)+"/projects/"+url.PathEscape(project.ID)+"/api_keys")
		}
	}
	if len(projects) != 1 {
		return "", fmt.Errorf("Z.ai account has %d personal projects; exactly one is required for OAuth linking", len(projects))
	}
	keysURL := projects[0]
	var keys []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
	}
	if err := h.zaiRequest(ctx, http.MethodGet, keysURL, "Bearer "+businessToken, nil, &keys); err != nil {
		return "", err
	}
	var keyID string
	for _, key := range keys {
		if key.Name == zaiKeyName {
			if keyID != "" || key.APIKey == "" {
				return "", errors.New("Z.ai returned ambiguous codex-pool API keys")
			}
			keyID = key.APIKey
		}
	}
	if keyID == "" {
		var created struct {
			APIKey string `json:"apiKey"`
		}
		if err := h.zaiRequest(ctx, http.MethodPost, keysURL, "Bearer "+businessToken, map[string]string{"name": zaiKeyName}, &created); err != nil {
			return "", err
		}
		keyID = created.APIKey
	}
	if keyID == "" {
		return "", errors.New("Z.ai did not return an API key ID")
	}
	var secret struct {
		SecretKey string `json:"secretKey"`
	}
	if err := h.zaiRequest(ctx, http.MethodGet, keysURL+"/copy/"+url.PathEscape(keyID), "Bearer "+businessToken, nil, &secret); err != nil {
		return "", err
	}
	if secret.SecretKey == "" {
		return "", errors.New("Z.ai did not return an API key secret")
	}
	return keyID + "." + secret.SecretKey, nil
}

func (h *proxyHandler) validateZAICodingPlan(ctx context.Context, apiKey string) error {
	var subscriptions []struct {
		ProductID       string `json:"productId"`
		ProductName     string `json:"productName"`
		Status          string `json:"status"`
		InCurrentPeriod *bool  `json:"inCurrentPeriod"`
	}
	if err := h.zaiRequest(ctx, http.MethodGet, zaiAPIOrigin+"/api/biz/subscription/list", apiKey, nil, &subscriptions); err != nil {
		return err
	}
	active := false
	for _, subscription := range subscriptions {
		coding := strings.Contains(strings.ToLower(subscription.ProductID), "coding") || strings.Contains(strings.ToLower(subscription.ProductName), "coding")
		if coding && subscription.Status == "VALID" && subscription.InCurrentPeriod != nil && *subscription.InCurrentPeriod {
			active = true
		}
	}
	if !active {
		return errors.New("Z.ai account has no active Individual Coding Plan")
	}
	return nil
}

func (h *proxyHandler) validateZAICodingPlanKey(ctx context.Context, apiKey string) error {
	requestBody := []byte(`{"model":"glm-5.3","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.zaiBase.String()+"/v1/messages", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", ccAnthropicVersion)
	client, err := h.zaiHTTPClient()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Z.ai Coding Plan model request returned HTTP %d", resp.StatusCode)
	}
	return nil
}
