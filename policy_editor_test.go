package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEffectivePolicyIntersectsEverySource(t *testing.T) {
	p, client := testPolicyPassport(t)
	draft := ClientPolicy{Models: PolicySelector{Allow: []string{"gpt-5.5", "gpt-6-astra"}}, Limits: PolicyLimits{DailyRequests: 3}, Priority: 70}
	if err := p.saveEditedPolicy("operator", "policy-user", policyEditorRequest{Target: "principal", Policy: &draft}); err != nil {
		t.Fatal(err)
	}
	credential := ClientPolicy{Models: PolicySelector{Allow: []string{"gpt-5.5"}}, Limits: PolicyLimits{ConcurrentRequests: 1}, Priority: 30}
	if err := p.saveEditedPolicy("operator", "policy-user", policyEditorRequest{Revision: 1, Target: "client", ClientID: client.ID, Policy: &credential}); err != nil {
		t.Fatal(err)
	}
	configured := map[string]ClientPolicy{"*": {Providers: PolicySelector{Deny: []string{"grok"}}}, "role:member": {Limits: PolicyLimits{DailyRequests: 10}}, client.ID: {Models: PolicySelector{Deny: []string{"gpt-6-astra"}}}}
	a, err := p.beginPolicyRequest("policy-user", client.ID, configured, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	if a.priority != 30 || a.policy.Limits.DailyRequests != 3 || a.policy.Limits.ConcurrentRequests != 1 {
		t.Fatalf("effective policy: %+v", a.policy)
	}
	if err := a.CheckModel("gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	requirePolicyCode(t, a.CheckModel("gpt-6-astra"), "policy_model_denied")
	requirePolicyCode(t, a.CheckModel(""), "policy_model_required")
	requirePolicyCode(t, a.CheckProvider(AccountTypeGrok), "policy_provider_denied")
	combined, err := combinePolicySources([]policySource{{Policy: ClientPolicy{Models: PolicySelector{Allow: []string{"one"}}}}, {Policy: ClientPolicy{Models: PolicySelector{Allow: []string{"two"}}}}}, PrincipalMember)
	if err != nil || policyAllows(combined.Models, "one") || policyAllows(combined.Models, "two") {
		t.Fatal("disjoint allow lists widened access")
	}
}

func TestPolicyInvalidBindingsFailClosedIncludingShadow(t *testing.T) {
	p, client := testPolicyPassport(t)
	for _, configured := range []map[string]ClientPolicy{{"workstation": {Models: PolicySelector{Deny: []string{"*"}}}}, {client.ID: {Limits: PolicyLimits{DailyRequests: -1}}}, {"*": {Routing: PolicyRouting{Profile: "one"}}, client.ID: {Routing: PolicyRouting{Profile: "two"}}}} {
		if _, err := p.beginPolicyRequest("policy-user", client.ID, configured, time.Now()); err == nil {
			t.Fatal("invalid configuration admitted")
		}
		if _, err := p.beginPolicyRequestReadOnly("policy-user", client.ID, configured); err == nil {
			t.Fatal("invalid shadow configuration admitted")
		}
	}
	a, err := p.beginPolicyRequest("policy-user", client.ID, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	if a.configured {
		t.Fatal("default role priority treated as explicit policy")
	}
}

func TestPolicyAcceptsCanonicalQualifiedModelsAndRejectsDuplicates(t *testing.T) {
	if err := validateClientPolicy(ClientPolicy{Models: PolicySelector{Allow: []string{"antigravity/gemini-3-pro"}}}); err != nil {
		t.Fatal(err)
	}
	if err := validateClientPolicy(ClientPolicy{Models: PolicySelector{Allow: []string{"antigravity/gemini-3-pro", "gemini-3-pro"}}}); err == nil {
		t.Fatal("equivalent selectors duplicated")
	}
}

func TestPolicyPreviewChecksConservativeTokenCeiling(t *testing.T) {
	p, client := testPolicyPassport(t)
	h := &proxyHandler{passport: p, cfg: &config{}, pool: newPoolState(nil, false)}
	policy := ClientPolicy{Limits: PolicyLimits{DailyTokens: 1000, TokenReservation: 100}}
	view, err := h.policyEditorView("policy-user", &policyEditorRequest{Target: "principal", ClientID: client.ID, Policy: &policy, Model: "gpt-5.5", Provider: AccountTypeCodex, Tokens: 3})
	if err != nil {
		t.Fatal(err)
	}
	if view.Allowed || !strings.Contains(strings.Join(view.Reasons, " "), "consumption ceiling") {
		t.Fatal("preview ignored actual upstream ceiling")
	}
}

func TestPolicyPreviewIncludesGrantSourcesAndSeparateConsumption(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	grant := testGrant("preview", "bob", a)
	grant.Budget.DailyRequests = 1
	created, err := p.createAccountGrant("alice", 1, grant)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := p.reserveGrantRequest(created, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Release()
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	view, err := h.policyEditorView("bob", &policyEditorRequest{Provider: a.Type, AccountID: a.ID, Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Allowed || len(view.Usage) != 2 || view.Usage[1].Scope != "grant:preview" || view.Usage[1].Day.Requests != 1 || view.Sources[len(view.Sources)-1].Source != "grant:preview:1" {
		t.Fatalf("grant preview: %+v", view)
	}
}

func TestPolicyPreviewIsReadOnlyAndIncludesLiveBudget(t *testing.T) {
	p, client := testPolicyPassport(t)
	h := &proxyHandler{passport: p, cfg: &config{}, pool: newPoolState(nil, false)}
	policy := ClientPolicy{Limits: PolicyLimits{DailyRequests: 1, ConcurrentRequests: 1}}
	if err := p.saveEditedPolicy("operator", "policy-user", policyEditorRequest{Target: "principal", Policy: &policy}); err != nil {
		t.Fatal(err)
	}
	a, err := p.beginPolicyRequest("policy-user", client.ID, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	for range 3 {
		view, err := h.policyEditorView("policy-user", &policyEditorRequest{Revision: 1, ClientID: client.ID, Target: "principal", Policy: &policy})
		if err != nil {
			t.Fatal(err)
		}
		if view.Allowed || len(view.Usage) != 2 || view.Usage[0].Day.Requests != 1 || view.Usage[0].Inflight != 1 {
			t.Fatalf("preview: %+v", view)
		}
	}
	if p.principal("policy-user").PolicyRevision != 1 {
		t.Fatal("preview mutated policy")
	}
	_, err = h.policyEditorView("policy-user", &policyEditorRequest{Revision: 0, Target: "principal", Policy: &policy})
	requirePolicyCode(t, err, "policy_revision_conflict")
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.principal("policy-user").Budget.DailyRequests != 1 || reloaded.principal("policy-user").PolicyRevision != 1 {
		t.Fatal("policy not durable")
	}
}

func TestPolicyEditorAuthenticatedRevisionAndCSRF(t *testing.T) {
	p, _ := testPolicyPassport(t)
	cookie, csrf := addTestPassportOperator(t, p, "editor")
	h := &proxyHandler{passport: p, cfg: &config{}, pool: newPoolState(nil, false)}
	call := func(method, token, body string, preview bool) *httptest.ResponseRecorder {
		r := passportContributionRequest(method, "/api/console/policies/policy-user", cookie, token, []byte(body))
		w := httptest.NewRecorder()
		h.handlePolicyEditor(w, r, "policy-user", preview)
		return w
	}
	body := `{"revision":0,"target":"principal","policy":{"models":{"allow":["gpt-5.5"]},"limits":{"daily_requests":2}}}`
	if w := call("PUT", "", body, false); w.Code != 403 {
		t.Fatalf("CSRF: %d", w.Code)
	}
	if w := call("POST", csrf, body, true); w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if p.principal("policy-user").PolicyRevision != 0 {
		t.Fatal("preview wrote")
	}
	w := call("PUT", csrf, body, false)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var view policyEditorView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 || !strings.Contains(w.Body.String(), "principal:policy-user") {
		t.Fatal("revision/sources missing")
	}
	if w := call("PUT", csrf, body, false); w.Code != 409 {
		t.Fatalf("stale save: %d", w.Code)
	}
	if w := call("PUT", csrf, `{"revision":1,"target":"principal","unknown":1,"policy":{}}`, false); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
}
