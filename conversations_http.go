package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

type conversationHistoryPage struct {
	Conversation conversationRecord  `json:"conversation"`
	Session      runnerproto.Session `json:"session"`
	Events       []sessionEvent      `json:"events"`
	Before       uint64              `json:"before"`
	Through      uint64              `json:"through"`
	HasOlder     bool                `json:"has_older"`
}

func (b *sessionBroker) recentHistory(principal, id string, before, through *uint64, limit int) (*conversationHistoryPage, error) {
	if limit < 1 || limit > 256 {
		return nil, sessionError(400, "invalid history page size")
	}
	page := &conversationHistoryPage{Events: []sessionEvent{}}
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket([]byte(bucketConversations)).Get([]byte(id))
		if raw == nil {
			return sessionError(404, "conversation not found")
		}
		if err := json.Unmarshal(raw, &page.Conversation); err != nil {
			return err
		}
		c := page.Conversation
		if c.PrincipalID != principal {
			return sessionError(404, "conversation not found")
		}
		if !c.HistoryComplete {
			return sessionError(409, "history was not recorded for this conversation")
		}
		raw = tx.Bucket([]byte(bucketRunnerSessions)).Get([]byte(c.SessionID))
		if raw == nil {
			return errors.New("conversation session missing")
		}
		var s sessionRecord
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if s.PrincipalID != principal || s.ConversationID != id || s.EventCursor != c.EventCursor {
			return errors.New("conversation journal identity mismatch")
		}
		page.Session = s.Session
		end := c.EventCursor
		if through != nil {
			end = *through
		}
		if end > c.EventCursor {
			return sessionError(409, "invalid history boundary")
		}
		start := end + 1
		if before != nil {
			start = *before
		}
		if start == 0 || start > end+1 {
			return sessionError(409, "invalid history cursor")
		}
		page.Through = end
		page.Before = start
		if end == 0 {
			return nil
		}
		reader, err := newConversationEventReader(tx, &c)
		if err != nil {
			return err
		}
		expected := start - 1
		size := 0
		for expected > 0 && len(page.Events) < limit && size < 512<<10 {
			event, err := reader.event(expected)
			if err != nil {
				return err
			}
			page.Events = append(page.Events, event)
			size += len(event.Data)
			page.Before = event.ID
			expected--
		}
		page.HasOlder = expected > 0
		slices.Reverse(page.Events)
		return nil
	})
	return page, err
}

func (h *proxyHandler) handleConversations(w http.ResponseWriter, r *http.Request) {
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
	path := strings.TrimPrefix(r.URL.Path, "/api/conversations")
	if path == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			respondJSONError(w, 405, "method not allowed")
			return
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			respondJSONError(w, 400, "conversation page size required")
			return
		}
		page, err := b.conversations(principal.ID, r.URL.Query().Get("storage"), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, page)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	id := parts[0]
	c, err := b.conversation(principal.ID, id)
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			respondJSONError(w, 405, "method not allowed")
			return
		}
		respondJSON(w, c)
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	if parts[1] == "resume" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			respondJSONError(w, 405, "method not allowed")
			return
		}
		s, err := b.session(principal.ID, c.SessionID)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		if err := h.authorizeSessionModel(principal.ID+"-c-"+client.ID, runnerproto.CreateSession{Agent: s.Agent, Model: s.Model, Workspace: s.Workspace, PermissionMode: s.PermissionMode}); err != nil {
			sessionHTTPError(w, err)
			return
		}
		resumed, err := b.resumeConversation(principal.ID, client.ID, c.ID)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, resumed)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	switch parts[1] {
	case "events":
		h.serveSessionEvents(w, r, b, principal.ID, c.SessionID)
	case "history":
		for name := range r.URL.Query() {
			if name != "limit" && name != "before" && name != "through" {
				respondJSONError(w, 400, "unknown history parameter")
				return
			}
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			respondJSONError(w, 400, "history page size required")
			return
		}
		parse := func(name string) (*uint64, error) {
			raw := r.URL.Query().Get(name)
			if raw == "" {
				return nil, nil
			}
			value, err := strconv.ParseUint(raw, 10, 64)
			return &value, err
		}
		before, err := parse("before")
		if err != nil {
			respondJSONError(w, 400, "invalid history cursor")
			return
		}
		through, err := parse("through")
		if err != nil {
			respondJSONError(w, 400, "invalid history boundary")
			return
		}
		page, err := b.recentHistory(principal.ID, id, before, through, limit)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, page)
	default:
		http.NotFound(w, r)
	}
}
