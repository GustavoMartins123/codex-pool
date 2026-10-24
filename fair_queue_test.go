package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
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

func TestFairQueueDefaultAllowsEightConcurrentRequestsPerPrincipal(t *testing.T) {
	q := newFairQueue()
	q.wait = 10 * time.Millisecond
	releases := make([]func(), 0, 8)
	for i := 0; i < 8; i++ {
		release, err := q.Acquire(context.Background(), "owner")
		if err != nil {
			t.Fatalf("acquire %d: %v", i+1, err)
		}
		releases = append(releases, release)
	}
	_, err := q.Acquire(context.Background(), "owner")
	requirePolicyCode(t, err, "queue_wait_expired")
	for _, release := range releases {
		release()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 || q.waiting != 0 || len(q.inflight)+len(q.pending)+len(q.order) != 0 {
		t.Fatal("leaked queue state")
	}
}

func TestIdleWebSocketDoesNotHoldFairQueueCapacity(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgradeRequest(r) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			for {
				if _, _, err = conn.Read(r.Context()); err != nil {
					return
				}
			}
		}
		io.WriteString(w, `{"output":[]}`)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	q := newFairQueue()
	q.perPrincipal = 1
	q.wait = 50 * time.Millisecond
	h := &proxyHandler{cfg: &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, disableRefresh: true}, pool: newPoolState([]*Account{{ID: "codex", Type: AccountTypeCodex, AccessToken: "token", AccountID: "acct", PlanType: "pro"}}, false), registry: NewProviderRegistry(NewCodexProviderWithRealtime(base, base, base, base), nil, nil), transport: http.DefaultTransport, metrics: newMetrics(), recent: newRecentErrors(5), requestQueue: q}
	h.queueOnce.Do(func() {})
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	idle, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + generateClaudePoolToken("test-secret", "alice")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer idle.CloseNow()
	time.Sleep(100 * time.Millisecond)
	q.mu.Lock()
	active := q.active
	q.mu.Unlock()
	if active != 0 {
		t.Fatalf("idle websocket consumed fair queue capacity: active=%d", active)
	}
	body := strings.NewReader(`{"model":"gpt-5.6-sol","input":"ping","stream":false}`)
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "alice"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post blocked by idle websocket session: status=%d", resp.StatusCode)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != 0 || q.waiting != 0 {
		t.Fatalf("leaked queue state: active=%d waiting=%d", q.active, q.waiting)
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
