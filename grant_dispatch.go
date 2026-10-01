package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

type grantRequestContextKey struct{}
type grantAdmissionContextKey struct{}

type grantRequestState struct {
	mu         sync.Mutex
	admissions map[string]*policyAdmission
}

func newGrantRequestState() *grantRequestState {
	return &grantRequestState{admissions: map[string]*policyAdmission{}}
}

func (s *grantRequestState) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.admissions {
		a.Release()
	}
}

func (h *proxyHandler) acquireGovernedAccount(r *http.Request, identity, conversation string, a *Account, model string) (*http.Request, func(), error) {
	if a == nil {
		return nil, nil, accountControlError("account_unavailable", 503)
	}
	if h.pool == nil || h.pool.accountAuthority == nil {
		return r, func() {}, nil
	}
	pinned := h.pool.isAccountPinned(identity, conversation, a)
	p := h.pool.accountAuthority
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := h.checkAccountUse(identity, a); err != nil {
		return nil, nil, err
	}
	grant, err := p.accountGrantForUse(identity, a, model)
	if err != nil {
		return nil, nil, err
	}
	if grant != nil && model == "" {
		return nil, nil, accountControlError("grant_model_required", 422)
	}
	release, err := p.controlAdmissionLocked(a.Type, a.ID, pinned, true)
	if err != nil {
		return nil, nil, err
	}
	if grant == nil {
		return r, release, nil
	}
	rollback := func(err error) (*http.Request, func(), error) {
		key := resourceKey(a.Type, a.ID)
		p.accountInflight[key]--
		if p.accountInflight[key] == 0 {
			delete(p.accountInflight, key)
		}
		return nil, nil, err
	}
	state, _ := r.Context().Value(grantRequestContextKey{}).(*grantRequestState)
	if state == nil {
		return rollback(accountControlError("grant_request_state_unavailable", 503))
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	admission := state.admissions[grant.ID]
	if admission == nil {
		admission, err = p.reserveGrantRequest(grant, time.Now().UTC())
		if err != nil {
			return rollback(err)
		}
		state.admissions[grant.ID] = admission
	}
	return r.WithContext(context.WithValue(r.Context(), grantAdmissionContextKey{}, admission)), release, nil
}

func (h *proxyHandler) policyRoundTrip(r *http.Request, provider AccountType, admission *policyAdmission) (*http.Response, error) {
	grant, _ := r.Context().Value(grantAdmissionContextKey{}).(*policyAdmission)
	if !grant.hasTokenBudget() {
		return h.clientPolicyRoundTrip(r, provider, admission)
	}
	bound, err := policyRequestTokenBound(r, provider)
	if err != nil {
		return nil, err
	}
	attempt, err := grant.reserveTokenAttempt(bound)
	if err != nil {
		return nil, err
	}
	resp, err := h.clientPolicyRoundTrip(r, provider, admission)
	var blocked *policyError
	if errors.As(err, &blocked) {
		grant.attemptMu.Lock()
		attempt.known = true
		attempt.complete = true
		attempt.tokens = 0
		attempt.expected = 0
		grant.settleTokenAttempt(attempt)
		grant.attemptMu.Unlock()
	}
	if err == nil && resp != nil && resp.Body != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.Body = &policyCompletionBody{ReadCloser: resp.Body, admission: grant, attempt: attempt, json: strings.Contains(resp.Header.Get("Content-Type"), "application/json"), settleReported: true}
	}
	return resp, err
}

func (h *proxyHandler) reserveWebSocketGrant(identity string, a *Account, model string) (*policyAdmission, error) {
	if err := h.checkLivePolicy(identity, model, a.Type); err != nil {
		return nil, err
	}
	if h.pool == nil || h.pool.accountAuthority == nil {
		return nil, nil
	}
	p := h.pool.accountAuthority
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := h.checkAccountUse(identity, a); err != nil {
		return nil, err
	}
	grant, err := p.accountGrantForUse(identity, a, model)
	if err != nil || grant == nil {
		return nil, err
	}
	if model == "" || a.Type != AccountTypeCodex || grant.Budget.DailyTokens > 0 || grant.Budget.MonthlyTokens > 0 {
		return nil, unboundedPolicyRequest()
	}
	return p.reserveGrantRequest(grant, time.Now().UTC())
}

func (h *proxyHandler) checkWebSocketGrant(identity string, a *Account) error {
	if h.pool == nil || h.pool.accountAuthority == nil {
		return nil
	}
	grant, err := h.pool.accountAuthority.accountGrantForUse(identity, a, "")
	if err != nil {
		return err
	}
	if grant != nil && (a.Type != AccountTypeCodex || grant.Budget.DailyTokens > 0 || grant.Budget.MonthlyTokens > 0) {
		return unboundedPolicyRequest()
	}
	return nil
}
