package main

import (
	"testing"
	"testing/synctest"
	"time"

	"codex-pool-proxy/internal/errclass"
	"codex-pool-proxy/internal/routing"
)

func TestAccountRoutingContract(t *testing.T) {
	const model = "gpt-daybreak-blue-latest"
	for _, profile := range []RoutingProfile{RoutingLegacy, RoutingBalanced} {
		t.Run(string(profile), func(t *testing.T) {
			t.Run("model entitlement and quota", func(t *testing.T) {
				models := map[string]DiscoveredModel{model: {ID: model}}
				pool := newPoolState([]*Account{
					{ID: "different-model", Type: AccountTypeCodex, PlanType: "pro"},
					{ID: "quota-exhausted", Type: AccountTypeCodex, PlanType: "pro",
						Models: models, Usage: UsageSnapshot{PrimaryUsedPercent: 0.97}},
					{ID: "available", Type: AccountTypeCodex, PlanType: "pro", Models: models,
						Usage: UsageSnapshot{PrimaryUsedPercent: 0.42}},
				}, false)
				account := pool.selectRoute(routing.Request{
					UserID: "alice", Provider: string(AccountTypeCodex), RequiredPlan: "pro",
					ClientIP: "127.0.0.1", Model: model, Profile: string(profile), Trace: true,
				}).Account
				if account == nil || account.ID != "available" {
					t.Fatalf("selection = %+v, want available", account)
				}
			})

			t.Run("account visibility", func(t *testing.T) {
				_, pool := ownershipFixture(t)
				for _, user := range []string{"alice", "bob"} {
					account := pool.selectRoute(routing.Request{
						UserID: user, Provider: string(AccountTypeCodex), RequiredPlan: "pro",
						ClientIP: "127.0.0.1", Model: "gpt-5.5", Profile: string(profile), Trace: true,
					}).Account
					if user == "alice" && (account == nil || account.ID != "private") {
						t.Fatalf("owner selection = %+v, want private", account)
					}
					if user == "bob" && account != nil {
						t.Fatalf("non-owner selection = %+v, want no account", account)
					}
				}
			})

			t.Run("rate limited and open circuit", func(t *testing.T) {
				pool := newPoolState([]*Account{
					{ID: "limited", Type: AccountTypeClaude, PlanType: "max", RateLimitUntil: time.Now().Add(time.Hour)},
					{ID: "open", Type: AccountTypeClaude, PlanType: "max"},
				}, false)
				for i := 0; i < 3; i++ {
					pool.circuitBreakers.RecordFailure("claude", "open", "", nil, errclass.Transient.Retryable())
				}
				account := pool.selectRoute(routing.Request{
					UserID: "alice", Provider: string(AccountTypeClaude), ClientIP: "127.0.0.1",
					Profile: string(profile), Trace: true,
				}).Account
				if account != nil {
					t.Fatalf("selection = %+v, want no eligible account", account)
				}
			})
		})
	}
}

func TestCircuitBreakerCandidateSelectionDoesNotConsumeUnselectedProbes(t *testing.T) {
	const model = "gpt-5.5"
	for _, profile := range []RoutingProfile{RoutingLegacy, RoutingBalanced} {
		t.Run(string(profile), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first := &Account{ID: "acc_1", Type: AccountTypeCodex, PlanType: "pro",
					Usage: UsageSnapshot{PrimaryUsedPercent: 0.8}}
				second := &Account{ID: "acc_2", Type: AccountTypeCodex, PlanType: "pro",
					Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
				pool := newPoolState([]*Account{first, second}, false)
				for _, account := range []*Account{first, second} {
					for i := 0; i < 3; i++ {
						pool.circuitBreakers.RecordFailure("codex", account.ID, model, nil, true)
					}
				}
				time.Sleep(46 * time.Second)

				got := pool.selectRoute(routing.Request{
					Provider: string(AccountTypeCodex), Model: model, Profile: string(profile),
				}).Account
				if got != second {
					t.Fatalf("selected %v, want %s", got, second.ID)
				}
				pool.circuitBreakers.RecordSuccess("codex", second.ID, model, nil)
				if allowed, _, err := pool.circuitBreakers.AllowTarget("codex", first.ID, model, nil); !allowed {
					t.Fatalf("unselected account could not probe: %v", err)
				}
			})
		})
	}
}
