package main

import (
	"sort"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/routing"
)

type routeSelection struct {
	Account      *Account
	Policy       string
	Reasons      []string
	Score        float64
	Alternatives []RouteAlternative
	Breakdown    *ScoreBreakdownView
}

func (p *poolState) selectRoute(req routing.Request) routeSelection {
	if p == nil {
		return routeSelection{Policy: "none"}
	}
	switch req.Mode {
	case routing.MatchAccountID:
		return routeSelection{Account: p.selectAccountByID(req)}
	case routing.MatchCyberAccess:
		return routeSelection{Account: p.selectCyberAccount(req)}
	case routing.MatchImageFanout:
		return routeSelection{Account: p.selectImageFanoutAccount(req)}
	case routing.MatchEligible, routing.MatchModelFamily:
	default:
		return routeSelection{Policy: "none", Reasons: []string{"invalid_routing_mode"}}
	}

	if req.Profile == "" {
		req.Profile = string(p.defaultRoutingProfile())
	}
	if RoutingProfile(req.Profile) != RoutingLegacy {
		decision := p.selectSmartRoute(req)
		if req.Mode == routing.MatchModelFamily && req.ConversationID != "" && decision.Account != nil {
			canonical := antigravityCanonicalModel(req.Model)
			p.mu.Lock()
			err := p.pinForUserLocked(req.UserID, "antigravity:"+canonical+":"+req.ConversationID, decision.Account.ID, time.Now())
			p.mu.Unlock()
			if err != nil {
				return routeSelection{Policy: req.Profile, Reasons: []string{err.Error()}}
			}
		}
		return decision
	}

	var account *Account
	if req.Mode == routing.MatchModelFamily {
		req.Exclude = p.accountExclusions(req.UserID, req.Exclude, req.Model)
		req.Exclude = p.controlExclusions(req.UserID, req.ConversationID, req.Exclude)
		account = p.selectAntigravityAccount(req)
	} else {
		account = p.selectTierAccount(req)
	}
	if account == nil {
		return routeSelection{Policy: "none"}
	}
	if req.Trace {
		return p.describeTierSelection(req, account)
	}
	return routeSelection{Account: account, Policy: req.Profile}
}

func accountMatchesRouteLocked(account *Account, req routing.Request) bool {
	return !account.Dead && !account.Disabled && !accountHealthBlockedLocked(account) &&
		(req.Provider == "" || string(account.Type) == req.Provider) &&
		planMatchesRequired(account.PlanType, req.RequiredPlan) &&
		accountAllowsClientIPLocked(account, req.ClientIP)
}

func quotaAllowsRouteLocked(account *Account) bool {
	return !primaryUsageBlocksRoutingLocked(account, accountPrimaryUsageLocked(account)) &&
		accountSecondaryUsageLocked(account) < secondaryHardExcludeThreshold
}

