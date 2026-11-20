package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// costAnalyticsRepository supplies the cost and model views read by the pool
// statistics and signal room.
type costAnalyticsRepository interface {
	getCostByAccount(days int) (map[string]float64, error)
	getDailyCosts(days int) ([]DailyCostEntry, error)
	getModelDailyUsage(days int) ([]ModelDailyUsageEntry, error)
	getSignalEconomics(accountIDs []string, now time.Time) (signalEconomicsSnapshot, error)
	getAllTimeAccountCostStats() (map[string]AccountCostStats, error)
}

type legacyCostAnalyticsRepository interface {
	costAnalyticsRepository
	recordRequest(RequestUsage, float64) error
}

var _ costAnalyticsRepository = (*DuckAnalytics)(nil)

func (a *DuckAnalytics) getCostByAccount(days int) (map[string]float64, error) {
	a.queryMu.RLock()
	defer a.queryMu.RUnlock()
	since := time.Now().AddDate(0, 0, -days).Format(time.DateOnly)
	rows, err := a.db.Query(`
        SELECT account_id, SUM(cost_usd) FROM cost_activity
        WHERE day >= CAST(? AS DATE) GROUP BY account_id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]float64)
	for rows.Next() {
		var id string
		var cost float64
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, err
		}
		result[id] = cost
	}
	return result, rows.Err()
}

func (a *DuckAnalytics) getDailyCosts(days int) ([]DailyCostEntry, error) {
	a.queryMu.RLock()
	defer a.queryMu.RUnlock()
	now := time.Now().UTC()
	rows, err := a.db.Query(`
        SELECT CAST(day AS VARCHAR), account_type, SUM(cost_usd), SUM(request_count)
        FROM cost_activity
        WHERE day >= CAST(? AS DATE) AND day < CAST(? AS DATE)
        GROUP BY day, account_type ORDER BY day, account_type`,
		now.AddDate(0, 0, -days).Format(time.DateOnly), now.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DailyCostEntry
	for rows.Next() {
		var entry DailyCostEntry
		if err := rows.Scan(&entry.Date, &entry.AccountType, &entry.CostUSD, &entry.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (a *DuckAnalytics) getModelDailyUsage(days int) ([]ModelDailyUsageEntry, error) {
	return a.getModelDailyUsageAt(days, time.Now())
}

func (a *DuckAnalytics) getModelDailyUsageAt(days int, now time.Time) ([]ModelDailyUsageEntry, error) {
	if days <= 0 {
		return nil, fmt.Errorf("model usage window must be positive")
	}
	a.queryMu.RLock()
	defer a.queryMu.RUnlock()
	utc := now.UTC()
	rows, err := a.db.Query(`
        SELECT CAST(day AS VARCHAR), account_type, model, SUM(input_tokens), SUM(cached_tokens),
               SUM(cache_creation_tokens), SUM(output_tokens), SUM(reasoning_tokens),
               SUM(request_count), SUM(cost_usd)
        FROM cost_activity
        WHERE day >= CAST(? AS DATE) AND day < CAST(? AS DATE)
        GROUP BY day, account_type, model ORDER BY day, account_type, model`,
		utc.AddDate(0, 0, -days).Format(time.DateOnly), utc.AddDate(0, 0, 1).Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ModelDailyUsageEntry, 0)
	for rows.Next() {
		var entry ModelDailyUsageEntry
		if err := rows.Scan(&entry.Date, &entry.AccountType, &entry.Model,
			&entry.InputTokens, &entry.CachedTokens, &entry.CacheCreationTokens, &entry.OutputTokens,
			&entry.ReasoningTokens, &entry.RequestCount, &entry.CostUSD); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (a *DuckAnalytics) getSignalEconomics(accountIDs []string, now time.Time) (signalEconomicsSnapshot, error) {
	result := signalEconomicsSnapshot{firstSeen: make(map[string]time.Time, len(accountIDs))}
	if len(accountIDs) == 0 {
		return result, nil
	}
	encoded, err := json.Marshal(accountIDs)
	if err != nil {
		return result, err
	}
	a.queryMu.RLock()
	defer a.queryMu.RUnlock()
	today := now.UTC().Format(time.DateOnly)
	rows, err := a.db.Query(`
        WITH current_accounts AS (
            SELECT json_extract_string(value, '$') AS account_id FROM json_each(CAST(? AS JSON))
        ), account_activity AS MATERIALIZED (
            SELECT activity.*
            FROM cost_activity AS activity
            WHERE activity.account_id IN (SELECT account_id FROM current_accounts)
              AND activity.day <= CAST(? AS DATE)
        )
        SELECT 0, account_id, '',
               MIN(CASE WHEN day < CAST(? AS DATE)
                        THEN CAST(day AS TIMESTAMP) AT TIME ZONE 'UTC' ELSE first_seen END), 0.0
        FROM account_activity GROUP BY account_id
        UNION ALL
        SELECT 1, CAST(day AS VARCHAR), account_type, NULL::TIMESTAMPTZ, SUM(cost_usd)
        FROM account_activity GROUP BY day, account_type
        ORDER BY 1, 2, 3`, string(encoded), today, today)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind int
		var key, value string
		var seen sql.NullTime
		var cost float64
		if err := rows.Scan(&kind, &key, &value, &seen, &cost); err != nil {
			return signalEconomicsSnapshot{}, err
		}
		if kind == 0 {
			if !seen.Valid {
				return signalEconomicsSnapshot{}, fmt.Errorf("missing economics measurement date for account %q", key)
			}
			result.firstSeen[key] = seen.Time.UTC()
		} else {
			result.dailyCosts = append(result.dailyCosts, DailyCostEntry{Date: key, AccountType: value, CostUSD: cost})
		}
	}
	return result, rows.Err()
}

func (a *DuckAnalytics) getAllTimeAccountCostStats() (map[string]AccountCostStats, error) {
	a.queryMu.RLock()
	defer a.queryMu.RUnlock()
	rows, err := a.db.Query(`
        SELECT account_id, SUM(cost_usd),
               MIN(CASE WHEN day < CAST(? AS DATE)
                        THEN CAST(day AS TIMESTAMP) AT TIME ZONE 'UTC' ELSE first_seen END)
        FROM cost_activity GROUP BY account_id`, time.Now().UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]AccountCostStats)
	for rows.Next() {
		var id string
		var stats AccountCostStats
		if err := rows.Scan(&id, &stats.CostUSD, &stats.FirstSeen); err != nil {
			return nil, err
		}
		stats.FirstSeen = stats.FirstSeen.UTC()
		result[id] = stats
	}
	return result, rows.Err()
}
