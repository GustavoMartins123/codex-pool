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
	mu       sync.Mutex
	attempts map[string]*attemptRecord
	stopCh   chan struct{}
	stopOnce sync.Once
}

func newBruteForceTracker() *bruteForceTracker {
	t := &bruteForceTracker{
		attempts: make(map[string]*attemptRecord),
		stopCh:   make(chan struct{}),
	}
	go t.cleanupLoop()
	return t
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
	ip = bruteForceKey(ip)
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.attempts[ip]
	if !ok {
		return false
	}
	if rec.bannedAt.IsZero() {
		return false
	}
	if time.Since(rec.bannedAt) > bruteForceBanDuration {
		// Ban expired — clear it.
		delete(t.attempts, ip)
		return false
	}
	return true
}

// recordFailure increments the failure count for an IP. Returns true if
// the IP is now banned (just crossed the threshold).
func (t *bruteForceTracker) recordFailure(ip string) bool {
	ip = bruteForceKey(ip)
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.attempts[ip]
	if !ok {
		if len(t.attempts) >= maxTrackedBruteForceIPs {
			t.cleanupLocked(time.Now())
			if len(t.attempts) >= maxTrackedBruteForceIPs {
				t.evictLocked()
			}
		}
		rec = &attemptRecord{firstAt: time.Now()}
		t.attempts[ip] = rec
	}
	// If already banned, don't re-count.
	if !rec.bannedAt.IsZero() {
		return true
	}
	rec.count++
	if rec.count >= bruteForceMaxAttempts {
		rec.bannedAt = time.Now()
		log.Printf("brute-force: banning IP %s after %d failed auth attempts", ip, rec.count)
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
}

// recordSuccess clears the failure count for an IP.
func (t *bruteForceTracker) recordSuccess(ip string) {
	ip = bruteForceKey(ip)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, ip)
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
	for ip, rec := range t.attempts {
		if !rec.bannedAt.IsZero() && now.Sub(rec.bannedAt) > bruteForceBanDuration {
			delete(t.attempts, ip)
		} else if rec.bannedAt.IsZero() && now.Sub(rec.firstAt) > bruteForceBanDuration {
			// Stale non-banned entries.
			delete(t.attempts, ip)
		}
	}
}

func (t *bruteForceTracker) stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
	})
}

