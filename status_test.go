package main

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// Regression test: serveStatusPage used to hold p.mu.RLock across the whole
// render while getPoolUtilization took its own RLock. A pool writer queued
// between the two acquisitions deadlocked the status page forever while it
// kept the pool read lock. The page must complete under sustained writer
// pressure.
func TestServeStatusPageCompletesUnderPoolWriterPressure(t *testing.T) {
	account := &Account{Type: AccountTypeCodex, ID: "codex-one"}
	pool := newPoolState([]*Account{account}, false)
	handler := &proxyHandler{pool: pool}

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
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("GET", "/status", nil)
			request.Header.Set("Accept", "application/json")
			handler.serveStatusPage(recorder, request)
			if recorder.Code != 200 {
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
