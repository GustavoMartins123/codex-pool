package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type queueWaiter struct {
	ready    chan struct{}
	admitted bool
}

type fairQueue struct {
	mu                                                      sync.Mutex
	capacity, perPrincipal, maxWaiting, perPrincipalWaiting int
	wait                                                    time.Duration
	active, waiting                                         int
	inflight                                                map[string]int
	pending                                                 map[string][]*queueWaiter
	order                                                   []string
}

func newFairQueue() *fairQueue {
	return &fairQueue{capacity: 64, perPrincipal: 4, maxWaiting: 256, perPrincipalWaiting: 16, wait: 30 * time.Second, inflight: map[string]int{}, pending: map[string][]*queueWaiter{}}
}

func queueError(code string) error {
	return &policyError{Status: http.StatusTooManyRequests, Code: code, Message: code}
}

func (q *fairQueue) dispatchLocked() {
	for q.active < q.capacity && len(q.order) > 0 {
		selected := -1
		for i, owner := range q.order {
			if q.inflight[owner] < q.perPrincipal {
				selected = i
				break
			}
		}
		if selected < 0 {
			return
		}
		owner := q.order[selected]
		q.order = append(q.order[:selected], q.order[selected+1:]...)
		w := q.pending[owner][0]
		q.pending[owner] = q.pending[owner][1:]
		if len(q.pending[owner]) > 0 {
			q.order = append(q.order, owner)
		} else {
			delete(q.pending, owner)
		}
		q.waiting--
		q.active++
		q.inflight[owner]++
		w.admitted = true
		close(w.ready)
	}
}

func (q *fairQueue) releaseLocked(owner string) {
	q.active--
	q.inflight[owner]--
	if q.inflight[owner] == 0 {
		delete(q.inflight, owner)
	}
	q.dispatchLocked()
}

func (q *fairQueue) Acquire(ctx context.Context, owner string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == "" || len(owner) > 256 {
		return nil, queueError("queue_identity_invalid")
	}
	w := &queueWaiter{ready: make(chan struct{})}
	q.mu.Lock()
	if q.waiting >= q.maxWaiting || len(q.pending[owner]) >= q.perPrincipalWaiting {
		q.mu.Unlock()
		return nil, queueError("queue_full")
	}
	if len(q.pending[owner]) == 0 {
		q.order = append(q.order, owner)
	}
	q.pending[owner] = append(q.pending[owner], w)
	q.waiting++
	q.dispatchLocked()
	q.mu.Unlock()
	timer := time.NewTimer(q.wait)
	defer timer.Stop()
	var err error
	select {
	case <-w.ready:
		err = ctx.Err()
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = queueError("queue_wait_expired")
	}
	if err != nil {
		q.mu.Lock()
		if w.admitted {
			q.releaseLocked(owner)
		} else {
			for i, candidate := range q.pending[owner] {
				if candidate == w {
					q.pending[owner] = append(q.pending[owner][:i], q.pending[owner][i+1:]...)
					q.waiting--
					break
				}
			}
			if len(q.pending[owner]) == 0 {
				delete(q.pending, owner)
				for i, key := range q.order {
					if key == owner {
						q.order = append(q.order[:i], q.order[i+1:]...)
						break
					}
				}
			}
		}
		q.mu.Unlock()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { q.mu.Lock(); defer q.mu.Unlock(); q.releaseLocked(owner) }) }, nil
}

func (h *proxyHandler) admitQueuedRequest(r *http.Request, principalID string) (func(), error) {
	h.queueOnce.Do(func() { h.requestQueue = newFairQueue() })
	release, err := h.requestQueue.Acquire(r.Context(), principalID)
	if err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return nil, &policyError{Status: http.StatusRequestTimeout, Code: "queue_cancelled", Message: err.Error()}
		}
		return nil, err
	}
	_, current, _, kind, allowed := h.authorizePoolCredentialRequest(r)
	if kind == "" || !allowed || current != principalID {
		release()
		return nil, &policyError{Status: 403, Code: "queue_authorization_changed", Message: "credential revoked, expired, or changed while waiting"}
	}
	return release, nil
}
