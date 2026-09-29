package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestScorePrefersHeadroomAndPlan(t *testing.T) {
	now := time.Now()
	pro := &Account{PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.2, SecondaryUsedPercent: 0.2}}
	plus := &Account{PlanType: "plus", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1}, Penalty: 0.5}

	if scoreAccount(pro, now) <= scoreAccount(plus, now) {
		t.Fatalf("expected pro with headroom to win")
	}
}

func TestScoreBoostsEmptyPrimaryWindow(t *testing.T) {
	now := time.Now()

	// Account with 76% 7d used but 0% 5h used, 4h until primary reset, 40h until secondary reset.
	// Should score much higher than base headroom (0.24) because the 5h window is wide open.
	highWeeklyEmptyPrimary := &Account{
		PlanType: "max",
		Usage: UsageSnapshot{
			PrimaryUsedPercent:   0.0,
			SecondaryUsedPercent: 0.76,
			PrimaryResetAt:       now.Add(4*time.Hour + 6*time.Minute),
			SecondaryResetAt:     now.Add(40 * time.Hour),
		},
	}
	score := scoreAccount(highWeeklyEmptyPrimary, now)
	// With the primary pace bonus, score should be well above 0.5
	if score < 0.5 {
		t.Fatalf("expected score > 0.5 for empty primary window, got %.2f", score)
	}

	// Compare: same 7d usage but primary window is also heavily used. Should score lower.
	highWeeklyHighPrimary := &Account{
		PlanType: "max",
		Usage: UsageSnapshot{
			PrimaryUsedPercent:   0.70,
			SecondaryUsedPercent: 0.76,
			PrimaryResetAt:       now.Add(4*time.Hour + 6*time.Minute),
			SecondaryResetAt:     now.Add(40 * time.Hour),
		},
	}
	scoreBusy := scoreAccount(highWeeklyHighPrimary, now)
	if score <= scoreBusy {
		t.Fatalf("empty primary (%.2f) should outscore busy primary (%.2f)", score, scoreBusy)
	}
}

func TestPenaltyDecay(t *testing.T) {
	now := time.Now()
	a := &Account{Penalty: 1.0, LastPenalty: now.Add(-10 * time.Minute)}
	scoreAccount(a, now)
	if a.Penalty >= 1.0 {
		t.Fatalf("penalty should decay")
	}
}

func TestCandidateUsesPinUnlessExcluded(t *testing.T) {
	// Pro plan is required: the pin path rejects non-pro codex accounts, so
	// a pin test without a plan would silently pass via score selection.
	// a1 is deliberately the WORSE-scoring account — without the pin it can
	// never be selected, so returning it proves the pin branch ran.
	a1 := &Account{ID: "a1", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.90}}
	a2 := &Account{ID: "a2", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.05}}
	p := newPoolState([]*Account{a1, a2}, true)

	// Control: with no pin in place, selection must prefer a2. If this ever
	// flips, the pinned assertions below would stop proving pinning.
	if got := p.candidate("fresh-conversation", nil, "", "", ""); got == nil || got.ID != "a2" {
		t.Fatalf("control: unpinned selection returned %+v, want a2 (best score)", got)
	}

	p.pin("c1", "a1")
	if got := p.candidate("c1", nil, "", "", ""); got == nil || got.ID != "a1" {
		t.Fatalf("expected pinned a1 despite worse score, got %+v", got)
	}
	if got := p.candidate("c1", map[string]bool{"a1": true}, "", "", ""); got == nil || got.ID != "a2" {
		t.Fatalf("expected a2 when pinned excluded, got %+v", got)
	}
}

func TestCandidateSkipsDeadOrDisabled(t *testing.T) {
	dead := &Account{ID: "dead", Type: AccountTypeCodex, Dead: true, Usage: UsageSnapshot{PrimaryUsedPercent: 0.0}}
	disabled := &Account{ID: "disabled", Type: AccountTypeCodex, Disabled: true, Usage: UsageSnapshot{PrimaryUsedPercent: 0.0}}
	ok := &Account{ID: "ok", Type: AccountTypeCodex, Usage: UsageSnapshot{PrimaryUsedPercent: 0.5}}
	p := newPoolState([]*Account{dead, disabled, ok}, false)

	got := p.candidate("", nil, "", "", "")
	if got == nil || got.ID != "ok" {
		t.Fatalf("expected ok, got %+v", got)
	}
}

