package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestBruteForceTracker(t *testing.T) *bruteForceTracker {
	t.Helper()
	tracker := newBruteForceTracker()
	t.Cleanup(tracker.stop)
	return tracker
}

func banForTest(tracker *bruteForceTracker, ip string) {
	for i := 0; i < bruteForceMaxAttempts; i++ {
		tracker.recordFailure(ip)
	}
}

func TestBruteForceFloodKeepsActiveBans(t *testing.T) {
	tracker := newTestBruteForceTracker(t)
	banForTest(tracker, "198.51.100.7")
	for i := 0; len(tracker.attempts) < maxTrackedBruteForceIPs; i++ {
		tracker.recordFailure(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	for i := 0; i < 64; i++ {
		tracker.recordFailure(fmt.Sprintf("172.16.%d.%d", i>>8&255, i&255))
	}
	if !tracker.isBanned("198.51.100.7") {
		t.Fatal("flooding the tracker with fresh addresses lifted an active ban")
	}
	if len(tracker.attempts) > maxTrackedBruteForceIPs {
		t.Fatalf("tracked %d addresses, limit %d", len(tracker.attempts), maxTrackedBruteForceIPs)
	}
}

func TestBruteForceGroupsIPv6By64(t *testing.T) {
	tracker := newTestBruteForceTracker(t)
	for i := 1; i <= bruteForceMaxAttempts; i++ {
		tracker.recordFailure(fmt.Sprintf("2001:db8:0:1::%x", i))
	}
	if !tracker.isBanned("2001:db8:0:1:ffff:ffff:ffff:ffff") {
		t.Fatal("rotating addresses inside one /64 avoided the ban")
	}
	if tracker.isBanned("2001:db8:0:2::1") {
		t.Fatal("ban leaked into a neighbouring /64")
	}
	banForTest(tracker, "::ffff:203.0.113.9")
	if !tracker.isBanned("203.0.113.9") || tracker.isBanned("203.0.113.10") {
		t.Fatal("IPv4-mapped addresses must share the IPv4 key and nothing else")
	}
}

func TestAdminTokenRefusedFromBannedPeer(t *testing.T) {
	tracker := newTestBruteForceTracker(t)
	h := &proxyHandler{cfg: &config{adminToken: "break-glass-admin-token"}, bruteForce: tracker}
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	banForTest(tracker, getClientIP(req))

	req.Header.Set("X-Admin-Token", "break-glass-admin-token")
	rr := httptest.NewRecorder()
	if h.checkAdminAuth(rr, req) {
		t.Fatal("banned peer authenticated with the admin token")
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if !tracker.isBanned(getClientIP(req)) {
		t.Fatal("a correct guess from a banned peer cleared the ban")
	}

	contribution := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/codex/add", nil)
	contribution.Header.Set("X-Admin-Token", "break-glass-admin-token")
	rr = httptest.NewRecorder()
	if h.checkProviderContributionAuth(rr, contribution) || rr.Code != http.StatusTooManyRequests {
		t.Fatalf("contribution auth from banned peer: status = %d, want 429", rr.Code)
	}
}
