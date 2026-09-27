package main

import (
	"log"
	"strings"
	"time"

	"codex-pool-proxy/internal/accountstate"
)

// maxAccountStateHistory bounds the retained transition log per account.
const maxAccountStateHistory = 20

// accountStateFactsLocked projects an Account onto accountstate.Facts using
// the same predicates the router consumes (account_availability.go), so the
// unified state can never disagree with routing behavior.
func accountStateFactsLocked(a *Account, now time.Time) accountstate.Facts {
	if a == nil {
		return accountstate.Facts{Dead: true}
	}
	return accountstate.Facts{
		Disabled:           a.Disabled,
		Dead:               a.Dead,
		NeedsVerification:  a.NeedsVerification || strings.TrimSpace(a.VerificationURL) != "",
		HealthError:        strings.TrimSpace(a.HealthError) != "",
		CredentialsExpired: !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now),
		RateLimited:        accountCoolingDownLocked(a, now),
		UsageExhausted:     accountUsageExhaustedLocked(a),
	}
}

// accountLifecycleState computes the current unified state under a.mu.
func accountLifecycleStateLocked(a *Account, now time.Time) accountstate.State {
	return accountstate.Derive(accountStateFactsLocked(a, now))
}

// observeAccountStates refreshes the lifecycle projection of every account
// and records transitions. The state is derived from facts, so an edge the
// transition table marks invalid is applied anyway (parity with routing
// outranks the table) but logged loudly as an anomaly for the authoritative
// phase to tighten.
func (p *poolState) observeAccountStates(now time.Time) {
	if p == nil {
		return
	}
	p.mu.RLock()
	accounts := make([]*Account, len(p.accounts))
	copy(accounts, p.accounts)
	p.mu.RUnlock()

	for _, a := range accounts {
		if a == nil {
			continue
		}
		a.mu.Lock()
		facts := accountStateFactsLocked(a, now)
		state := accountstate.Derive(facts)
		switch {
		case a.LifecycleState == "":
			a.LifecycleState = state
			a.LifecycleReason = facts.Reason()
		case a.LifecycleState != state:
			tr := accountstate.Transition{
				From:   a.LifecycleState,
				To:     state,
				Reason: facts.Reason(),
				Actor:  "system",
				At:     now.UTC(),
			}
			if !accountstate.CanTransition(tr.From, tr.To) {
				log.Printf("account %s anomalous state jump %s -> %s (%s); applying fact-derived state",
					a.ID, tr.From, tr.To, tr.Reason)
			} else {
				log.Printf("account %s state %s -> %s (%s)", a.ID, tr.From, tr.To, tr.Reason)
			}
			a.LifecycleState = state
			a.LifecycleReason = tr.Reason
			a.LifecycleHistory = appendAccountTransition(a.LifecycleHistory, tr)
		}
		a.mu.Unlock()
	}
}

func appendAccountTransition(history []accountstate.Transition, tr accountstate.Transition) []accountstate.Transition {
	history = append(history, tr)
	if excess := len(history) - maxAccountStateHistory; excess > 0 {
		history = history[excess:]
	}
	return history
}

// accountStateSnapshotLocked copies the projection for admin surfaces.
type accountStateSnapshot struct {
	State       accountstate.State        `json:"state"`
	Reason      string                    `json:"reason,omitempty"`
	Routable    bool                      `json:"routable"`
	Transitions []accountstate.Transition `json:"transitions,omitempty"`
}

func accountStateSnapshotLocked(a *Account, now time.Time) accountStateSnapshot {
	facts := accountStateFactsLocked(a, now)
	state := accountstate.Derive(facts)
	history := a.LifecycleHistory
	if len(history) == 0 && a.LifecycleState != "" && a.LifecycleState != state {
		// Fresh transition not yet observed by the sweep.
		history = append(history, accountstate.Transition{From: a.LifecycleState, To: state, Reason: facts.Reason(), Actor: "system", At: now.UTC()})
	}
	return accountStateSnapshot{
		State:       state,
		Reason:      facts.Reason(),
		Routable:    accountstate.Routable(state),
		Transitions: history,
	}
}
