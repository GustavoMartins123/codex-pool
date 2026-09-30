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
	TokenReservation   int64 `toml:"token_reservation" json:"token_reservation,omitempty"`
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
	ReservedTokens int64 `json:"reserved_tokens,omitempty"`
	Requests       int64 `json:"requests"`
	Tokens         int64 `json:"tokens"`
}

type policyError struct {
	Status  int
	Code    string
	Message string
}

func (e *policyError) Error() string { return e.Message }

type policyAdmission struct {
	holds          []policyBudgetHold
	store          *PassportStore
	readOnly       bool
	principalID    string
	clientID       string
	policy         ClientPolicy
	priority       int
	configured     bool
	reservedTokens int64
	releaseOnce    sync.Once
	attemptMu      sync.Mutex
	attempt        *policyTokenAttempt
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
	if a == nil || a.store == nil || a.readOnly {
		return
	}
	a.releaseOnce.Do(a.releaseBudgetHolds)
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
	if a.hasTokenBudget() && provider != AccountTypeCodex && provider != AccountTypeClaude {
		return unboundedPolicyRequest()
	}
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
	if bucket == nil {
		return counter, errors.New("policy usage unavailable")
	}
	if raw := bucket.Get([]byte(key)); raw != nil {
		if err := json.Unmarshal(raw, &counter); err != nil {
			return counter, err
		}
	}
	if counter.Requests < 0 || counter.Tokens < 0 || counter.ReservedTokens < 0 {
		return counter, errors.New("invalid policy usage counter")
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

func (p *PassportStore) resolveClientPolicy(principalID, clientID string, configured map[string]ClientPolicy) (*Principal, ClientPolicy, bool, bool) {
	if p == nil || clientID == "" {
		return nil, ClientPolicy{}, false, false
	}
	principal := p.principal(principalID)
	if principal == nil {
		return nil, ClientPolicy{}, false, false
	}
	p.mu.RLock()
	client := p.clients[clientID]
	if client != nil && client.PrincipalID != principalID { p.mu.RUnlock(); return nil,ClientPolicy{},false,false }
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
	return principal, policy, client != nil, true
}

// beginPolicyRequestReadOnly admits a request for authorization checks
// (CheckModel/CheckProvider) without consuming any client budget: no
// request counters, no concurrency slot, no token reservation. Used by
// traffic-experiment legs whose consumption is accounted to the experiment.
func (p *PassportStore) beginPolicyRequestReadOnly(principalID, clientID string, configured map[string]ClientPolicy) (*policyAdmission, error) {
	principal, policy, _, ok := p.resolveClientPolicy(principalID, clientID, configured)
	if !ok {
		return nil, nil
	}
	if principal == nil {
		return nil, &policyError{Status: http.StatusForbidden, Code: "policy_identity_missing", Message: "client policy identity is unavailable"}
	}
	priority := policy.Priority
	explicitPolicy := policy.configured()
	if priority == 0 {
		priority = defaultPolicyPriority(principal.Kind)
	}
	return &policyAdmission{store: nil, principalID: principalID, clientID: clientID, policy: policy, priority: priority, configured: explicitPolicy, readOnly: true}, nil
}

func (p *PassportStore) beginPolicyRequest(principalID, clientID string, configured map[string]ClientPolicy, now time.Time) (*policyAdmission, error) {
	return p.reservePolicyRequest(principalID, clientID, configured, now)
}

func policyTokenReservation(limits PolicyLimits, day, month policyUsageCounter, reserved int64) int64 {
	if limits.DailyTokens <= 0 && limits.MonthlyTokens <= 0 {
		return 0
	}
	estimate := limits.TokenReservation
	if limits.DailyTokens > 0 {
		headroom := limits.DailyTokens - day.Tokens - day.ReservedTokens - reserved
		if estimate > headroom {
			estimate = headroom
		}
	}
	if limits.MonthlyTokens > 0 {
		headroom := limits.MonthlyTokens - month.Tokens - month.ReservedTokens - reserved
		if estimate > headroom {
			estimate = headroom
		}
	}
	if estimate < 0 {
		return 0
	}
	return estimate
}

func (p *PassportStore) recordPolicyTokens(clientID string, tokens int64, now time.Time) error {
	if p == nil || clientID == "" || tokens <= 0 {
		return nil
	}
	p.mu.RLock()
	client := p.clients[clientID]
	var principalID string
	if client != nil {
		principalID = client.PrincipalID
	}
	p.mu.RUnlock()
	if principalID == "" {
		return errors.New("policy token usage references an unknown client")
	}
	_, dayKey, monthKey := policyUsageKeys(clientID, now)
	_, principalDay, principalMonth := policyUsageKeys("principal:"+principalID, now)
	return p.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketPassportPolicyUsage))
		for _, key := range []string{dayKey, monthKey, principalDay, principalMonth} {
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