func TestNearestUsageResetPrefersSoonestKnownWindow(t *testing.T) {
	now := time.Now()
	primaryExhausted := &Account{ID: "p1", Type: AccountTypeCodex, Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.96, PrimaryResetAt: now.Add(10 * time.Minute),
	}}
	secondaryExhausted := &Account{ID: "s1", Type: AccountTypeCodex, Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.2, SecondaryUsedPercent: 0.995, SecondaryResetAt: now.Add(3 * time.Minute),
	}}
	underThreshold := &Account{ID: "ok", Type: AccountTypeCodex, Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.94, PrimaryResetAt: now.Add(time.Minute),
	}}
	unknownReset := &Account{ID: "u1", Type: AccountTypeCodex, Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.99,
	}}
	p := newPoolState([]*Account{primaryExhausted, secondaryExhausted, underThreshold, unknownReset}, false)

	got, known := p.nearestUsageReset(AccountTypeCodex, nil)
	if !known {
		t.Fatal("expected a known usage reset")
	}
	if got <= 0 || got > 3*time.Minute {
		t.Fatalf("nearest usage reset = %s, want the 3m secondary window", got)
	}

	got, known = p.nearestUsageReset(AccountTypeCodex, map[string]bool{"s1": true})
	if !known {
		t.Fatal("expected a known usage reset after excluding the secondary window")
	}
	if got <= 3*time.Minute || got > 10*time.Minute {
		t.Fatalf("nearest usage reset = %s, want the 10m primary window", got)
	}

	if _, known = p.nearestUsageReset(AccountTypeCodex, map[string]bool{"p1": true, "s1": true}); known {
		t.Fatal("expected no known reset when exhausted accounts lack reset timestamps")
	}

	if _, known = p.nearestUsageReset(AccountTypeGemini, nil); known {
		t.Fatal("expected no known reset for a type with no accounts")
	}
}

func TestHasRoutableAccountOfType(t *testing.T) {
	exhausted := &Account{ID: "ex", Type: AccountTypeCodex, Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.96, PrimaryResetAt: time.Now().Add(time.Hour),
	}}
	cooling := &Account{ID: "cool", Type: AccountTypeCodex, RateLimitUntil: time.Now().Add(time.Minute)}
	healthy := &Account{ID: "ok", Type: AccountTypeClaude, Usage: UsageSnapshot{PrimaryUsedPercent: 0.2}}
	p := newPoolState([]*Account{exhausted, cooling, healthy}, false)

	if p.hasRoutableAccountOfType(AccountTypeCodex) {
		t.Fatal("expected no routable codex accounts while exhausted or cooling down")
	}
	if !p.hasRoutableAccountOfType(AccountTypeClaude) {
		t.Fatal("expected the healthy claude account to be routable")
	}

	exhausted.mu.Lock()
	exhausted.Usage.PrimaryUsedPercent = 0.1
	exhausted.mu.Unlock()
	if !p.hasRoutableAccountOfType(AccountTypeCodex) {
		t.Fatal("expected the recovered codex account to be routable")
	}
}

func TestCandidateSkipsHealthBlockedAccounts(t *testing.T) {
	blocked := &Account{ID: "blocked", Type: AccountTypeCodex, NeedsVerification: true}
	healthy := &Account{ID: "healthy", Type: AccountTypeCodex, Usage: UsageSnapshot{PrimaryUsedPercent: 0.5}}
	pool := newPoolState([]*Account{blocked, healthy}, false)

	if got := pool.candidate("", nil, AccountTypeCodex, "", ""); got != healthy {
		t.Fatalf("candidate = %v, want healthy account", got)
	}
}

