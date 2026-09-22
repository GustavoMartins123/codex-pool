package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const bucketPassportPolicyUsage = "passport_policy_usage"

type PolicySelector struct {
	Allow []string `toml:"allow" json:"allow,omitempty"`
	Deny  []string `toml:"deny" json:"deny,omitempty"`
}

type PolicyLimits struct {
	RequestsPerMinute  int   `toml:"requests_per_minute" json:"requests_per_minute,omitempty"`
	ConcurrentRequests int   `toml:"concurrent_requests" json:"concurrent_requests,omitempty"`
	DailyRequests      int64 `toml:"daily_requests" json:"daily_requests,omitempty"`
	MonthlyRequests    int64 `toml:"monthly_requests" json:"monthly_requests,omitempty"`
	DailyTokens        int64 `toml:"daily_tokens" json:"daily_tokens,omitempty"`
	MonthlyTokens      int64 `toml:"monthly_tokens" json:"monthly_tokens,omitempty"`
}

type PolicyRouting struct {
	Profile string `toml:"profile" json:"profile,omitempty"`
}

type ClientPolicy struct {
	Models    PolicySelector `toml:"models" json:"models,omitempty"`
	Providers PolicySelector `toml:"providers" json:"providers,omitempty"`
	Limits    PolicyLimits   `toml:"limits" json:"limits,omitempty"`
	Routing   PolicyRouting  `toml:"routing" json:"routing,omitempty"`
	Priority  int            `toml:"priority" json:"priority,omitempty"`
}

func (p ClientPolicy) configured() bool {
	return len(p.Models.Allow)+len(p.Models.Deny)+len(p.Providers.Allow)+len(p.Providers.Deny) > 0 ||
		p.Limits != (PolicyLimits{}) || strings.TrimSpace(p.Routing.Profile) != "" || p.Priority != 0
}

func defaultPolicyPriority(kind PrincipalKind) int {
	switch kind {
	case PrincipalOperator:
		return 100
	case PrincipalMember:
		return 50
	default:
		return 10
	}
}

type policyUsageCounter struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

type policyError struct {
	Status  int
	Code    string
	Message string
}

func (e *policyError) Error() string { return e.Message }

type policyAdmission struct {
	store       *PassportStore
	principalID string
	clientID    string
	policy      ClientPolicy
	priority    int
	configured  bool
	releaseOnce sync.Once
}

type policyAdmissionContextKey struct{}

func policyAdmissionFromRequest(r *http.Request) *policyAdmission {
	if r == nil {
		return nil
	}
	admission, _ := r.Context().Value(policyAdmissionContextKey{}).(*policyAdmission)
	return admission
}

func (a *policyAdmission) Release() {
	if a == nil || a.store == nil {
		return
	}
	a.releaseOnce.Do(func() {
		a.store.policyMu.Lock()
		if a.store.policyInflight[a.clientID] <= 1 {
			delete(a.store.policyInflight, a.clientID)
		} else {
			a.store.policyInflight[a.clientID]--
		}
		a.store.policyMu.Unlock()
	})
}

func normalizePolicyValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "antigravity/")
	return value
}

func policyMatches(values []string, candidate string) bool {
	candidate = normalizePolicyValue(candidate)
	for _, value := range values {
		value = normalizePolicyValue(value)
		if value == "*" || value == candidate {
			return true
		}
	}
	return false
}

func policyAllows(selector PolicySelector, candidate string) bool {
	if policyMatches(selector.Deny, candidate) {
		return false
	}
	return len(selector.Allow) == 0 || policyMatches(selector.Allow, candidate)
}

func (a *policyAdmission) CheckModel(model string) error {
	if a == nil || strings.TrimSpace(model) == "" || policyAllows(a.policy.Models, model) {
		return nil
	}
	return &policyError{
		Status:  http.StatusForbidden,
		Code:    "policy_model_denied",
		Message: fmt.Sprintf("client policy does not allow model %q", model),
	}
}

func (a *policyAdmission) CheckProvider(provider AccountType) error {
	if a == nil || provider == "" || policyAllows(a.policy.Providers, string(provider)) {
		return nil
	}
	return &policyError{
		Status:  http.StatusForbidden,
		Code:    "policy_provider_denied",
		Message: fmt.Sprintf("client policy does not allow provider %q", provider),
	}
}

func readPolicyCounter(bucket *bbolt.Bucket, key string) (policyUsageCounter, error) {
	var counter policyUsageCounter
	if raw := bucket.Get([]byte(key)); raw != nil {
		if err := json.Unmarshal(raw, &counter); err != nil {
			return counter, err
		}
	}
	return counter, nil
}

func writePolicyCounter(bucket *bbolt.Bucket, key string, counter policyUsageCounter) error {
	raw, err := json.Marshal(counter)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(key), raw)
}

func policyUsageKeys(clientID string, now time.Time) (minute, day, month string) {
	now = now.UTC()
	return "minute|" + clientID + "|" + now.Format("200601021504"),
		"day|" + clientID + "|" + now.Format("20060102"),
		"month|" + clientID + "|" + now.Format("200601")
}

