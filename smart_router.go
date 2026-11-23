package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/requestjson"
	"codex-pool-proxy/internal/routing"
)

type RoutingProfile string

const (
	RoutingBalanced   RoutingProfile = "balanced"
	RoutingFast       RoutingProfile = "fast"
	RoutingThroughput RoutingProfile = "throughput"
	RoutingQuotaSaver RoutingProfile = "quota-saver"
	RoutingDrain      RoutingProfile = "drain"
	RoutingSticky     RoutingProfile = "sticky"
	RoutingLegacy     RoutingProfile = "legacy"
)

func defaultRoutingProfiles() map[RoutingProfile]routing.Weights {
	return map[RoutingProfile]routing.Weights{
		RoutingBalanced: {
			QuotaHeadroom: 0.25, ResetUrgency: 0.15, Health: 0.20,
			TTFT: 0.10, Throughput: 0.10, Affinity: 0.10,
			Inflight: 0.05, RecentFailure: 0.05,
		},
		RoutingFast: {
			TTFT: 0.35, Throughput: 0.25, Health: 0.15,
			Inflight: 0.10, QuotaHeadroom: 0.10, Affinity: 0.05,
		},
		RoutingThroughput: {
			Throughput: 0.40, TTFT: 0.15, Health: 0.15,
			Inflight: 0.10, QuotaHeadroom: 0.10, Affinity: 0.10,
		},
		RoutingQuotaSaver: {
			QuotaHeadroom: 0.35, ProjectedQuota: 0.25, ResetTiming: 0.15,
			Health: 0.10, Affinity: 0.10, Performance: 0.05,
		},
		RoutingDrain: {
			ResetProximity: 0.35, RemainingQuota: 0.30, Health: 0.15,
			Throughput: 0.10, Inflight: 0.10,
		},
		RoutingSticky: {
			Affinity: 0.55, Health: 0.15, QuotaHeadroom: 0.10,
			TTFT: 0.05, Throughput: 0.05, Inflight: 0.05, RecentFailure: 0.05,
		},
	}
}

func mergeRoutingWeights(base, override routing.Weights) routing.Weights {
	if math.Abs(override.Total()-1) <= 0.000001 {
		return override
	}
	if override.QuotaHeadroom > 0 {
		base.QuotaHeadroom = override.QuotaHeadroom
	}
	if override.ProjectedQuota > 0 {
		base.ProjectedQuota = override.ProjectedQuota
	}
	if override.RemainingQuota > 0 {
		base.RemainingQuota = override.RemainingQuota
	}
	if override.ResetUrgency > 0 {
		base.ResetUrgency = override.ResetUrgency
	}
	if override.ResetTiming > 0 {
		base.ResetTiming = override.ResetTiming
	}
	if override.ResetProximity > 0 {
		base.ResetProximity = override.ResetProximity
	}
	if override.Health > 0 {
		base.Health = override.Health
	}
	if override.TTFT > 0 {
		base.TTFT = override.TTFT
	}
	if override.Throughput > 0 {
		base.Throughput = override.Throughput
	}
	if override.Performance > 0 {
		base.Performance = override.Performance
	}
	if override.Affinity > 0 {
		base.Affinity = override.Affinity
	}
	if override.Inflight > 0 {
		base.Inflight = override.Inflight
	}
	if override.RecentFailure > 0 {
		base.RecentFailure = override.RecentFailure
	}
	return base
}

type routingPolicySet struct {
	DefaultProfile RoutingProfile
	DefaultModel   string
	Profiles       map[RoutingProfile]routing.Weights
}

