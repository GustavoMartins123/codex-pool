package main

import (
	"codex-pool-proxy/internal/runnerproto"
	"sort"
)

// Historical benchmark baseline only; these contracts are not shipped in production.
func (b *sessionBroker) list(principal string) []runnerproto.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []runnerproto.Session{}
	for _, s := range b.sessions {
		if s.PrincipalID == principal {
			out = append(out, copySession(s).Session)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

type sessionHistoryPage struct {
	Session runnerproto.Session `json:"session"`
	Events  []sessionEvent      `json:"events"`
	Cursor  uint64              `json:"cursor"`
	Through uint64              `json:"through"`
	HasMore bool                `json:"has_more"`
}

func (b *sessionBroker) history(principal, id string, after uint64, through *uint64) (*sessionHistoryPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		return nil, err
	}
	if !s.HistoryComplete {
		return nil, sessionError(409, "history was not recorded for this session")
	}
	end := s.EventCursor
	if through != nil {
		end = *through
	}
	events, err := b.readEventsLocked(s, after, end, 256)
	if err != nil {
		return nil, err
	}
	cursor := after
	if len(events) > 0 {
		cursor = events[len(events)-1].ID
	}
	return &sessionHistoryPage{copySession(s).Session, events, cursor, end, cursor < end}, nil
}
