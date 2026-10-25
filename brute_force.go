package main

import (
	"log"
	"net/netip"
	"sync"
	"time"
)

const (
	bruteForceMaxAttempts = 5
	bruteForceBanDuration = 30 * time.Minute
	bruteForceCleanup     = 10 * time.Minute
)

type attemptRecord struct {
	count    int
	firstAt  time.Time
	bannedAt time.Time
}

// bruteForceTracker tracks failed authentication attempts per IP address
// and bans IPs after too many failures.
const maxTrackedBruteForceIPs = 16384

type bruteForceTracker struct {
	mu          sync.Mutex
	attempts    map[string]*attemptRecord
	stopCh      chan struct{}
	stopOnce    sync.Once
	maxAttempts int
	banDuration time.Duration
	// store, when set, keeps active bans across restarts. Client keys pass
	// through store.Key, so memory and disk only hold salted digests.
	store bruteForceBanStore
}

// bruteForceBanStore persists a tracker's active bans. Ban events are rare,
// so the tracker calls it while holding its lock to keep writes ordered.
type bruteForceBanStore interface {
	Key(client string) string
	SaveBan(key string, bannedAt time.Time) error
	DeleteBans(keys []string) error
	LoadBans() (map[string]time.Time, error)
}

func newBruteForceTracker() *bruteForceTracker {
	return newBruteForceTrackerWithPolicy(bruteForceMaxAttempts, bruteForceBanDuration)
}

func newBruteForceTrackerWithPolicy(maxAttempts int, banDuration time.Duration) *bruteForceTracker {
	t := &bruteForceTracker{
		attempts:    make(map[string]*attemptRecord),
		stopCh:      make(chan struct{}),
		maxAttempts: maxAttempts,
		banDuration: banDuration,
	}
	go t.cleanupLoop()
	return t
}

// persistBans restores the active bans held by store and records later bans
// there, so a restart does not hand an attacker a fresh set of guesses.
func (t *bruteForceTracker) persistBans(store bruteForceBanStore, now time.Time) error {
	bans, err := store.LoadBans()
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.store = store
	var expired []string
	for key, bannedAt := range bans {
		if now.Sub(bannedAt) > t.banDuration {
			expired = append(expired, key)
			continue
		}
		t.attempts[key] = &attemptRecord{count: t.maxAttempts, firstAt: bannedAt, bannedAt: bannedAt}
	}
	t.forgetLocked(expired)
	return nil
}

func (t *bruteForceTracker) keyLocked(client string) string {
	key := bruteForceKey(client)
	if t.store != nil {
		return t.store.Key(key)
	}
	return key
}

// forgetLocked drops expired or evicted bans from the store. A failure only
// leaves a stale record that the next load discards as expired.
func (t *bruteForceTracker) forgetLocked(keys []string) {
	if t.store == nil || len(keys) == 0 {
		return
	}
	if err := t.store.DeleteBans(keys); err != nil {
		log.Printf("brute-force: forgetting bans failed: %v", err)
	}
}

// bruteForceKey groups IPv6 clients by their /64 prefix, the smallest block a
// single host commonly controls, so rotating addresses inside it does not
// reset the failure count. IPv4 and IPv4-mapped addresses keep one key each.
func bruteForceKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

// isBanned returns true if the IP is currently banned.
func (t *bruteForceTracker) isBanned(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := t.keyLocked(ip)
	rec, ok := t.attempts[key]
	if !ok {
		return false
	}
	if rec.bannedAt.IsZero() {
		return false
	}
	if time.Since(rec.bannedAt) > t.banDuration {
		// Ban expired — clear it.
		delete(t.attempts, key)
		t.forgetLocked([]string{key})
		return false
	}
	return true
}

// recordFailure increments the failure count for an IP. Returns true if
// the IP is now banned (just crossed the threshold).
func (t *bruteForceTracker) recordFailure(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := t.keyLocked(ip)
	rec, ok := t.attempts[key]
	if !ok {
		if len(t.attempts) >= maxTrackedBruteForceIPs {
			t.cleanupLocked(time.Now())
			if len(t.attempts) >= maxTrackedBruteForceIPs {
				t.evictLocked()
			}
		}
		rec = &attemptRecord{firstAt: time.Now()}
		t.attempts[key] = rec
	}
	// If already banned, don't re-count.
	if !rec.bannedAt.IsZero() {
		return true
	}
	rec.count++
	if rec.count >= t.maxAttempts {
		rec.bannedAt = time.Now()
		log.Printf("brute-force: banning %s after %d failed auth attempts", key, rec.count)
		if t.store != nil {
			if err := t.store.SaveBan(key, rec.bannedAt); err != nil {
				log.Printf("brute-force: persisting ban failed: %v", err)
			}
		}
		return true
	}
	return false
}

// evictLocked frees one slot. Active bans survive while any unbanned record
// remains, so flooding the table from fresh addresses cannot lift a ban: the
// oldest unbanned record goes first, otherwise the ban closest to expiry.
func (t *bruteForceTracker) evictLocked() {
	var victim string
	var victimAt time.Time
	victimBanned := true
	for key, rec := range t.attempts {
		banned := !rec.bannedAt.IsZero()
		at := rec.firstAt
		if banned {
			at = rec.bannedAt
		}
		if victim == "" || victimBanned && !banned || banned == victimBanned && at.Before(victimAt) {
			victim, victimAt, victimBanned = key, at, banned
		}
	}
	delete(t.attempts, victim)
	if victimBanned {
		t.forgetLocked([]string{victim})
	}
}

// recordSuccess clears the failure count for an IP.
func (t *bruteForceTracker) recordSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := t.keyLocked(ip)
	if rec, ok := t.attempts[key]; ok && !rec.bannedAt.IsZero() {
		t.forgetLocked([]string{key})
	}
	delete(t.attempts, key)
}

func (t *bruteForceTracker) cleanupLoop() {
	ticker := time.NewTicker(bruteForceCleanup)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.cleanup()
		case <-t.stopCh:
			return
		}
	}
}

func (t *bruteForceTracker) cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanupLocked(time.Now())
}

func (t *bruteForceTracker) cleanupLocked(now time.Time) {
	var expired []string
	for key, rec := range t.attempts {
		if !rec.bannedAt.IsZero() && now.Sub(rec.bannedAt) > t.banDuration {
			delete(t.attempts, key)
			expired = append(expired, key)
		} else if rec.bannedAt.IsZero() && now.Sub(rec.firstAt) > t.banDuration {
			// Stale non-banned entries.
			delete(t.attempts, key)
		}
	}
	t.forgetLocked(expired)
}

func (t *bruteForceTracker) stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
	})
}

