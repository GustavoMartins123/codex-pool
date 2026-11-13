package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const (
	qwenLoginOrigin   = "https://modelstudio.console.alibabacloud.com"
	qwenLoginCallback = "/api/pool/accounts/qwen/login/callback"
	qwenLoginTTL      = 15 * time.Minute
)

type qwenLoginSession struct {
	actor, state, callbackHost string
	accountID, status, err     string
	token                      string
	expiresAt                  time.Time
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

func qwenLoginLocalOrigin(r *http.Request) (*url.URL, error) {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Scheme != "http" || origin.User != nil || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || !inferredHostPattern.MatchString(origin.Host) {
		return nil, errors.New("Alibaba Cloud browser sign-in requires opening the pool through an HTTP localhost address")
	}
	ip := net.ParseIP(origin.Hostname())
	if !strings.EqualFold(origin.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("Alibaba Cloud only returns browser authorization to a local HTTP address; open the pool through localhost")
	}
	return origin, nil
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
	origin, err := qwenLoginLocalOrigin(r)
	if err != nil {
		respondJSONError(w, http.StatusBadRequest, err.Error())
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
	sessions := &h.qwenLogin
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.byID == nil {
		sessions.byID = make(map[string]*qwenLoginSession)
	}
	now := time.Now()
	for previousID, session := range sessions.byID {
		if session.actor == actor && session.status == "exchanging" {
			respondJSONError(w, http.StatusConflict, "Qwen login is still being finalized")
			return
		}
		if session.actor == actor || !now.Before(session.expiresAt) {
			session.token = ""
			delete(sessions.byID, previousID)
		}
	}
	if len(sessions.byID) >= 128 {
		respondJSONError(w, http.StatusTooManyRequests, "too many pending Qwen logins")
		return
	}
	sessions.byID[id] = &qwenLoginSession{actor: actor, state: state, callbackHost: origin.Host, accountID: input.AccountID, status: "pending", expiresAt: now.Add(qwenLoginTTL)}
	notice := origin.Host + qwenLoginCallback + "?" + url.Values{"session_id": {id}, "state": {state}}.Encode()
	authorize := qwenLoginOrigin + "/console-login?" + url.Values{"notice": {notice}}.Encode()
	respondJSON(w, map[string]any{"session_id": id, "oauth_url": authorize})
}

func (h *proxyHandler) handleQwenLoginCallback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Add("Vary", "Origin")
	if r.Header.Get("Origin") != qwenLoginOrigin {
		respondJSONError(w, http.StatusForbidden, "Qwen login origin denied")
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", qwenLoginOrigin)
	if r.Method != http.MethodOptions && r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		respondJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessions := &h.qwenLogin
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	session := sessions.byID[r.URL.Query().Get("session_id")]
	if session == nil || !secureSecretEquals(r.URL.Query().Get("state"), session.state) || !strings.EqualFold(r.Host, session.callbackHost) {
		respondJSONError(w, http.StatusForbidden, "invalid Qwen login callback")
		return
	}
	if !time.Now().Before(session.expiresAt) {
		session.status, session.err, session.token = "error", "Qwen login expired; sign in again", ""
		respondJSONError(w, http.StatusGone, session.err)
		return
	}
	if session.status != "pending" {
		respondJSONError(w, http.StatusConflict, "Qwen login callback has already been consumed")
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Add("Vary", "Access-Control-Request-Private-Network")
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/json" {
		respondJSONError(w, http.StatusUnsupportedMediaType, "Qwen callback requires JSON")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input struct {
		AccessToken   string `json:"access_token"`
		ConsoleSite   string `json:"console_site"`
		ConsoleRegion string `json:"console_region"`
	}
	decoder := json.NewDecoder(r.Body)
	var extra any
	if decoder.Decode(&input) != nil || !errors.Is(decoder.Decode(&extra), io.EOF) || input.ConsoleSite != "international" || input.ConsoleRegion != "ap-southeast-1" || strings.TrimSpace(input.AccessToken) == "" || strings.ContainsAny(input.AccessToken, " \t\r\n") {
		respondJSONError(w, http.StatusBadRequest, "Qwen callback must contain an international Singapore console session")
		return
	}
	session.token, session.status = input.AccessToken, "authorized"
	respondJSON(w, map[string]bool{"success": true})
}

func qwenLoginView(session *qwenLoginSession) map[string]any {
	return map[string]any{"status": session.status, "account_id": session.accountID, "error": session.err}
}

func (h *proxyHandler) handleQwenLoginPoll(w http.ResponseWriter, r *http.Request) {
	h.handleQwenLoginProgress(w, r, false)
}

func (h *proxyHandler) handleQwenLoginComplete(w http.ResponseWriter, r *http.Request) {
	h.handleQwenLoginProgress(w, r, true)
}

func (h *proxyHandler) handleQwenLoginProgress(w http.ResponseWriter, r *http.Request, complete bool) {
	noStore(w)
	var input struct {
		SessionID string `json:"session_id"`
		APIKey    string `json:"api_key"`
	}
	if decodeGovernanceJSON(w, r, &input) != nil || input.SessionID == "" {
		respondJSONError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	actor := providerContributionActor(r)
	sessions := &h.qwenLogin
	sessions.mu.Lock()
	session := sessions.byID[input.SessionID]
	if session == nil {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusNotFound, "Qwen login session not found")
		return
	}
	if session.actor != actor {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusForbidden, "Qwen login belongs to another principal")
		return
	}
	if !time.Now().Before(session.expiresAt) && session.status != "complete" && session.status != "exchanging" {
		session.status, session.err, session.token = "error", "Qwen login expired; sign in again", ""
	}
	if session.status == "authorized" && session.accountID == "" {
		session.status = "awaiting_key"
	}
	key := strings.TrimSpace(input.APIKey)
	if complete && (session.status != "awaiting_key" || key == "") {
		sessions.mu.Unlock()
		respondJSONError(w, http.StatusBadRequest, "finish Alibaba Cloud sign-in and enter your existing Token Plan API key")
		return
	}
	if !complete && (session.status != "authorized" || session.accountID == "") {
		view := qwenLoginView(session)
		sessions.mu.Unlock()
		respondJSON(w, view)
		return
	}
	session.status = "exchanging"
	token, accountID := session.token, session.accountID
	sessions.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalBrowser, ConsoleToken: token}
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
		err = h.linkQwenCloudSession(actor, accountID, plan, credits)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	session.token = ""
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
