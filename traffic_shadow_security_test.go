package main

import (
	"context"
	"errors"
	"go.etcd.io/bbolt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrafficShadowCancellationDeadlineAndRelease(t *testing.T) {
	for _, mode := range []string{"client_cancel", "deadline", "detached_deadline"} {
		t.Run(mode, func(t *testing.T) {
			cfg := enabledTraffic()
			cfg.MaxInflight, cfg.TimeoutSeconds = 1, 1
			cfg.DetachFromClient = mode == "detached_deadline"
			h, _, _, _ := trafficShadowFixture(t, cfg, trafficShadowAccounts())
			started := make(chan struct{}, 1)
			finished := make(chan error, 1)
			h.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if _, shadow := trafficShadowFromRequest(r); shadow {
					started <- struct{}{}
					<-r.Context().Done()
					finished <- r.Context().Err()
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"real","output":[]}`)), Request: r}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"probe","stream":false}`)).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
			w := httptest.NewRecorder()
			testPoolServeHTTP(t, h, w, r)
			if w.Code != 200 {
				t.Fatalf("real request: %d %s", w.Code, w.Body.String())
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("shadow did not reach transport")
			}
			expected := context.DeadlineExceeded
			if mode != "deadline" {
				cancel()
			}
			if mode == "client_cancel" {
				expected = context.Canceled
			}
			if mode == "detached_deadline" {
				select {
				case err := <-finished:
					t.Fatalf("detached leg canceled with client: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
			}
			select {
			case err := <-finished:
				if !errors.Is(err, expected) {
					t.Fatalf("shadow termination: %v want %v", err, expected)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shadow exceeded its deadline")
			}
			end := time.Now().Add(time.Second)
			for time.Now().Before(end) {
				h.trafficShadow.mu.Lock()
				n := h.trafficShadow.inflight
				h.trafficShadow.mu.Unlock()
				if n == 0 {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("canceled shadow retained concurrency slot")
		})
	}
}

func TestTrafficShadowForgedHeaderCannotBypassQuota(t *testing.T) {
	traffic := enabledTraffic()
	traffic.Principals = []string{"different-authorized-principal"}
	h, _, calls, _ := trafficShadowFixture(t, traffic, trafficShadowAccounts())
	h.experiments = nil
	h.cfg.setHotReloadable(0, RoutingConfigFile{}, map[string]ClientPolicy{
		"*": {Limits: PolicyLimits{DailyRequests: 1}},
	}, ExperimentsConfig{})
	owner := testPoolIdentity(t, h, "user")
	if h.trafficShadow.enabledFor("gpt-5.6-sol->gpt-5.6-sol", owner) {
		t.Fatal("caller must be outside the experiment principal allowlist")
	}
	if w := trafficShadowRequest(t, h); w.Code != http.StatusOK {
		t.Fatalf("real request failed: %d %s", w.Code, w.Body.String())
	}
	if w := trafficShadowRequest(t, h); w.Code != http.StatusTooManyRequests {
		t.Fatalf("quota control failed: %d %s", w.Code, w.Body.String())
	}
	before := atomic.LoadInt32(calls)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"probe","stream":false}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
	r.Header.Set("X-Pool-Shadow", "forged-by-client")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != http.StatusTooManyRequests || atomic.LoadInt32(calls) != before {
		t.Fatalf("forged header bypassed quota: status=%d calls=%d->%d", w.Code, before, atomic.LoadInt32(calls))
	}
}

func TestTrafficShadowPublicHeaderDoesNotChangeConversationNamespace(t *testing.T) {
	h, _, _, _ := trafficShadowFixture(t, TrafficShadowConfig{}, trafficShadowAccounts())
	h.experiments = nil
	body := `{"model":"gpt-5.6-sol","conversation_id":"real-conversation","input":"probe","stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
	r.Header.Set("X-Pool-Shadow", "forged-by-client")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("public header changed routing: %d %s", w.Code, w.Body.String())
	}
	owner := testPoolIdentity(t, h, "user")
	if _, ok := h.getContextHandoff().State(conversationScopedKey(owner, "real-conversation")); !ok {
		t.Fatal("real conversation state missing")
	}
	if _, ok := h.getContextHandoff().State(conversationScopedKey(owner, "shadow:forged-by-client\x00real-conversation")); ok {
		t.Fatal("public header created internal shadow state")
	}
}

func budgetTestConfig() TrafficShadowConfig {
	return TrafficShadowConfig{Enabled: true, Experiments: []string{"e"}, Accounts: []string{"a"}, Principals: []string{"p"}, MaxInflight: 10, DailyBudget: 1, TimeoutSeconds: 5}
}
func TestTrafficShadowBudgetStorageFailureDenies(t *testing.T) {
	for _, mode := range []string{"closed", "readonly", "corrupt", "negative", "null"} {
		t.Run(mode, func(t *testing.T) {
			store := testUsageStore(t)
			db := store.db
			now := time.Now()
			if mode == "readonly" {
				path := filepath.Join(t.TempDir(), "budget.db")
				w, err := bbolt.Open(path, 0600, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = w.Close(); err != nil {
					t.Fatal(err)
				}
				db, err = bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
			}
			r := mustTrafficShadowRuntime(t, db, budgetTestConfig())
			if mode == "closed" {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			} else if mode != "readonly" {
				raw := map[string]string{"corrupt": "oops", "negative": "-1", "null": "null"}[mode]
				if err := db.Update(func(tx *bbolt.Tx) error {
					b, err := tx.CreateBucketIfNotExists([]byte(bucketTrafficShadowBudget))
					if err != nil {
						return err
					}
					return b.Put([]byte(now.UTC().Format("2006-01-02")), []byte(raw))
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, ok, err := r.begin("e", "p", now)
			if ok || err == nil || r.inflight != 0 {
				t.Fatalf("storage failure admitted traffic: ok=%v err=%v inflight=%d", ok, err, r.inflight)
			}
		})
	}
}
func TestTrafficShadowBudgetSharedRuntimesCannotOverspend(t *testing.T) {
	db := testUsageStore(t).db
	cfg := budgetTestConfig()
	runtimes := []*trafficShadowRuntime{mustTrafficShadowRuntime(t, db, cfg), mustTrafficShadowRuntime(t, db, cfg)}
	var wg sync.WaitGroup
	var admitted atomic.Int32
	now := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := runtimes[i%2].begin("e", "p", now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("budget of one admitted %d legs", admitted.Load())
	}
}
func TestTrafficShadowAdmissionRechecksAuthorization(t *testing.T) {
	r := mustTrafficShadowRuntime(t, testUsageStore(t).db, budgetTestConfig())
	cfg := budgetTestConfig()
	cfg.Principals = []string{"other"}
	if err := r.Update(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.begin("e", "p", time.Now()); ok || err != nil {
		t.Fatalf("revoked principal admitted: %v %v", ok, err)
	}
}
