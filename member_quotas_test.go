package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func memberQuotaFixture(t *testing.T) (*proxyHandler, string, string, string, string, string) {
	t.Helper()
	p, operator := testPassportWithOperator(t)
	opSession, opCSRF, err := p.createSession(operator.ID)
	if err != nil {
		t.Fatal(err)
	}
	link, err := p.createMemberLink(operator.ID, "quota-member@example.test", "Quota member", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	session, csrf, err := p.createSession(link.Principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	u := UsageSnapshot{PrimaryUsedPercent: .25, SecondaryUsedPercent: .6, PrimaryUsageReported: true, SecondaryUsageReported: true,
		primarySet: true, secondarySet: true, RetrievedAt: now, PrimaryResetAt: now.Add(time.Hour), SecondaryResetAt: now.Add(24 * time.Hour)}
	accounts := []*Account{
		{ID: "private-claude-secret-id", Type: AccountTypeClaude, AccessToken: "never-expose-token", Email: "owner@example.test", Usage: u},
		{ID: "private-codex-secret-id", Type: AccountTypeCodex, Usage: u},
		{ID: "foreign-claude-secret-id", Type: AccountTypeClaude, Usage: UsageSnapshot{PrimaryUsedPercent: .99, PrimaryUsageReported: true, primarySet: true, RetrievedAt: now}},
	}
	if err := p.initializeAccountAuthority(accounts); err != nil {
		t.Fatal(err)
	}
	for i, model := range []string{"claude-opus-5-5", "gpt-5.6-sol"} {
		grant := testGrant([]string{"quota-claude", "quota-codex"}[i], link.Principal.ID, accounts[i])
		grant.Models = []string{model}
		if _, err := p.createAccountGrant(operator.ID, 1, grant); err != nil {
			t.Fatal(err)
		}
	}
	pool := newPoolState(accounts, false)
	pool.accountAuthority = p
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	return h, link.Principal.ID, session, csrf, opSession, opCSRF
}

func quotaHTTP(t *testing.T, h *proxyHandler, session string) []memberModelQuota {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas?principal_id=someone-else&provider=codex", session, "", nil))
	if w.Code != 200 {
		t.Fatalf("quota: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("quota response is cacheable")
	}
	for _, secret := range []string{"secret-id", "never-expose-token", "owner@example", "recipient_id", "access_token", "secret_ref", "grants"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
	var body struct {
		Models []memberModelQuota `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Models == nil {
		t.Fatal("models must be a JSON array")
	}
	return body.Models
}

func saveQuotaPolicy(t *testing.T, h *proxyHandler, id string, policy ClientPolicy) {
	t.Helper()
	if err := h.passport.saveEditedPolicy("test-operator", id, policyEditorRequest{Revision: h.passport.principal(id).PolicyRevision, Target: "principal", Policy: &policy}); err != nil {
		t.Fatal(err)
	}
}

func TestMemberQuotaPermissionsAndRevocation(t *testing.T) {
	h, id, session, _, _, _ := memberQuotaFixture(t)
	if rows := quotaHTTP(t, h, session); len(rows) != 0 {
		t.Fatal("new member has implicit access")
	}
	client, err := h.passport.createClient(id, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := h.passport.beginPolicyRequestReadOnly(id, client.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	requirePolicyCode(t, admission.CheckProvider(AccountTypeClaude), "policy_provider_denied")
	saveQuotaPolicy(t, h, id, ClientPolicy{Providers: PolicySelector{Allow: []string{"claude"}}})
	rows := quotaHTTP(t, h, session)
	if len(rows) != 1 || rows[0].Model != "claude-opus-5-5" || rows[0].Primary == nil || rows[0].Primary.RemainingPercent != 75 || rows[0].SupportingAccounts != 1 || rows[0].Secondary.RemainingPercent != 40 {
		t.Fatalf("wrong authorized quota: %+v", rows)
	}
	admission, err = h.passport.beginPolicyRequestReadOnly(id, client.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.CheckProvider(AccountTypeClaude); err != nil {
		t.Fatal(err)
	}
	requirePolicyCode(t, admission.CheckProvider(AccountTypeCodex), "policy_provider_denied")
	saveQuotaPolicy(t, h, id, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	if rows := quotaHTTP(t, h, session); len(rows) != 2 {
		t.Fatalf("allow all: %+v", rows)
	}
	saveQuotaPolicy(t, h, id, ClientPolicy{Models: PolicySelector{Deny: []string{"claude-opus-5-5"}}, Providers: PolicySelector{Allow: []string{"*"}}})
	if rows := quotaHTTP(t, h, session); len(rows) != 1 || rows[0].Provider != "codex" {
		t.Fatal("model deny did not apply")
	}
	saveQuotaPolicy(t, h, id, ClientPolicy{Providers: PolicySelector{Allow: []string{"claude"}}})
	if err := h.passport.revokeAccountGrant(h.passport.principal(id).ID, "quota-claude", 1); err == nil {
		t.Fatal("member revoked owner grant")
	}
	grant, err := h.passport.accountGrantForUse(id, h.pool.allAccounts()[0], "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.passport.revokeAccountGrant(grant.CreatedBy, grant.ID, 1); err != nil {
		t.Fatal(err)
	}
	if rows := quotaHTTP(t, h, session); len(rows) != 0 {
		t.Fatal("revoked grant still visible")
	}
	// Revoked access stays revoked after reopening persisted state.
	reopened, err := newPassportStore(h.passport.db)
	if err != nil {
		t.Fatal(err)
	}
	h.passport, h.pool.accountAuthority = reopened, reopened
	if rows := quotaHTTP(t, h, session); len(rows) != 0 {
		t.Fatal("restart restored revoked access")
	}
}

func TestMemberQuotaHTTPBoundary(t *testing.T) {
	h, id, session, csrf, operator, operatorCSRF := memberQuotaFixture(t)
	for _, tc := range []struct {
		method, path, session, csrf string
		body                        []byte
		status                      int
	}{
		{"GET", "/api/me/quotas", "", "", nil, 401},
		{"GET", "/api/me/quotas", "forged-session", "", nil, 401},
		{"POST", "/api/me/quotas", session, csrf, nil, 405},
		{"GET", "/admin/accounts", session, "", nil, 403},
		{"GET", "/api/pool/stats", session, "", nil, 403},
		{"GET", "/api/console/policies/" + id, session, "", nil, 403},
		{"PUT", "/api/console/policies/" + id, session, csrf, []byte(`{"revision":0,"target":"principal","policy":{"providers":{"allow":["*"]}}}`), 403},
		{"PUT", "/api/console/policies/" + id, operator, "", []byte(`{"revision":0,"target":"principal","policy":{"providers":{"allow":["*"]}}}`), 403},
		{"PUT", "/api/console/policies/" + id, operator, operatorCSRF, []byte(`{"revision":0,"target":"principal","policy":{"providers":{"allow":["claude"]}}}`), 200},
		{"PUT", "/api/console/policies/" + id, operator, operatorCSRF, []byte(`{"revision":0,"target":"principal","policy":{"providers":{"allow":["*"]}}}`), 409},
	} {
		t.Run(tc.method+tc.path+tc.session[:min(3, len(tc.session))], func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, passportContributionRequest(tc.method, tc.path, tc.session, tc.csrf, tc.body))
			if w.Code != tc.status {
				t.Fatalf("got %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	guest, _, _, _, err := h.passport.createGuest("test", "guest", "guest", nil)
	if err != nil {
		t.Fatal(err)
	}
	guestSession, _, err := h.passport.createSession(guest.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas", guestSession, "", nil))
	if w.Code != 403 {
		t.Fatalf("guest: %d", w.Code)
	}
	h.cfg.clientPolicies = map[string]ClientPolicy{"unknown-credential": {}}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas", session, "", nil))
	if w.Code != 503 {
		t.Fatal("invalid policy configuration did not fail explicitly")
	}
	h.cfg.clientPolicies = nil
	if _, err := h.passport.setPrincipalStatus("test", id, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas", session, "", nil))
	if w.Code != 401 {
		t.Fatal("suspended member retained quota access")
	}
}

func TestMemberQuotaWindowsFailClosed(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name           string
		used           float64
		reported       bool
		updated, reset time.Time
		known          bool
	}{
		{"zero usage", 0, true, now, now.Add(time.Hour), true},
		{"exhausted", 1, true, now, now.Add(time.Hour), true},
		{"missing", 0, false, now, time.Time{}, false},
		{"stale", .2, true, now.Add(-16 * time.Minute), time.Time{}, false},
		{"reset passed", .2, true, now, now.Add(-time.Second), false},
		{"missing time", .2, true, time.Time{}, time.Time{}, false},
		{"future time", .2, true, now.Add(time.Minute), time.Time{}, false},
		{"nan", math.NaN(), true, now, time.Time{}, false},
		{"infinity", math.Inf(1), true, now, time.Time{}, false},
		{"negative", -.1, true, now, time.Time{}, false},
		{"over one", 1.1, true, now, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var window *memberQuotaWindow
			addMemberQuotaWindow(&window, tc.used, tc.reported, tc.updated, tc.reset, now)
			if (window != nil) != tc.known {
				t.Fatalf("window: %+v", window)
			}
			if window != nil && window.RemainingPercent != (1-tc.used)*100 {
				t.Fatal("incorrect percentage")
			}
		})
	}
}

func TestMemberQuotaUsesReportedProviderData(t *testing.T) {
	h, id, session, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, id, ClientPolicy{Providers: PolicySelector{Allow: []string{"claude"}}})
	a := h.pool.allAccounts()[0]
	a.AccessToken = "sk-ant-oat-test-only"
	h.cfg.claudeBase, _ = url.Parse("http://provider.invalid")
	body := `{"five_hour":{"utilization":0,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":100,"resets_at":"2099-01-02T00:00:00Z"}}`
	h.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	if err := h.fetchClaudeUsage(time.Now().UTC(), a); err != nil {
		t.Fatal(err)
	}
	rows := quotaHTTP(t, h, session)
	if len(rows) != 1 || rows[0].Primary == nil || rows[0].Secondary == nil || rows[0].Primary.RemainingPercent != 100 || rows[0].Secondary.RemainingPercent != 0 {
		t.Fatalf("reported Claude zero/full quota: %+v", rows)
	}
	// An empty refresh must not re-label old utilization as newly reported.
	body = `{}`
	if err := h.fetchClaudeUsage(time.Now().UTC(), a); err != nil {
		t.Fatal(err)
	}
	rows = quotaHTTP(t, h, session)
	if rows[0].Primary != nil || rows[0].Secondary != nil {
		t.Fatal("missing Claude data became a percentage")
	}
	snapshot, ok := normalizeCodexUsageWindows(&codexUsageWindow{UsedPercent: 0, WindowMinutes: 300}, &codexUsageWindow{UsedPercent: .4, WindowMinutes: 10080}, time.Now(), "wham")
	if !ok || !snapshot.PrimaryUsageReported || !snapshot.SecondaryUsageReported {
		t.Fatal("Codex windows lost reporting metadata")
	}
	provider := &ClaudeProvider{}
	provider.ParseUsageHeaders(a, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.25"}, "Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.6"}})
	if !a.Usage.PrimaryUsageReported || !a.Usage.SecondaryUsageReported || !a.Usage.primarySet || !a.Usage.secondarySet {
		t.Fatal("Claude headers lost reporting metadata")
	}
}

func TestMemberQuotaConfigurationAndAuthority(t *testing.T) {
	h, id, session, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, id, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	h.cfg.clientPolicies = map[string]ClientPolicy{"*": {Providers: PolicySelector{Deny: []string{"claude"}}}}
	rows := quotaHTTP(t, h, session)
	if len(rows) != 1 || rows[0].Provider != "codex" {
		t.Fatal("global provider denial ignored")
	}
	h.cfg.clientPolicies = map[string]ClientPolicy{"role:member": {Models: PolicySelector{Deny: []string{"gpt-5.6-sol"}}}}
	rows = quotaHTTP(t, h, session)
	if len(rows) != 1 || rows[0].Provider != "claude" {
		t.Fatal("role model denial ignored")
	}
	h.pool.accountAuthority = nil
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest("GET", "/api/me/quotas", session, "", nil))
	if w.Code != 503 {
		t.Fatal("missing authority did not fail explicitly")
	}
}