func (p *poolState) selectAccountByID(req routing.Request) *Account {
	if req.AccountID == "" || p.accountExclusions(req.UserID, req.Exclude)[req.AccountID] {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.getLocked(req.AccountID)
	if account == nil {
		return nil
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if !accountMatchesRouteLocked(account, req) || !quotaAllowsRouteLocked(account) ||
		!account.ExpiresAt.IsZero() && account.ExpiresAt.Before(time.Now()) {
		return nil
	}
	return account
}

func (p *poolState) selectImageFanoutAccount(req routing.Request) *Account {
	p.mu.RLock()
	ids := make([]string, 0, len(p.accounts))
	for _, account := range p.accounts {
		if account != nil && account.Type == AccountTypeCodex && !req.Exclude[account.ID] {
			ids = append(ids, account.ID)
		}
	}
	p.mu.RUnlock()
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	start := req.Index % len(ids)
	if start < 0 {
		start += len(ids)
	}
	req.Provider = string(AccountTypeCodex)
	for offset := 0; offset < len(ids); offset++ {
		req.AccountID = ids[(start+offset)%len(ids)]
		if account := p.selectAccountByID(req); account != nil {
			return account
		}
	}
	return nil
}

func (p *poolState) selectCyberAccount(req routing.Request) *Account {
	exclude := p.accountExclusions(req.UserID, req.Exclude)
	exclude = p.controlExclusions(req.UserID, "", exclude)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *Account
	var bestScore float64
	for _, account := range p.accounts {
		if account == nil || exclude[account.ID] {
			continue
		}
		account.mu.Lock()
		// The caller refreshes an expired access token before its upstream call.
		if !account.CyberAccess || !accountMatchesRouteLocked(account, req) || !quotaAllowsRouteLocked(account) {
			account.mu.Unlock()
			continue
		}
		score := scoreAccountLocked(account, now)
		account.mu.Unlock()
		score -= float64(atomic.LoadInt64(&account.Inflight)) * 0.02
		if best == nil || score > bestScore {
			best = account
			bestScore = score
		}
	}
	if best != nil {
		p.rr++
	}
	return best
}

func (p *poolState) selectTierAccount(req routing.Request) *Account {
	req.Exclude = p.accountExclusions(req.UserID, req.Exclude, req.Model)
	req.Exclude = p.controlExclusions(req.UserID, req.ConversationID, req.Exclude)
	requiresDiscovery := p.discoveredModelRequiresEntitlement(AccountType(req.Provider), req.Model)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if account := p.selectPinnedTierAccount(req, requiresDiscovery, now); account != nil {
		return account
	}

	count := len(p.accounts)
	if count == 0 {
		return nil
	}
	var candidates []routing.TierCandidate[*Account]
	start := int(p.rr % uint64(count))
	for i := 0; i < count; i++ {
		account := p.accounts[(start+i)%count]
		if account == nil || req.Exclude[account.ID] {
			continue
		}
		account.mu.Lock()
		if !tierAccountEligibleLocked(account, req, requiresDiscovery, now) {
			account.mu.Unlock()
			continue
		}
		if allowed, _, _ := p.circuitBreakers.CanAllowTarget(string(account.Type), account.ID, req.Model, nil); !allowed {
			account.mu.Unlock()
			continue
		}
		candidate := routing.TierCandidate[*Account]{
			Account: account, ID: account.ID, Tier: accountTier(account.Type, account.PlanType),
			SecondaryUse: accountSecondaryUsageLocked(account), Score: scoreAccountLocked(account, now),
			CyberAccess: account.CyberAccess,
		}
		account.mu.Unlock()
		candidate.Score -= float64(atomic.LoadInt64(&account.Inflight)) * 0.02
		candidates = append(candidates, candidate)
	}

	selected, ok := routing.PickTiered(candidates, p.tierThreshold, req.Provider == string(AccountTypeCodex), p.rr)
	if !ok {
		return nil
	}
	if allowed, _, _ := p.circuitBreakers.AllowTarget(string(selected.Type), selected.ID, req.Model, nil); !allowed {
		return nil
	}
	p.rr++
	return selected
}

func tierAccountEligibleLocked(account *Account, req routing.Request, requiresDiscovery bool, now time.Time) bool {
	if !accountMatchesRouteLocked(account, req) || !quotaAllowsRouteLocked(account) || account.RateLimitUntil.After(now) {
		return false
	}
	if requiresDiscovery {
		_, supported := accountDiscoveredModel(account, req.Model)
		return supported
	}
	return true
}

func (p *poolState) selectPinnedTierAccount(req routing.Request, requiresDiscovery bool, now time.Time) *Account {
	conversation := req.ConversationID
	if conversation == "" {
		return nil
	}
	if updated, ok := p.convUpdatedAt[conversation]; ok && now.Sub(updated) > conversationPinTTL {
		p.unpinLocked(conversation)
	}
	if owner := p.convOwner[conversation]; owner != "" && req.UserID != "" && owner != req.UserID {
		return nil
	}
	id := p.convPin[conversation]
	if id == "" || req.Exclude[id] {
		return nil
	}

	if account := p.getLocked(id); account != nil {
		if allowed, _ := p.circuitBreakers.CanAllowAccountModel(id, req.Model); !allowed {
			return nil
		}
		account.mu.Lock()
		if requiresDiscovery {
			if _, supported := accountDiscoveredModel(account, req.Model); !supported {
				account.mu.Unlock()
				return nil
			}
		}
		eligible := tierAccountEligibleLocked(account, req, false, now) &&
			(account.Type != AccountTypeCodex || isCodexProAccessPlan(account.PlanType)) &&
			(account.ExpiresAt.IsZero() || !account.ExpiresAt.Before(now))
		account.mu.Unlock()
		if allowed, _, _ := p.circuitBreakers.CanAllowTarget(string(account.Type), id, "", nil); eligible && allowed {
			if allowed, _, _ := p.circuitBreakers.AllowTarget(string(account.Type), id, req.Model, nil); allowed {
				if p.convUpdatedAt == nil {
					p.convUpdatedAt = make(map[string]time.Time)
				}
				p.convUpdatedAt[conversation] = now
				return account
			}
			// A probe claimed by another request makes this pin unavailable.
			// Preserve the pin until the circuit can be checked again.
			return nil
		}
	}
	p.debugf("unpinning conversation %s from ineligible account %s", conversation, id)
	p.unpinLocked(conversation)
	return nil
}

func (p *poolState) describeTierSelection(req routing.Request, account *Account) routeSelection {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	decision := routeSelection{Account: account, Policy: "balanced", Reasons: []string{"healthy"}}
	family := req.Mode == routing.MatchModelFamily
	pinned := p.convPin[req.ConversationID] == account.ID
	if family {
		key := "antigravity:" + antigravityCanonicalModel(req.Model) + ":" + req.ConversationID
		pinned = pinned || p.convPin[key] == account.ID
	}
	if req.ConversationID != "" && pinned {
		decision.Policy = "pinned"
		decision.Reasons = append(decision.Reasons, "conversation_pin")
	}

	account.mu.Lock()
	score := scoreAccountBreakdownLocked(account, now)
	inflight := atomic.LoadInt64(&account.Inflight)
	healthBlocked := accountHealthBlockedLocked(account)
	account.mu.Unlock()
	decision.Score = score.Score - float64(inflight)*0.02
	decision.Breakdown = newScoreBreakdownView(score, inflight, healthBlocked)
	if family {
		decision.Reasons = append(decision.Reasons, "quota_headroom")
	} else {
		switch score.BaseWindow {
		case "7d":
			decision.Reasons = append(decision.Reasons, "weekly_headroom")
		case "5h":
			decision.Reasons = append(decision.Reasons, "five_hour_headroom")
		}
		if score.PrimaryPaceBonus > 0 {
			decision.Reasons = append(decision.Reasons, "primary_pace")
		}
		if score.RecentUseBonus > 0 {
			decision.Reasons = append(decision.Reasons, "recent_success")
		}
		if score.CreditBonus > 1 {
			decision.Reasons = append(decision.Reasons, "credit_bonus")
		}
	}

	for _, alt := range p.accounts {
		if alt == nil || alt.ID == account.ID || req.Provider != "" && string(alt.Type) != req.Provider {
			continue
		}
		alt.mu.Lock()
		if alt.Dead || alt.Disabled || accountHealthBlockedLocked(alt) {
			alt.mu.Unlock()
			continue
		}
		altScore := scoreAccountBreakdownLocked(alt, now)
		altInflight := atomic.LoadInt64(&alt.Inflight)
		alt.mu.Unlock()
		reasons := []string{"healthy"}
		if family {
			reasons = append(reasons, "quota_headroom")
		} else if altScore.BaseWindow == "7d" {
			reasons = append(reasons, "weekly_headroom")
		}
		decision.Alternatives = append(decision.Alternatives, RouteAlternative{
			Provider: string(alt.Type), Model: req.Model, AccountID: alt.ID,
			Score: altScore.Score - float64(altInflight)*0.02, Reasons: reasons,
			ScoreBreakdown: newScoreBreakdownView(altScore, altInflight, false),
		})
	}
	return decision
}
