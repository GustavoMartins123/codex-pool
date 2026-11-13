package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQwenCloudTokenCacheExpiresAndSeparatesCredentials(t *testing.T) {
	var calls atomic.Int32
	h := qwenTestHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return qwenJSONResponse(200, fmt.Sprintf(`{"cliAccessToken":"token-%d"}`, calls.Add(1))), nil
	}))
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalCloud, AccessKeyID: "key", AccessKeySecret: "secret"}
	now := time.Now()
	first, err := h.qwenCloudAccessToken(context.Background(), plan, now)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := h.qwenCloudAccessToken(context.Background(), plan, now.Add(qwenCreditPollInterval))
	if err != nil || reused != first || calls.Load() != 1 {
		t.Fatalf("token not reused for the next credit poll: token=%q calls=%d err=%v", reused, calls.Load(), err)
	}
	renewed, err := h.qwenCloudAccessToken(context.Background(), plan, now.Add(qwenCloudTokenTTL))
	if err != nil || renewed == first || calls.Load() != 2 {
		t.Fatalf("expired token was reused: calls=%d err=%v", calls.Load(), err)
	}
	rotated := *plan
	rotated.AccessKeySecret = "new-secret"
	if _, err := h.qwenCloudAccessToken(context.Background(), &rotated, now); err != nil || calls.Load() != 3 {
		t.Fatal("rotated credentials reused the previous token")
	}
	rotated.SecurityToken = "new-sts-token"
	if _, err := h.qwenCloudAccessToken(context.Background(), &rotated, now); err != nil || calls.Load() != 4 {
		t.Fatal("different STS credentials reused the previous token")
	}
	h.cfg.qwenPlanBase = &url.URL{Scheme: "https", Host: "other.example"}
	if _, err := h.qwenCloudAccessToken(context.Background(), &rotated, now); err != nil || calls.Load() != 5 {
		t.Fatal("a different management endpoint reused the previous token")
	}
}

func TestQwenCloudTokenCacheSharesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return qwenJSONResponse(200, `{"cliAccessToken":"shared-token"}`), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}))
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalCloud, AccessKeyID: "key", AccessKeySecret: "secret"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var requests sync.WaitGroup
	for range 12 {
		requests.Go(func() {
			token, err := h.qwenCloudAccessToken(ctx, plan, time.Now())
			if err != nil || token != "shared-token" {
				t.Errorf("shared token request failed: %v", err)
			}
		})
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("token exchange did not start")
	}
	close(release)
	requests.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent callers issued %d authentication requests", calls.Load())
	}
}

func TestQwenCloudTokenCacheDiscardsRejectedTokensWithoutRetry(t *testing.T) {
	for _, rejection := range []int{401, 403, 200} {
		t.Run(fmt.Sprint(rejection), func(t *testing.T) {
			var issued, queried int
			h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/modelstudio/cli/generateAccessToken" {
					issued++
					return qwenJSONResponse(200, fmt.Sprintf(`{"cliAccessToken":"token-%d"}`, issued)), nil
				}
				queried++
				return qwenJSONResponse(rejection, `{"code":"200","successResponse":true,"data":{"success":false,"errorCode":"BailianGateway.Login.NotLogined"}}`), nil
			}))
			plan := &QwenTokenPlanConfig{Mode: qwenPersonalCloud, AccessKeyID: "key", AccessKeySecret: "secret"}
			token, err := h.qwenCloudAccessToken(context.Background(), plan, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var response any
			if err := h.qwenCloudRequest(context.Background(), token, "subscription", map[string]any{}, &response); err == nil || issued != 1 || queried != 1 {
				t.Fatal("a rejected request retried or succeeded")
			}
			next, err := h.qwenCloudAccessToken(context.Background(), plan, time.Now())
			if err != nil || next == token || issued != 2 {
				t.Fatal("a rejected token remained cached")
			}
		})
	}
}

func TestQwenCloudTokenCacheDoesNotCacheAuthenticationFailures(t *testing.T) {
	calls := 0
	h := qwenTestHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("upstream unavailable")
		}
		return qwenJSONResponse(200, `{"cliAccessToken":"valid-token"}`), nil
	}))
	plan := &QwenTokenPlanConfig{Mode: qwenPersonalCloud, AccessKeyID: "key", AccessKeySecret: "secret"}
	if _, err := h.qwenCloudAccessToken(context.Background(), plan, time.Now()); err == nil {
		t.Fatal("authentication failure was hidden")
	}
	if token, err := h.qwenCloudAccessToken(context.Background(), plan, time.Now()); err != nil || token != "valid-token" || calls != 2 {
		t.Fatalf("an earlier failure was reused: calls=%d err=%v", calls, err)
	}
}
