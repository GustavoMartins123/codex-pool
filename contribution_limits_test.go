package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestContributionAttemptsAreDurableConcurrentAndIsolated(t *testing.T) {
	p, _ := ownershipFixture(t)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.admitContributionAttempt("alice", now)
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else {
				var pe *policyError
				if !errors.As(err, &pe) || pe.Status != 429 {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if accepted != contributionAttemptsPerHour {
		t.Fatalf("accepted %d", accepted)
	}
	restarted, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.admitContributionAttempt("alice", now) == nil {
		t.Fatal("restart reset rate limit")
	}
	if err := restarted.admitContributionAttempt("bob", now); err != nil {
		t.Fatal("foreign actor consumed limit", err)
	}
	if err := restarted.admitContributionAttempt("alice", now.Add(time.Hour)); err != nil {
		t.Fatal("window did not reset", err)
	}
}

func TestContributionAccountLimitAndExpiredPendingRemainClosed(t *testing.T) {
	base, _ := url.Parse("https://provider.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, nil, nil, NewKimiProvider(base))}
	attachContributionFixture(t, h, "alice")
	for i := range contributionAccountsPerPrincipal {
		identity := fmt.Sprintf("key-%d", i)
		if _, err := h.persistContribution("alice", AccountTypeKimi, identity, func(string) error { return errors.New("interrupted") }); err == nil {
			t.Fatal("failure swallowed")
		}
	}
	called := false
	if _, err := h.persistContribution("alice", AccountTypeKimi, "overflow", func(string) error { called = true; return nil }); err == nil || called {
		t.Fatal("account limit bypassed")
	}
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		return b.ForEach(func(k, v []byte) error {
			var r accountResource
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			expired := time.Now().Add(-time.Minute)
			r.PendingExpiresAt = &expired
			return putJSON(b, string(k), r)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.persistContribution("alice", AccountTypeKimi, "key-0", func(string) error { called = true; return nil }); err == nil || called {
		t.Fatal("expired pending resumed")
	}
	if _, err := h.persistContribution("alice", AccountTypeKimi, "new", func(path string) error { return writeAccountFile(path, []byte(`{"api_key":"new"}`)) }); err != nil {
		t.Fatal("expired pending blocked capacity", err)
	}
}
