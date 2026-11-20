package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
)

const costHistorySchema = `
CREATE TABLE IF NOT EXISTS legacy_cost_days (
    day DATE NOT NULL,
    account_id VARCHAR NOT NULL,
    account_type VARCHAR NOT NULL,
    model VARCHAR NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    input_tokens BIGINT NOT NULL,
    cached_tokens BIGINT NOT NULL,
    cache_creation_tokens BIGINT NOT NULL,
    output_tokens BIGINT NOT NULL,
    reasoning_tokens BIGINT NOT NULL,
    request_count BIGINT NOT NULL,
    cost_usd DOUBLE NOT NULL,
    PRIMARY KEY (day, account_id, account_type, model)
);
CREATE TABLE IF NOT EXISTS cost_history_import (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    through_sequence UBIGINT NOT NULL,
    imported_days BIGINT NOT NULL
);
CREATE OR REPLACE VIEW cost_activity AS
    SELECT day, account_id, account_type, model, first_seen,
           input_tokens, cached_tokens, cache_creation_tokens, output_tokens,
           reasoning_tokens, request_count, cost_usd
    FROM legacy_cost_days
    UNION ALL
    SELECT CAST(observed_at AT TIME ZONE 'UTC' AS DATE), account_id, account_type,
           COALESCE(model_normalized, ''), observed_at,
           input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens,
           reasoning_tokens, 1, CAST(api_equivalent_cost_usd AS DOUBLE)
    FROM usage_events AS event
    WHERE source = 'live' AND source_grain = 'request'
      AND NOT EXISTS (
          SELECT 1 FROM cost_history_import AS imported
          JOIN legacy_cost_days AS legacy
            ON legacy.day = CAST(event.observed_at AT TIME ZONE 'UTC' AS DATE)
           AND legacy.account_id = event.account_id
           AND legacy.account_type = event.account_type
           AND legacy.model = COALESCE(event.model_normalized, '')
          WHERE event.delivery_sequence IS NULL OR event.delivery_sequence <= imported.through_sequence
      );
`

const sqliteCostHistoryQuery = `
WITH requests AS (
    SELECT substr(timestamp, 1, 10) AS date, account_id, account_type,
           COALESCE(model, '') AS model, MIN(timestamp) AS first_seen,
           SUM(input_tokens) AS input_tokens, SUM(cached_tokens) AS cached_tokens,
           SUM(%s) AS cache_creation_tokens, SUM(output_tokens) AS output_tokens,
           SUM(reasoning_tokens) AS reasoning_tokens, COUNT(*) AS request_count, SUM(cost_usd) AS cost_usd
    FROM request_costs
    GROUP BY 1, 2, 3, 4
)
SELECT daily.date, daily.account_id, daily.account_type, daily.model,
       daily.date || 'T00:00:00Z',
       daily.input_tokens, daily.cached_tokens, %s,
       daily.output_tokens, daily.reasoning_tokens, daily.request_count, daily.cost_usd
FROM daily_costs AS daily
WHERE daily.date <> ? OR NOT EXISTS (
    SELECT 1 FROM requests AS raw
    WHERE raw.date = daily.date AND raw.account_id = daily.account_id
      AND raw.account_type = daily.account_type AND raw.model = daily.model
)
UNION ALL
SELECT raw.date, raw.account_id, raw.account_type, raw.model, raw.first_seen,
       raw.input_tokens, raw.cached_tokens, raw.cache_creation_tokens,
       raw.output_tokens, raw.reasoning_tokens, raw.request_count, raw.cost_usd
FROM requests AS raw
WHERE raw.date = ? OR NOT EXISTS (
    SELECT 1 FROM daily_costs AS daily
    WHERE daily.date = raw.date AND daily.account_id = raw.account_id
      AND daily.account_type = raw.account_type AND daily.model = raw.model
)
`

