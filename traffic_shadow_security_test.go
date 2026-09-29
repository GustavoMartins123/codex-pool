package main

import (
	"go.etcd.io/bbolt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
