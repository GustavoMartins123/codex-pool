package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

type policySource struct {
	Source string       `json:"source"`
	Policy ClientPolicy `json:"policy"`
}

func validateClientPolicy(policy ClientPolicy) error {
	if err := validatePolicyLimits(policy.Limits); err != nil {
		return err
	}
	if policy.Priority < 0 || policy.Priority > 100 {
		return errors.New("priority must be between 0 and 100")
	}
	for _, selector := range []PolicySelector{policy.Models, policy.Providers} {
		for _, values := range [][]string{selector.Allow, selector.Deny} {
			if len(values) > 100 {
				return errors.New("too many policy selectors")
			}
			seen := map[string]bool{}
			for _, v := range values {
				canonical := normalizePolicyValue(v)
				if v == "" || len(v) > 160 || strings.ToLower(strings.TrimSpace(v)) != v || canonical == "" || seen[canonical] {
					return errors.New("selectors must be unique normalized values")
				}
				seen[canonical] = true
			}
		}
	}
	return nil
}

func intersectPolicySelectors(a, b PolicySelector) PolicySelector {
	result := PolicySelector{Deny: append(append([]string{}, a.Deny...), b.Deny...)}
	switch {
	case len(a.Allow) == 0:
		result.Allow = append([]string{}, b.Allow...)
	case len(b.Allow) == 0:
		result.Allow = append([]string{}, a.Allow...)
	default:
		for _, v := range append(append([]string{}, a.Allow...), b.Allow...) {
			if policyMatches(a.Allow, v) && policyMatches(b.Allow, v) {
				result.Allow = append(result.Allow, v)
			}
		}
		if len(result.Allow) == 0 {
			result.Deny = append(result.Deny, "*")
		}
	}
	unique := func(values []string) []string {
		seen := map[string]bool{}
		out := []string{}
		for _, v := range values {
			if !seen[v] {
				out = append(out, v)
				seen[v] = true
			}
		}
		sort.Strings(out)
		return out
	}
	result.Allow, result.Deny = unique(result.Allow), unique(result.Deny)
	return result
}

func combinePolicySources(sources []policySource, kind PrincipalKind) (ClientPolicy, error) {
	result := ClientPolicy{}
	minimum := func(a, b int64) int64 {
		if a == 0 || b > 0 && b < a {
			return b
		}
		return a
	}
	for _, source := range sources {
		p := source.Policy
		if err := validateClientPolicy(p); err != nil {
			return ClientPolicy{}, fmt.Errorf("invalid %s policy: %w", source.Source, err)
		}
		result.Models = intersectPolicySelectors(result.Models, p.Models)
		result.Providers = intersectPolicySelectors(result.Providers, p.Providers)
		a, b := &result.Limits, p.Limits
		a.TokenReservation = minimum(a.TokenReservation, b.TokenReservation)
		a.RequestsPerMinute = int(minimum(int64(a.RequestsPerMinute), int64(b.RequestsPerMinute)))
		a.ConcurrentRequests = int(minimum(int64(a.ConcurrentRequests), int64(b.ConcurrentRequests)))
		a.DailyRequests = minimum(a.DailyRequests, b.DailyRequests)
		a.MonthlyRequests = minimum(a.MonthlyRequests, b.MonthlyRequests)
		a.DailyTokens = minimum(a.DailyTokens, b.DailyTokens)
		a.MonthlyTokens = minimum(a.MonthlyTokens, b.MonthlyTokens)
		if p.Priority > 0 {
			priority := min(p.Priority, defaultPolicyPriority(kind))
			if result.Priority == 0 || priority < result.Priority {
				result.Priority = priority
			}
		}
		if p.Routing.Profile != "" {
			if result.Routing.Profile != "" && p.Routing.Profile != result.Routing.Profile {
				return ClientPolicy{}, errors.New("conflicting routing profiles")
			}
			result.Routing = p.Routing
		}
	}
	return result, nil
}

func principalClientPolicy(principal *Principal) ClientPolicy {
	policy := principal.Policy
	policy.Limits = principal.Budget
	return policy
}

func policySourcesFor(principal *Principal, client *ClientCredential, configured map[string]ClientPolicy) []policySource {
	sources := []policySource{{Source: "global", Policy: configured["*"]}, {Source: "role:" + string(principal.Kind), Policy: configured["role:"+string(principal.Kind)]}, {Source: "principal:" + principal.ID, Policy: principalClientPolicy(principal)}}
	if client != nil {
		sources = append(sources, policySource{Source: "configuration:" + client.ID, Policy: configured[client.ID]}, policySource{Source: "credential:" + client.ID, Policy: client.Policy})
	}
	return sources
}