func newRoutingPolicySet(cfg RoutingConfigFile) routingPolicySet {
	profiles := defaultRoutingProfiles()
	for rawName, override := range cfg.Profiles {
		name := RoutingProfile(strings.ToLower(strings.TrimSpace(rawName)))
		if _, ok := profiles[name]; !ok || override.Total() <= 0 {
			continue
		}
		profiles[name] = mergeRoutingWeights(profiles[name], override).Normalized()
	}
	for name, weights := range profiles {
		profiles[name] = weights.Normalized()
	}
	defaultProfile := RoutingProfile(strings.ToLower(strings.TrimSpace(cfg.DefaultProfile)))
	if defaultProfile != RoutingLegacy {
		if _, ok := profiles[defaultProfile]; !ok {
			defaultProfile = RoutingBalanced
		}
	}
	if defaultProfile == "" {
		defaultProfile = RoutingBalanced
	}
	defaultModel := strings.TrimSpace(cfg.DefaultModel)
	if defaultModel == "" {
		defaultModel = "gpt-5.5"
	}
	return routingPolicySet{DefaultProfile: defaultProfile, DefaultModel: defaultModel, Profiles: profiles}
}

func (s routingPolicySet) weights(profile RoutingProfile) (routing.Weights, bool) {
	if profile == RoutingLegacy {
		return routing.Weights{}, true
	}
	weights, ok := s.Profiles[profile]
	return weights, ok
}

type routingTelemetry struct {
	Samples          int64
	SuccessEWMA      float64
	TTFTMsEWMA       float64
	ThroughputEWMA   float64
	ConsecutiveFails int
	LastFailure      time.Time
	LastUpdated      time.Time
}

type routingScore struct {
	Profile   RoutingProfile
	Score     float64
	Signals   routing.Signals
	Weights   routing.Weights
	Telemetry routingTelemetry
}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func routingResetProximity(usage UsageSnapshot, now time.Time) float64 {
	resetAt := usage.SecondaryResetAt
	windowMinutes := usage.SecondaryWindowMinutes
	if resetAt.IsZero() || windowMinutes <= 0 {
		resetAt = usage.PrimaryResetAt
		windowMinutes = usage.PrimaryWindowMinutes
	}
	if resetAt.IsZero() || windowMinutes <= 0 {
		return 0
	}
	remaining := resetAt.Sub(now).Minutes()
	if remaining <= 0 {
		return 1
	}
	return clampUnit(1 - remaining/float64(windowMinutes))
}

func routingProjectedQuotaScore(usage UsageSnapshot, now time.Time, headroom float64) float64 {
	used := usageSecondaryUsed(usage)
	resetAt := usage.SecondaryResetAt
	windowMinutes := usage.SecondaryWindowMinutes
	if !usageSecondaryWindowAvailable(usage) {
		used = usagePrimaryUsed(usage)
		resetAt = usage.PrimaryResetAt
		windowMinutes = usage.PrimaryWindowMinutes
	}
	if resetAt.IsZero() || windowMinutes <= 0 {
		return headroom
	}
	elapsed := float64(windowMinutes) - resetAt.Sub(now).Minutes()
	if elapsed < float64(windowMinutes)/100 || used <= 0 {
		return headroom
	}
	projected := used * float64(windowMinutes) / elapsed
	if projected <= 1 {
		return 1
	}
	return clampUnit(1 / projected)
}

