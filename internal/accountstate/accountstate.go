// Package accountstate defines the unified account lifecycle model (CP-03).
//
// v1 is a PROJECTION: states are derived from the observable facts the pool
// already tracks (dead, disabled, verification, health, cooldowns, expiry,
// usage thresholds) rather than stored authoritatively. This keeps the
// migration risk near zero — every consumer of the old booleans keeps
// working — while operators, the admin API, and transition history get one
// predictable vocabulary. Later phases (CP-04 controls) promote
// DRAINING/MAINTENANCE to authoritative states driven by operator input.
package accountstate

import "time"

// State is the unified lifecycle state of a pool account.
type State string

const (
	// StateDiscovered marks an account seen on disk but not yet validated.
	StateDiscovered State = "discovered"
	// StateVerifying marks an account mid-enrollment verification.
	StateVerifying State = "verifying"
	// StateHealthy marks an account routable right now.
	StateHealthy State = "healthy"
	// StateDegraded marks an account blocked by an observed health problem
	// that is not a hard auth failure (e.g. health check error).
	StateDegraded State = "degraded"
	// StateCooldown marks an account temporarily excluded: rate limited or
	// usage-exhausted until a known reset.
	StateCooldown State = "cooldown"
	// StateDraining marks an operator-requested graceful removal (CP-04).
	StateDraining State = "draining"
	// StateMaintenance marks an operator-requested pause (CP-04).
	StateMaintenance State = "maintenance"
	// StateNeedsLogin marks an account whose interactive login expired.
	StateNeedsLogin State = "needs_login"
	// StateNeedsVerification marks an account pending upstream verification.
	StateNeedsVerification State = "needs_verification"
	// StateExpired marks an account whose credentials expired.
	StateExpired State = "expired"
	// StateDead marks an account permanently failed (auth/refresh).
	StateDead State = "dead"
	// StateDisabled marks an account switched off by the operator.
	StateDisabled State = "disabled"
)

// routableStates are the states eligible for new traffic. Expiry does NOT
// block routing in this pool: credentials are refreshed on demand during the
// request (only conversation pin revalidation avoids expired accounts to skip
// the refresh latency), so StateExpired stays routable and degrades to dead
// only when refresh actually fails.
var routableStates = map[State]bool{
	StateHealthy: true,
	StateExpired: true,
	// Draining keeps existing conversations but should not receive new pins;
	// availability of drained accounts for pinned traffic is a CP-04 decision.
}

// Facts are the observable inputs the projection derives a State from.
// Booleans mirror the predicates in account_availability.go so the mapping
// stays honest.
type Facts struct {
	Disabled           bool
	Dead               bool
	NeedsVerification  bool // NeedsVerification || VerificationURL set
	HealthError        bool
	CredentialsExpired bool
	RateLimited        bool // RateLimitUntil in the future
	UsageExhausted     bool // >= primary/secondary hard-exclude thresholds
}

// Derive maps facts onto the unified state. Precedence follows removal
// semantics: operator overrides first, then permanent failure, then
// interaction-required, then temporal blocks and health, then expiry
// (routable — refresh happens on demand), then healthy.
func Derive(f Facts) State {
	switch {
	case f.Disabled:
		return StateDisabled
	case f.Dead:
		return StateDead
	case f.NeedsVerification:
		return StateNeedsVerification
	case f.HealthError:
		return StateDegraded
	case f.RateLimited, f.UsageExhausted:
		return StateCooldown
	case f.CredentialsExpired:
		return StateExpired
	default:
		return StateHealthy
	}
}

// Reason explains why the state was derived (for admin surfaces and logs).
func (f Facts) Reason() string {
	switch Derive(f) {
	case StateDisabled:
		return "disabled_by_operator"
	case StateDead:
		return "permanent_failure"
	case StateNeedsVerification:
		return "upstream_verification_required"
	case StateExpired:
		return "credentials_expired"
	case StateDegraded:
		return "health_error"
	case StateCooldown:
		if f.RateLimited {
			return "rate_limited_until_reset"
		}
		return "usage_exhausted_until_reset"
	default:
		return "routable"
	}
}

// Routable reports whether the state admits new traffic.
func Routable(s State) bool { return routableStates[s] }

// Transition records one observed lifecycle change.
type Transition struct {
	From   State
	To     State
	Reason string
	Actor  string // "system" or an operator/admin identity
	At     time.Time
}

// validTransitions enumerates allowed edges. The projection can observe any
// fact-driven change, so the table is intentionally permissive; the entries
// marked false are nonsensical jumps reserved for the authoritative phase
// (e.g. leaving disabled without an operator action).
var validTransitions = map[State]map[State]bool{
	StateDiscovered:        {StateVerifying: true, StateHealthy: true, StateDead: true, StateDisabled: true, StateNeedsVerification: true},
	StateVerifying:         {StateHealthy: true, StateNeedsVerification: true, StateDead: true, StateDisabled: true},
	StateHealthy:           {StateCooldown: true, StateDegraded: true, StateDead: true, StateDisabled: true, StateDraining: true, StateMaintenance: true, StateNeedsLogin: true, StateNeedsVerification: true, StateExpired: true},
	StateDegraded:          {StateHealthy: true, StateDead: true, StateDisabled: true, StateCooldown: true, StateNeedsVerification: true},
	StateCooldown:          {StateHealthy: true, StateDegraded: true, StateDead: true, StateDisabled: true, StateNeedsVerification: true},
	StateDraining:          {StateHealthy: true, StateDisabled: true, StateDead: true},
	StateMaintenance:       {StateHealthy: true, StateDisabled: true, StateDead: true},
	StateNeedsLogin:        {StateHealthy: true, StateDead: true, StateDisabled: true, StateNeedsVerification: true},
	StateNeedsVerification: {StateHealthy: true, StateDead: true, StateDisabled: true, StateExpired: true},
	StateExpired:           {StateHealthy: true, StateDead: true, StateDisabled: true, StateNeedsLogin: true},
	StateDead:              {StateHealthy: true, StateDisabled: true}, // resurrection happens via successful refresh
	StateDisabled:          {StateHealthy: true, StateDead: true, StateMaintenance: true, StateDraining: true},
}

// CanTransition reports whether the from -> to edge is allowed.
func CanTransition(from, to State) bool {
	if from == to {
		return true
	}
	if edges, ok := validTransitions[from]; ok {
		return edges[to]
	}
	return false
}
