package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/bruteforce"
	"codex-pool-proxy/internal/clientip"
	"go.etcd.io/bbolt"
)

func newTestBruteForceTracker(t *testing.T) *bruteforce.Tracker {
	t.Helper()
	tracker := bruteforce.New()
	t.Cleanup(tracker.Stop)
	return tracker
}

func banForTest(tracker *bruteforce.Tracker, ip string) {
	for i := 0; i < bruteforce.MaxAttempts; i++ {
		tracker.RecordFailure(ip)
	}
}

func TestAdminTokenRefusedFromBannedPeer(t *testing.T) {
	tracker := newTestBruteForceTracker(t)
	h := &proxyHandler{cfg: &config{adminToken: "break-glass-admin-token"}, bruteForce: tracker}
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req = clientip.With(req, testResolvedClientIP(t, h.ipAccess.Snapshot(), req))
	banForTest(tracker, clientip.Get(req))

	req.Header.Set("X-Admin-Token", "break-glass-admin-token")
	rr := httptest.NewRecorder()
	if h.checkAdminAuth(rr, req) {
		t.Fatal("banned peer authenticated with the admin token")
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if !tracker.IsBanned(clientip.Get(req)) {
		t.Fatal("a correct guess from a banned peer cleared the ban")
	}

	contribution := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/codex/add", nil)
	contribution = clientip.With(contribution, testResolvedClientIP(t, h.ipAccess.Snapshot(), contribution))
	contribution.Header.Set("X-Admin-Token", "break-glass-admin-token")
	rr = httptest.NewRecorder()
	if h.checkProviderContributionAuth(rr, contribution) || rr.Code != http.StatusTooManyRequests {
		t.Fatalf("contribution auth from banned peer: status = %d, want 429", rr.Code)
	}
}

func TestBruteForceBansSurviveRestartWithoutRawAddresses(t *testing.T) {
	passport, _ := newNonceTestPassport(t)
	store := passportBanStore{passport: passport, namespace: "ip"}
	first := newTestBruteForceTracker(t)
	if err := first.PersistBans(store, time.Now()); err != nil {
		t.Fatal(err)
	}
	banForTest(first, "198.51.100.7")
	banForTest(first, "2001:db8:0:1::5")

	expired := store.Key("203.0.113.1")
	if err := store.SaveBan(expired, time.Now().Add(-2*bruteforce.BanDuration)); err != nil {
		t.Fatal(err)
	}

	restarted := newTestBruteForceTracker(t)
	if err := restarted.PersistBans(store, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !restarted.IsBanned("198.51.100.7") || !restarted.IsBanned("2001:db8:0:1::ffff") {
		t.Fatal("bans were lost across a restart")
	}
	if restarted.IsBanned("198.51.100.8") {
		t.Fatal("restart banned an unrelated address")
	}
	bans, err := store.LoadBans()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bans[expired]; ok || len(bans) != 2 {
		t.Fatalf("persisted bans = %d (expired kept: %v), want the 2 active ones", len(bans), ok)
	}
	if err := passport.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketAuthBans)).ForEach(func(k, v []byte) error {
			if strings.Contains(string(k), "198.51.100") || strings.Contains(string(k), "2001:db8") {
				t.Fatalf("raw client address persisted: %s", k)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	restarted.RecordSuccess("198.51.100.7")
	if bans, _ := store.LoadBans(); len(bans) != 1 {
		t.Fatalf("lifting a ban left %d persisted bans, want 1", len(bans))
	}
}
