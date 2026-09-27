package accountstate

import (
	"testing"
	"time"
)

func TestDerivePrecedence(t *testing.T) {
	cases := []struct {
		name string
		f    Facts
		want State
	}{
		{"healthy", Facts{}, StateHealthy},
		{"disabled wins over dead", Facts{Disabled: true, Dead: true}, StateDisabled},
		{"dead wins over verification", Facts{Dead: true, NeedsVerification: true}, StateDead},
		{"verification wins over health error", Facts{NeedsVerification: true, HealthError: true}, StateNeedsVerification},
		{"health error wins over cooldown", Facts{HealthError: true, RateLimited: true}, StateDegraded},
		{"cooldown wins over expiry", Facts{CredentialsExpired: true, RateLimited: true}, StateCooldown},
		{"expired but otherwise fine", Facts{CredentialsExpired: true}, StateExpired},
		{"rate limited", Facts{RateLimited: true}, StateCooldown},
		{"usage exhausted", Facts{UsageExhausted: true}, StateCooldown},
		{"both cooldowns", Facts{RateLimited: true, UsageExhausted: true}, StateCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Derive(tc.f); got != tc.want {
				t.Fatalf("Derive(%+v) = %s, want %s", tc.f, got, tc.want)
			}
		})
	}
}

func TestReason(t *testing.T) {
	if got := (Facts{RateLimited: true}).Reason(); got != "rate_limited_until_reset" {
		t.Fatalf("reason = %q", got)
	}
	if got := (Facts{UsageExhausted: true}).Reason(); got != "usage_exhausted_until_reset" {
		t.Fatalf("reason = %q", got)
	}
	if got := (Facts{}).Reason(); got != "routable" {
		t.Fatalf("reason = %q", got)
	}
}

func TestRoutable(t *testing.T) {
	if !Routable(StateHealthy) {
		t.Fatal("healthy must be routable")
	}
	if !Routable(StateExpired) {
		t.Fatal("expired must stay routable: credentials refresh on demand")
	}
	for _, s := range []State{StateCooldown, StateDegraded, StateDead, StateDisabled, StateNeedsVerification, StateMaintenance, StateDraining, StateNeedsLogin, StateVerifying, StateDiscovered} {
		if Routable(s) {
			t.Fatalf("%s must not be routable in v1", s)
		}
	}
}

func TestCanTransition(t *testing.T) {
	if !CanTransition(StateHealthy, StateCooldown) {
		t.Fatal("healthy -> cooldown must be allowed")
	}
	if !CanTransition(StateDead, StateHealthy) {
		t.Fatal("resurrection via refresh must be allowed")
	}
	if !CanTransition(StateDisabled, StateHealthy) {
		t.Fatal("operator re-enable must be allowed")
	}
	if CanTransition(StateDead, StateCooldown) {
		t.Fatal("dead -> cooldown is nonsensical")
	}
	if CanTransition(StateExpired, StateVerifying) {
		t.Fatal("expired -> verifying is nonsensical")
	}
	if !CanTransition(StateHealthy, StateHealthy) {
		t.Fatal("self transitions are no-ops and always allowed")
	}
}

func TestTransitionRecord(t *testing.T) {
	tr := Transition{From: StateHealthy, To: StateCooldown, Reason: "usage_exhausted_until_reset", Actor: "system", At: time.Now()}
	if tr.From != StateHealthy || tr.To != StateCooldown {
		t.Fatalf("unexpected record: %+v", tr)
	}
}
