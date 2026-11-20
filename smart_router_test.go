package main

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"codex-pool-proxy/internal/routing"
)

func routingTestAccount(id string, used float64, resetAt time.Time) *Account {
	return &Account{
		ID:       id,
		Type:     AccountTypeCodex,
		PlanType: "pro",
		Usage: UsageSnapshot{
			SecondaryUsedPercent:   used,
			SecondaryUsageReported: true,
			SecondaryWindowMinutes: 7 * 24 * 60,
			SecondaryResetAt:       resetAt,
		},
	}
}

func TestDefaultRoutingProfileWeights(t *testing.T) {
	profiles := defaultRoutingProfiles()
	tests := []struct {
		profile RoutingProfile
		want    routing.Weights
	}{
		{
			profile: RoutingBalanced,
			want: routing.Weights{
				QuotaHeadroom: 0.25, ResetUrgency: 0.15, Health: 0.20,
				TTFT: 0.10, Throughput: 0.10, Affinity: 0.10,
				Inflight: 0.05, RecentFailure: 0.05,
			},
		},
		{
			profile: RoutingFast,
			want: routing.Weights{
				TTFT: 0.35, Throughput: 0.25, Health: 0.15,
				Inflight: 0.10, QuotaHeadroom: 0.10, Affinity: 0.05,
			},
		},
		{
			profile: RoutingQuotaSaver,
			want: routing.Weights{
				QuotaHeadroom: 0.35, ProjectedQuota: 0.25, ResetTiming: 0.15,
				Health: 0.10, Affinity: 0.10, Performance: 0.05,
			},
		},
		{
			profile: RoutingDrain,
			want: routing.Weights{
				ResetProximity: 0.35, RemainingQuota: 0.30, Health: 0.15,
				Throughput: 0.10, Inflight: 0.10,
			},
		},
	}
	for _, test := range tests {
		t.Run(string(test.profile), func(t *testing.T) {
			got := profiles[test.profile]
			if got != test.want {
				t.Fatalf("weights = %+v, want %+v", got, test.want)
			}
			if math.Abs(got.Total()-1) > 1e-9 {
				t.Fatalf("weight total = %.6f, want 1", got.Total())
			}
		})
	}
}

func TestSmartRouterProfilesSelectDeterministically(t *testing.T) {
	now := time.Now()
	abundant := routingTestAccount("abundant", 0.10, now.Add(6*24*time.Hour))
	speedy := routingTestAccount("speedy", 0.80, now.Add(6*24*time.Hour))
	expiring := routingTestAccount("expiring", 0.15, now.Add(30*time.Minute))
	pinned := routingTestAccount("pinned", 0.85, now.Add(6*24*time.Hour))

	pool := newPoolState([]*Account{abundant, speedy, expiring, pinned}, false)
	pool.routingTelemetry = map[string]routingTelemetry{
		"abundant": {Samples: 10, SuccessEWMA: 1, TTFTMsEWMA: 1000, ThroughputEWMA: 10},
		"speedy":   {Samples: 10, SuccessEWMA: 1, TTFTMsEWMA: 50, ThroughputEWMA: 220},
		"expiring": {Samples: 10, SuccessEWMA: 1, TTFTMsEWMA: 400, ThroughputEWMA: 80},
		"pinned":   {Samples: 10, SuccessEWMA: 1, TTFTMsEWMA: 1800, ThroughputEWMA: 5},
	}

	tests := []struct {
		profile RoutingProfile
		convID  string
		want    string
	}{
		{profile: RoutingBalanced, want: "expiring"},
		{profile: RoutingFast, want: "speedy"},
		{profile: RoutingThroughput, want: "speedy"},
		{profile: RoutingQuotaSaver, want: "expiring"},
		{profile: RoutingDrain, want: "expiring"},
		{profile: RoutingSticky, convID: "sticky-conversation", want: "pinned"},
	}
	pool.convPin["sticky-conversation"] = "pinned"

	for _, test := range tests {
		t.Run(string(test.profile), func(t *testing.T) {
			decision := pool.selectRoute(routing.Request{ConversationID: test.convID, Provider: string(AccountTypeCodex), RequiredPlan: "pro", Model: "gpt-5.5", Profile: string(test.profile)})
			if decision.Account == nil {
				t.Fatal("no account selected")
			}
			if decision.Account.ID != test.want {
				t.Fatalf("selected %q, want %q (score=%f reasons=%v)", decision.Account.ID, test.want, decision.Score, decision.Reasons)
			}
			if decision.Policy != string(test.profile) {
				t.Fatalf("routing policy = %q, want %q", decision.Policy, test.profile)
			}
		})
	}
}

