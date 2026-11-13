package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const qwenLoginTTL = 15 * time.Minute

type qwenLoginSession struct {
	actor, state, verifier string
	accountID, status, err string
	oauth                  *QwenCloudOAuth
	expiresAt              time.Time
}

func (session *qwenLoginSession) clearCredentials() {
	session.verifier, session.oauth = "", nil
}

type qwenLoginSessions struct {
	mu   sync.Mutex
	byID map[string]*qwenLoginSession
}

func qwenLoginRandom(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (h *proxyHandler) qwenLoginAccount(actor, id string) (*Account, error) {
	if h.passport == nil || h.pool == nil {
		return nil, errors.New("account authority unavailable")
	}
	if err := h.passport.authorizeAccount(actor, &Account{Type: AccountTypeQwen, ID: id}, "manage"); err != nil {
		return nil, errors.New("account management denied")
	}
	for _, account := range h.pool.allAccounts() {
		if account.Type == AccountTypeQwen && account.ID == id {
			return account, nil
		}
	}
	return nil, errors.New("Qwen account not found")
}

func (h *proxyHandler) handleQwenLoginInit(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.checkContributionAttempt(w, r) {
		return
	}
	var input struct {
		AccountID string `json:"account_id"`
	}
	if decodeGovernanceJSON(w, r, &input) != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid Qwen login request")
		return
	}
	actor := providerContributionActor(r)
	if input.AccountID != "" {
		if _, err := h.qwenLoginAccount(actor, input.AccountID); err != nil {
			respondJSONError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	id, err := qwenLoginRandom(24)
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "could not create Qwen login session")
		return
	}
	state, err := qwenLoginRandom(32)
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "could not create Qwen login state")
		return
	}
	verifier, err := qwenLoginRandom(32)
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "could not create Qwen authorization verifier")
		return
	}
	sessions := &h.qwenLogin
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.byID == nil {
		sessions.byID = make(map[string]*qwenLoginSession)
	}
	now := time.Now()
	for previousID, session := range sessions.byID {
		if session.status == "exchanging" {
			if session.actor == actor {
				respondJSONError(w, http.StatusConflict, "Qwen login is still being finalized")
				return
			}
			continue
		}
		if session.actor == actor || !now.Before(session.expiresAt) {
			session.clearCredentials()
			delete(sessions.byID, previousID)
		}
	}
	if len(sessions.byID) >= 128 {
		respondJSONError(w, http.StatusTooManyRequests, "too many pending Qwen logins")
		return
	}
	sessions.byID[id] = &qwenLoginSession{actor: actor, state: state, verifier: verifier, accountID: input.AccountID, status: "pending", expiresAt: now.Add(qwenLoginTTL)}
	challenge := sha256.Sum256([]byte(verifier))
	authorize := qwenCloudSignInOrigin + "/oauth2/v1/auth?" + url.Values{
		"response_type": {"code"}, "client_id": {qwenCloudOAuthClient}, "redirect_uri": {qwenOAuthRedirectURI},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	respondJSON(w, map[string]any{"session_id": id, "oauth_url": authorize})
}

func parseQwenOAuthCallback(raw, state string) (string, error) {
	callback, err := url.Parse(strings.TrimSpace(raw))
	expected, _ := url.Parse(qwenOAuthRedirectURI)
	if err != nil || callback.Scheme != expected.Scheme || callback.Host != expected.Host || callback.Path != expected.Path || callback.User != nil || callback.Fragment != "" || callback.Opaque != "" {
		return "", errors.New("paste the complete Alibaba Cloud authorization callback URL")
	}
	query, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(query["state"]) != 1 || !secureSecretEquals(query.Get("state"), state) {
		return "", errors.New("Alibaba Cloud callback does not match this sign-in session")
	}
	if query.Has("error") {
		return "", errors.New("Alibaba Cloud authorization was declined; sign in again")
	}
	if len(query["code"]) != 1 || strings.TrimSpace(query.Get("code")) == "" {
		return "", errors.New("Alibaba Cloud callback is missing its authorization code")
	}
	return query.Get("code"), nil
}

