package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func sessionHTTPError(w http.ResponseWriter, err error) {
	var fault *sessionFault
	if errors.As(err, &fault) {
		respondJSONError(w, fault.status, fault.message)
		return
	}
	respondJSONError(w, http.StatusServiceUnavailable, "session service unavailable")
}

func decodeSessionJSON(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return sessionError(400, "invalid request JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return sessionError(400, "request must contain one JSON value")
	}
	return nil
}

func (h *proxyHandler) handleRunnerEnrollment(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	var q runnerproto.EnrollRequest
	if err := decodeSessionJSON(w, r, &q); err != nil {
		sessionHTTPError(w, err)
		return
	}
	result, err := b.enroll(q)
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	respondJSON(w, result)
}

func (h *proxyHandler) handleMyRunners(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	principal, session, _ := h.authenticateMemberRequest(r)
	if principal == nil {
		respondJSONError(w, 401, "authentication required")
		return
	}
	if r.Method != http.MethodGet && session != nil && !h.passportCSRF(r, session) {
		respondJSONError(w, 403, "csrf validation failed")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/me/runners")
	if path == "" {
		switch r.Method {
		case http.MethodGet:
			items, err := b.listRunners(principal.ID)
			if err != nil {
				sessionHTTPError(w, err)
				return
			}
			respondJSON(w, map[string]any{"runners": items})
		case http.MethodPost:
			result, err := b.enrollment(principal.ID)
			if err != nil {
				sessionHTTPError(w, err)
				return
			}
			respondJSON(w, result)
		default:
			w.Header().Set("Allow", "GET, POST")
			respondJSONError(w, 405, "method not allowed")
		}
		return
	}
	id := strings.TrimPrefix(path, "/")
	if parts := strings.Split(id, "/"); len(parts) == 2 && parts[1] == "rotate" && parts[0] != "" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			respondJSONError(w, 405, "method not allowed")
			return
		}
		result, err := b.newEnrollment(principal.ID, parts[0])
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, result)
		return
	}
	if strings.Contains(id, "/") || id == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	if err := b.revokeRunner(principal.ID, id); err != nil {
		sessionHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *proxyHandler) authorizeSessionModel(identity string, q runnerproto.CreateSession) error {
	if h.pool == nil || h.cfg == nil || h.pool.accountAuthority != h.passport {
		return errors.New("account authority unavailable")
	}
	visible, err := h.memberQuotaSnapshot(identity, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, model := range poolModelsForClients(visible) {
		if model.ID == q.Model {
			protocol := "responses"
			if q.Agent == "claude" {
				protocol = "anthropic"
			}
			if model.Protocol == protocol || supportsAgent(model.Protocols, protocol) {
				return nil
			}
			// Protocol labels in the public catalog name wire formats explicitly.
			if q.Agent == "codex" && (model.Protocol == "openai" || supportsAgent(model.Protocols, "openai")) {
				return nil
			}
			return sessionError(400, "model does not support the selected agent protocol")
		}
	}
	return sessionError(403, "model is not in the authorized catalog")
}

func (h *proxyHandler) handleSessions(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	principal, client := h.authenticateAppRequest(r)
	if principal == nil {
		respondJSONError(w, 401, "app authentication required")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	select {
	case <-b.stopped:
		respondJSONError(w, 503, "session broker unavailable")
		return
	default:
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions")
	if path == "" {
		switch r.Method {
		case http.MethodGet:
			respondJSON(w, map[string]any{"sessions": b.list(principal.ID)})
		case http.MethodPost:
			var q runnerproto.CreateSession
			if err := decodeSessionJSON(w, r, &q); err != nil {
				sessionHTTPError(w, err)
				return
			}
			if err := validateSessionRequest(q); err != nil {
				sessionHTTPError(w, err)
				return
			}
			if err := h.authorizeSessionModel(principal.ID+"-c-"+client.ID, q); err != nil {
				sessionHTTPError(w, err)
				return
			}
			result, err := b.create(principal.ID, client.ID, q)
			if err != nil {
				sessionHTTPError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			respondJSON(w, result)
		default:
			w.Header().Set("Allow", "GET, POST")
			respondJSONError(w, 405, "method not allowed")
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if _, err := b.session(principal.ID, id); err != nil {
		sessionHTTPError(w, err)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			result, err := b.session(principal.ID, id)
			if err != nil {
				sessionHTTPError(w, err)
				return
			}
			respondJSON(w, result)
		case http.MethodDelete:
			if err := b.requestClose(principal.ID, id); err != nil {
				sessionHTTPError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			respondJSONError(w, 405, "method not allowed")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			respondJSONError(w, 405, "method not allowed")
			return
		}
		h.serveSessionEvents(w, r, b, principal.ID, id)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	if len(parts) == 2 && parts[1] == "turns" {
		var q struct {
			Input string `json:"input"`
		}
		if err := decodeSessionJSON(w, r, &q); err != nil {
			sessionHTTPError(w, err)
			return
		}
		if strings.TrimSpace(q.Input) == "" || len(q.Input) > runnerproto.MaxInputBytes {
			respondJSONError(w, 400, "invalid turn input")
			return
		}
		turn, err := b.startTurn(principal.ID, id, q.Input)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, map[string]any{"turn_id": turn})
		return
	}
	if len(parts) == 2 && parts[1] == "turns:cancel" {
		if err := b.cancelTurn(principal.ID, id); err != nil {
			sessionHTTPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 3 && parts[1] == "approvals" && parts[2] != "" {
		var q struct {
			Decision string `json:"decision"`
		}
		if err := decodeSessionJSON(w, r, &q); err != nil {
			sessionHTTPError(w, err)
			return
		}
		if err := b.resolveApproval(principal.ID, id, parts[2], q.Decision); err != nil {
			sessionHTTPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

func (h *proxyHandler) serveSessionEvents(w http.ResponseWriter, r *http.Request, b *sessionBroker, principal, id string) {
	if _, ok := w.(http.Flusher); !ok {
		respondJSONError(w, 503, "streaming unavailable")
		return
	}
	var cursor uint64
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			respondJSONError(w, 400, "invalid event cursor")
			return
		}
		cursor = value
	}
	b.mu.Lock()
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		b.mu.Unlock()
		sessionHTTPError(w, err)
		return
	}
	stream := b.streams[id]
	if stream == nil {
		stream = &sessionStream{subscribers: map[*sessionSubscription]struct{}{}}
		b.streams[id] = stream
	}
	if len(stream.subscribers) >= 8 {
		b.mu.Unlock()
		respondJSONError(w, 429, "too many event subscriptions")
		return
	}
	if cursor > stream.next || (cursor > 0 && len(stream.history) > 0 && cursor+1 < stream.history[0].ID) {
		b.mu.Unlock()
		respondJSONError(w, 409, "event cursor expired; reload session")
		return
	}
	sub := &sessionSubscription{events: make(chan sessionEvent, 128), failed: make(chan struct{})}
	stream.subscribers[sub] = struct{}{}
	initial := []sessionEvent{}
	if cursor > 0 {
		for _, event := range stream.history {
			if event.ID > cursor {
				initial = append(initial, event)
			}
		}
	} else {
		data, _ := json.Marshal(map[string]any{"state": s.State})
		initial = append(initial, sessionEvent{Type: "state", Data: data})
		for _, a := range s.Approvals {
			data, _ := json.Marshal(a)
			initial = append(initial, sessionEvent{Type: "approval.requested", Data: data})
		}
		if s.State == "closed" {
			data, _ := json.Marshal(map[string]any{"reason": s.ClosedReason})
			initial = append(initial, sessionEvent{Type: "session.closed", Data: data})
		}
	}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(stream.subscribers, sub); b.mu.Unlock() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	if controller.Flush() != nil {
		return
	}
	write := func(event sessionEvent) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if event.ID > 0 {
			if _, err := fmt.Fprintf(w, "id: %d\n", event.ID); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Data); err != nil {
			return err
		}
		return controller.Flush()
	}
	for _, event := range initial {
		if write(event) != nil {
			return
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-b.stopped:
			return
		case <-sub.failed:
			_ = write(sessionEvent{Type: "error", Data: json.RawMessage(`{"message":"event consumer too slow; reload session"}`)})
			return
		case event := <-sub.events:
			if write(event) != nil {
				return
			}
		case <-heartbeat.C:
			if p, _ := h.authenticateAppRequest(r); p == nil {
				_ = write(sessionEvent{Type: "error", Data: json.RawMessage(`{"message":"authentication expired"}`)})
				return
			}
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}
