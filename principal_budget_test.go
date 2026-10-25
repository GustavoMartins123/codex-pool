package main

import (
	"go.etcd.io/bbolt"
	"sync"
	"testing"
	"time"
)

func setPrincipalBudget(t *testing.T, p *PassportStore, id string, limits PolicyLimits) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	updated := *p.principals[id]
	updated.Budget = limits
	if err := p.db.Update(func(tx *bbolt.Tx) error { return putJSON(tx.Bucket([]byte(bucketPrincipals)), id, updated) }); err != nil {
		t.Fatal(err)
	}
	p.principals[id] = &updated
}

func TestPrincipalBudgetCannotBeMultipliedByCredentials(t *testing.T) {
	p, first := testPolicyPassport(t)
	second, err := p.createClient("policy-user", "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{DailyRequests: 3, ConcurrentRequests: 3})
	now := time.Date(2026, 9, 29, 23, 0, 0, 0, time.FixedZone("local", -3*3600))
	var wg sync.WaitGroup
	results := make(chan *policyAdmission, 12)
	for i := range 12 {
		wg.Go(func() {
			client := first
			if i%2 == 1 {
				client = second
			}
			admission, err := p.beginPolicyRequest("policy-user", client.ID, nil, now)
			if err == nil {
				results <- admission
			}
		})
	}
	wg.Wait()
	close(results)
	count := 0
	for admission := range results {
		count++
		admission.Release()
	}
	if count != 3 {
		t.Fatalf("admitted=%d, want 3 across credentials", count)
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reloaded.beginPolicyRequest("policy-user", second.ID, nil, now)
	requirePolicyCode(t, err, "policy_daily_requests_exceeded")
	next, err := reloaded.beginPolicyRequest("policy-user", first.ID, nil, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next.Release()
}

func TestPrincipalTokenReservationsSurviveRestart(t *testing.T) {
	p, first := testPolicyPassport(t)
	second, err := p.createClient("policy-user", "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{DailyTokens: 100, MonthlyTokens: 100, TokenReservation: 100})
	now := time.Date(2026, 9, 29, 23, 59, 0, 0, time.UTC)
	admission, err := p.beginPolicyRequest("policy-user", first.ID, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reloaded.beginPolicyRequest("policy-user", second.ID, nil, now)
	requirePolicyCode(t, err, "policy_daily_tokens_exceeded")
	if err := p.recordPolicyTokens(first.ID, 100, now); err != nil {
		t.Fatal(err)
	}
	admission.Release()
	_, err = reloaded.beginPolicyRequest("policy-user", second.ID, nil, now.Add(2*time.Minute))
	requirePolicyCode(t, err, "policy_monthly_tokens_exceeded")
}

func TestFailedReservationReleaseFreesConcurrencySlot(t *testing.T) {
	p, client := testPolicyPassport(t)
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{ConcurrentRequests: 1, DailyTokens: 1000, MonthlyTokens: 1000, TokenReservation: 10})
	now := time.Now().UTC()
	admission, err := p.beginPolicyRequest("policy-user", client.ID, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	// Drift the persisted reservation so the release transaction fails.
	_, day, _ := policyUsageKeys("principal:policy-user", now)
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		counter, err := readPolicyCounter(b, day)
		if err != nil {
			return err
		}
		counter.ReservedTokens = 0
		return writePolicyCounter(b, day, counter)
	}); err != nil {
		t.Fatal(err)
	}
	admission.Release()
	next, err := p.beginPolicyRequest("policy-user", client.ID, nil, now)
	if err != nil {
		t.Fatalf("finished request kept its concurrency slot after a failed release: %v", err)
	}
	next.Release()
}

func TestBudgetReservationRollbackAndExplicitConfiguration(t *testing.T) {
	p, client := testPolicyPassport(t)
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{DailyRequests: 1})
	now := time.Now().UTC()
	first, err := p.beginPolicyRequest("policy-user", client.ID, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	_, err = p.beginPolicyRequest("policy-user", client.ID, nil, now)
	requirePolicyCode(t, err, "policy_daily_requests_exceeded")
	if err := p.db.View(func(tx *bbolt.Tx) error {
		_, key, _ := policyUsageKeys(client.ID, now)
		counter, err := readPolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), key)
		if err != nil {
			return err
		}
		if counter.Requests != 1 {
			t.Fatalf("denied request consumed client budget: %d", counter.Requests)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{DailyTokens: 100})
	if _, err := p.beginPolicyRequest("policy-user", client.ID, nil, now); err == nil {
		t.Fatal("implicit token reservation accepted")
	}
}

func TestGuestRequestsSpendSponsorBudget(t *testing.T) {
	p, sponsorClient := testPolicyPassport(t)
	expires := time.Now().Add(24 * time.Hour)
	guest, _, guestClient, _, err := p.createGuest("policy-user", "friend", "F", &expires)
	if err != nil {
		t.Fatal(err)
	}
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{DailyRequests: 2})
	now := time.Now().UTC()
	for i := range 2 {
		admission, err := p.beginPolicyRequest(guest.ID, guestClient.ID, nil, now)
		if err != nil {
			t.Fatalf("guest request %d: %v", i, err)
		}
		admission.Release()
	}
	_, err = p.beginPolicyRequest(guest.ID, guestClient.ID, nil, now)
	requirePolicyCode(t, err, "policy_daily_requests_exceeded")
	_, err = p.beginPolicyRequest("policy-user", sponsorClient.ID, nil, now)
	requirePolicyCode(t, err, "policy_daily_requests_exceeded")

	if err := p.recordPolicyTokens(guestClient.ID, 50, now); err != nil {
		t.Fatal(err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		_, day, _ := policyUsageKeys("principal:policy-user", now)
		counter, err := readPolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), day)
		if err == nil && counter.Tokens != 50 {
			t.Fatalf("sponsor tokens = %d, want the guest's 50", counter.Tokens)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
