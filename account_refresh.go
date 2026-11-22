package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const refreshMinInterval = 5 * time.Second

type accountRefreshCoordinator struct {
	mu          sync.Mutex
	calls       map[string]*refreshCall
	lastAttempt time.Time
}

type refreshCall struct {
	done chan struct{}
	err  error
}

func (c *accountRefreshCoordinator) run(ctx context.Context, key string, refresh func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	call := c.calls[key]
	if call == nil {
		if c.calls == nil {
			c.calls = make(map[string]*refreshCall)
		}
		call = &refreshCall{done: make(chan struct{})}
		c.calls[key] = call
		go c.complete(key, call, refresh)
	}
	c.mu.Unlock()

	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *accountRefreshCoordinator) complete(key string, call *refreshCall, refresh func(context.Context) error) {
	defer func() {
		c.mu.Lock()
		delete(c.calls, key)
		close(call.done)
		c.mu.Unlock()
	}()

	// The shared refresh can finish after the initiating request disconnects.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	call.err = refresh(ctx)
}

func (c *accountRefreshCoordinator) waitForSlot(ctx context.Context) error {
	for {
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return err
		}
		now := time.Now()
		wait := refreshMinInterval - now.Sub(c.lastAttempt)
		if c.lastAttempt.IsZero() || wait <= 0 {
			c.lastAttempt = now
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (h *proxyHandler) refreshAccount(ctx context.Context, a *Account) error {
	if a == nil {
		return errors.New("nil account")
	}
	key := fmt.Sprintf("%s:%s", a.Type, a.ID)
	return h.refreshCoordinator.run(ctx, key, func(ctx context.Context) error {
		err := h.refreshAccountOnce(ctx, a)
		switch {
		case err == nil:
			a.mu.Lock()
			a.RefreshBlocked = false
			a.mu.Unlock()
		case !isPermanentRefreshTokenError(err) && !isRateLimitError(err) && !errors.Is(err, context.Canceled):
			// A transient refresh failure leaves the existing access token in use.
			a.mu.Lock()
			a.RefreshBlocked = true
			a.mu.Unlock()
		}
		return err
	})
}

// isPermanentRefreshTokenError reports auth-server failures that mean the
// refresh token can never recover without a fresh login. OpenAI currently
// returns code refresh_token_invalidated ("Your session has ended") rather
// than classic invalid_grant; both (and reuse) must retire the account.
func isPermanentRefreshTokenError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"invalid_grant",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"session has ended",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func (h *proxyHandler) needsRefresh(a *Account) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.RefreshToken == "" || a.RefreshBlocked {
		return false
	}
	now := time.Now()

	// Per-account rate limiting: don't refresh too frequently
	// This prevents hammering the OAuth endpoint when refresh tokens are invalid
	if !a.LastRefresh.IsZero() && now.Sub(a.LastRefresh) < refreshPerAccountInterval {
		return false
	}

	// Antigravity refreshes with the same 3000-second headroom as the native
	// client so a long generation does not cross the token expiry boundary.
	if a.Type == AccountTypeAntigravity && !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now.Add(50*time.Minute)) {
		return true
	}

	// Codex access JWTs last about ten days. Refresh from the usage poller
	// when that window is nearly closed instead of spending the refresh token
	// on every WHAM/proxy 401.
	if a.Type == AccountTypeCodex && !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now.Add(codexRefreshHeadroom)) {
		return true
	}

	// Other providers refresh after expiry and recover early invalidation with
	// their existing same-account 401 retry.
	if !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now) {
		return true
	}
	// If no expiry time known, refresh after 12 hours since last refresh
	if a.ExpiresAt.IsZero() && !a.LastRefresh.IsZero() && now.Sub(a.LastRefresh) > 12*time.Hour {
		return true
	}
	return false
}

// codexRefreshHeadroom is how soon before access-token expiry the poller
// should rotate. Codex access JWTs carry an `exp` claim of about ten days.
const codexRefreshHeadroom = 6 * time.Hour

// refreshPerAccountInterval is the minimum time between refresh attempts for a single account
// This is persisted to disk and survives restarts, preventing hammering OAuth endpoints
// 15 minutes balances between preventing hammering and allowing recovery from expired tokens
const refreshPerAccountInterval = 15 * time.Minute

func (h *proxyHandler) refreshAccountAfterAuthFailure(ctx context.Context, a *Account) error {
	if a == nil {
		return errors.New("nil account")
	}
	if a.Type == AccountTypeCodex {
		// A 401 with a still-valid access JWT is not permission to spend the
		// refresh token. OpenAI treats reuse as "session has ended" and can
		// log out every copy of that login.
		if codexAccessFarFromExpiry(a, time.Now()) {
			return fmt.Errorf("codex access token still live, skipping refresh")
		}
		return h.refreshAccount(ctx, a)
	}
	a.mu.Lock()
	a.LastRefresh = time.Time{}
	a.RefreshBlocked = false
	a.mu.Unlock()
	return h.refreshAccount(ctx, a)
}

func (h *proxyHandler) refreshAccountOnce(ctx context.Context, a *Account) error {
	// Per-account rate limiting (persisted to disk via LastRefresh)
	a.mu.Lock()
	sinceLastRefresh := time.Since(a.LastRefresh)
	if !a.LastRefresh.IsZero() && sinceLastRefresh < refreshPerAccountInterval {
		a.mu.Unlock()
		return fmt.Errorf("account refresh rate limited (%s), wait %v", a.ID, refreshPerAccountInterval-sinceLastRefresh)
	}
	accType := a.Type
	a.mu.Unlock()

	if err := h.refreshCoordinator.waitForSlot(ctx); err != nil {
		return err
	}

	// Use the provider's RefreshToken method
	provider := h.registry.ForType(accType)
	if provider == nil {
		return fmt.Errorf("no provider for account type %s", accType)
	}
	err := provider.RefreshToken(ctx, a, h.refreshTransport)

	if err == nil {
		a.mu.Lock()
		a.LastRefresh = time.Now().UTC()
		if a.NeedsLogin || a.AuthError != "" {
			a.NeedsLogin = false
			a.AuthError = ""
		}
		a.mu.Unlock()
		if saveErr := saveAccount(h.pool.antigravity, a); saveErr != nil {
			log.Printf("warning: failed to save account %s after refresh: %v", a.ID, saveErr)
		}
	}

	return err
}
