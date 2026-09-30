package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Codex OAuth constants (from codex-rs/login/src/server.rs)

const (
	CodexOAuthClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	CodexOAuthTokenURL     = "https://auth.openai.com/oauth/token"
	CodexOAuthAuthorizeURL = "https://auth.openai.com/oauth/authorize"
)

// CodexOAuthSession stores pending OAuth state
type CodexOAuthSession struct {
	ActorID   string
	AccountID string
	Verifier  string
	Challenge string
	State     string
	// ReloginAccountID, when set, binds this session to an existing pool
	// account: the exchange replaces that account's credentials instead of
	// creating a new account file.
	ReloginAccountID string
	CreatedAt        time.Time
}

// In-memory store for pending Codex OAuth sessions
var codexOAuthSessions = struct {
	sync.RWMutex
	sessions map[string]*CodexOAuthSession
}{sessions: make(map[string]*CodexOAuthSession)}

// CodexTokenResponse is the response from the token endpoint
type CodexTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// serveCodexAdmin routes Codex admin requests
func (h *proxyHandler) serveCodexAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/codex")
	if path == "" {
		path = "/"
	}

	switch {
	case path == "/" && r.Method == http.MethodGet:
		h.handleCodexList(w, r)

	case path == "/add" && r.Method == http.MethodPost:
		h.handleCodexAdd(w, r)

	case path == "/relogin" && r.Method == http.MethodPost:
		h.handleCodexRelogin(w, r)

	case path == "/exchange" && r.Method == http.MethodPost:
		h.handleCodexExchange(w, r)

	default:
		http.NotFound(w, r)
	}
}

// GET /admin/codex - list all Codex accounts
func (h *proxyHandler) handleCodexList(w http.ResponseWriter, r *http.Request) {
	accounts := h.requestVisiblePool(r).allAccounts()

	type accountInfo struct {
		ID          string    `json:"id"`
		PlanType    string    `json:"plan_type"`
		Dead        bool      `json:"dead"`
		Disabled    bool      `json:"disabled"`
		CyberAccess bool      `json:"cyber_access,omitempty"`
		ExpiresAt   time.Time `json:"expires_at,omitempty"`
		LastRefresh time.Time `json:"last_refresh,omitempty"`
	}

	var result []accountInfo
	for _, acc := range accounts {
		if acc.Type == AccountTypeCodex {
			result = append(result, accountInfo{
				ID:          acc.ID,
				PlanType:    acc.PlanType,
				Dead:        acc.Dead,
				Disabled:    acc.Disabled,
				CyberAccess: acc.CyberAccess,
				ExpiresAt:   acc.ExpiresAt,
				LastRefresh: acc.LastRefresh,
			})
		}
	}

	respondJSON(w, map[string]any{
		"accounts": result,
		"count":    len(result),
	})
}

// startCodexOAuthSession builds the PKCE pair, the authorize URL and stores
// the pending session. actor binds the session to the contributing principal
// (empty disables the binding); reloginAccountID binds it to an existing
// account (see handleCodexRelogin).
func startCodexOAuthSession(actor, reloginAccountID string) (oauthURL, verifier, state string) {
	// Generate PKCE verifier and challenge
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", ""
	}
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)

	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	// Generate state
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", "", ""
	}
	state = base64.RawURLEncoding.EncodeToString(stateBytes)

	// Build OAuth URL
	u, _ := url.Parse(CodexOAuthAuthorizeURL)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", CodexOAuthClientID)
	q.Set("redirect_uri", CodexOAuthRedirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "codex_cli_rs")
	u.RawQuery = q.Encode()

	// Store session
	session := &CodexOAuthSession{
		ActorID:          actor,
		ReloginAccountID: reloginAccountID,
		Verifier:         verifier,
		Challenge:        challenge,
		State:            state,
		CreatedAt:        time.Now(),
	}

	codexOAuthSessions.Lock()
	codexOAuthSessions.sessions[verifier] = session
	codexOAuthSessions.Unlock()

	// Clean up old sessions
	go cleanupOldCodexSessions()

	return u.String(), verifier, state
}

