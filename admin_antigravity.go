package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"codex-pool-proxy/internal/clientip"
	"codex-pool-proxy/internal/ids"
	"codex-pool-proxy/internal/routing"
)

const (
	antigravityOAuthAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	antigravityOAuthCallbackURL  = "http://localhost:51121/oauth-callback"
)

// antigravityAccountBoundVerificationURL rebinds an upstream Google verification
// link to the pool account e-mail. Google otherwise continues the flow with
// whichever account the browser session already has, which silently verifies the
// wrong identity. accounts.google.com/AccountChooser honours Email= by selecting
// the matching session account, or by pre-filling the identifier form.
func antigravityAccountBoundVerificationURL(rawURL, email string) string {
	raw := strings.TrimSpace(rawURL)
	email = strings.TrimSpace(email)
	if raw == "" || email == "" {
		return raw
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" {
		return raw
	}
	host := strings.ToLower(target.Hostname())
	if host != "accounts.google.com" && host != "accounts.google.co.uk" && !strings.HasSuffix(host, ".google.com") {
		return raw
	}
	chooser := &url.URL{Scheme: "https", Host: "accounts.google.com", Path: "/AccountChooser"}
	query := chooser.Query()
	query.Set("Email", email)
	query.Set("continue", raw)
	query.Set("oauth", "1")
	query.Set("scc", "1")
	chooser.RawQuery = query.Encode()
	return chooser.String()
}

var antigravityOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

type antigravityOAuthSession struct {
	ActorID          string
	ID               string
	State            string
	Verifier         string
	CreatedAt        time.Time
	Status           string
	AccountID        string
	ReloginAccountID string
	Error            string
	RedirectURI      string
	TargetOrigin     string
}

type antigravityOAuthSessionStore struct {
	mu      sync.Mutex
	byID    map[string]*antigravityOAuthSession
	byState map[string]*antigravityOAuthSession
}

func newAntigravityOAuthSession() (*antigravityOAuthSession, error) {
	random := func(size int) (string, error) {
		buffer := make([]byte, size)
		if _, err := rand.Read(buffer); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(buffer), nil
	}
	id, err := random(24)
	if err != nil {
		return nil, err
	}
	state, err := random(32)
	if err != nil {
		return nil, err
	}
	verifier, err := random(48)
	if err != nil {
		return nil, err
	}
	return &antigravityOAuthSession{ID: id, State: state, Verifier: verifier, CreatedAt: time.Now(), Status: "pending"}, nil
}

func antigravityOAuthRedirectURI() string {
	if value := strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_REDIRECT_URI")); value != "" {
		return value
	}
	return antigravityOAuthCallbackURL
}

func (h *proxyHandler) startAntigravityOAuth(w http.ResponseWriter, r *http.Request, reloginAccountID string) {
	clientID := antigravityOAuthClientID()
	if clientID == "" {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity OAuth is not configured.")
		return
	}
	redirectURI := antigravityOAuthRedirectURI()
	session, err := newAntigravityOAuthSession()
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "failed to create OAuth session")
		return
	}
	session.ActorID = providerContributionActor(r)
	session.ReloginAccountID = strings.TrimSpace(reloginAccountID)
	loginHint := ""
	if session.ReloginAccountID != "" {
		for _, account := range h.pool.allAccounts() {
			if account.ID != session.ReloginAccountID {
				continue
			}
			account.mu.Lock()
			loginHint = strings.TrimSpace(account.Email)
			account.mu.Unlock()
			break
		}
	}
	session.RedirectURI = redirectURI
	session.TargetOrigin, err = antigravityOAuthTargetOrigin(r, h)
	if err != nil {
		respondJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	challenge := sha256.Sum256([]byte(session.Verifier))
	u, _ := url.Parse(antigravityOAuthAuthorizeURL)
	query := u.Query()
	query.Set("response_type", "code")
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", strings.Join(antigravityOAuthScopes, " "))
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	if loginHint != "" {
		query.Set("login_hint", loginHint)
	}
	query.Set("state", session.State)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	u.RawQuery = query.Encode()
	sessions := &h.antigravityOAuth
	sessions.mu.Lock()
	if sessions.byID == nil {
		sessions.byID = make(map[string]*antigravityOAuthSession)
		sessions.byState = make(map[string]*antigravityOAuthSession)
	}
	sessions.byID[session.ID] = session
	sessions.byState[session.State] = session
	for id, candidate := range sessions.byID {
		if time.Since(candidate.CreatedAt) > 30*time.Minute {
			delete(sessions.byID, id)
			delete(sessions.byState, candidate.State)
		}
	}
	sessions.mu.Unlock()
	callbackMode := "public"
	if isAntigravityLoopbackRedirect(redirectURI) {
		callbackMode = "manual"
		if err := ensureAntigravityLoopbackListener(h, redirectURI); err != nil {
			respondJSONError(w, 503, "could not start OAuth callback listener")
			return
		}
		callbackMode = "automatic"
	}
	respondJSON(w, map[string]any{"oauth_url": u.String(), "session_id": session.ID, "state": session.State, "callback_mode": callbackMode, "replaced": session.ReloginAccountID != "", "login_hint": loginHint != ""})
}

