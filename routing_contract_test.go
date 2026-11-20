package main

import (
	"testing"
	"time"
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
				account, _, _, _, _, _ := pool.candidateWithRoutingTraceForUser(
					"alice", "", nil, AccountTypeCodex, "pro", "127.0.0.1", model, profile)
				if account == nil || account.ID != "available" {
					t.Fatalf("selection = %+v, want available", account)
				}
			})

			t.Run("account visibility", func(t *testing.T) {
				_, pool := ownershipFixture(t)
				for _, user := range []string{"alice", "bob"} {
					account, _, _, _, _, _ := pool.candidateWithRoutingTraceForUser(
						user, "", nil, AccountTypeCodex, "pro", "127.0.0.1", "gpt-5.5", profile)
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
					pool.circuitBreakers.RecordFailure("claude", "open", "", nil, ErrorClassTransient)
				}
				account, _, _, _, _, _ := pool.candidateWithRoutingTraceForUser(
					"alice", "", nil, AccountTypeClaude, "", "127.0.0.1", "", profile)
				if account != nil {
					t.Fatalf("selection = %+v, want no eligible account", account)
				}
			})
		})
	}
}