func (p *PassportStore) evaluateClientPolicy(principalID, clientID string, configured map[string]ClientPolicy) (*Principal, ClientPolicy, bool, error) {
	p.mu.RLock()
	if err := p.validatePolicyBindingsLocked(configured); err != nil {
		p.mu.RUnlock()
		return nil, ClientPolicy{}, false, err
	}
	pr := p.principals[principalID]
	client := p.clients[clientID]
	if pr == nil || clientID != "" && (client == nil || client.PrincipalID != principalID) {
		p.mu.RUnlock()
		return nil, ClientPolicy{}, false, errors.New("policy identity unavailable")
	}
	principal := *pr
	if principal.Status != PrincipalActive || principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now()) || client != nil && (client.Status != "active" || client.ExpiresAt != nil && !client.ExpiresAt.After(time.Now())) {
		p.mu.RUnlock()
		return nil, ClientPolicy{}, false, errors.New("policy identity inactive")
	}
	var credential *ClientCredential
	if client != nil {
		cp := *client
		credential = &cp
	}
	p.mu.RUnlock()
	policy, err := p.cachedEffectivePolicy(&principal, credential, configured)
	return &principal, policy, credential != nil, err
}

func (p *PassportStore) validatePolicyBindingsLocked(configured map[string]ClientPolicy) error {
	for key, policy := range configured {
		if key != "*" && key != "role:operator" && key != "role:member" && key != "role:guest" && p.clients[key] == nil {
			return fmt.Errorf("unknown policy binding %q", key)
		}
		if err := validateClientPolicy(policy); err != nil {
			return fmt.Errorf("invalid policy binding %q: %w", key, err)
		}
	}
	return nil
}

type policyEditorRequest struct {
	Revision  uint64        `json:"revision"`
	Target    string        `json:"target"`
	ClientID  string        `json:"client_id"`
	Policy    *ClientPolicy `json:"policy"`
	Model     string        `json:"model"`
	Provider  AccountType   `json:"provider"`
	AccountID string        `json:"account_id"`
	Tokens    int64         `json:"tokens"`
}

type policyBudgetProjection struct {
	Scope    string             `json:"scope"`
	Limits   PolicyLimits       `json:"limits"`
	Minute   policyUsageCounter `json:"minute"`
	Day      policyUsageCounter `json:"day"`
	Month    policyUsageCounter `json:"month"`
	Inflight int                `json:"inflight"`
}

type policyEditorView struct {
	PrincipalID     string                   `json:"principal_id"`
	Revision        uint64                   `json:"revision"`
	PrincipalPolicy ClientPolicy             `json:"principal_policy"`
	Clients         []clientCredentialView   `json:"clients"`
	Sources         []policySource           `json:"sources"`
	Effective       ClientPolicy             `json:"effective"`
	Allowed         bool                     `json:"allowed"`
	Reasons         []string                 `json:"reasons"`
	Usage           []policyBudgetProjection `json:"usage"`
}