func (h *proxyHandler) handleAntigravityAdd(w http.ResponseWriter, r *http.Request) {
	if !h.checkContributionAttempt(w, r) {
		return
	}
	h.startAntigravityOAuth(w, r, "")
}

func (h *proxyHandler) handleAntigravityRelogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	accountID := strings.TrimSpace(input.AccountID)
	if accountID == "" {
		respondJSONError(w, http.StatusBadRequest, "account_id is required")
		return
	}
	var target *Account
	for _, account := range h.pool.allAccounts() {
		if account.ID == accountID {
			target = account
			break
		}
	}
	if target == nil {
		respondJSONError(w, http.StatusNotFound, "Antigravity account not found")
		return
	}
	if target.Type != AccountTypeAntigravity {
		respondJSONError(w, http.StatusBadRequest, "selected account is not Antigravity")
		return
	}
	h.startAntigravityOAuth(w, r, accountID)
}

func (h *proxyHandler) handleAntigravityStatus(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	h.antigravityOAuth.mu.Lock()
	session := h.antigravityOAuth.byID[input.SessionID]
	var status, accountID, sessionError string
	if session != nil {
		status, accountID, sessionError = session.Status, session.AccountID, session.Error
	}
	h.antigravityOAuth.mu.Unlock()
	if session == nil || time.Since(session.CreatedAt) > 30*time.Minute {
		respondJSONError(w, http.StatusNotFound, "OAuth session expired")
		return
	}
	if session.ActorID != providerContributionActor(r) {
		respondJSONError(w, http.StatusForbidden, "OAuth session belongs to another principal")
		return
	}
	respondJSON(w, map[string]any{"status": status, "account_id": accountID, "error": sessionError})
}