// migrateSQLiteCosts copies account and model aggregates before the proxy starts
// accepting traffic. The delivery sequence distinguishes pre-migration activity
// from new records even if older outbox entries are still waiting to be drained.
func (a *DuckAnalytics) migrateSQLiteCosts(ctx context.Context, storePath, configuredPath string, now time.Time) error {
	a.queryMu.Lock()
	defer a.queryMu.Unlock()

	var imported int64
	err := a.db.QueryRowContext(ctx, `SELECT imported_days FROM cost_history_import WHERE id=1`).Scan(&imported)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check cost history import: %w", err)
	}
	path, err := legacySQLiteCostPath(storePath, configuredPath)
	if err != nil {
		return err
	}

	var through uint64
	deleted := make(map[string]struct{})
	if err := a.bolt.View(func(tx *bbolt.Tx) error {
		outbox := tx.Bucket([]byte(bucketAnalyticsOutbox))
		if outbox == nil {
			return errors.New("analytics outbox is missing")
		}
		through = outbox.Sequence()
		tombstones := tx.Bucket([]byte(bucketAccountDeletions))
		if tombstones == nil {
			return nil
		}
		return tombstones.ForEach(func(key, _ []byte) error {
			deleted[string(key)] = struct{}{}
			return nil
		})
	}); err != nil {
		return fmt.Errorf("read cost history boundary: %w", err)
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if path != "" {
		imported, err = importLegacyCostDays(ctx, tx, path, now.UTC().Format(time.DateOnly), deleted)
		if err != nil {
			return fmt.Errorf("import SQLite cost history: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cost_history_import (id, through_sequence, imported_days) VALUES (1, ?, ?)`, through, imported); err != nil {
		return err
	}
	return tx.Commit()
}

func legacySQLiteCostPath(storePath, configuredPath string) (string, error) {
	path := configuredPath
	if path == "" {
		path = filepath.Join(filepath.Dir(storePath), "analytics.db")
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) && configuredPath == "" {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read legacy analytics path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("legacy analytics path is not a file")
	}
	return path, nil
}

func importLegacyCostDays(ctx context.Context, tx *sql.Tx, path, today string, deleted map[string]struct{}) (int64, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	filePath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" && filePath[0] != '/' {
		filePath = "/" + filePath
	}
	dsn := url.URL{Scheme: "file", Path: filePath, RawQuery: "mode=ro"}
	source, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return 0, err
	}
	defer source.Close()
	// The SQLite schema tracked cache reads before tracking cache creation.
	var requestCacheCreation, dailyCacheCreation bool
	err = source.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pragma_table_info('request_costs') WHERE name = 'cache_creation_tokens'),
		       EXISTS (SELECT 1 FROM pragma_table_info('daily_costs') WHERE name = 'cache_creation_tokens')
	`).Scan(&requestCacheCreation, &dailyCacheCreation)
	if err != nil {
		return 0, fmt.Errorf("inspect cache creation columns: %w", err)
	}
	requestField, dailyField := "cache_creation_tokens", "daily.cache_creation_tokens"
	if !requestCacheCreation {
		requestField = "0"
	}
	if !dailyCacheCreation {
		dailyField = "0"
	}
	if !requestCacheCreation || !dailyCacheCreation {
		log.Print("SQLite cost import: source predates cache creation accounting")
	}
	query := fmt.Sprintf(sqliteCostHistoryQuery, requestField, dailyField)
	rows, err := source.QueryContext(ctx, query, today, today)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	insert, err := tx.PrepareContext(ctx, `INSERT INTO legacy_cost_days VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insert.Close()

	var imported int64
	for rows.Next() {
		var row ModelDailyUsageEntry
		var accountID, first string
		if err := rows.Scan(&row.Date, &accountID, &row.AccountType, &row.Model, &first,
			&row.InputTokens, &row.CachedTokens, &row.CacheCreationTokens, &row.OutputTokens,
			&row.ReasoningTokens, &row.RequestCount, &row.CostUSD); err != nil {
			return 0, err
		}
		if _, removed := deleted[resourceKey(AccountType(row.AccountType), accountID)]; removed {
			continue
		}
		day, err := time.Parse(time.DateOnly, row.Date)
		if err != nil {
			return 0, fmt.Errorf("invalid legacy analytics day: %w", err)
		}
		firstSeen, err := time.Parse(time.RFC3339, first)
		if err != nil {
			return 0, fmt.Errorf("invalid legacy analytics observation: %w", err)
		}
		if _, err := insert.ExecContext(ctx, day, accountID, row.AccountType, row.Model, firstSeen.UTC(),
			row.InputTokens, row.CachedTokens, row.CacheCreationTokens, row.OutputTokens,
			row.ReasoningTokens, row.RequestCount, row.CostUSD); err != nil {
			return 0, err
		}
		imported++
	}
	return imported, rows.Err()
}