func TestCandidateSkipsRateLimitedCodexAccount(t *testing.T) {
	rateLimited := &Account{
		ID:             "limited",
		Type:           AccountTypeCodex,
		PlanType:       "pro",
		RateLimitUntil: time.Now().Add(time.Hour),
	}
	healthy := &Account{ID: "healthy", Type: AccountTypeCodex, PlanType: "pro"}
	pool := newPoolState([]*Account{rateLimited, healthy}, false)

	if got := pool.candidate("", nil, AccountTypeCodex, "", ""); got != healthy {
		t.Fatalf("candidate = %v, want healthy account", got)
	}
}

func TestCandidateUnpinsRateLimitedCodexAccount(t *testing.T) {
	rateLimited := &Account{
		ID:             "limited",
		Type:           AccountTypeCodex,
		PlanType:       "pro",
		RateLimitUntil: time.Now().Add(time.Hour),
	}
	healthy := &Account{ID: "healthy", Type: AccountTypeCodex, PlanType: "pro"}
	pool := newPoolState([]*Account{rateLimited, healthy}, false)
	pool.pin("conversation", rateLimited.ID)

	if got := pool.candidate("conversation", nil, AccountTypeCodex, "", ""); got != healthy {
		t.Fatalf("candidate = %v, want healthy account", got)
	}
}

func TestCandidateRequiredPlanFiltersAccounts(t *testing.T) {
	plus := &Account{ID: "plus", Type: AccountTypeCodex, PlanType: "plus", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
	pro := &Account{ID: "pro", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.2}}
	p := newPoolState([]*Account{plus, pro}, false)

	got := p.candidate("", nil, AccountTypeCodex, "pro", "")
	if got == nil || got.ID != "pro" {
		t.Fatalf("expected pro account, got %+v", got)
	}
}

func TestCodexProLiteHasProAccessAndTier(t *testing.T) {
	for _, plan := range []string{"prolite", "PROLITE", " ProLite "} {
		if !isCodexProAccessPlan(plan) {
			t.Fatalf("expected %q to have Codex Pro access", plan)
		}
		if !planMatchesRequired(plan, "pro") {
			t.Fatalf("expected %q to satisfy a Pro plan requirement", plan)
		}
		if got := accountTier(AccountTypeCodex, plan); got != 1 {
			t.Fatalf("accountTier(codex, %q) = %d, want 1", plan, got)
		}
	}
}

func TestCandidateUsesCodexProLiteAlongsidePro(t *testing.T) {
	pro := &Account{ID: "pro", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.7, SecondaryUsedPercent: 0.7}}
	proLite := &Account{ID: "prolite", Type: AccountTypeCodex, PlanType: "prolite", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1}}
	p := newPoolState([]*Account{pro, proLite}, false)

	got := p.candidate("", nil, AccountTypeCodex, "pro", "")
	if got == nil || got.ID != "prolite" {
		t.Fatalf("expected lower-usage Pro Lite account to compete with Pro, got %+v", got)
	}
}

func TestCandidateKeepsPinnedCodexProLite(t *testing.T) {
	pro := &Account{ID: "pro", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1}}
	proLite := &Account{ID: "prolite", Type: AccountTypeCodex, PlanType: "prolite", Usage: UsageSnapshot{PrimaryUsedPercent: 0.5, SecondaryUsedPercent: 0.5}}
	p := newPoolState([]*Account{pro, proLite}, false)
	p.pin("conversation", proLite.ID)

	got := p.candidate("conversation", nil, AccountTypeCodex, "pro", "")
	if got == nil || got.ID != "prolite" {
		t.Fatalf("expected pinned Pro Lite account to remain eligible, got %+v", got)
	}
}

