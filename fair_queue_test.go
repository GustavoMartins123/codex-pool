package main

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func awaitQueued(t *testing.T, q *fairQueue, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		n := q.waiting
		q.mu.Unlock()
		if n == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue did not reach expected size")
}

func TestFairQueueRotatesConsumersAndBoundsActiveWork(t *testing.T) {
	q := newFairQueue()
	q.capacity = 1
	release, err := q.Acquire(context.Background(), "busy")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		owner   string
		release func()
	}
	results := make(chan result, 4)
	for i, owner := range []string{"busy", "busy", "light", "busy"} {
		go func(owner string) {
			done, err := q.Acquire(context.Background(), owner)
			if err != nil {
				t.Error(err)
				return
			}
			results <- result{owner, done}
		}(owner)
		awaitQueued(t, q, i+1)
	}
	release()
	for _, expected := range []string{"busy", "light", "busy", "busy"} {
		select {
		case got := <-results:
			if got.owner != expected {
				t.Fatalf("got %s, want %s", got.owner, expected)
			}
			got.release()
			got.release()
		case <-time.After(2 * time.Second):
			t.Fatal("queue stalled")
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 || q.waiting != 0 || len(q.inflight)+len(q.pending)+len(q.order) != 0 {
		t.Fatal("leaked queue state")
	}
}

func TestFairQueuePerConsumerCapacityCancellationAndTimeout(t *testing.T) {
	q := newFairQueue()
	q.perPrincipal = 1
	q.perPrincipalWaiting = 1
	release, _ := q.Acquire(context.Background(), "busy")
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := q.Acquire(ctx, "busy"); result <- err }()
	awaitQueued(t, q, 1)
	_, err := q.Acquire(context.Background(), "busy")
	requirePolicyCode(t, err, "queue_full")
	light, err := q.Acquire(context.Background(), "light")
	if err != nil {
		t.Fatal(err)
	}
	light()
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatal(err)
	}
	awaitQueued(t, q, 0)
	q.wait = 10 * time.Millisecond
	_, err = q.Acquire(context.Background(), "busy")
	requirePolicyCode(t, err, "queue_wait_expired")
	awaitQueued(t, q, 0)
}

func TestFairQueueCancellationDuringAdmissionDoesNotLeak(t *testing.T) {
	q := newFairQueue()
	q.capacity = 2
	q.perPrincipal = 1
	var workers sync.WaitGroup
	for i := 0; i < 100; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ctx, cancel := context.WithCancel(context.Background())
			go cancel()
			release, err := q.Acquire(ctx, "same")
			if err == nil {
				release()
			}
		}()
	}
	workers.Wait()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 || q.waiting != 0 {
		t.Fatal("cancelled requests leaked")
	}
}

func TestFairQueueRevalidatesRevokedCredentials(t *testing.T) {
	p, client := testPolicyPassport(t)
	t.Setenv("POOL_JWT_SECRET", "queue-test-jwt-secret-long-enough")
	identity := client.PrincipalID + "-c-" + client.ID
	token := generateClaudePoolToken(getPoolJWTSecret(), identity)
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	q := newFairQueue()
	q.capacity = 1
	h := &proxyHandler{passport: p, metrics: newMetrics(), requestQueue: q}
	h.queueOnce.Do(func() {})
	release, _ := q.Acquire(context.Background(), "occupant")
	result := make(chan error, 1)
	go func() {
		done, err := h.admitQueuedRequest(r, client.PrincipalID)
		if done != nil {
			done()
		}
		result <- err
	}()
	awaitQueued(t, q, 1)
	p.mu.Lock()
	p.clients[client.ID].Status = "revoked"
	p.mu.Unlock()
	release()
	requirePolicyCode(t, <-result, "queue_authorization_changed")
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 {
		t.Fatal("revoked request retained capacity")
	}
}