func routingSignalsLocked(account *Account, telemetry routingTelemetry, pinned bool, now time.Time) routing.Signals {
	headroom := 1.0
	if usageSecondaryWindowAvailable(account.Usage) {
		headroom = 1 - usageSecondaryUsed(account.Usage)
	} else if usagePrimaryWindowAvailable(account.Usage) {
		headroom = 1 - usagePrimaryUsed(account.Usage)
	}
	headroom = clampUnit(headroom)
	resetProximity := routingResetProximity(account.Usage, now)
	health := 1.0
	if accountHealthBlockedLocked(account) {
		health = 0
	} else if telemetry.Samples > 0 {
		health = clampUnit(telemetry.SuccessEWMA)
	}
	if account.Penalty > 0 {
		health *= clampUnit(1 - account.Penalty/10)
	}
	ttft := 0.5
	if telemetry.TTFTMsEWMA > 0 {
		ttft = 1 / (1 + telemetry.TTFTMsEWMA/500)
	}
	throughput := 0.5
	if telemetry.ThroughputEWMA > 0 {
		throughput = telemetry.ThroughputEWMA / (telemetry.ThroughputEWMA + 50)
	}
	recentFailure := 1.0
	if telemetry.ConsecutiveFails > 0 {
		recentFailure = math.Exp(-float64(telemetry.ConsecutiveFails))
		if !telemetry.LastFailure.IsZero() {
			recovery := now.Sub(telemetry.LastFailure).Minutes() / 15
			recentFailure = clampUnit(recentFailure + recovery)
		}
	}
	inflight := 1 / (1 + float64(atomic.LoadInt64(&account.Inflight)))
	affinity := 0.0
	if pinned {
		affinity = 1
	}
	return routing.Signals{
		QuotaHeadroom:  headroom,
		ProjectedQuota: routingProjectedQuotaScore(account.Usage, now, headroom),
		RemainingQuota: headroom,
		ResetUrgency:   clampUnit(resetProximity * headroom),
		ResetTiming:    resetProximity,
		ResetProximity: resetProximity,
		Health:         health,
		TTFT:           clampUnit(ttft),
		Throughput:     clampUnit(throughput),
		Performance:    clampUnit((ttft + throughput) / 2),
		Affinity:       affinity,
		Inflight:       clampUnit(inflight),
		RecentFailure:  recentFailure,
	}
}

func (p *poolState) configureRouting(cfg RoutingConfigFile) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.routing = newRoutingPolicySet(cfg)
	if p.routingTelemetry == nil {
		p.routingTelemetry = make(map[string]routingTelemetry)
	}
	p.mu.Unlock()
}

func (p *poolState) defaultRoutingProfile() RoutingProfile {
	if p == nil {
		return RoutingBalanced
	}
	p.mu.RLock()
	profile := p.routing.DefaultProfile
	p.mu.RUnlock()
	if profile == "" {
		return RoutingBalanced
	}
	return profile
}

func (p *poolState) resolveRoutingProfile(raw string) (RoutingProfile, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return p.defaultRoutingProfile(), true
	}
	profile := RoutingProfile(raw)
	if profile == RoutingLegacy {
		return profile, true
	}
	p.mu.RLock()
	_, ok := p.routing.Profiles[profile]
	p.mu.RUnlock()
	return profile, ok
}

func (p *poolState) routingDefaultModel() string {
	p.mu.RLock()
	model := p.routing.DefaultModel
	p.mu.RUnlock()
	if model == "" {
		return "gpt-5.5"
	}
	return model
}

func (p *poolState) recordRoutingOutcome(accountID string, duration time.Duration, tokensPerSecond float64, status int, now time.Time) {
	if p == nil || accountID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routingTelemetry == nil {
		p.routingTelemetry = make(map[string]routingTelemetry)
	}
	row := p.routingTelemetry[accountID]
	const alpha = 0.25
	success := 0.0
	if status >= 200 && status < 400 {
		success = 1
	}
	if row.Samples == 0 {
		row.SuccessEWMA = success
		if duration > 0 {
			row.TTFTMsEWMA = float64(duration.Milliseconds())
		}
		if tokensPerSecond > 0 {
			row.ThroughputEWMA = tokensPerSecond
		}
	} else {
		row.SuccessEWMA = alpha*success + (1-alpha)*row.SuccessEWMA
		if duration > 0 {
			row.TTFTMsEWMA = alpha*float64(duration.Milliseconds()) + (1-alpha)*row.TTFTMsEWMA
		}
		if tokensPerSecond > 0 {
			row.ThroughputEWMA = alpha*tokensPerSecond + (1-alpha)*row.ThroughputEWMA
		}
	}
	if success == 1 {
		row.ConsecutiveFails = 0
	} else {
		row.ConsecutiveFails++
		row.LastFailure = now
	}
	row.Samples++
	row.LastUpdated = now
	p.routingTelemetry[accountID] = row
}

