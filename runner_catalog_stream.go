package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

type runnerCatalogSnapshot struct {
	Revision string                `json:"revision"`
	Models   []poolModelDescriptor `json:"models"`
	Runners  []runnerproto.Runner  `json:"runners"`
}

func (b *sessionBroker) signalCatalogLocked() {
	if b.catalogChanged != nil {
		close(b.catalogChanged)
	}
	b.catalogChanged = make(chan struct{})
}

func (b *sessionBroker) catalogChanges() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.catalogChanged == nil {
		b.catalogChanged = make(chan struct{})
	}
	return b.catalogChanged
}

func (h *proxyHandler) appRunnerCatalog(r *http.Request, b *sessionBroker) (runnerCatalogSnapshot, error) {
	var snapshot runnerCatalogSnapshot
	principal, client := h.authenticateAppRequest(r)
	if principal == nil {
		return snapshot, sessionError(401, "app authentication required")
	}
	if h.pool == nil || h.cfg == nil || h.pool.accountAuthority != h.passport {
		return snapshot, sessionError(503, "account authority unavailable")
	}
	visible, err := h.memberQuotaSnapshot(principal.ID+"-c-"+client.ID, time.Now().UTC())
	if err != nil {
		return snapshot, err
	}
	snapshot.Models = poolModelsForClients(visible)
	for i := range snapshot.Models {
		m := &snapshot.Models[i]
		m.SupportingAccounts, m.AvailableAccounts = 0, 0
		m.QuotaRemaining, m.NextResetAt = nil, nil
	}
	snapshot.Runners, err = b.listRunners(principal.ID)
	if err != nil {
		return snapshot, err
	}
	slices.SortFunc(snapshot.Models, func(a, b poolModelDescriptor) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(snapshot.Runners, func(a, b runnerproto.Runner) int { return strings.Compare(a.ID, b.ID) })
	snapshot.Revision = runnerproto.ContentRevision(snapshot)
	return snapshot, nil
}

func (h *proxyHandler) handleAppRunnerCatalog(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	changes := b.catalogChanges() // Subscribe before reading so an update cannot fall between the two.
	snapshot, err := h.appRunnerCatalog(r, b)
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	if r.URL.Path != "/api/me/runner-catalog/events" {
		w.Header().Set("ETag", `"`+snapshot.Revision+`"`)
		if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
			w.WriteHeader(304)
			return
		}
		respondJSON(w, snapshot)
		return
	}
	principal, client := h.authenticateAppRequest(r)
	if principal == nil {
		respondJSONError(w, 401, "app authentication required")
		return
	}
	owner := principal.ID + "|" + client.ID
	b.mu.Lock()
	if b.catalogSubscribers == nil {
		b.catalogSubscribers = map[string]int{}
	}
	if b.catalogSubscribers[owner] >= 8 {
		b.mu.Unlock()
		respondJSONError(w, 429, "too many catalog subscriptions")
		return
	}
	b.catalogSubscribers[owner]++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.catalogSubscribers[owner]--
		if b.catalogSubscribers[owner] == 0 {
			delete(b.catalogSubscribers, owner)
		}
		b.mu.Unlock()
	}()
	if _, ok := w.(http.Flusher); !ok {
		respondJSONError(w, 503, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(snapshot runnerCatalogSnapshot) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: catalog.snapshot\ndata: %s\n\n", snapshot.Revision, raw); err != nil {
			return err
		}
		return controller.Flush()
	}
	// Always send authoritative state on reconnect, including a lost or unknown cursor.
	if write(snapshot) != nil {
		return
	}
	last := snapshot.Revision
	poll := time.NewTicker(5 * time.Second) // Quota, model discovery and policy changes also affect the pool view.
	defer poll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-b.stopped:
			return
		case <-changes:
		case <-poll.C:
		}
		changes = b.catalogChanges()
		next, err := h.appRunnerCatalog(r, b)
		if err != nil {
			return
		} // Revoked credentials must never keep receiving updates.
		if next.Revision != last {
			if write(next) != nil {
				return
			}
			last = next.Revision
		} else {
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}
