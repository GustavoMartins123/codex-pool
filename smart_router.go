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

type RoutingProfileWeights struct {
	QuotaHeadroom  float64 `toml:"quota_headroom" json:"quota_headroom"`
	ProjectedQuota float64 `toml:"projected_quota" json:"projected_quota"`
	RemainingQuota float64 `toml:"remaining_quota" json:"remaining_quota"`
	ResetUrgency   float64 `toml:"reset_urgency" json:"reset_urgency"`
	ResetTiming    float64 `toml:"reset_timing" json:"reset_timing"`
	ResetProximity float64 `toml:"reset_proximity" json:"reset_proximity"`
	Health         float64 `toml:"health" json:"health"`
	TTFT           float64 `toml:"ttft" json:"ttft"`
	Throughput     float64 `toml:"throughput" json:"throughput"`
	Performance    float64 `toml:"performance" json:"performance"`
	Affinity       float64 `toml:"affinity" json:"affinity"`
	Inflight       float64 `toml:"inflight" json:"inflight"`
	RecentFailure  float64 `toml:"recent_failure" json:"recent_failure"`
}

func (w RoutingProfileWeights) total() float64 {
	return w.QuotaHeadroom + w.ProjectedQuota + w.RemainingQuota +
		w.ResetUrgency + w.ResetTiming + w.ResetProximity +
		w.Health + w.TTFT + w.Throughput + w.Performance +
		w.Affinity + w.Inflight + w.RecentFailure
}

func (w RoutingProfileWeights) normalized() RoutingProfileWeights {
	total := w.total()
	if total <= 0 {
		return w
	}
	scale := func(value float64) float64 {
		if value < 0 {
			return 0
		}
		return value / total
	}
	w.QuotaHeadroom = scale(w.QuotaHeadroom)
	w.ProjectedQuota = scale(w.ProjectedQuota)
	w.RemainingQuota = scale(w.RemainingQuota)
	w.ResetUrgency = scale(w.ResetUrgency)
	w.ResetTiming = scale(w.ResetTiming)
	w.ResetProximity = scale(w.ResetProximity)
	w.Health = scale(w.Health)
	w.TTFT = scale(w.TTFT)
	w.Throughput = scale(w.Throughput)
	w.Performance = scale(w.Performance)
	w.Affinity = scale(w.Affinity)
	w.Inflight = scale(w.Inflight)
	w.RecentFailure = scale(w.RecentFailure)
	return w
}