func (h *proxyHandler) policyEditorView(principalID string, q *policyEditorRequest) (*policyEditorView, error) {
	p := h.passport
	p.mu.RLock()
	if err := p.validatePolicyBindingsLocked(h.cfg.hotClientPolicies()); err != nil {
		p.mu.RUnlock()
		return nil, &policyError{Status: 503, Code: "policy_configuration_invalid", Message: err.Error()}
	}
	pr := p.principals[principalID]
	if pr == nil {
		p.mu.RUnlock()
		return nil, accountControlError("principal_not_found", 404)
	}
	principal := *pr
	clients := []clientCredentialView{}
	var selected *ClientCredential
	for _, c := range p.clients {
		if c.PrincipalID == principalID {
			clients = append(clients, clientCredentialView{ID: c.ID, Label: c.Label, Status: c.Status, Policy: c.Policy, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt})
			if c.ID == q.ClientID {
				copy := *c
				selected = &copy
			}
		}
	}
	p.mu.RUnlock()
	sort.Slice(clients, func(i, j int) bool { return clients[i].ID < clients[j].ID })
	if q.ClientID != "" && selected == nil {
		return nil, accountControlError("client_not_found", 404)
	}
	if q.Policy != nil {
		if q.Revision != principal.PolicyRevision {
			return nil, accountControlError("policy_revision_conflict", 409)
		}
		if err := validateClientPolicy(*q.Policy); err != nil {
			return nil, &policyError{Status: 400, Code: "policy_invalid", Message: err.Error()}
		}
		if q.Policy.Routing.Profile != "" {
			if h.pool == nil {
				return nil, accountControlError("routing_unavailable", 503)
			}
			if _, ok := h.pool.resolveRoutingProfile(q.Policy.Routing.Profile); !ok {
				return nil, accountControlError("routing_profile_invalid", 400)
			}
		}
		switch q.Target {
		case "principal":
			principal.Policy = *q.Policy
			principal.Budget = q.Policy.Limits
		case "client":
			if selected == nil {
				return nil, accountControlError("client_required", 400)
			}
			selected.Policy = *q.Policy
		default:
			return nil, accountControlError("policy_target_invalid", 400)
		}
	}
	sources := policySourcesFor(&principal, selected, h.cfg.hotClientPolicies())
	policy, err := combinePolicySources(sources, principal.Kind)
	if err != nil {
		return nil, &policyError{Status: 400, Code: "policy_invalid", Message: err.Error()}
	}
	credentialPolicy := policy
	var grant *accountGrant
	var accountErr error
	if q.AccountID != "" {
		if q.Provider == "" {
			return nil, accountControlError("policy_provider_required", 400)
		}
		grant, accountErr = p.accountGrantForUse(principalID, &Account{ID: q.AccountID, Type: q.Provider}, q.Model)
		if accountErr == nil {
			_, accountErr = p.controlAdmission(q.Provider, q.AccountID, false, false)
		}
		if grant != nil {
			sources = append(sources, policySource{Source: fmt.Sprintf("grant:%s:%d", grant.ID, grant.Revision), Policy: ClientPolicy{Models: PolicySelector{Allow: grant.Models}, Limits: grant.Budget}})
			policy, err = combinePolicySources(sources, principal.Kind)
			if err != nil {
				return nil, err
			}
		}
	}
	view := &policyEditorView{PrincipalID: principalID, Revision: principal.PolicyRevision, PrincipalPolicy: principalClientPolicy(&principal), Clients: clients, Sources: sources, Effective: policy, Allowed: true, Reasons: []string{}}
	if accountErr != nil {
		view.Allowed = false
		view.Reasons = append(view.Reasons, accountErr.Error())
	}
	if q.Tokens < 0 {
		return nil, accountControlError("policy_tokens_invalid", 400)
	}
	projections := []policyBudgetProjection{{Scope: "principal:" + principalID, Limits: principal.Budget}}
	if selected != nil {
		projections = append(projections, policyBudgetProjection{Scope: selected.ID, Limits: credentialPolicy.Limits})
	}
	if grant != nil {
		projections = append(projections, policyBudgetProjection{Scope: "grant:" + grant.ID, Limits: grant.Budget})
	}
	p.policyMu.Lock()
	err = p.db.View(func(tx *bbolt.Tx) error {
		for i := range projections {
			projection := &projections[i]
			minute, day, month := policyUsageKeys(projection.Scope, time.Now().UTC())
			var err error
			b := tx.Bucket([]byte(bucketPassportPolicyUsage))
			if projection.Minute, err = readPolicyCounter(b, minute); err != nil {
				return err
			}
			if projection.Day, err = readPolicyCounter(b, day); err != nil {
				return err
			}
			if projection.Month, err = readPolicyCounter(b, month); err != nil {
				return err
			}
			projection.Inflight = p.policyInflight[projection.Scope]
		}
		return nil
	})
	p.policyMu.Unlock()
	if err != nil {
		return nil, accountControlError("policy_usage_unavailable", 503)
	}
	view.Usage = projections
	for _, projection := range projections {
		blocked := checkBudget(projection.Limits, projection.Minute, projection.Day, projection.Month)
		l := projection.Limits
		if l.ConcurrentRequests > 0 && projection.Inflight >= l.ConcurrentRequests {
			blocked = errors.New("concurrent request limit exhausted")
		}
		if q.Tokens > 0 && ((l.DailyTokens > 0 && q.Tokens > l.DailyTokens-projection.Day.Tokens-projection.Day.ReservedTokens) || (l.MonthlyTokens > 0 && q.Tokens > l.MonthlyTokens-projection.Month.Tokens-projection.Month.ReservedTokens)) {
			blocked = errors.New("requested tokens exceed remaining budget")
		}
		if blocked != nil {
			view.Allowed = false
			view.Reasons = append(view.Reasons, projection.Scope+": "+blocked.Error())
		}
	}
	admission := &policyAdmission{policy: policy}
	if q.Model != "" {
		if err := admission.CheckModel(q.Model); err != nil {
			view.Allowed = false
			view.Reasons = append(view.Reasons, err.Error())
		}
	}
	if q.Provider != "" {
		if !policyAllows(policy.Providers, string(q.Provider)) {
			view.Allowed = false
			view.Reasons = append(view.Reasons, "provider denied")
		}
		if policy.Limits.DailyTokens > 0 || policy.Limits.MonthlyTokens > 0 {
			model, known := modelForProvider(q.Provider, q.Model)
			if (q.Provider != AccountTypeCodex && q.Provider != AccountTypeClaude) || !known || model.ContextWindow <= 0 || model.MaxTokens <= 0 {
				view.Allowed = false
				view.Reasons = append(view.Reasons, "token consumption ceiling unavailable for sample request")
			} else {
				contextTokens := model.ContextWindow
				if contextTokens < 1000000 {
					contextTokens = 1000000
				}
				bound := int64(contextTokens) + int64(model.MaxTokens)
				for _, projection := range projections {
					l := projection.Limits
					if (l.DailyTokens > 0 && bound > l.DailyTokens-projection.Day.Tokens-projection.Day.ReservedTokens) || (l.MonthlyTokens > 0 && bound > l.MonthlyTokens-projection.Month.Tokens-projection.Month.ReservedTokens) {
						view.Allowed = false
						view.Reasons = append(view.Reasons, projection.Scope+": insufficient budget for the upstream consumption ceiling")
					}
				}
			}
		}
	}
	if principal.Status != PrincipalActive || principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now()) || selected != nil && (selected.Status != "active" || selected.ExpiresAt != nil && !selected.ExpiresAt.After(time.Now())) {
		view.Allowed = false
		view.Reasons = append(view.Reasons, "identity unavailable")
	}
	return view, nil
}