func TestSmartRouterSkipsHealthBlockedAccounts(t *testing.T) {
	now := time.Now()
	blocked := routingTestAccount("blocked", 0.01, now.Add(6*24*time.Hour))
	blocked.NeedsVerification = true
	healthy := routingTestAccount("healthy", 0.80, now.Add(6*24*time.Hour))
	pool := newPoolState([]*Account{blocked, healthy}, false)

	decision := pool.selectRoute(routing.Request{Provider: string(AccountTypeCodex), RequiredPlan: "pro", Model: "gpt-5.5", Profile: string(RoutingBalanced)})
	if decision.Account != healthy {
		t.Fatalf("selected %v, want healthy account", decision.Account)
	}
}

func TestSmartRouterConfigurationAndAliases(t *testing.T) {
	cfg := RoutingConfigFile{
		DefaultProfile: "fast",
		DefaultModel:   "gpt-test",
		Profiles: map[string]routing.Weights{
			"fast": {TTFT: 0.70, Throughput: 0.30},
		},
	}
	if err := validateRoutingConfig(cfg); err != nil {
		t.Fatalf("validate routing config: %v", err)
	}
	pool := newPoolState(nil, false)
	pool.configureRouting(cfg)
	if got := pool.defaultRoutingProfile(); got != RoutingFast {
		t.Fatalf("default profile = %q, want fast", got)
	}
	weights, _ := pool.routing.weights(RoutingFast)
	if weights.TTFT <= weights.Throughput {
		t.Fatalf("override was not applied: %+v", weights)
	}
	onlyHealth := newRoutingPolicySet(RoutingConfigFile{
		Profiles: map[string]routing.Weights{"balanced": {Health: 1}},
	})
	weights, _ = onlyHealth.weights(RoutingBalanced)
	if weights.Health != 1 || weights.QuotaHeadroom != 0 {
		t.Fatalf("full profile replacement must allow zero weights: %+v", weights)
	}
	legacy := newRoutingPolicySet(RoutingConfigFile{DefaultProfile: "legacy"})
	if legacy.DefaultProfile != RoutingLegacy {
		t.Fatalf("legacy default profile = %q", legacy.DefaultProfile)
	}

	tests := []struct {
		name        string
		header      string
		model       string
		wantProfile RoutingProfile
		wantModel   string
	}{
		{name: "default", model: "gpt-5.5", wantProfile: RoutingFast, wantModel: "gpt-5.5"},
		{name: "header", header: "quota-saver", model: "gpt-5.5", wantProfile: RoutingQuotaSaver, wantModel: "gpt-5.5"},
		{name: "alias default model", model: "pool/drain", wantProfile: RoutingDrain, wantModel: "gpt-test"},
		{name: "alias slash model", model: "pool/throughput/gpt-5.6", wantProfile: RoutingThroughput, wantModel: "gpt-5.6"},
		{name: "alias colon model", model: "pool/sticky:gpt-5.4", wantProfile: RoutingSticky, wantModel: "gpt-5.4"},
		{name: "header wins", header: "balanced", model: "pool/fast/gpt-5.6", wantProfile: RoutingBalanced, wantModel: "gpt-5.6"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/responses", nil)
			req.Header.Set("X-Pool-Routing", test.header)
			body, _ := json.Marshal(map[string]any{"model": test.model, "input": "hello"})
			profile, model, rewritten, err := resolveRequestRouting(pool, req, test.model, body)
			if err != nil {
				t.Fatalf("resolve routing: %v", err)
			}
			if profile != test.wantProfile || model != test.wantModel {
				t.Fatalf("resolved (%q, %q), want (%q, %q)", profile, model, test.wantProfile, test.wantModel)
			}
			var object map[string]any
			if err := json.Unmarshal(rewritten, &object); err != nil {
				t.Fatalf("decode rewritten body: %v", err)
			}
			if got, _ := object["model"].(string); got != test.wantModel {
				t.Fatalf("body model = %q, want %q", got, test.wantModel)
			}
		})
	}

	bad := []RoutingConfigFile{
		{DefaultProfile: "random"},
		{Profiles: map[string]routing.Weights{"random": {Health: 1}}},
		{Profiles: map[string]routing.Weights{"fast": {TTFT: -1}}},
	}
	for _, invalid := range bad {
		if err := validateRoutingConfig(invalid); err == nil {
			t.Fatalf("expected invalid config to fail: %+v", invalid)
		}
	}
}

