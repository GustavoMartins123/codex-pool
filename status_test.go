package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func statusAuthedRequest(handler *proxyHandler) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/status", nil)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Admin-Token", handler.cfg.adminToken)
	handler.serveStatusPage(recorder, request)
	return recorder
}

// Regression test: the status page must read the atomic Inflight counter with
// atomic loads while proxied requests mutate it without holding the account
// mutex. Run with -race to catch the unsynchronized read.
func TestServeStatusPageConcurrentInflightMutations(t *testing.T) {
	account := &Account{Type: AccountTypeCodex, ID: "codex-one"}
	handler := &proxyHandler{pool: newPoolState([]*Account{account}, false), cfg: &config{adminToken: "admin"}}

	var stop atomic.Bool
	var writers sync.WaitGroup
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for !stop.Load() {
				atomic.AddInt64(&account.Inflight, 1)
				atomic.AddInt64(&account.Inflight, -1)
			}
		}()
	}

	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if recorder := statusAuthedRequest(handler); recorder.Code != 200 {
				t.Errorf("status = %d", recorder.Code)
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	writers.Wait()

	if got := atomic.LoadInt64(&account.Inflight); got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
}

// Regression test: serveStatusPage used to hold p.mu.RLock across the whole
// render while getPoolUtilization took its own RLock. A pool writer queued
// between the two acquisitions deadlocked the status page forever while it
// kept the pool read lock. The page must complete under sustained writer
// pressure.
func TestServeStatusPageCompletesUnderPoolWriterPressure(t *testing.T) {
	account := &Account{Type: AccountTypeCodex, ID: "codex-one"}
	pool := newPoolState([]*Account{account}, false)
	handler := &proxyHandler{pool: pool, cfg: &config{adminToken: "admin"}}

	var writers sync.WaitGroup
	writersStopped := make(chan struct{})
	for i := 0; i < 2; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-writersStopped:
					return
				default:
				}
				pool.mu.Lock()
				pool.rr++
				pool.mu.Unlock()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if recorder := statusAuthedRequest(handler); recorder.Code != 200 {
				t.Errorf("status = %d", recorder.Code)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serveStatusPage deadlocked against pool writers")
	}
	close(writersStopped)
	writers.Wait()
}

func TestStatusPageRequiresAuthentication(t *testing.T) {
	handler := &proxyHandler{pool: newPoolState(nil, false), cfg: &config{adminToken: "admin"}}
	recorder := httptest.NewRecorder()
	handler.serveStatusPage(recorder, httptest.NewRequest("GET", "/status", nil))
	if recorder.Code != 401 {
		t.Fatalf("unauthenticated status = %d, want 401", recorder.Code)
	}
}

func newStatusMemberHandler(t *testing.T, providers PolicySelector) (*proxyHandler, string) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := passport.createMemberLink("operator", "viewer@example.com", "V", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	member := passport.byEmail("viewer@example.com")
	client, err := passport.createClient(member.ID, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Policy.Providers = providers
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketClientCredentials)), client.ID, client)
	}); err != nil {
		t.Fatal(err)
	}
	passport.mu.Lock()
	passport.clients[client.ID] = client
	passport.mu.Unlock()
	session, _, err := passport.createSession(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	handler := &proxyHandler{
		pool: newPoolState([]*Account{
			{ID: "cx", Type: AccountTypeCodex},
			{ID: "gm", Type: AccountTypeGemini},
			{ID: "cl", Type: AccountTypeClaude},
		}, false),
		cfg: &config{adminToken: "admin"}, passport: passport, startTime: time.Now(),
	}
	return handler, session
}

func decodeStatusJSON(t *testing.T, recorder *httptest.ResponseRecorder) StatusData {
	t.Helper()
	var data StatusData
	if err := json.Unmarshal(recorder.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStatusPageFiltersMemberToAllowedProviders(t *testing.T) {
	handler, session := newStatusMemberHandler(t, PolicySelector{Allow: []string{"codex"}})
	request := httptest.NewRequest("GET", "/status", nil)
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
	recorder := httptest.NewRecorder()
	handler.serveStatusPage(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	data := decodeStatusJSON(t, recorder)
	if data.Operator {
		t.Fatal("member was treated as operator")
	}
	if data.TotalCount != 1 || data.CodexCount != 1 || data.GeminiCount != 0 || data.ClaudeCount != 0 {
		t.Fatalf("filtered counts = total %d codex %d gemini %d claude %d", data.TotalCount, data.CodexCount, data.GeminiCount, data.ClaudeCount)
	}
	for _, account := range data.Accounts {
		if account.Type != string(AccountTypeCodex) {
			t.Fatalf("non-codex account visible to codex-only member: %+v", account)
		}
	}
	if data.TokenAnalytics != nil {
		t.Fatal("member saw capacity analytics")
	}
}

func TestStatusPageUnrestrictedMemberSeesAllProviders(t *testing.T) {
	handler, session := newStatusMemberHandler(t, PolicySelector{})
	request := httptest.NewRequest("GET", "/status", nil)
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
	recorder := httptest.NewRecorder()
	handler.serveStatusPage(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
	data := decodeStatusJSON(t, recorder)
	if data.TotalCount != 3 {
		t.Fatalf("unrestricted member total = %d, want 3", data.TotalCount)
	}
}

func TestStatusPageMemberWithoutCredentialsSeesNothing(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := passport.createMemberLink("operator", "bare@example.com", "B", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	session, _, err := passport.createSession(passport.byEmail("bare@example.com").ID)
	if err != nil {
		t.Fatal(err)
	}
	handler := &proxyHandler{
		pool:     newPoolState([]*Account{{ID: "cx", Type: AccountTypeCodex}}, false),
		cfg:      &config{adminToken: "admin"},
		passport: passport, startTime: time.Now(),
	}
	request := httptest.NewRequest("GET", "/status", nil)
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
	recorder := httptest.NewRecorder()
	handler.serveStatusPage(recorder, request)
	data := decodeStatusJSON(t, recorder)
	if data.TotalCount != 0 {
		t.Fatalf("member without credentials saw %d accounts", data.TotalCount)
	}
}

func TestStatusPageOperatorSeesEverything(t *testing.T) {
	handler, _ := newStatusMemberHandler(t, PolicySelector{Allow: []string{"codex"}})
	request := httptest.NewRequest("GET", "/status", nil)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Admin-Token", "admin")
	recorder := httptest.NewRecorder()
	handler.serveStatusPage(recorder, request)
	data := decodeStatusJSON(t, recorder)
	if !data.Operator || data.TotalCount != 3 {
		t.Fatalf("break-glass view = operator %v total %d", data.Operator, data.TotalCount)
	}
}
