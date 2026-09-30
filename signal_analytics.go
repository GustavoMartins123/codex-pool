package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

type SignalEconomicsPoint struct {
	Date                        string             `json:"date"`
	DailyAPIValue               float64            `json:"daily_api_value"`
	CumulativeAPIValue          float64            `json:"cumulative_api_value"`
	CumulativeSubscriptionSpend float64            `json:"cumulative_subscription_spend"`
	ProviderAPIValue            map[string]float64 `json:"provider_api_value"`
}

type SignalAnalyticsResponse struct {
	GeneratedAt       time.Time              `json:"generated_at"`
	OriginDataSince   time.Time              `json:"origin_data_since"`
	Economics         []SignalEconomicsPoint `json:"economics"`
	Hourly            []UserHourlyUsage      `json:"hourly"`
	OriginWeekly      []OriginWeeklyUsage    `json:"origin_weekly"`
	ModelDaily        []ModelDailyUsageEntry `json:"model_daily"`
	QuotaCapacity     []QuotaCapacityPoint   `json:"quota_capacity"`
	ModelEfficiency   []ModelQuotaEfficiency `json:"model_efficiency"`
	ResetObservations []ResetObservation     `json:"reset_observations"`
	QuotaGeneratedAt  time.Time              `json:"quota_generated_at,omitempty"`
}

// handleSignalAnalytics returns chart-ready time series that preserve the
// attribution boundaries needed by the signal-room UI. It intentionally keeps
// account identifiers hashed; raw origin metadata remains admin-only.
func (h *proxyHandler) handleSignalAnalytics(w http.ResponseWriter, r *http.Request) {
	weeks := 6
	if raw := r.URL.Query().Get("weeks"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			weeks = min(parsed, 12)
		}
	}

	response := SignalAnalyticsResponse{
		GeneratedAt:       time.Now().UTC(),
		Economics:         []SignalEconomicsPoint{},
		Hourly:            []UserHourlyUsage{},
		OriginWeekly:      []OriginWeeklyUsage{},
		ModelDaily:        []ModelDailyUsageEntry{},
		QuotaCapacity:     []QuotaCapacityPoint{},
		ModelEfficiency:   []ModelQuotaEfficiency{},
		ResetObservations: []ResetObservation{},
	}
	if h.store != nil {
		response.OriginDataSince = response.GeneratedAt.Add(-h.store.retention)
		originWeekly, err := h.store.getOriginWeeklyUsage(weeks)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, "failed to build origin drain matrix")
			return
		}
		response.OriginWeekly = append(response.OriginWeekly, originWeekly...)

		hourly, err := h.store.getGlobalHourlyUsage(24 * 14)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, "failed to load burn velocity")
			return
		}
		response.Hourly = append(response.Hourly, hourly...)

		quota := h.quotaIntelligenceSnapshot()
		response.QuotaCapacity = append(response.QuotaCapacity, quota.capacity...)
		response.ModelEfficiency = append(response.ModelEfficiency, quota.modelEfficiency...)
		response.ResetObservations = append(response.ResetObservations, quota.resetEvents...)
		response.QuotaGeneratedAt = quota.updatedAt
	}

	if h.analyticsStore != nil {
		economics, err := h.buildSignalEconomics(response.GeneratedAt)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, "failed to build subscription economics")
			return
		}
		response.Economics = append(response.Economics, economics...)
		modelDaily, err := h.analyticsStore.getModelDailyUsage(42)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, "failed to build model demand mix")
			return
		}
		response.ModelDaily = append(response.ModelDaily, modelDaily...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (h *proxyHandler) buildSignalEconomics(now time.Time) ([]SignalEconomicsPoint, error) {
	now = now.UTC()
	type subscription struct {
		monthly   float64
		firstSeen time.Time
	}
	currentAccounts := make(map[string]subscription)
	accountIDs := make([]string, 0)
	for _, account := range h.pool.allAccounts() {
		account.mu.Lock()
		monthly, _ := getSubscriptionCost(account.Type, accountPlanForSubscription(account.PlanType))
		currentAccounts[account.ID] = subscription{monthly: monthly}
		accountIDs = append(accountIDs, account.ID)
		account.mu.Unlock()
	}
	snapshot, err := h.analyticsStore.getSignalEconomics(accountIDs, now)
	if err != nil {
		return nil, err
	}
	for id, sub := range currentAccounts {
		sub.firstSeen = snapshot.firstSeen[id]
		currentAccounts[id] = sub
	}

	dailyValue := make(map[string]float64)
	dailyProviders := make(map[string]map[string]float64)
	var firstDate time.Time
	for _, row := range snapshot.dailyCosts {
		date, err := time.Parse("2006-01-02", row.Date)
		if err != nil {
			return nil, err
		}
		if firstDate.IsZero() || date.Before(firstDate) {
			firstDate = date
		}
		dailyValue[row.Date] += row.CostUSD
		if dailyProviders[row.Date] == nil {
			dailyProviders[row.Date] = make(map[string]float64)
		}
		dailyProviders[row.Date][row.AccountType] += row.CostUSD
	}
	for _, sub := range currentAccounts {
		if sub.firstSeen.IsZero() {
			continue
		}
		date := time.Date(sub.firstSeen.Year(), sub.firstSeen.Month(), sub.firstSeen.Day(), 0, 0, 0, 0, time.UTC)
		if firstDate.IsZero() || date.Before(firstDate) {
			firstDate = date
		}
	}
	if firstDate.IsZero() {
		if len(currentAccounts) == 0 {
			return nil, nil
		}
		firstDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	points := make([]SignalEconomicsPoint, 0, int(today.Sub(firstDate).Hours()/24)+1)
	cumulativeAPIValue := 0.0
	for date := firstDate; !date.After(today); date = date.AddDate(0, 0, 1) {
		dateKey := date.Format("2006-01-02")
		cumulativeAPIValue += dailyValue[dateKey]
		cumulativeSpend := 0.0
		endOfDay := date.Add(24*time.Hour - time.Nanosecond)
		for _, sub := range currentAccounts {
			spend := sub.monthly
			if !sub.firstSeen.IsZero() {
				spend, _ = estimateSubscriptionSpend(sub.monthly, sub.firstSeen, endOfDay)
			}
			cumulativeSpend += spend
		}
		providerValues := dailyProviders[dateKey]
		if providerValues == nil {
			providerValues = map[string]float64{}
		}
		points = append(points, SignalEconomicsPoint{
			Date:                        dateKey,
			DailyAPIValue:               dailyValue[dateKey],
			CumulativeAPIValue:          cumulativeAPIValue,
			CumulativeSubscriptionSpend: cumulativeSpend,
			ProviderAPIValue:            providerValues,
		})
	}

	return points, nil
}
