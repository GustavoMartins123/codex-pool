package main

import (
	"net/http"
	"sync"
	"time"
)

type recentError struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

type recentErrors struct {
	mu   sync.Mutex
	max  int
	list []recentError
}

func newRecentErrors(max int) *recentErrors {
	return &recentErrors{max: max}
}

func (r *recentErrors) add(msg string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append([]recentError{{At: time.Now().UTC(), Message: msg}}, r.list...)
	if len(r.list) > r.max {
		r.list = r.list[:r.max]
	}
}

func (r *recentErrors) snapshot() []recentError {
	if r == nil {
		return []recentError{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recentError, len(r.list))
	copy(out, r.list)
	return out
}

func (h *proxyHandler) serveRecentErrors(w http.ResponseWriter) {
	respondJSON(w, map[string]any{"errors": h.recent.snapshot()})
}