func (h *proxyHandler) handleAntigravityExchange(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID   string `json:"session_id"`
		Code        string `json:"code"`
		CallbackURL string `json:"callback_url"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	h.antigravityOAuth.mu.Lock()
	session := h.antigravityOAuth.byID[input.SessionID]
	h.antigravityOAuth.mu.Unlock()
	if session == nil || time.Since(session.CreatedAt) > 30*time.Minute {
		respondJSONError(w, http.StatusBadRequest, "invalid or expired OAuth session")
		return
	}
	if session.ReloginAccountID != "" && !strings.HasPrefix(r.URL.Path, "/admin/antigravity/") {
		respondJSONError(w, http.StatusForbidden, "relogin session requires operator access")
		return
	}
	if session.ActorID != providerContributionActor(r) {
		respondJSONError(w, http.StatusForbidden, "OAuth session belongs to another principal")
		return
	}
	code := strings.TrimSpace(input.Code)
	if strings.TrimSpace(input.CallbackURL) != "" {
		callback, err := url.Parse(strings.TrimSpace(input.CallbackURL))
		if err != nil || callback.Query().Get("state") != session.State {
			respondJSONError(w, http.StatusBadRequest, "callback state does not match the OAuth session")
			return
		}
		code = callback.Query().Get("code")
	} else if strings.TrimSpace(input.State) != session.State {
		respondJSONError(w, http.StatusBadRequest, "state is required when submitting a raw authorization code")
		return
	}
	if code == "" {
		respondJSONError(w, http.StatusBadRequest, "authorization code is required")
		return
	}
	accountID, err := h.completeAntigravityOAuth(r.Context(), session, code)
	if err != nil {
		respondJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	respondJSON(w, map[string]any{"success": true, "account_id": accountID})
}

func (h *proxyHandler) handleAntigravityCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	h.antigravityOAuth.mu.Lock()
	session := h.antigravityOAuth.byState[state]
	h.antigravityOAuth.mu.Unlock()
	if session == nil || time.Since(session.CreatedAt) > 30*time.Minute {
		h.renderAntigravityCallback(w, nil, "error", "", "invalid or expired OAuth state")
		return
	}
	if upstreamError := strings.TrimSpace(r.URL.Query().Get("error")); upstreamError != "" {
		h.antigravityOAuth.mu.Lock()
		session.Status, session.Error = "error", upstreamError
		h.antigravityOAuth.mu.Unlock()
		h.renderAntigravityCallback(w, session, "error", "", upstreamError)
		return
	}
	accountID, err := h.completeAntigravityOAuth(r.Context(), session, strings.TrimSpace(r.URL.Query().Get("code")))
	if err != nil {
		h.renderAntigravityCallback(w, session, "error", "", err.Error())
		return
	}
	h.renderAntigravityCallback(w, session, "complete", accountID, "")
}

func (h *proxyHandler) renderAntigravityCallback(w http.ResponseWriter, session *antigravityOAuthSession, status, accountID, message string) {
	sessionID, targetOrigin := "", "*"
	if session != nil {
		sessionID = session.ID
		if session.TargetOrigin != "" {
			targetOrigin = session.TargetOrigin
		}
	}
	payload, _ := json.Marshal(map[string]string{
		"type": "codex-pool-antigravity-oauth", "session_id": sessionID,
		"status": status, "account_id": accountID, "error": message,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	encodedPayload := base64.StdEncoding.EncodeToString(payload)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Antigravity sign-in</title><p>%s</p><script>const payload=JSON.parse(atob(%q));if(window.opener){window.opener.postMessage(payload,%q)}window.close()</script>`, template.HTMLEscapeString(status), encodedPayload, targetOrigin)
}

type antigravityLoopbackListeners struct {
	mu        sync.Mutex
	listeners map[string]net.Listener
}

func isAntigravityLoopbackRedirect(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	return parsed.Scheme == "http" && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1")
}

func antigravityOAuthTargetOrigin(r *http.Request, h *proxyHandler) (string, error) {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if parsed, err := url.Parse(origin); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/") {
			return parsed.Scheme + "://" + parsed.Host, nil
		}
		return "", errors.New("invalid OAuth request Origin")
	}
	if h == nil {
		return "", errors.New("OAuth public origin unavailable")
	}
	base, err := h.getEffectivePublicURL(r)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return "", errors.New("invalid OAuth public origin")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func ensureAntigravityLoopbackListener(h *proxyHandler, rawRedirect string) error {
	parsed, err := url.Parse(rawRedirect)
	if err != nil || !isAntigravityLoopbackRedirect(rawRedirect) || parsed.Port() == "" {
		return errors.New("Antigravity OAuth redirect is not a loopback URL with a port")
	}
	key := parsed.Host
	loopback := &h.antigravityLoopback
	loopback.mu.Lock()
	defer loopback.mu.Unlock()
	if loopback.listeners == nil {
		loopback.listeners = make(map[string]net.Listener)
	}
	if loopback.listeners[key] != nil {
		return nil
	}
	address := net.JoinHostPort("127.0.0.1", parsed.Port())
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(parsed.Path, h.handleAntigravityCallback)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	loopback.listeners[key] = listener
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			// The status poll and manual callback paste remain available if the listener exits.
		}
		loopback.mu.Lock()
		delete(loopback.listeners, key)
		loopback.mu.Unlock()
	}()
	return nil
}

type antigravityTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

func antigravityValidationModel(snapshot AntigravityAccountSnapshot) string {
	if _, ok := snapshot.Models["gemini-3.8-flash-high"]; ok {
		return "gemini-3.8-flash-high"
	}
	model := ""
	for candidate := range snapshot.Models {
		if model == "" || candidate < model {
			model = candidate
		}
	}
	return model
}