func (p *PassportStore) beginPolicyRequest(principalID, clientID string, configured map[string]ClientPolicy, now time.Time) (*policyAdmission, error) {
	if p == nil || clientID == "" {
		return nil, nil
	}
	principal := p.principal(principalID)
	if principal == nil {
		return nil, &policyError{Status: http.StatusForbidden, Code: "policy_identity_missing", Message: "client policy identity is unavailable"}
	}
	p.mu.RLock()
	client := p.clients[clientID]
	var policy ClientPolicy
	if client != nil {
		policy = client.Policy
	}
	p.mu.RUnlock()
	if !policy.configured() && configured != nil {
		if candidate, ok := configured[clientID]; ok {
			policy = candidate
		} else if client != nil {
			label := strings.ToLower(strings.TrimSpace(client.Label))
			for key, candidate := range configured {
				if strings.ToLower(strings.TrimSpace(key)) == label {
					policy = candidate
					break
				}
			}
		}
		if !policy.configured() {
			policy = configured["*"]
		}
	}
	priority := policy.Priority
	explicitPolicy := policy.configured()
	if priority == 0 {
		priority = defaultPolicyPriority(principal.Kind)
	}

	p.policyMu.Lock()
	if limit := policy.Limits.ConcurrentRequests; limit > 0 && p.policyInflight[clientID] >= limit {
		p.policyMu.Unlock()
		return nil, &policyError{Status: http.StatusTooManyRequests, Code: "policy_concurrency_exceeded", Message: fmt.Sprintf("client concurrency limit of %d requests exceeded", limit)}
	}
	p.policyInflight[clientID]++
	p.policyMu.Unlock()

	admission := &policyAdmission{store: p, principalID: principalID, clientID: clientID, policy: policy, priority: priority, configured: explicitPolicy}
	minuteKey, dayKey, monthKey := policyUsageKeys(clientID, now)
	err := p.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketPassportPolicyUsage))
		minute, err := readPolicyCounter(bucket, minuteKey)
		if err != nil {
			return err
		}
		day, err := readPolicyCounter(bucket, dayKey)
		if err != nil {
			return err
		}
		month, err := readPolicyCounter(bucket, monthKey)
		if err != nil {
			return err
		}
		switch {
		case policy.Limits.RequestsPerMinute > 0 && minute.Requests >= int64(policy.Limits.RequestsPerMinute):
			return &policyError{Status: http.StatusTooManyRequests, Code: "policy_rate_limit_exceeded", Message: fmt.Sprintf("client rate limit of %d requests per minute exceeded", policy.Limits.RequestsPerMinute)}
		case policy.Limits.DailyRequests > 0 && day.Requests >= policy.Limits.DailyRequests:
			return &policyError{Status: http.StatusTooManyRequests, Code: "policy_daily_requests_exceeded", Message: fmt.Sprintf("client daily request budget of %d exhausted", policy.Limits.DailyRequests)}
		case policy.Limits.MonthlyRequests > 0 && month.Requests >= policy.Limits.MonthlyRequests:
			return &policyError{Status: http.StatusTooManyRequests, Code: "policy_monthly_requests_exceeded", Message: fmt.Sprintf("client monthly request budget of %d exhausted", policy.Limits.MonthlyRequests)}
		case policy.Limits.DailyTokens > 0 && day.Tokens >= policy.Limits.DailyTokens:
			return &policyError{Status: http.StatusTooManyRequests, Code: "policy_daily_tokens_exceeded", Message: fmt.Sprintf("client daily token budget of %d exhausted", policy.Limits.DailyTokens)}
		case policy.Limits.MonthlyTokens > 0 && month.Tokens >= policy.Limits.MonthlyTokens:
			return &policyError{Status: http.StatusTooManyRequests, Code: "policy_monthly_tokens_exceeded", Message: fmt.Sprintf("client monthly token budget of %d exhausted", policy.Limits.MonthlyTokens)}
		}
		minute.Requests++
		day.Requests++
		month.Requests++
		if err := writePolicyCounter(bucket, minuteKey, minute); err != nil {
			return err
		}
		if err := writePolicyCounter(bucket, dayKey, day); err != nil {
			return err
		}
		return writePolicyCounter(bucket, monthKey, month)
	})
	if err != nil {
		admission.Release()
		return nil, err
	}
	return admission, nil
}

func (p *PassportStore) recordPolicyTokens(clientID string, tokens int64, now time.Time) error {
	if p == nil || clientID == "" || tokens <= 0 {
		return nil
	}
	_, dayKey, monthKey := policyUsageKeys(clientID, now)
	return p.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketPassportPolicyUsage))
		for _, key := range []string{dayKey, monthKey} {
			counter, err := readPolicyCounter(bucket, key)
			if err != nil {
				return err
			}
			counter.Tokens += tokens
			if err := writePolicyCounter(bucket, key, counter); err != nil {
				return err
			}
		}
		return nil
	})
}

func respondPolicyError(w http.ResponseWriter, err error) {
	var blocked *policyError
	if !errors.As(err, &blocked) {
		blocked = &policyError{Status: http.StatusInternalServerError, Code: "policy_unavailable", Message: "client policy could not be evaluated"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(blocked.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": blocked.Code, "message": blocked.Message}})
}

func (h *proxyHandler) auditPolicyDecision(admission *policyAdmission, action string, err error) {
	if h == nil || h.passport == nil || admission == nil {
		return
	}
	if err == nil && !admission.configured {
		return
	}
	detail := fmt.Sprintf("client=%s priority=%d", admission.clientID, admission.priority)
	if err != nil {
		detail += " reason=" + err.Error()
	}
	_ = h.passport.recordAudit(admission.principalID, action, admission.clientID, detail)
}

func (h *proxyHandler) enforcePolicy(w http.ResponseWriter, admission *policyAdmission, check func() error) bool {
	if admission == nil {
		return true
	}
	if err := check(); err != nil {
		h.auditPolicyDecision(admission, "policy.request_blocked", err)
		respondPolicyError(w, err)
		return false
	}
	return true
}
