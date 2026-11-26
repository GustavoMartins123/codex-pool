package bruteforce

import (
	"fmt"
	"testing"
)

func newTestTracker(t *testing.T) *Tracker {
	t.Helper()
	tracker := New()
	t.Cleanup(tracker.Stop)
	return tracker
}

func banForTest(tracker *Tracker, ip string) {
	for i := 0; i < MaxAttempts; i++ {
		tracker.RecordFailure(ip)
	}
}

func TestFloodKeepsActiveBans(t *testing.T) {
	tracker := newTestTracker(t)
	banForTest(tracker, "198.51.100.7")
	for i := 0; len(tracker.attempts) < MaxTrackedClients; i++ {
		tracker.RecordFailure(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	for i := 0; i < 64; i++ {
		tracker.RecordFailure(fmt.Sprintf("172.16.%d.%d", i>>8&255, i&255))
	}
	if !tracker.IsBanned("198.51.100.7") {
		t.Fatal("flooding the tracker with fresh addresses lifted an active ban")
	}
	if len(tracker.attempts) > MaxTrackedClients {
		t.Fatalf("tracked %d addresses, limit %d", len(tracker.attempts), MaxTrackedClients)
	}
}

func TestGroupsIPv6By64(t *testing.T) {
	tracker := newTestTracker(t)
	for i := 1; i <= MaxAttempts; i++ {
		tracker.RecordFailure(fmt.Sprintf("2001:db8:0:1::%x", i))
	}
	if !tracker.IsBanned("2001:db8:0:1:ffff:ffff:ffff:ffff") {
		t.Fatal("rotating addresses inside one /64 avoided the ban")
	}
	if tracker.IsBanned("2001:db8:0:2::1") {
		t.Fatal("ban leaked into a neighbouring /64")
	}
	banForTest(tracker, "::ffff:203.0.113.9")
	if !tracker.IsBanned("203.0.113.9") || tracker.IsBanned("203.0.113.10") {
		t.Fatal("IPv4-mapped addresses must share the IPv4 key and nothing else")
	}
}

func TestTrackerBoundedAndSafeStop(t *testing.T) {
	bf := New()
	defer bf.Stop()

	// Fill with more than MaxTrackedClients.
	for i := 0; i < MaxTrackedClients+500; i++ {
		bf.RecordFailure(fmt.Sprintf("192.168.%d.%d", (i>>8)&255, i&255))
	}

	bf.mu.Lock()
	count := len(bf.attempts)
	bf.mu.Unlock()

	if count > MaxTrackedClients {
		t.Fatalf("expected brute force attempts bounded to <= %d, got %d", MaxTrackedClients, count)
	}

	// Multiple calls to Stop must not panic.
	bf.Stop()
	bf.Stop()
}
