package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func testPolicyPassport(t *testing.T) (*PassportStore, *ClientCredential) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-policy-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
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

	reloaded, err := newPassportStore(passport.db)
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

// Regression test for the token-budget TOCTOU: concurrent admissions used to
// check committed tokens only, so N in-flight requests could each pass the
// check before any usage landed. Reservations must bound concurrent overshoot:
// with a budget of exactly three default reservations, a fourth admission
// while three are in flight is blocked, and releasing them restores headroom.
func TestClientPolicyTokenBudgetBlocksConcurrentOvershoot(t *testing.T) {
	passport, client := testPolicyPassport(t)
	policies := map[string]ClientPolicy{client.ID: {
		Limits: PolicyLimits{DailyTokens: 3 * defaultPolicyTokenReservation},
	}}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	var admitted []*policyAdmission
	for i := 0; i < 4; i++ {
		admission, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
		if err == nil {
			admitted = append(admitted, admission)
		}
	}
	if len(admitted) != 3 {
		t.Fatalf("admitted %d requests against a 3-reservation budget, want 3", len(admitted))
	}
	_, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	requirePolicyCode(t, err, "policy_daily_tokens_exceeded")

	for _, admission := range admitted {
		admission.Release()
	}
	after, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	if err != nil {
		t.Fatalf("release did not restore reserved headroom: %v", err)
	}
	after.Release()
}

// Same invariant under real concurrency: every goroutine passes the barrier,
// then races beginPolicyRequest. The serialized check-and-reserve must admit
// exactly three and block the rest — before the reservation fix every
// goroutine observed zero committed usage and was admitted.
func TestClientPolicyTokenBudgetConcurrentAdmissionsRace(t *testing.T) {
	passport, client := testPolicyPassport(t)
	policies := map[string]ClientPolicy{client.ID: {
		Limits: PolicyLimits{DailyTokens: 3 * defaultPolicyTokenReservation},
	}}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	const attempts = 12
	results := make([]*policyAdmission, attempts)
	failures := make([]error, attempts)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			results[i], failures[i] = passport.beginPolicyRequest("policy-user", client.ID, policies, now)
		}(i)
	}
	close(barrier)
	wg.Wait()

	admitted := 0
	for i := range results {
		if failures[i] == nil {
			admitted++
			results[i].Release()
		}
	}
	if admitted != 3 {
		t.Fatalf("concurrently admitted %d requests against a 3-reservation budget, want exactly 3", admitted)
	}
}

// Sequential request flow (admit -> debit -> release) must keep working
// exactly as before: the debit replaces the reservation once the request
// finishes, so follow-up admissions see committed usage, not a stuck hold.
func TestClientPolicyTokenBudgetSequentialDebitFlow(t *testing.T) {
	passport, client := testPolicyPassport(t)
	policies := map[string]ClientPolicy{client.ID: {
		Limits: PolicyLimits{DailyTokens: 500},
	}}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	first, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.reservedTokens <= 0 || first.reservedTokens > 500 {
		t.Fatalf("reservation = %d, want it bounded by the daily headroom", first.reservedTokens)
	}
	if err := passport.recordPolicyTokens(client.ID, 300, now); err != nil {
		t.Fatal(err)
	}
	first.Release()

	second, err := passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	if err != nil {
		t.Fatalf("sequential admission after debit+release failed: %v", err)
	}
	second.Release()

	if err := passport.recordPolicyTokens(client.ID, 200, now); err != nil {
		t.Fatal(err)
	}
	_, err = passport.beginPolicyRequest("policy-user", client.ID, policies, now)
	requirePolicyCode(t, err, "policy_daily_tokens_exceeded")
}

func TestPolicyTokenReservationEstimate(t *testing.T) {
	limits := PolicyLimits{DailyTokens: 100_000}
	day := policyUsageCounter{Requests: 4, Tokens: 40_000}
	if got := policyTokenReservation(limits, day, policyUsageCounter{}, 0); got != 10_000 {
		t.Fatalf("reservation = %d, want the observed average 10000", got)
	}
	capped := policyTokenReservation(limits, policyUsageCounter{Requests: 4, Tokens: 95_000}, policyUsageCounter{}, 0)
	if capped != 5_000 {
		t.Fatalf("reservation = %d, want it capped by the 5000 headroom", capped)
	}
	if got := policyTokenReservation(PolicyLimits{}, day, policyUsageCounter{}, 0); got != 0 {
		t.Fatalf("reservation = %d without token limits, want 0", got)
	}
	if got := policyTokenReservation(limits, policyUsageCounter{}, policyUsageCounter{}, 0); got != defaultPolicyTokenReservation {
		t.Fatalf("reservation = %d without history, want the default %d", got, defaultPolicyTokenReservation)
	}
	monthly := policyTokenReservation(PolicyLimits{MonthlyTokens: 30_000}, policyUsageCounter{}, policyUsageCounter{Requests: 3, Tokens: 21_000}, 0)
	if monthly != 7_000 {
		t.Fatalf("monthly reservation = %d, want the observed average 7000", monthly)
	}
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
