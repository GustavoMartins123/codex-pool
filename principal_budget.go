package main

import (
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"log"
	"net/http"
	"strings"
	"time"
)

type policyBudgetHold struct {
	Scope  string
	Day    string
	Month  string
	Tokens int64
}

func validatePolicyLimits(l PolicyLimits) error {
	if l.RequestsPerMinute < 0 || l.ConcurrentRequests < 0 || l.DailyRequests < 0 || l.MonthlyRequests < 0 || l.DailyTokens < 0 || l.MonthlyTokens < 0 || l.TokenReservation < 0 {
		return errors.New("policy limits cannot be negative")
	}
	if (l.DailyTokens > 0 || l.MonthlyTokens > 0) && l.TokenReservation <= 0 {
		return errors.New("token limits require an explicit token reservation")
	}
	return nil
}

func checkBudget(l PolicyLimits, minute, day, month policyUsageCounter) error {
	code := ""
	message := ""
	switch {
	case l.RequestsPerMinute > 0 && minute.Requests >= int64(l.RequestsPerMinute):
		code = "policy_rate_limit_exceeded"
		message = "request rate limit exhausted"
	case l.DailyRequests > 0 && day.Requests >= l.DailyRequests:
		code = "policy_daily_requests_exceeded"
		message = "daily request budget exhausted"
	case l.MonthlyRequests > 0 && month.Requests >= l.MonthlyRequests:
		code = "policy_monthly_requests_exceeded"
		message = "monthly request budget exhausted"
	case l.DailyTokens > 0 && day.Tokens+day.ReservedTokens >= l.DailyTokens:
		code = "policy_daily_tokens_exceeded"
		message = "daily token budget exhausted"
	case l.MonthlyTokens > 0 && month.Tokens+month.ReservedTokens >= l.MonthlyTokens:
		code = "policy_monthly_tokens_exceeded"
		message = "monthly token budget exhausted"
	}
	if code != "" {
		return &policyError{Status: 429, Code: code, Message: message}
	}
	return nil
}