func (p *PassportStore) saveEditedPolicy(actor, principalID string, q policyEditorRequest) error {
	if q.Policy == nil {
		return accountControlError("policy_missing", 400)
	}
	if err := validateClientPolicy(*q.Policy); err != nil {
		return err
	}
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.principals[principalID]
	if pr == nil || q.Policy == nil {
		return accountControlError("policy_identity_missing", 404)
	}
	if q.Revision != pr.PolicyRevision {
		return accountControlError("policy_revision_conflict", 409)
	}
	updated := *pr
	updated.PolicyRevision++
	var client *ClientCredential
	var previous ClientPolicy
	switch q.Target {
	case "principal":
		previous = principalClientPolicy(pr)
		updated.Policy = *q.Policy
		updated.Policy.Limits = PolicyLimits{}
		updated.Budget = q.Policy.Limits
	case "client":
		c := p.clients[q.ClientID]
		if c == nil || c.PrincipalID != principalID {
			return accountControlError("client_not_found", 404)
		}
		copy := *c
		previous = c.Policy
		copy.Policy = *q.Policy
		client = &copy
	default:
		return accountControlError("policy_target_invalid", 400)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		if client != nil {
			if err := putJSON(tx.Bucket([]byte(bucketClientCredentials)), client.ID, client); err != nil {
				return err
			}
		}
		if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), updated.ID, updated); err != nil {
			return err
		}
		detail, err := json.Marshal(struct {
			Target   string       `json:"target"`
			ClientID string       `json:"client_id,omitempty"`
			Revision uint64       `json:"revision"`
			Before   ClientPolicy `json:"before"`
			After    ClientPolicy `json:"after"`
		}{q.Target, q.ClientID, updated.PolicyRevision, previous, *q.Policy})
		if err != nil {
			return err
		}
		return p.audit(tx, actor, "policy.updated", principalID, string(detail))
	}); err != nil {
		return err
	}
	p.principals[principalID] = &updated
	if client != nil {
		p.clients[client.ID] = client
	}
	return nil
}

func (h *proxyHandler) checkLivePolicy(identity, model string, provider AccountType) error {
	if h.passport == nil || identity == "" || identity == "break-glass" {
		return nil
	}
	principal, client := splitClientIdentity(identity)
	_, policy, _, err := h.passport.evaluateClientPolicy(principal, client, h.cfg.hotClientPolicies())
	if err != nil {
		return accountControlError("policy_unavailable", 503)
	}
	a := &policyAdmission{policy: policy}
	if err := a.CheckModel(model); err != nil {
		return err
	}
	return a.CheckProvider(provider)
}