func TestCandidatePrefersClaudeMaxOverPro(t *testing.T) {
	// Claude pro should be tier 3 (last resort), max should be tier 1
	maxAcc := &Account{ID: "max1", Type: AccountTypeClaude, PlanType: "max", Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.5, SecondaryUsedPercent: 0.7,
	}}
	proAcc := &Account{ID: "pro1", Type: AccountTypeClaude, PlanType: "pro", Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.0, SecondaryUsedPercent: 0.1,
	}}
	p := newPoolState([]*Account{proAcc, maxAcc}, false)

	got := p.candidate("", nil, AccountTypeClaude, "", "")
	if got == nil || got.ID != "max1" {
		t.Fatalf("expected max account preferred over pro even with worse usage, got %+v", got)
	}
}

func TestCandidateFallsBackToClaudeProWhenMaxExhausted(t *testing.T) {
	// If max is at hard limit, should fall back to pro
	maxAcc := &Account{ID: "max1", Type: AccountTypeClaude, PlanType: "max", Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.96, SecondaryUsedPercent: 0.5,
	}}
	proAcc := &Account{ID: "pro1", Type: AccountTypeClaude, PlanType: "pro", Usage: UsageSnapshot{
		PrimaryUsedPercent: 0.1, SecondaryUsedPercent: 0.1,
	}}
	p := newPoolState([]*Account{proAcc, maxAcc}, false)

	got := p.candidate("", nil, AccountTypeClaude, "", "")
	if got == nil || got.ID != "pro1" {
		t.Fatalf("expected pro fallback when max exhausted, got %+v", got)
	}
}

func TestCandidateRequiredPlanOverridesPinnedConversation(t *testing.T) {
	plus := &Account{ID: "plus", Type: AccountTypeCodex, PlanType: "plus", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
	pro := &Account{ID: "pro", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.2}}
	p := newPoolState([]*Account{plus, pro}, false)
	p.pin("c1", "plus")

	got := p.candidate("c1", nil, AccountTypeCodex, "pro", "")
	if got == nil || got.ID != "pro" {
		t.Fatalf("expected pinned plus to be bypassed for required plan, got %+v", got)
	}
}

func TestCandidateBypassesPinnedNonProCodex(t *testing.T) {
	plus := &Account{ID: "plus", Type: AccountTypeCodex, PlanType: "plus", Usage: UsageSnapshot{PrimaryUsedPercent: 0.01, SecondaryUsedPercent: 0.01}}
	pro := &Account{ID: "pro", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.6, SecondaryUsedPercent: 0.6}}
	p := newPoolState([]*Account{plus, pro}, false)
	p.pin("c1", "plus")

	got := p.candidate("c1", nil, AccountTypeCodex, "", "")
	if got == nil || got.ID != "pro" {
		t.Fatalf("expected codex pro to bypass pinned non-pro account, got %+v", got)
	}
}

func TestCandidatePrefersCodexProEvenWhenTierTwoScoresBetter(t *testing.T) {
	plus := &Account{ID: "plus", Type: AccountTypeCodex, PlanType: "plus", Usage: UsageSnapshot{PrimaryUsedPercent: 0.01, SecondaryUsedPercent: 0.01}}
	pro := &Account{ID: "pro", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.8, SecondaryUsedPercent: 0.8}}
	p := newPoolState([]*Account{plus, pro}, false)

	got := p.candidate("", nil, AccountTypeCodex, "", "")
	if got == nil || got.ID != "pro" {
		t.Fatalf("expected codex pro to stay preferred, got %+v", got)
	}
}

func TestCandidateWithCyberAccessOnlyReturnsMarkedAccounts(t *testing.T) {
	ordinary := &Account{ID: "ordinary", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.01, SecondaryUsedPercent: 0.01}}
	cyber := &Account{ID: "cyber", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true, Usage: UsageSnapshot{PrimaryUsedPercent: 0.2, SecondaryUsedPercent: 0.2}}
	p := newPoolState([]*Account{ordinary, cyber}, false)

	got := p.candidateWithCyberAccess(nil, AccountTypeCodex, "", "")
	if got == nil || got.ID != "cyber" {
		t.Fatalf("expected cyber access account, got %+v", got)
	}
	if got := p.candidateWithCyberAccess(map[string]bool{"cyber": true}, AccountTypeCodex, "", ""); got != nil {
		t.Fatalf("expected no cyber access account after exclusion, got %+v", got)
	}
}