func (p *PassportStore) reservePolicyRequest(principalID, clientID string, configured map[string]ClientPolicy, now time.Time) (*policyAdmission, error) {
	principal, policy, exists, ok := p.resolveClientPolicy(principalID, clientID, configured)
	if !ok || !exists || principal == nil {
		return nil, &policyError{Status: 403, Code: "policy_identity_missing", Message: "policy identity unavailable"}
	}
	if err := validatePolicyLimits(policy.Limits); err != nil {
		return nil, err
	}
	if err := validatePolicyLimits(principal.Budget); err != nil {
		return nil, err
	}
	priority := policy.Priority
	if priority == 0 {
		priority = defaultPolicyPriority(principal.Kind)
	}
	a := &policyAdmission{store: p, principalID: principalID, clientID: clientID, policy: policy, priority: priority, configured: policy.configured() || principal.Budget != (PolicyLimits{})}
	type scope struct {
		id     string
		limits PolicyLimits
	}
	scopes := []scope{{clientID, policy.Limits}, {"principal:" + principalID, principal.Budget}}
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	for _, s := range scopes {
		if s.limits.ConcurrentRequests > 0 && p.policyInflight[s.id] >= s.limits.ConcurrentRequests {
			return nil, &policyError{Status: 429, Code: "policy_concurrency_exceeded", Message: "concurrent request limit exhausted"}
		}
	}
	err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		if b == nil {
			return errors.New("policy usage unavailable")
		}
		for _, s := range scopes {
			minuteKey, dayKey, monthKey := policyUsageKeys(s.id, now)
			minute, err := readPolicyCounter(b, minuteKey)
			if err != nil {
				return err
			}
			day, err := readPolicyCounter(b, dayKey)
			if err != nil {
				return err
			}
			month, err := readPolicyCounter(b, monthKey)
			if err != nil {
				return err
			}
			if err := checkBudget(s.limits, minute, day, month); err != nil {
				return err
			}
			reserved := policyTokenReservation(s.limits, day, month, 0)
			minute.Requests++
			day.Requests++
			month.Requests++
			day.ReservedTokens += reserved
			month.ReservedTokens += reserved
			for key, counter := range map[string]policyUsageCounter{minuteKey: minute, dayKey: day, monthKey: month} {
				if err := writePolicyCounter(b, key, counter); err != nil {
					return err
				}
			}
			a.holds = append(a.holds, policyBudgetHold{Scope: s.id, Day: dayKey, Month: monthKey, Tokens: reserved})
			if s.id == clientID {
				a.reservedTokens = reserved
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, hold := range a.holds {
		p.policyInflight[hold.Scope]++
		p.policyReserved[hold.Scope] += hold.Tokens
	}
	return a, nil
}

func (a *policyAdmission) releaseBudgetHolds() {
	p := a.store
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		if b == nil {
			return errors.New("policy usage unavailable")
		}
		for _, hold := range a.holds {
			if hold.Tokens == 0 {
				continue
			}
			for _, key := range []string{hold.Day, hold.Month} {
				counter, err := readPolicyCounter(b, key)
				if err != nil {
					return err
				}
				if counter.ReservedTokens < hold.Tokens {
					return errors.New("policy reservation underflow")
				}
				counter.ReservedTokens -= hold.Tokens
				if err := writePolicyCounter(b, key, counter); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		log.Printf("policy reservation release failed: %v", err)
		return
	}
	for _, hold := range a.holds {
		p.policyInflight[hold.Scope]--
		if p.policyInflight[hold.Scope] <= 0 {
			delete(p.policyInflight, hold.Scope)
		}
		p.policyReserved[hold.Scope] -= hold.Tokens
		if p.policyReserved[hold.Scope] <= 0 {
			delete(p.policyReserved, hold.Scope)
		}
	}
}

func (p *PassportStore) migratePrincipalPolicyUsage() error {
	return p.db.Update(func(tx *bbolt.Tx) error {
		state := tx.Bucket([]byte(bucketAnalyticsState))
		if state.Get([]byte("principal_policy_usage_migrated")) != nil {
			return nil
		}
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		totals := map[string]policyUsageCounter{}
		if err := b.ForEach(func(k, v []byte) error {
			parts := strings.Split(string(k), "|")
			if len(parts) != 3 || strings.HasPrefix(parts[1], "principal:") {
				return nil
			}
			client := p.clients[parts[1]]
			if client == nil {
				return errors.New("policy usage references an unknown client")
			}
			var counter policyUsageCounter
			if err := json.Unmarshal(v, &counter); err != nil {
				return err
			}
			key := parts[0] + "|principal:" + client.PrincipalID + "|" + parts[2]
			total := totals[key]
			total.Requests += counter.Requests
			total.Tokens += counter.Tokens
			total.ReservedTokens += counter.ReservedTokens
			totals[key] = total
			return nil
		}); err != nil {
			return err
		}
		for key, counter := range totals {
			if err := writePolicyCounter(b, key, counter); err != nil {
				return err
			}
		}
		return state.Put([]byte("principal_policy_usage_migrated"), []byte{1})
	})
}

func (h *proxyHandler) handlePrincipalBudget(w http.ResponseWriter, r *http.Request) {
	operator, session, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !h.passportCSRF(r, session) {
		respondJSONError(w, 403, "invalid CSRF token")
		return
	}
	var q struct {
		PrincipalID string       `json:"principal_id"`
		Limits      PolicyLimits `json:"limits"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil {
		respondJSONError(w, 400, "invalid json")
		return
	}
	if err := validatePolicyLimits(q.Limits); err != nil {
		respondJSONError(w, 400, err.Error())
		return
	}
	p := h.passport
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.principals[q.PrincipalID]
	if pr == nil {
		respondJSONError(w, 404, "principal not found")
		return
	}
	updated := *pr
	updated.Budget = q.Limits
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), updated.ID, updated); err != nil {
			return err
		}
		return p.audit(tx, operator.ID, "principal.budget_changed", updated.ID, "")
	}); err != nil {
		respondJSONError(w, 500, "could not update principal budget")
		return
	}
	p.principals[updated.ID] = &updated
	respondJSON(w, map[string]any{"principal_id": updated.ID, "limits": updated.Budget})
}
