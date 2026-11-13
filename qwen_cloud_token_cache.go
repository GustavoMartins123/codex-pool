package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	qwenCloudTokenTTL        = 10 * time.Minute
	qwenCloudTokenCacheLimit = 128
)

type qwenCloudTokenEntry struct {
	token     string
	expiresAt time.Time
}

type qwenCloudTokenCache struct {
	mu       sync.Mutex
	tokens   map[[32]byte]qwenCloudTokenEntry
	requests singleflight.Group
}

func qwenCloudTokenKey(endpoint string, plan *QwenTokenPlanConfig) [32]byte {
	encoded, _ := json.Marshal([4]string{endpoint, plan.AccessKeyID, plan.AccessKeySecret, plan.SecurityToken})
	return sha256.Sum256(encoded)
}

func (cache *qwenCloudTokenCache) get(key [32]byte, now time.Time) (string, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, exists := cache.tokens[key]
	if !exists {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		delete(cache.tokens, key)
		return "", false
	}
	return entry.token, true
}

func (cache *qwenCloudTokenCache) put(key [32]byte, token string, now time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.tokens == nil {
		cache.tokens = make(map[[32]byte]qwenCloudTokenEntry)
	}
	for key, entry := range cache.tokens {
		if !now.Before(entry.expiresAt) {
			delete(cache.tokens, key)
		}
	}
	if len(cache.tokens) >= qwenCloudTokenCacheLimit {
		var oldest [32]byte
		var expiresAt time.Time
		for key, entry := range cache.tokens {
			if expiresAt.IsZero() || entry.expiresAt.Before(expiresAt) {
				oldest, expiresAt = key, entry.expiresAt
			}
		}
		delete(cache.tokens, oldest)
	}
	cache.tokens[key] = qwenCloudTokenEntry{token: token, expiresAt: now.Add(qwenCloudTokenTTL)}
}

func (cache *qwenCloudTokenCache) discard(token string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for key, entry := range cache.tokens {
		if entry.token == token {
			delete(cache.tokens, key)
		}
	}
}

func (h *proxyHandler) qwenCloudAccessToken(ctx context.Context, plan *QwenTokenPlanConfig, now time.Time) (string, error) {
	if plan.Mode == qwenPersonalBrowser {
		return h.qwenOAuthCloudAccessToken(ctx, plan.CloudOAuth, now)
	}
	if h.cfg.qwenPlanBase == nil {
		return "", errors.New("Alibaba Cloud management endpoint is not configured")
	}
	key := qwenCloudTokenKey(h.cfg.qwenPlanBase.String(), plan)
	cache := &h.qwenCloudTokens
	if token, found := cache.get(key, now); found {
		return token, nil
	}
	result := cache.requests.DoChan(hex.EncodeToString(key[:]), func() (any, error) {
		if token, found := cache.get(key, now); found {
			return token, nil
		}
		token, err := h.issueQwenCloudAccessToken(ctx, plan, now)
		if err != nil {
			return "", err
		}
		cache.put(key, token, now)
		return token, nil
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return "", result.Err
		}
		return result.Val.(string), nil
	}
}
