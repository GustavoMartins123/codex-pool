package main

import (
	"context"
	"errors"
	"fmt"
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