func (h *proxyHandler) validateAntigravityRelogin(ctx context.Context, target, account *Account, provider *AntigravityProvider, snapshot AntigravityAccountSnapshot) (bool, error) {
	model := antigravityValidationModel(snapshot)
	if model == "" {
		return false, errors.New("Antigravity revalidation found no usable model")
	}
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"Reply with OK."}]}]}`)
	prepared, err := prepareAntigravityRequest(h.pool.antigravity, "/v1beta/models/"+model+":streamGenerateContent", body, "antigravity/"+model, account.google.ProjectID, "")
	if err != nil {
		return false, err
	}
	validationCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	resp, err := h.doAntigravityRequest(validationCtx, nil, account, provider, prepared)
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, errors.New("Antigravity revalidation returned no response")
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return false, readErr
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	providerErr := classifyAntigravityError(resp.StatusCode, responseBody)
	if providerErr.Class == ProviderErrorQuota {
		return true, nil
	}
	if providerErr.Class == ProviderErrorContext || resp.StatusCode == http.StatusForbidden {
		h.persistAntigravityReloginFailure(target, responseBody)
	}
	return false, fmt.Errorf("Antigravity revalidation failed (%s): %s", providerErr.Class, antigravityErrorSummary(responseBody))
}

func (h *proxyHandler) persistAntigravityReloginFailure(target *Account, responseBody []byte) {
	if target == nil {
		return
	}
	needsVerification, banned, verificationURL := classifyAntigravityForbidden(responseBody)
	if !needsVerification && !banned {
		return
	}
	target.mu.Lock()
	target.NeedsVerification = needsVerification
	target.VerificationURL = verificationURL
	target.HealthError = string(responseBody)
	if banned {
		target.Dead = true
	}
	file := target.File
	target.mu.Unlock()
	if err := saveAntigravityAccount(h.pool.antigravity, target); err != nil {
		log.Printf("warning: failed to persist Antigravity relogin failure for %s: %v", file, err)
	}
}

func (h *proxyHandler) completeAntigravityOAuth(ctx context.Context, session *antigravityOAuthSession, code string) (string, error) {
	if code == "" {
		return "", errors.New("Google callback did not contain an authorization code")
	}
	h.antigravityOAuth.mu.Lock()
	if session.Status == "complete" {
		accountID := session.AccountID
		h.antigravityOAuth.mu.Unlock()
		return accountID, nil
	}
	if session.Status != "pending" || time.Since(session.CreatedAt) > 30*time.Minute {
		h.antigravityOAuth.mu.Unlock()
		return "", errors.New("OAuth session is already being exchanged")
	}
	session.Status = "exchanging"
	h.antigravityOAuth.mu.Unlock()

	fail := func(err error) (string, error) {
		h.antigravityOAuth.mu.Lock()
		session.Status, session.Error = "error", err.Error()
		h.antigravityOAuth.mu.Unlock()
		return "", err
	}
	token, err := h.exchangeAntigravityCode(ctx, code, session.Verifier, session.RedirectURI)
	if err != nil {
		return fail(err)
	}
	email, err := h.fetchAntigravityEmail(ctx, token.AccessToken)
	if err != nil {
		return fail(err)
	}
	projectID, planType, err := h.discoverAntigravityProject(ctx, token.AccessToken)
	if err != nil {
		return fail(err)
	}
	accountID := safeAntigravityAccountID(email)
	if token.ExpiresIn <= 0 {
		token.ExpiresIn = 3600
	}
	file := filepath.Join(h.cfg.poolDir, "antigravity", accountID+".json")
	account := &Account{
		Type: AccountTypeAntigravity, ID: accountID, File: file, Email: email, vault: h.cfg.vault,
		google: googleAccountState{ProjectID: projectID, ModelRateLimits: make(map[string]time.Time)}, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		PlanType: planType, ExpiresAt: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second),
		LastRefresh: time.Now().UTC(), AddedAt: time.Now().UTC(),
	}
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		return fail(errors.New("Antigravity provider is not configured"))
	}
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	snapshot, err := fetchAntigravityModels(syncCtx, h.pool.antigravity, h.transport, account, provider.DailyURL(), provider.ProductionURL())
	cancel()
	if err != nil {
		return fail(fmt.Errorf("model discovery failed: %w", err))
	}
	if session.ReloginAccountID != "" {
		var target *Account
		for _, candidate := range h.pool.allAccounts() {
			if candidate.Type == AccountTypeAntigravity && candidate.ID == session.ReloginAccountID {
				target = candidate
				break
			}
		}
		if target == nil {
			return fail(fmt.Errorf("Antigravity account %q not found", session.ReloginAccountID))
		}
		if !antigravityReloginMatchesTarget(target, accountID, email) {
			return fail(fmt.Errorf("signed-in Antigravity account does not match pool account %s", session.ReloginAccountID))
		}
		quotaLimited, err := h.validateAntigravityRelogin(ctx, target, account, provider, snapshot)
		if err != nil {
			return fail(err)
		}
		if quotaLimited {
			log.Printf("Antigravity relogin validated account %s with quota cooldown", target.ID)
		}
		if err := h.replaceAntigravityAccountCredentials(target, token, email, projectID, planType, snapshot); err != nil {
			return fail(err)
		}
		h.reloadAccounts()
		if h.passport != nil {
			if err := h.passport.recordAudit(session.ActorID, "provider.account_relogin", target.ID, "antigravity"); err != nil {
				log.Printf("record provider relogin audit: %v", err)
			}
		}
		h.antigravityOAuth.mu.Lock()
		session.Status, session.AccountID, session.Error = "complete", target.ID, ""
		h.antigravityOAuth.mu.Unlock()
		return target.ID, nil
	}
	accountID, err = h.persistContribution(session.ActorID, AccountTypeAntigravity, contributionIdentity{Value: email}, func(path string) error {
		account.File = path
		account.ID = strings.TrimSuffix(filepath.Base(path), ".json")
		h.pool.antigravity.models.ReplaceAccount(account.ID, snapshot)
		return saveAntigravityAccount(h.pool.antigravity, account)
	})
	if err != nil {
		return fail(err)
	}

	h.antigravityOAuth.mu.Lock()
	session.Status, session.AccountID, session.Error = "complete", accountID, ""
	h.antigravityOAuth.mu.Unlock()
	return accountID, nil
}

func (h *proxyHandler) replaceAntigravityAccountCredentials(target *Account, token antigravityTokenResponse, email, projectID, planType string, snapshot AntigravityAccountSnapshot) error {
	if target == nil || target.Type != AccountTypeAntigravity || strings.TrimSpace(target.File) == "" {
		return errors.New("Antigravity account file is unavailable")
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(maxInt64(token.ExpiresIn, 3600)) * time.Second)
	target.mu.Lock()
	target.AccessToken = token.AccessToken
	target.RefreshToken = token.RefreshToken
	target.Email = email
	target.google.ProjectID = projectID
	if strings.TrimSpace(planType) != "" {
		target.PlanType = planType
	}
	target.ExpiresAt = expiresAt
	target.LastRefresh = now
	target.Dead = false
	target.NeedsLogin = false
	target.AuthError = ""
	target.placeholder = false
	target.NeedsVerification = false
	target.VerificationURL = ""
	target.HealthError = ""
	target.mu.Unlock()
	h.pool.antigravity.models.ReplaceAccount(target.ID, snapshot)
	if err := saveAntigravityAccount(h.pool.antigravity, target); err != nil {
		return fmt.Errorf("save Antigravity account: %w", err)
	}
	return nil
}

func maxInt64(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

func (h *proxyHandler) exchangeAntigravityCode(ctx context.Context, code, verifier, redirectURI string) (antigravityTokenResponse, error) {
	clientID := antigravityOAuthClientID()
	if clientID == "" {
		return antigravityTokenResponse{}, errors.New("antigravity OAuth client ID is not configured")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code":          {code},
		"code_verifier": {verifier},
	}
	if secret := antigravityOAuthClientSecret(); secret != "" {
		form.Set("client_secret", secret)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, antigravityOAuthTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return antigravityTokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return antigravityTokenResponse{}, fmt.Errorf("Google token exchange failed: %s: %s", resp.Status, safeText(body))
	}
	var token antigravityTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return token, err
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return token, errors.New("Google token exchange did not return access and refresh tokens")
	}
	return token, nil
}

func (h *proxyHandler) fetchAntigravityEmail(ctx context.Context, accessToken string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var profile struct {
		Email string `json:"email"`
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile) != nil || profile.Email == "" {
		return "", fmt.Errorf("Google userinfo failed: %s", resp.Status)
	}
	return profile.Email, nil
}

func (h *proxyHandler) discoverAntigravityProject(ctx context.Context, accessToken string) (string, string, error) {
	payload := map[string]any{"metadata": map[string]any{"ideType": "ANTIGRAVITY"}}
	root, err := h.callAntigravityInternal(ctx, accessToken, h.cfg.antigravityProdBase, "/v1internal:loadCodeAssist", payload)
	if err != nil {
		return "", "", err
	}
	projectID := antigravityLoadProjectID(root)
	plan := findAntigravityTierID(root)
	if projectID != "" {
		return projectID, plan, nil
	}
	tierID := findDefaultAntigravityTier(root)
	if tierID == "" {
		return "", "", errors.New("Google did not return a project or an allowed onboarding tier")
	}
	for attempt := 0; attempt < 5; attempt++ {
		onboard := map[string]any{
			"tier_id": tierID,
			"metadata": map[string]any{
				"ide_type": "ANTIGRAVITY", "ide_name": "antigravity", "ide_version": antigravityClientVersion(h.pool.antigravity),
			},
		}
		result, callErr := h.callAntigravityInternal(ctx, accessToken, h.cfg.antigravityOnboardBase, "/v1internal:onboardUser", onboard)
		if callErr == nil {
			if projectID = antigravityOnboardProjectID(result); projectID != "" {
				return projectID, tierID, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", "", errors.New("Google Antigravity project onboarding did not complete")
}

func (h *proxyHandler) callAntigravityInternal(ctx context.Context, accessToken string, base *url.URL, path string, payload any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	u := *base
	u.Path = singleJoin(u.Path, path)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	if strings.HasSuffix(path, ":onboardUser") {
		req.Header.Set("User-Agent", antigravityOnboardUserAgent(h.pool.antigravity))
		req.Header.Set("X-Goog-Api-Client", "gl-node/22.21.1")
	} else {
		req.Header.Set("User-Agent", antigravityUserAgent(h.pool.antigravity))
	}
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s failed: %s: %s", path, resp.Status, safeText(responseBody))
	}
	var result map[string]any
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func antigravityLoadProjectID(root map[string]any) string {
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch value := root[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case map[string]any:
			if id, _ := value["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

func antigravityOnboardProjectID(root map[string]any) string {
	done, _ := root["done"].(bool)
	if !done {
		return ""
	}
	response, _ := root["response"].(map[string]any)
	return antigravityLoadProjectID(response)
}

func findDefaultAntigravityTier(root map[string]any) string {
	items, _ := root["allowedTiers"].([]any)
	for _, item := range items {
		tier, _ := item.(map[string]any)
		if preferred, _ := tier["isDefault"].(bool); preferred {
			if id, _ := tier["id"].(string); id != "" {
				return id
			}
		}
	}
	if current, ok := root["currentTier"].(map[string]any); ok {
		if id, _ := current["id"].(string); id != "" {
			return id
		}
	}
	return "free-tier"
}

func findAntigravityTierID(root map[string]any) string {
	for _, key := range []string{"paidTier", "currentTier"} {
		if tier, ok := root[key].(map[string]any); ok {
			if id, _ := tier["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

func (h *proxyHandler) checkAntigravityPlan(ctx context.Context, account *Account) error {
	account.mu.Lock()
	accessToken := account.AccessToken
	account.mu.Unlock()
	root, err := h.callAntigravityInternal(ctx, accessToken, h.cfg.antigravityProdBase, "/v1internal:loadCodeAssist", map[string]any{"metadata": map[string]any{"ideType": "ANTIGRAVITY"}})
	if err != nil {
		return err
	}
	plan := findAntigravityTierID(root)
	if plan == "" {
		return errors.New("loadCodeAssist returned no tier")
	}
	updateAccountPlan(h.pool.antigravity, account, plan)
	return nil
}

var antigravityAccountIDPattern = regexp.MustCompile(`[^a-z0-9._-]+`)

// antigravityReloginMatchesTarget reports whether a finished sign-in belongs
// to the account its relogin session was bound to. Accounts contributed
// through Passport are named by a fingerprint of the email instead of the
// email itself, so the recorded email is the identity both naming schemes
// share. An account imported without credentials carries that email too.
func antigravityReloginMatchesTarget(target *Account, signedInID, email string) bool {
	if target == nil {
		return false
	}
	if signedInID == target.ID {
		return true
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	recorded := strings.TrimSpace(target.Email)
	return recorded != "" && strings.EqualFold(recorded, strings.TrimSpace(email))
}

func safeAntigravityAccountID(email string) string {
	id := antigravityAccountIDPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(email)), "-")
	id = strings.Trim(id, "-.")
	if id == "" {
		return "antigravity-" + ids.Random()
	}
	return "antigravity-" + id
}

func (h *proxyHandler) handleAntigravityModelSync(w http.ResponseWriter, r *http.Request) {
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity provider is not configured")
		return
	}
	type result struct {
		AccountID string `json:"account_id"`
		Error     string `json:"error,omitempty"`
	}
	results := make([]result, 0)
	for _, account := range h.pool.allAccounts() {
		if account.Type != AccountTypeAntigravity {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err := syncAntigravityModels(ctx, h.pool.antigravity, h.transport, account, provider.DailyURL(), provider.ProductionURL())
		cancel()
		entry := result{AccountID: account.ID}
		if err != nil {
			entry.Error = err.Error()
		}
		results = append(results, entry)
	}
	respondJSON(w, map[string]any{"accounts": results, "models": h.pool.antigravity.models.Models(h.pool)})
}

func (h *proxyHandler) handleAntigravityModelVerify(w http.ResponseWriter, r *http.Request) {
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity provider is not configured")
		return
	}
	type verification struct {
		Model     string `json:"model"`
		AccountID string `json:"account_id,omitempty"`
		Status    int    `json:"status"`
		LatencyMS int64  `json:"latency_ms"`
		Error     string `json:"error,omitempty"`
	}
	requested := strings.TrimSpace(r.URL.Query().Get("model"))
	models := h.pool.antigravity.models.Models(h.pool)
	results := make([]verification, 0, len(models))
	for _, model := range models {
		if requested != "" && requested != model.ID && requested != "antigravity/"+model.ID {
			continue
		}
		entry := verification{Model: model.ID}
		account := h.pool.selectRoute(routing.Request{
			Mode: routing.MatchModelFamily, Provider: string(AccountTypeAntigravity),
			Model: model.ID, ClientIP: clientip.Get(r), Profile: string(RoutingLegacy),
		}).Account
		if account == nil {
			entry.Error = "no available account supports this model"
			results = append(results, entry)
			continue
		}
		entry.AccountID = account.ID
		account.mu.Lock()
		projectID := account.google.ProjectID
		account.mu.Unlock()
		requestBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"Reply with OK."}]}]}`)
		prepared, err := prepareAntigravityRequest(h.pool.antigravity, "/v1beta/models/"+model.ID+":generateContent", requestBody, "antigravity/"+model.ID, projectID, "")
		if err != nil {
			entry.Error = err.Error()
			results = append(results, entry)
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		started := time.Now()
		resp, err := h.doAntigravityRequest(ctx, nil, account, provider, prepared)
		entry.LatencyMS = time.Since(started).Milliseconds()
		if err != nil {
			entry.Error = err.Error()
			cancel()
			results = append(results, entry)
			continue
		}
		entry.Status = resp.StatusCode
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_, _, err = collectAntigravitySSE(resp.Body)
			clearAntigravityModelCooldown(h.pool.antigravity, account, model.ID)
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			entry.Error = strings.TrimSpace(string(body))
			if resp.StatusCode == http.StatusTooManyRequests {
				if until, ok := parseAntigravityRetry(body, time.Now()); ok {
					setAntigravityModelCooldown(h.pool.antigravity, account, model.ID, until)
				}
			}
		}
		resp.Body.Close()
		cancel()
		if err != nil {
			entry.Error = err.Error()
		}
		results = append(results, entry)
	}
	respondJSON(w, map[string]any{"verified_at": time.Now().UTC(), "models": results})
}