func TestCandidateSoftlyPrefersCyberAccessForOrdinaryTraffic(t *testing.T) {
	ordinary := &Account{
		ID: "ordinary", Type: AccountTypeCodex, PlanType: "pro",
		Usage: UsageSnapshot{SecondaryUsedPercent: 0.10},
	}
	cyber := &Account{
		ID: "cyber", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true,
		Usage: UsageSnapshot{SecondaryUsedPercent: 0.20},
	}
	pool := newPoolState([]*Account{ordinary, cyber}, false)

	if got := pool.candidate("", nil, AccountTypeCodex, "", ""); got != cyber {
		t.Fatalf("candidate = %v, want cyber account when quota health is competitive", got)
	}
}

func TestCandidateWeightsOrdinaryTrafficWithoutStarvingNonCyberAccounts(t *testing.T) {
	accounts := []*Account{
		{ID: "cyber-a", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true, Usage: UsageSnapshot{SecondaryUsedPercent: 0.10}},
		{ID: "cyber-b", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true, Usage: UsageSnapshot{SecondaryUsedPercent: 0.10}},
		{ID: "ordinary-a", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{SecondaryUsedPercent: 0.10}},
		{ID: "ordinary-b", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{SecondaryUsedPercent: 0.10}},
	}
	pool := newPoolState(accounts, false)
	counts := make(map[string]int)
	for range 60 {
		candidate := pool.candidate("", nil, AccountTypeCodex, "", "")
		if candidate == nil {
			t.Fatal("expected a Codex candidate")
		}
		counts[candidate.ID]++
	}

	for _, id := range []string{"cyber-a", "cyber-b"} {
		if counts[id] != 20 {
			t.Fatalf("%s selections = %d, want 20", id, counts[id])
		}
	}
	for _, id := range []string{"ordinary-a", "ordinary-b"} {
		if counts[id] != 10 {
			t.Fatalf("%s selections = %d, want 10", id, counts[id])
		}
	}
}

func TestCandidateDoesNotPreferBadlyDrainedCyberAccess(t *testing.T) {
	ordinary := &Account{
		ID: "ordinary", Type: AccountTypeCodex, PlanType: "pro",
		Usage: UsageSnapshot{SecondaryUsedPercent: 0.10},
	}
	cyber := &Account{
		ID: "cyber", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true,
		Usage: UsageSnapshot{SecondaryUsedPercent: 0.50},
	}
	pool := newPoolState([]*Account{ordinary, cyber}, false)

	if got := pool.candidate("", nil, AccountTypeCodex, "", ""); got != ordinary {
		t.Fatalf("candidate = %v, want substantially healthier ordinary account", got)
	}
}

// Regression: when the only cyber-access account has an expired access
// token, candidateWithCyberAccess must still return it — the caller's
// dial path will lazy-refresh. Excluding expired accounts here causes
// cyber_policy errors to leak to the client when the only cyber account
// has been idle long enough to expire.
func TestCandidateWithCyberAccessReturnsExpiredAccount(t *testing.T) {
	expired := &Account{
		ID: "cyber-expired", Type: AccountTypeCodex, PlanType: "pro",
		CyberAccess: true,
		ExpiresAt:   time.Now().Add(-5 * time.Minute),
		Usage:       UsageSnapshot{PrimaryUsedPercent: 0.05, SecondaryUsedPercent: 0.05},
	}
	p := newPoolState([]*Account{expired}, false)
	got := p.candidateWithCyberAccess(nil, AccountTypeCodex, "", "")
	if got == nil || got.ID != "cyber-expired" {
		t.Fatalf("expected expired cyber account to still be picked, got %+v", got)
	}
}