// POST /admin/codex/add - start OAuth flow
func (h *proxyHandler) handleCodexAdd(w http.ResponseWriter, r *http.Request) {
	oauthURL, verifier, state := startCodexOAuthSession(providerContributionActor(r), "")
	if oauthURL == "" {
		respondJSONError(w, http.StatusInternalServerError, "failed to generate OAuth session")
		return
	}

	respondJSON(w, map[string]any{
		"oauth_url": oauthURL,
		"verifier":  verifier,
		"state":     state,
	})
}

// POST /admin/codex/relogin - start an OAuth flow bound to an existing pool
// account. The exchange replaces that account's stale credentials (e.g. after
// "refresh_token_invalidated") instead of creating a new account.
func (h *proxyHandler) handleCodexRelogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		respondJSONError(w, http.StatusBadRequest, "account_id is required")
		return
	}

	var target *Account
	for _, acc := range h.pool.allAccounts() {
		if acc.ID == accountID {
			target = acc
			break
		}
	}
	if target == nil {
		var codexIDs []string
		for _, acc := range h.pool.allAccounts() {
			if acc.Type == AccountTypeCodex {
				codexIDs = append(codexIDs, acc.ID)
			}
		}
		log.Printf("codex relogin: account %q not found; codex accounts: %v", accountID, codexIDs)
		respondJSONError(w, http.StatusNotFound, "codex account not found: "+accountID)
		return
	}
	if h.pool.accountAuthority != nil {
		if err := h.pool.accountAuthority.authorizeAccount(providerContributionActor(r), target, "manage"); err != nil {
			respondJSONError(w, 403, "account management denied")
			return
		}
	}
	if target.Type != AccountTypeCodex {
		respondJSONError(w, http.StatusBadRequest, "account "+accountID+" is a "+string(target.Type)+" account, not codex")
		return
	}

	oauthURL, verifier, state := startCodexOAuthSession(providerContributionActor(r), accountID)
	if oauthURL == "" {
		respondJSONError(w, http.StatusInternalServerError, "failed to generate OAuth session")
		return
	}

	log.Printf("codex relogin started for account %s (file %s)", accountID, target.File)
	respondJSON(w, map[string]any{
		"oauth_url":  oauthURL,
		"verifier":   verifier,
		"state":      state,
		"account_id": accountID,
	})
}

