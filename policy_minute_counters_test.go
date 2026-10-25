package main

import (
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func policyUsageKeysWithPrefix(t *testing.T, p *PassportStore, prefix string) []string {
	t.Helper()
	var keys []string
	if err := p.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketPassportPolicyUsage)).ForEach(func(k, _ []byte) error {
			if strings.HasPrefix(string(k), prefix) {
				keys = append(keys, string(k))
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestMinuteCountersArePrunedOnReservation(t *testing.T) {
	p, client := testPolicyPassport(t)
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		admission, err := p.beginPolicyRequest("policy-user", client.ID, nil, start.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		admission.Release()
	}
	last := start.Add(2 * time.Minute)
	clientMinute, clientDay, _ := policyUsageKeys(client.ID, last)
	principalMinute, _, _ := policyUsageKeys("principal:policy-user", last)
	keys := policyUsageKeysWithPrefix(t, p, policyMinutePrefix)
	if len(keys) != 2 || !strings.Contains(strings.Join(keys, " "), clientMinute) || !strings.Contains(strings.Join(keys, " "), principalMinute) {
		t.Fatalf("minute counters = %v, want only %s and %s", keys, clientMinute, principalMinute)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		day, err := readPolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), clientDay)
		if err == nil && day.Requests != 3 {
			t.Fatalf("daily requests = %d, want 3 (pruning must not touch day counters)", day.Requests)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupSweepRemovesStaleMinuteCounters(t *testing.T) {
	p, client := testPolicyPassport(t)
	now := time.Now().UTC()
	stale := policyMinutePrefix + client.ID + "|" + now.Add(-48*time.Hour).Format("200601021504")
	current, day, month := policyUsageKeys(client.ID, now)
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		for _, key := range []string{stale, current, day, month} {
			if err := writePolicyCounter(b, key, policyUsageCounter{Requests: 1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.sweepMinuteCounters(now); err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(policyUsageKeysWithPrefix(t, p, ""), " ")
	if strings.Contains(keys, stale) {
		t.Fatalf("stale minute counter survived the sweep: %s", keys)
	}
	for _, key := range []string{current, day, month} {
		if !strings.Contains(keys, key) {
			t.Fatalf("sweep removed live counter %s: %s", key, keys)
		}
	}
}
