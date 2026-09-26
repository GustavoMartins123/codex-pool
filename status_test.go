package main

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// Regression test: the status page must read the atomic Inflight counter with
// atomic loads while proxied requests mutate it without holding the account
// mutex. Run with -race to catch the unsynchronized read.
func TestServeStatusPageConcurrentInflightMutations(t *testing.T) {
	account := &Account{Type: AccountTypeCodex, ID: "codex-one"}
	handler := &proxyHandler{pool: newPoolState([]*Account{account}, false)}

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
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("GET", "/status", nil)
			request.Header.Set("Accept", "application/json")
			handler.serveStatusPage(recorder, request)
			if recorder.Code != 200 {
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