// POST /admin/codex/exchange - exchange OAuth code for tokens
func (h *proxyHandler) handleCodexExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
	}

	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	} else {
		req.Code = r.FormValue("code")
		req.Verifier = r.FormValue("verifier")
	}

	code := strings.TrimSpace(req.Code)
	verifier := strings.TrimSpace(req.Verifier)

	if code == "" || verifier == "" {
		respondJSONError(w, http.StatusBadRequest, "code and verifier are required")
		return
	}

	// Look up session
	codexOAuthSessions.Lock()
	session, ok := codexOAuthSessions.sessions[verifier]
	if ok && session.ActorID == providerContributionActor(r) {
		delete(codexOAuthSessions.sessions, verifier)
	}
	codexOAuthSessions.Unlock()

	if !ok || time.Since(session.CreatedAt) > 10*time.Minute {
		respondJSONError(w, http.StatusBadRequest, "invalid or expired session")
		return
	}
	if session.ActorID != providerContributionActor(r) {
		respondJSONError(w, http.StatusForbidden, "OAuth session belongs to another principal")
		return
	}

	// Exchange code for tokens
	tokens, err := codexExchangeCode(code, verifier)
	if err != nil {
		log.Printf("Codex token exchange failed: %v", err)
		respondJSONError(w, http.StatusInternalServerError, "token exchange failed: "+err.Error())
		return
	}

	// Relogin flow: replace the bound account's credentials in place.
	if session.ReloginAccountID != "" {
		if err := h.replaceCodexAccountCredentials(session.ReloginAccountID, tokens); err != nil {
			respondJSONError(w, http.StatusForbidden, "relogin failed: "+err.Error())
			return
		}
		codexOAuthSessions.Lock()
		delete(codexOAuthSessions.sessions, verifier)
		codexOAuthSessions.Unlock()
		h.reloadAccounts()
		h.auditProviderContribution(r, "codex-relogin", session.ReloginAccountID)
		respondJSON(w, map[string]any{
			"success":    true,
			"account_id": session.ReloginAccountID,
			"replaced":   true,
		})
		return
	}

	claims := parseCodexClaims(tokens.IDToken)
	accountID, err := h.saveContribution(r, AccountTypeCodex, claims.ChatGPTAccountID, map[string]any{"tokens": map[string]any{"id_token": tokens.IDToken, "access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken, "account_id": claims.ChatGPTAccountID}, "added_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		respondPolicyError(w, err)
		return
	}

	// Remove session
	codexOAuthSessions.Lock()
	delete(codexOAuthSessions.sessions, verifier)
	codexOAuthSessions.Unlock()


	respondJSON(w, map[string]any{
		"success":    true,
		"account_id": accountID,
	})
}

// replaceCodexAccountCredentials overwrites an existing codex account's auth
// file with fresh tokens, preserving unrelated state (added_at, cookies,
// model snapshot, disabled flag) and clearing any retirement markers. The
// upstream identity is verified so credentials cannot be swapped into the
// wrong account.
func (h *proxyHandler) replaceCodexAccountCredentials(accountID string, tokens *CodexTokenResponse) error {
	var target *Account
	for _, acc := range h.pool.allAccounts() {
		if acc.Type == AccountTypeCodex && acc.ID == accountID {
			target = acc
			break
		}
	}
	if target == nil {
		return fmt.Errorf("codex account %q not found", accountID)
	}

	newClaims := parseCodexClaims(tokens.IDToken)
	target.mu.Lock()
	existingIdentity := target.AccountID
	if existingIdentity == "" {
		existingIdentity = target.IDTokenChatGPTAccountID
	}
	authFile := target.File
	target.mu.Unlock()
	if newClaims.ChatGPTAccountID != "" && existingIdentity != "" && newClaims.ChatGPTAccountID != existingIdentity {
		return fmt.Errorf("signed-in account (%s) does not match pool account %s", newClaims.ChatGPTAccountID, accountID)
	}

	existing := make(map[string]any)
	if raw, err := readAccountFile(authFile); err == nil {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("parse %s: %w", authFile, err)
		}
	}
	existing["tokens"] = map[string]any{
		"id_token":      tokens.IDToken,
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
	}
	delete(existing, "dead")
	delete(existing, "last_refresh")

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	if err := writeAccountFile(authFile, data); err != nil {
		return err
	}

	log.Printf("codex relogin: replaced credentials for account %s (%s)", accountID, authFile)
	return nil
}

// codexExchangeCode exchanges an authorization code for tokens
func codexExchangeCode(code, verifier string) (*CodexTokenResponse, error) {
	return codexExchangeCodeWithClient(code, verifier, &http.Client{Timeout: 30 * time.Second})
}

// codexExchangeCodeWithClient is the testable seam of the OAuth token
// exchange: it owns the request shape and, critically, the redaction and size
// limit applied to the upstream error body, which is otherwise both logged and
// returned to the admin browser.
func codexExchangeCodeWithClient(code, verifier string, client *http.Client) (*CodexTokenResponse, error) {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", CodexOAuthClientID)
	data.Set("code", code)
	data.Set("redirect_uri", CodexOAuthRedirectURI)
	data.Set("code_verifier", verifier)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, CodexOAuthTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed: %s: %s", resp.Status, safeText(body))
	}

	var tokens CodexTokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in response")
	}

	return &tokens, nil
}

func cleanupOldCodexSessions() {
	codexOAuthSessions.Lock()
	defer codexOAuthSessions.Unlock()

	now := time.Now()
	for verifier, session := range codexOAuthSessions.sessions {
		if now.Sub(session.CreatedAt) > 10*time.Minute {
			delete(codexOAuthSessions.sessions, verifier)
		}
	}
}