func defaultRoutingProfiles() map[RoutingProfile]RoutingProfileWeights {
	return map[RoutingProfile]RoutingProfileWeights{
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

func mergeRoutingWeights(base, override RoutingProfileWeights) RoutingProfileWeights {
	if math.Abs(override.total()-1) <= 0.000001 {
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
	Profiles       map[RoutingProfile]RoutingProfileWeights
}

func newRoutingPolicySet(cfg RoutingConfigFile) routingPolicySet {
	profiles := defaultRoutingProfiles()
	for rawName, override := range cfg.Profiles {
		name := RoutingProfile(strings.ToLower(strings.TrimSpace(rawName)))
		if _, ok := profiles[name]; !ok || override.total() <= 0 {
			continue
		}
		profiles[name] = mergeRoutingWeights(profiles[name], override).normalized()
	}
	for name, weights := range profiles {
		profiles[name] = weights.normalized()
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

func (s routingPolicySet) weights(profile RoutingProfile) (RoutingProfileWeights, bool) {
	if profile == RoutingLegacy {
		return RoutingProfileWeights{}, true
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

type routingSignals struct {
	QuotaHeadroom  float64
	ProjectedQuota float64
	RemainingQuota float64
	ResetUrgency   float64
	ResetTiming    float64
	ResetProximity float64
	Health         float64
	TTFT           float64
	Throughput     float64
	Performance    float64
	Affinity       float64
	Inflight       float64
	RecentFailure  float64
}

type routingScore struct {
	Profile   RoutingProfile
	Score     float64
	Signals   routingSignals
	Weights   RoutingProfileWeights
	Telemetry routingTelemetry
}

type smartRouteDecision struct {
	Account      *Account
	Profile      RoutingProfile
	Score        routingScore
	Reasons      []string
	Alternatives []RouteAlternative
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

func routingSignalsLocked(account *Account, telemetry routingTelemetry, pinned bool, now time.Time) routingSignals {
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
	return routingSignals{
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

func weightedRoutingScore(profile RoutingProfile, weights RoutingProfileWeights, signals routingSignals, telemetry routingTelemetry) routingScore {
	score := signals.QuotaHeadroom*weights.QuotaHeadroom +
		signals.ProjectedQuota*weights.ProjectedQuota +
		signals.RemainingQuota*weights.RemainingQuota +
		signals.ResetUrgency*weights.ResetUrgency +
		signals.ResetTiming*weights.ResetTiming +
		signals.ResetProximity*weights.ResetProximity +
		signals.Health*weights.Health +
		signals.TTFT*weights.TTFT +
		signals.Throughput*weights.Throughput +
		signals.Performance*weights.Performance +
		signals.Affinity*weights.Affinity +
		signals.Inflight*weights.Inflight +
		signals.RecentFailure*weights.RecentFailure
	return routingScore{Profile: profile, Score: clampUnit(score), Signals: signals, Weights: weights, Telemetry: telemetry}
}

func routingReasons(score routingScore) []string {
	type reason struct {
		name  string
		value float64
	}
	values := []reason{
		{"quota_headroom", score.Signals.QuotaHeadroom * score.Weights.QuotaHeadroom},
		{"projected_quota", score.Signals.ProjectedQuota * score.Weights.ProjectedQuota},
		{"reset_urgency", score.Signals.ResetUrgency*score.Weights.ResetUrgency + score.Signals.ResetProximity*score.Weights.ResetProximity},
		{"health", score.Signals.Health * score.Weights.Health},
		{"ttft", score.Signals.TTFT * score.Weights.TTFT},
		{"throughput", score.Signals.Throughput * score.Weights.Throughput},
		{"conversation_affinity", score.Signals.Affinity * score.Weights.Affinity},
		{"inflight", score.Signals.Inflight * score.Weights.Inflight},
		{"recent_failure", score.Signals.RecentFailure * score.Weights.RecentFailure},
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].value == values[j].value {
			return values[i].name < values[j].name
		}
		return values[i].value > values[j].value
	})
	out := []string{"profile:" + string(score.Profile)}
	for _, item := range values {
		if item.value > 0 {
			out = append(out, item.name)
		}
		if len(out) == 4 {
			break
		}
	}
	return out
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

func (p *poolState) smartCandidateForModel(conversationID string, exclude map[string]bool, accountType AccountType, requiredPlan, clientIP, model string, profile RoutingProfile) smartRouteDecision {
	return p.smartCandidateForModelForUser("", conversationID, exclude, accountType, requiredPlan, clientIP, model, profile)
}

func (p *poolState) smartCandidateForModelForUser(userID, conversationID string, exclude map[string]bool, accountType AccountType, requiredPlan, clientIP, model string, profile RoutingProfile) smartRouteDecision {
	if p == nil {
		return smartRouteDecision{Profile: profile}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	weights, ok := p.routing.weights(profile)
	if !ok {
		profile = p.routing.DefaultProfile
		weights, _ = p.routing.weights(profile)
	}
	now := time.Now()
	pinnedID := ""
	if conversationID != "" {
		owner := p.convOwner[conversationID]
		if owner == "" || userID == "" || owner == userID {
			pinnedID = p.convPin[conversationID]
		}
	}
	canonicalAntigravityModel := ""
	if accountType == AccountTypeAntigravity {
		canonicalAntigravityModel = antigravityCanonicalModel(model)
		if canonicalAntigravityModel != "" {
			model = canonicalAntigravityModel
		}
	}
	requiresDiscovery := p.discoveredModelRequiresEntitlementLocked(accountType, model)
	type candidate struct {
		account *Account
		score   routingScore
		reasons []string
	}
	candidates := make([]candidate, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account == nil || exclude != nil && exclude[account.ID] {
			continue
		}
		account.mu.Lock()
		eligible := !account.Dead && !account.Disabled && !accountHealthBlockedLocked(account) &&
			(accountType == "" || account.Type == accountType) &&
			planMatchesRequired(account.PlanType, requiredPlan) &&
			accountAllowsClientIPLocked(account, clientIP) &&
			(account.RateLimitUntil.IsZero() || !account.RateLimitUntil.After(now)) &&
			accountPrimaryUsageLocked(account) < primaryHardExcludeThreshold &&
			accountSecondaryUsageLocked(account) < secondaryHardExcludeThreshold
		if eligible && account.Type == AccountTypeAntigravity {
			modelCooldown := account.ModelRateLimits[model]
			discoveryAvailable, _ := antigravityModels.DiscoveryAvailability(account.ID, model, now)
			eligible = antigravityModels.Supports(account.ID, model) &&
				!modelCooldown.After(now) && discoveryAvailable
		} else if eligible && requiresDiscovery {
			_, eligible = accountDiscoveredModel(account, model)
		}
		if eligible && p.circuitBreakers != nil {
			if allowed, _, _ := p.circuitBreakers.CanAllowTarget(string(account.Type), account.ID, model, nil); !allowed {
				eligible = false
			}
		}
		if eligible {
			telemetry := p.routingTelemetry[account.ID]
			signals := routingSignalsLocked(account, telemetry, pinnedID == account.ID, now)
			score := weightedRoutingScore(profile, weights, signals, telemetry)
			candidates = append(candidates, candidate{account: account, score: score, reasons: routingReasons(score)})
		}
		account.mu.Unlock()
	}
	if len(candidates) == 0 {
		return smartRouteDecision{Profile: profile}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score.Score == candidates[j].score.Score {
			return candidates[i].account.ID < candidates[j].account.ID
		}
		return candidates[i].score.Score > candidates[j].score.Score
	})
	selected := candidates[0]
	if pinnedID != "" {
		for _, item := range candidates {
			if item.account.ID == pinnedID && item.account.CyberAccess {
				selected = item
				break
			}
		}
	}
	if p.circuitBreakers != nil {
		_, _, _ = p.circuitBreakers.AllowTarget(string(selected.account.Type), selected.account.ID, model, nil)
	}
	if conversationID != "" {
		p.convPin[conversationID] = selected.account.ID
	}
	alternatives := make([]RouteAlternative, 0, len(candidates)-1)
	for _, item := range candidates[1:] {
		alternatives = append(alternatives, RouteAlternative{
			Provider: string(item.account.Type), Model: model, Score: item.score.Score,
			Reasons: item.reasons, AccountID: item.account.ID,
			ScoreBreakdown: newRoutingBreakdownView(item.score),
		})
	}
	return smartRouteDecision{
		Account: selected.account, Profile: profile, Score: selected.score,
		Reasons: selected.reasons, Alternatives: alternatives,
	}
}

func (p *poolState) candidateWithRoutingTrace(conversationID string, exclude map[string]bool, accountType AccountType, requiredPlan, clientIP, model string, profile RoutingProfile) (*Account, string, []string, float64, []RouteAlternative, *ScoreBreakdownView) {
	return p.candidateWithRoutingTraceForUser("", conversationID, exclude, accountType, requiredPlan, clientIP, model, profile)
}

func (p *poolState) candidateWithRoutingTraceForUser(userID, conversationID string, exclude map[string]bool, accountType AccountType, requiredPlan, clientIP, model string, profile RoutingProfile) (*Account, string, []string, float64, []RouteAlternative, *ScoreBreakdownView) {
	if profile == RoutingLegacy {
		return p.candidateWithTrace(conversationID, exclude, accountType, requiredPlan, clientIP, model)
	}
	decision := p.smartCandidateForModelForUser(userID, conversationID, exclude, accountType, requiredPlan, clientIP, model, profile)
	if decision.Account == nil {
		return nil, string(decision.Profile), nil, 0, nil, nil
	}
	return decision.Account, string(decision.Profile), decision.Reasons, decision.Score.Score, decision.Alternatives, newRoutingBreakdownView(decision.Score)
}

func (p *poolState) candidateForAntigravityModelWithRoutingTrace(conversationID string, exclude map[string]bool, model, clientIP string, profile RoutingProfile) (*Account, string, []string, float64, []RouteAlternative, *ScoreBreakdownView) {
	return p.candidateForAntigravityModelWithRoutingTraceForUser("", conversationID, exclude, model, clientIP, profile)
}

func (p *poolState) candidateForAntigravityModelWithRoutingTraceForUser(userID, conversationID string, exclude map[string]bool, model, clientIP string, profile RoutingProfile) (*Account, string, []string, float64, []RouteAlternative, *ScoreBreakdownView) {
	if profile == RoutingLegacy {
		return p.candidateForAntigravityModelWithTrace(conversationID, exclude, model, clientIP)
	}
	decision := p.smartCandidateForModelForUser(userID, conversationID, exclude, AccountTypeAntigravity, "", clientIP, model, profile)
	if decision.Account == nil {
		return nil, string(decision.Profile), nil, 0, nil, nil
	}
	if conversationID != "" {
		canonical := antigravityCanonicalModel(model)
		p.mu.Lock()
		p.convPin["antigravity:"+canonical+":"+conversationID] = decision.Account.ID
		if userID != "" {
			if p.convOwner == nil {
				p.convOwner = make(map[string]string)
			}
			p.convOwner["antigravity:"+canonical+":"+conversationID] = userID
		}
		p.mu.Unlock()
	}
	return decision.Account, string(decision.Profile), decision.Reasons, decision.Score.Score, decision.Alternatives, newRoutingBreakdownView(decision.Score)
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
		if rewritten := rewriteModelInBody(body, model); rewritten != nil {
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
		if weights.total() <= 0 {
			return fmt.Errorf("routing profile %s must contain at least one positive weight", name)
		}
	}
	set := newRoutingPolicySet(cfg)
	if _, ok := set.weights(set.DefaultProfile); !ok {
		return errors.New("routing default profile is invalid")
	}
	for name, weights := range set.Profiles {
		if math.Abs(weights.total()-1) > 0.000001 {
			return fmt.Errorf("routing profile %s weights do not normalize to 1", name)
		}
	}
	return nil
}