func (p *poolState) selectSmartRoute(req routing.Request) routeSelection {
	exclude := p.accountExclusions(req.UserID, req.Exclude, req.Model)
	exclude = p.controlExclusions(req.UserID, req.ConversationID, exclude)
	p.mu.Lock()
	defer p.mu.Unlock()

	profile := RoutingProfile(req.Profile)
	weights, ok := p.routing.weights(profile)
	if !ok {
		return routeSelection{Policy: req.Profile, Reasons: []string{"invalid_routing_profile"}}
	}
	now := time.Now()
	p.evictOldestPinsLocked(now)
	pinnedID := ""
	if owner := p.convOwner[req.ConversationID]; owner == "" || req.UserID == "" || owner == req.UserID {
		pinnedID = p.convPin[req.ConversationID]
	}
	model := req.Model
	accountType := AccountType(req.Provider)
	if accountType == AccountTypeAntigravity {
		if canonical := antigravityCanonicalModel(p.antigravity, model); canonical != "" {
			model = canonical
		}
	}
	requiresDiscovery := p.discoveredModelRequiresEntitlementLocked(accountType, model)
	type candidate struct {
		account   *Account
		score     routingScore
		preferPin bool
	}
	candidates := make([]candidate, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account == nil || exclude[account.ID] {
			continue
		}
		account.mu.Lock()
		eligible := accountMatchesRouteLocked(account, req) && !account.RateLimitUntil.After(now)
		// Antigravity exhaustion is decided for each model family.
		if eligible && account.Type != AccountTypeAntigravity {
			eligible = quotaAllowsRouteLocked(account)
		}
		if eligible && account.Type == AccountTypeAntigravity {
			discoveryAvailable, _ := p.antigravity.models.DiscoveryAvailability(account.ID, model, now)
			eligible = p.antigravity.models.Supports(account.ID, model) &&
				!antigravityModelCooldownLocked(account, model).After(now) && discoveryAvailable
		} else if eligible && requiresDiscovery {
			_, eligible = accountDiscoveredModel(account, model)
		}
		if eligible {
			eligible, _, _ = p.circuitBreakers.CanAllowTarget(string(account.Type), account.ID, model, nil)
		}
		if eligible {
			telemetry := p.routingTelemetry[account.ID]
			pinned := pinnedID == account.ID
			signals := routingSignalsLocked(account, telemetry, pinned, now)
			score := routingScore{Profile: profile, Score: routing.WeightedScore(weights, signals),
				Signals: signals, Weights: weights, Telemetry: telemetry}
			candidates = append(candidates, candidate{
				account: account, score: score, preferPin: pinned && account.codex.CyberAccess,
			})
		}
		account.mu.Unlock()
	}
	if len(candidates) == 0 {
		return routeSelection{Policy: req.Profile}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score.Score == candidates[j].score.Score {
			return candidates[i].account.ID < candidates[j].account.ID
		}
		return candidates[i].score.Score > candidates[j].score.Score
	})
	selected := candidates[0]
	for _, item := range candidates {
		if item.preferPin {
			selected = item
			break
		}
	}
	if allowed, scope, _ := p.circuitBreakers.AllowTarget(string(selected.account.Type), selected.account.ID, model, nil); !allowed {
		return routeSelection{Policy: req.Profile, Reasons: []string{"circuit_unavailable:" + scope}}
	}
	if req.ConversationID != "" {
		if err := p.pinForUserLocked(req.UserID, req.ConversationID, selected.account.ID, now); err != nil {
			return routeSelection{Policy: req.Profile, Reasons: []string{err.Error()}}
		}
	}
	decision := routeSelection{Account: selected.account, Policy: req.Profile, Score: selected.score.Score}
	if req.Trace {
		decision.Reasons = routing.Reasons(req.Profile, weights, selected.score.Signals)
		decision.Breakdown = newRoutingBreakdownView(selected.score)
		decision.Alternatives = make([]RouteAlternative, 0, len(candidates)-1)
		for _, item := range candidates {
			if item.account == selected.account {
				continue
			}
			decision.Alternatives = append(decision.Alternatives, RouteAlternative{
				Provider: string(item.account.Type), Model: model, Score: item.score.Score,
				Reasons: routing.Reasons(req.Profile, weights, item.score.Signals), AccountID: item.account.ID,
				ScoreBreakdown: newRoutingBreakdownView(item.score),
			})
		}
	}
	return decision
}