func TestSmartRouterExplainabilityAndLegacyMode(t *testing.T) {
	account := routingTestAccount("only", 0.25, time.Now().Add(24*time.Hour))
	pool := newPoolState([]*Account{account}, false)
	pool.routingTelemetry["only"] = routingTelemetry{
		Samples: 4, SuccessEWMA: 0.9, TTFTMsEWMA: 120, ThroughputEWMA: 75,
	}

	decision := pool.selectRoute(routing.Request{
		ConversationID: "conversation", Provider: string(AccountTypeCodex), RequiredPlan: "pro",
		Model: "gpt-5.5", Profile: string(RoutingBalanced), Trace: true,
	})
	if decision.Account != account || decision.Policy != "balanced" || decision.Score <= 0 {
		t.Fatalf("selection = (%v, %q, %.3f)", decision.Account, decision.Policy, decision.Score)
	}
	breakdown := decision.Breakdown
	if len(decision.Reasons) < 2 || breakdown == nil || breakdown.Weights == nil {
		t.Fatalf("missing explanation: reasons=%v breakdown=%+v", decision.Reasons, breakdown)
	}
	if breakdown.Profile != "balanced" || breakdown.QuotaScore <= 0 || breakdown.HealthScore <= 0 {
		t.Fatalf("incomplete breakdown: %+v", breakdown)
	}

	legacy := pool.selectRoute(routing.Request{
		Provider: string(AccountTypeCodex), RequiredPlan: "pro", Model: "gpt-5.5", Profile: string(RoutingLegacy),
	}).Account
	if legacy != account {
		t.Fatalf("legacy selected %v, want %v", legacy, account)
	}
}

func BenchmarkSmartRouterProfiles(b *testing.B) {
	now := time.Now()
	accounts := make([]*Account, 0, 32)
	pool := newPoolState(accounts, false)
	for index := 0; index < 32; index++ {
		account := routingTestAccount(
			string(rune('a'+index%26))+"-account",
			float64(index%9)/10,
			now.Add(time.Duration(index+1)*time.Hour),
		)
		accounts = append(accounts, account)
		pool.routingTelemetry[account.ID] = routingTelemetry{
			Samples: 20, SuccessEWMA: 0.95,
			TTFTMsEWMA:     float64(50 + index*20),
			ThroughputEWMA: float64(20 + index*5),
		}
	}
	pool.replace(accounts)
	profiles := []RoutingProfile{
		RoutingBalanced, RoutingFast, RoutingThroughput,
		RoutingQuotaSaver, RoutingDrain, RoutingSticky,
	}
	for _, profile := range profiles {
		b.Run(string(profile), func(b *testing.B) {
			req := routing.Request{
				Provider: string(AccountTypeCodex), RequiredPlan: "pro", Model: "gpt-5.5", Profile: string(profile),
			}
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				decision := pool.selectRoute(req)
				if decision.Account == nil {
					b.Fatal("no account selected")
				}
			}
			b.StopTimer()
			req.Trace = true
			decision := pool.selectRoute(req)
			b.ReportMetric(decision.Score, "route_score")
			b.ReportMetric(decision.Breakdown.HealthScore, "health")
			b.ReportMetric(decision.Breakdown.QuotaScore, "quota")
			b.ReportMetric(decision.Breakdown.LatencyScore, "ttft")
			b.ReportMetric(decision.Breakdown.ThroughputScore, "throughput")
		})
	}
}