func (h *proxyHandler) handleQwenLoginAuthorize(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var input struct {
		SessionID   string `json:"session_id"`
		CallbackURL string `json:"callback_url"`
	}
	if decodeGovernanceJSON(w, r, &input) != nil || input.SessionID == "" {
		respondJSONError(w, http.StatusBadRequest, "session_id and callback_url are required")
		return
	}
	sessions := &h.qwenLogin
	sessions.mu.Lock()
	session := sessions.byID[input.SessionID]
	if session == nil || session.actor != providerContributionActor(r) {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusForbidden, "Qwen login session is unavailable for this principal")
		return
	}
	if session.status != "pending" {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusConflict, "Qwen authorization has already been consumed")
		return
	}
	if !time.Now().Before(session.expiresAt) {
		session.status, session.err = "error", "Qwen login expired; sign in again"
		session.clearCredentials()
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusGone, session.err)
		return
	}
	code, err := parseQwenOAuthCallback(input.CallbackURL, session.state)
	if err != nil {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	session.status = "exchanging"
	verifier := session.verifier
	session.verifier = ""
	sessions.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tokens, err := h.fetchQwenOAuthTokens(ctx, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {qwenOAuthRedirectURI}, "code_verifier": {verifier},
	}, time.Now())
	sessions.mu.Lock()
	if err != nil {
		session.status, session.err = "error", err.Error()
		session.clearCredentials()
		view := qwenLoginView(session)
		sessions.mu.Unlock()
		respondJSON(w, view)
		return
	}
	session.oauth = &QwenCloudOAuth{tokens: tokens}
	if session.accountID == "" {
		session.status = "awaiting_key"
		view := qwenLoginView(session)
		sessions.mu.Unlock()
		respondJSON(w, view)
		return
	}
	sessions.mu.Unlock()
	h.finishQwenLogin(w, r, session, "")
}

func qwenLoginView(session *qwenLoginSession) map[string]any {
	return map[string]any{"status": session.status, "account_id": session.accountID, "error": session.err}
}

func (h *proxyHandler) handleQwenLoginComplete(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var input struct {
		SessionID string `json:"session_id"`
		APIKey    string `json:"api_key"`
	}
	if decodeGovernanceJSON(w, r, &input) != nil || input.SessionID == "" || strings.TrimSpace(input.APIKey) == "" {
		respondJSONError(w, http.StatusBadRequest, "session_id and the existing Token Plan api_key are required")
		return
	}
	sessions := &h.qwenLogin
	sessions.mu.Lock()
	session := sessions.byID[input.SessionID]
	if session == nil || session.actor != providerContributionActor(r) {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusForbidden, "Qwen login session is unavailable for this principal")
		return
	}
	if session.status != "awaiting_key" {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusConflict, "finish Alibaba Cloud authorization before adding the Token Plan key")
		return
	}
	if !time.Now().Before(session.expiresAt) {
		session.status, session.err = "error", "Qwen login expired; sign in again"
		session.clearCredentials()
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusGone, session.err)
		return
	}
	session.status = "exchanging"
	sessions.mu.Unlock()
	h.finishQwenLogin(w, r, session, strings.TrimSpace(input.APIKey))
}

func (h *proxyHandler) finishQwenLogin(w http.ResponseWriter, r *http.Request, session *qwenLoginSession, key string) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalBrowser, CloudOAuth: session.oauth}
	accountID := session.accountID
	credits, err := h.queryQwenCredits(ctx, plan, time.Now())
	if err == nil && accountID == "" {
		var snapshot providerModelSnapshot
		provider := h.registry.ForType(AccountTypeQwen)
		if provider == nil {
			err = errors.New("Qwen provider is unavailable")
		} else {
			snapshot, err = fetchProviderModels(ctx, h.transport, provider, &Account{Type: AccountTypeQwen, AccessToken: key})
			if err != nil {
				err = errors.New("Qwen Token Plan key validation or model discovery failed; the account was not added")
			}
		}
		if err == nil {
			accountID, err = h.saveContribution(r, AccountTypeQwen, key, map[string]any{
				"api_key": key, "added_at": time.Now().UTC().Format(time.RFC3339Nano),
				"provider_model_snapshot": snapshot, "token_plan": plan, "token_plan_credits": credits,
			})
		}
	} else if err == nil {
		err = h.linkQwenCloudSession(providerContributionActor(r), accountID, plan, credits)
	}
	h.qwenLogin.mu.Lock()
	defer h.qwenLogin.mu.Unlock()
	session.clearCredentials()
	if err != nil {
		session.status, session.err = "error", err.Error()
	} else {
		session.status, session.accountID = "complete", accountID
	}
	respondJSON(w, qwenLoginView(session))
}

func (h *proxyHandler) linkQwenCloudSession(actor, id string, plan *QwenTokenPlanConfig, credits *QwenCreditSnapshot) error {
	account, err := h.qwenLoginAccount(actor, id)
	if err != nil {
		return err
	}
	account.mu.Lock()
	previousPlan, previousCredits, previousError, previousUsage, previousType := account.QwenTokenPlan, account.QwenCredits, account.QwenCreditsError, account.Usage, account.PlanType
	account.QwenTokenPlan, account.QwenCredits, account.QwenCreditsError = plan, credits, ""
	account.Usage, account.PlanType = qwenCreditUsage(credits, time.Now()), "token_plan_personal"
	if err := saveAccount(account); err != nil {
		account.QwenTokenPlan, account.QwenCredits, account.QwenCreditsError, account.Usage, account.PlanType = previousPlan, previousCredits, previousError, previousUsage, previousType
		account.mu.Unlock()
		return errors.New("could not save Alibaba Cloud account link")
	}
	account.mu.Unlock()
	return h.passport.db.Update(func(tx *bbolt.Tx) error {
		return h.passport.audit(tx, actor, "account.token_plan_updated", id, "qwen")
	})
}