func TestCandidateDoesNotPileTrafficOntoHighWeeklyProLiteAccount(t *testing.T) {
	now := time.Now()
	proLite := &Account{
		ID:       "prolite",
		Type:     AccountTypeCodex,
		PlanType: "prolite",
		Usage: UsageSnapshot{
			SecondaryUsedPercent:   0.82,
			SecondaryWindowMinutes: codexWeeklyWindowMinutes,
			SecondaryResetAt:       now.Add(6 * 24 * time.Hour),
		},
	}
	pro := &Account{
		ID:       "pro",
		Type:     AccountTypeCodex,
		PlanType: "pro",
		Usage: UsageSnapshot{
			SecondaryUsedPercent:   0.02,
			SecondaryWindowMinutes: codexWeeklyWindowMinutes,
			SecondaryResetAt:       now.Add(6 * 24 * time.Hour),
		},
	}
	pool := newPoolState([]*Account{proLite, pro}, false)

	got := pool.candidate("", nil, AccountTypeCodex, "", "")
	if got == nil {
		t.Fatal("expected a Codex candidate")
	}
	if got.ID != "pro" {
		t.Fatalf("candidate = %q, want lower-weekly-usage pro account", got.ID)
	}
}

func TestCandidateSkipsAccountWhenClientIPNotAllowed(t *testing.T) {
	restricted := &Account{ID: "restricted", Type: AccountTypeCodex, PlanType: "pro", AllowedSourceIPs: []string{"199.45.144.95"}, Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
	fallback := &Account{ID: "fallback", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.2}}
	p := newPoolState([]*Account{restricted, fallback}, false)

	got := p.candidate("", nil, AccountTypeCodex, "", "203.0.113.10")
	if got == nil || got.ID != "fallback" {
		t.Fatalf("expected unrestricted fallback, got %+v", got)
	}
}

func TestCandidateAllowsRestrictedAccountWhenClientIPMatches(t *testing.T) {
	restricted := &Account{ID: "restricted", Type: AccountTypeCodex, PlanType: "pro", AllowedSourceIPs: []string{"199.45.144.95"}, Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
	fallback := &Account{ID: "fallback", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.4}}
	p := newPoolState([]*Account{restricted, fallback}, false)

	got := p.candidate("", nil, AccountTypeCodex, "", "199.45.144.95")
	if got == nil || got.ID != "restricted" {
		t.Fatalf("expected restricted account for matching IP, got %+v", got)
	}
}

func TestMergeUsagePreservesExistingFields(t *testing.T) {
	prev := UsageSnapshot{
		PrimaryUsedPercent:   0.2,
		SecondaryUsedPercent: 0.3,
		PrimaryWindowMinutes: 300,
		Source:               "old",
		RetrievedAt:          time.Now(),
	}
	next := UsageSnapshot{
		PrimaryUsedPercent: 0.25,
		RetrievedAt:        time.Now().Add(1 * time.Minute),
		Source:             "body",
	}
	merged := mergeUsage(prev, next)
	if merged.SecondaryUsedPercent != 0.3 {
		t.Fatalf("expected secondary preserved when new absent, got %v", merged.SecondaryUsedPercent)
	}
	if merged.PrimaryWindowMinutes != 300 {
		t.Fatalf("expected window preserved, got %d", merged.PrimaryWindowMinutes)
	}
	if merged.PrimaryUsedPercent != 0.25 {
		t.Fatalf("expected primary updated, got %v", merged.PrimaryUsedPercent)
	}
	if merged.Source != "body" {
		t.Fatalf("expected source updated, got %s", merged.Source)
	}
}

