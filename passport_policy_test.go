package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func testPolicyPassport(t *testing.T) (*PassportStore, *ClientCredential) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-policy-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal := &Principal{
		ID: "policy-user", Kind: PrincipalMember, Status: PrincipalActive,
		DisplayName: "Policy User", CreatedAt: time.Now().UTC(),
	}
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketPrincipals)), principal.ID, principal)
	}); err != nil {
		t.Fatal(err)
	}
	passport.mu.Lock()
	passport.principals[principal.ID] = principal
	passport.mu.Unlock()
	client, err := passport.createClient(principal.ID, "workstation", nil)
	if err != nil {
		t.Fatal(err)
	}
	return passport, client
}

func requirePolicyCode(t *testing.T, err error, code string) {
	t.Helper()
	var blocked *policyError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want policy error %q", err, code)
	}
	if blocked.Code != code {
		t.Fatalf("policy code = %q, want %q", blocked.Code, code)
	}
}

func TestClientPolicyModelProviderAndPriority(t *testing.T) {
	passport, client := testPolicyPassport(t)
	admission, err := passport.beginPolicyRequest("policy-user", client.ID, map[string]ClientPolicy{
		"workstation": {
			Models:    PolicySelector{Allow: []string{"gpt-5.6-sol"}},
			Providers: PolicySelector{Deny: []string{"grok"}},
			Routing:   PolicyRouting{Profile: "balanced"},
			Priority:  77,
		},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Release()
	if admission.priority != 77 || admission.policy.Routing.Profile != "balanced" {
		t.Fatalf("admission = %#v", admission)
	}
	if err := admission.CheckModel("gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	requirePolicyCode(t, admission.CheckModel("gpt-6-astra"), "policy_model_denied")
	requirePolicyCode(t, admission.CheckProvider(AccountTypeGrok), "policy_provider_denied")
}

func TestClientPolicyConcurrencyAndPersistentRequestBudget(t *testing.T) {
	passport, client := testPolicyPassport(t)
	policies := map[string]ClientPolicy{client.ID: {
		Limits: PolicyLimits{ConcurrentRequests: 1, RequestsPerMinute: 2},
	}}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	first, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	requirePolicyCode(t, err, "policy_concurrency_exceeded")
	first.Release()

	second, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	_, err = passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	requirePolicyCode(t, err, "policy_rate_limit_exceeded")

	reloaded, err := newPassportStore(passport.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reloaded.beginPolicyRequest("policy-user", client.ID, policies, now)
	requirePolicyCode(t, err, "policy_rate_limit_exceeded")
}

func TestClientPolicyPersistentTokenBudget(t *testing.T) {
	passport, client := testPolicyPassport(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := passport.recordPolicyTokens(client.ID, 300, now); err != nil {
		t.Fatal(err)
	}
	_, err := passport.beginPolicyRequest("policy-user", client.ID, map[string]ClientPolicy{
		client.ID: {Limits: PolicyLimits{DailyTokens: 300}},
	}, now)
	requirePolicyCode(t, err, "policy_daily_tokens_exceeded")
}

func TestPolicyErrorHasStableJSONShape(t *testing.T) {
	recorder := &jsonResponseRecorder{header: make(http.Header)}
	respondPolicyError(recorder, &policyError{Status: http.StatusForbidden, Code: "policy_model_denied", Message: "blocked"})
	var body map[string]map[string]any
	if err := json.Unmarshal(recorder.body, &body); err != nil {
		t.Fatal(err)
	}
	if recorder.status != http.StatusForbidden || body["error"]["code"] != "policy_model_denied" {
		t.Fatalf("status=%d body=%s", recorder.status, recorder.body)
	}
}

type jsonResponseRecorder struct {
	header http.Header
	body   []byte
	status int
}

func (r *jsonResponseRecorder) Header() http.Header    { return r.header }
func (r *jsonResponseRecorder) WriteHeader(status int) { r.status = status }
func (r *jsonResponseRecorder) Write(body []byte) (int, error) {
	r.body = append(r.body, body...)
	return len(body), nil
}