func (p *poolState) discoveredModelRequiresEntitlementLocked(accountType AccountType, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if entry, ok := modelForProvider(accountType, model); ok {
		return entry.RequiresDiscovery
	}
	for _, account := range p.accounts {
		if account.Type != accountType {
			continue
		}
		account.mu.Lock()
		_, ok := accountDiscoveredModel(account, model)
		account.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

func parseRoutingModelAlias(model string) (RoutingProfile, string, bool) {
	model = strings.TrimSpace(model)
	if !strings.HasPrefix(strings.ToLower(model), "pool/") {
		return "", model, false
	}
	if isPoolAutoModel(model) {
		return "", model, false
	}
	value := strings.TrimSpace(model[len("pool/"):])
	profilePart, target := value, ""
	if index := strings.IndexAny(value, "/:"); index >= 0 {
		profilePart, target = value[:index], strings.TrimSpace(value[index+1:])
	}
	return RoutingProfile(strings.ToLower(strings.TrimSpace(profilePart))), target, true
}

func resolveRequestRouting(pool *poolState, r *http.Request, model string, body []byte) (RoutingProfile, string, []byte, error) {
	if pool == nil {
		return RoutingBalanced, model, body, nil
	}
	rawProfile := strings.TrimSpace(r.Header.Get("X-Pool-Routing"))
	aliasProfile, aliasModel, hasAlias := parseRoutingModelAlias(model)
	if rawProfile == "" && hasAlias {
		rawProfile = string(aliasProfile)
	}
	profile, ok := pool.resolveRoutingProfile(rawProfile)
	if !ok {
		return "", model, body, fmt.Errorf("unknown routing profile %q", rawProfile)
	}
	if hasAlias {
		if aliasModel == "" {
			aliasModel = pool.routingDefaultModel()
		}
		model = aliasModel
		if rewritten := requestjson.RewriteModel(body, model); rewritten != nil {
			body = rewritten
		}
	}
	return profile, model, body, nil
}

func validateRoutingConfig(cfg RoutingConfigFile) error {
	defaultProfile := RoutingProfile(strings.ToLower(strings.TrimSpace(cfg.DefaultProfile)))
	if defaultProfile != "" && defaultProfile != RoutingLegacy {
		if _, ok := defaultRoutingProfiles()[defaultProfile]; !ok {
			return fmt.Errorf("unknown routing default profile %q", cfg.DefaultProfile)
		}
	}
	for rawName, weights := range cfg.Profiles {
		name := RoutingProfile(strings.ToLower(strings.TrimSpace(rawName)))
		if _, ok := defaultRoutingProfiles()[name]; !ok {
			return fmt.Errorf("unknown routing profile %q", rawName)
		}
		values := []float64{
			weights.QuotaHeadroom, weights.ProjectedQuota, weights.RemainingQuota,
			weights.ResetUrgency, weights.ResetTiming, weights.ResetProximity,
			weights.Health, weights.TTFT, weights.Throughput, weights.Performance,
			weights.Affinity, weights.Inflight, weights.RecentFailure,
		}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return fmt.Errorf("routing profile %s contains an invalid weight", name)
			}
		}
		if weights.Total() <= 0 {
			return fmt.Errorf("routing profile %s must contain at least one positive weight", name)
		}
	}
	set := newRoutingPolicySet(cfg)
	if _, ok := set.weights(set.DefaultProfile); !ok {
		return errors.New("routing default profile is invalid")
	}
	for name, weights := range set.Profiles {
		if math.Abs(weights.Total()-1) > 0.000001 {
			return fmt.Errorf("routing profile %s weights do not normalize to 1", name)
		}
	}
	return nil
}