func TestSaveAccountPreservesUnknownFields(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "auth.json")

	original := map[string]any{
		"tokens": map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"id_token":      "old-id",
			"account_id":    "acct_123",
			"extra_token": map[string]any{
				"foo": 1,
			},
		},
		"allowed_ip":   "199.45.144.95",
		"cyber_access": true,
		"last_refresh": "2025-12-01T00:00:00Z",
		"extra_top":    []any{1, 2, 3},
		"meta": map[string]any{
			"x": "y",
		},
	}
	buf, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	acc := &Account{
		ID:               "a1",
		File:             path,
		AccessToken:      "new-access",
		RefreshToken:     "new-refresh",
		IDToken:          "new-id",
		AccountID:        "acct_123",
		AllowedSourceIPs: []string{"199.45.144.95"},
		CyberAccess:      true,
		LastRefresh:      time.Date(2025, 12, 17, 0, 0, 0, 0, time.UTC),
	}
	if err := saveAccount(acc); err != nil {
		t.Fatalf("saveAccount: %v", err)
	}

	afterRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(afterRaw, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}

	// Top-level unknown fields preserved.
	if _, ok := after["extra_top"]; !ok {
		t.Fatalf("expected extra_top preserved")
	}
	if _, ok := after["meta"]; !ok {
		t.Fatalf("expected meta preserved")
	}
	if after["cyber_access"] != true {
		t.Fatalf("expected cyber_access preserved")
	}

	// Token fields updated, unknown token fields preserved.
	tokens, ok := after["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("expected tokens object")
	}
	if tokens["access_token"] != "new-access" {
		t.Fatalf("access_token=%v", tokens["access_token"])
	}
	if tokens["refresh_token"] != "new-refresh" {
		t.Fatalf("refresh_token=%v", tokens["refresh_token"])
	}
	if tokens["id_token"] != "new-id" {
		t.Fatalf("id_token=%v", tokens["id_token"])
	}
	if tokens["account_id"] != "acct_123" {
		t.Fatalf("account_id=%v", tokens["account_id"])
	}
	if _, ok := tokens["extra_token"]; !ok {
		t.Fatalf("expected tokens.extra_token preserved")
	}
}

func TestPoolConversationPinEvictionAndUnpinCleanup(t *testing.T) {
	a1 := &Account{ID: "a1", Type: AccountTypeCodex, PlanType: "pro"}
	a2 := &Account{ID: "a2", Type: AccountTypeCodex, PlanType: "pro"}
	p := newPoolState([]*Account{a1, a2}, false)

	// 1. Pin to a1, then mark a1 rate-limited -> candidateForUser must unpin and remove from convPin/convOwner.
	p.pinForUser("user1", "conv-rate-limited", "a1")
	a1.RateLimitUntil = time.Now().Add(time.Minute)
	got := p.candidateForUser("user1", "conv-rate-limited", nil, AccountTypeCodex, "", "")
	if got == nil || got.ID != "a2" {
		t.Fatalf("expected failover to a2, got %+v", got)
	}
	if _, exists := p.convPin["conv-rate-limited"]; exists {
		t.Fatal("stale pin for rate-limited account was not removed from convPin")
	}
	if _, exists := p.convOwner["conv-rate-limited"]; exists {
		t.Fatal("stale owner for rate-limited account was not removed from convOwner")
	}
	a1.RateLimitUntil = time.Time{}

	// 2. replace() preserves pins for surviving accounts and removes pins for dropped accounts.
	p.pinForUser("user1", "conv-survives", "a1")
	p.pinForUser("user1", "conv-dropped", "a2")
	p.replace([]*Account{a1})
	if p.convPin["conv-survives"] != "a1" {
		t.Fatalf("expected surviving account pin to be preserved across replace, got %q", p.convPin["conv-survives"])
	}
	if _, exists := p.convPin["conv-dropped"]; exists {
		t.Fatal("expected dropped account pin to be removed on replace")
	}

	// 3. Capacity bound: inserting more than maxConversationPins evicts oldest entries.
	base := time.Now().Add(-time.Hour)
	p.mu.Lock()
	for i := 0; i < maxConversationPins+500; i++ {
		p.pinForUserLocked("u", "conv-"+strconv.Itoa(i), "a1", base.Add(time.Duration(i)*time.Millisecond))
	}
	pinCount := len(p.convPin)
	ownerCount := len(p.convOwner)
	tsCount := len(p.convUpdatedAt)
	p.mu.Unlock()
	if pinCount > maxConversationPins || ownerCount > maxConversationPins || tsCount > maxConversationPins {
		t.Fatalf("expected conversation pin maps bounded to <= %d, got pins=%d owners=%d ts=%d",
			maxConversationPins, pinCount, ownerCount, tsCount)
	}
}
