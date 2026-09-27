package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
)

// requireAuditFile skips audit tests when their backing database is absent.
// These tests are ad-hoc audits over operator data (typically copied into
// /tmp on the operations host); on dev machines and CI they must not fail.
func requireAuditFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("audit database not present: %s", path)
	}
}

func TestAntigravityTokenAudit(t *testing.T) {
	requireAuditFile(t, "/tmp/codex-pool-usage-audit.duckdb")
	db, err := sql.Open("duckdb", "/tmp/codex-pool-usage-audit.duckdb?access_mode=read_only")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	auditQuery(t, db, "providers", `
		SELECT account_type, count(*), sum(input_tokens), sum(cache_read_tokens),
		       sum(output_tokens), sum(reasoning_tokens), sum(billable_tokens)
		FROM usage_events GROUP BY account_type ORDER BY account_type`)
	auditQuery(t, db, "summary", `
		SELECT count(*), count(DISTINCT proxy_request_id),
		       min(observed_at), max(observed_at),
		       sum(input_tokens), sum(cache_read_tokens), sum(output_tokens),
		       sum(reasoning_tokens), sum(billable_tokens)
		FROM usage_events WHERE account_type='antigravity'`)
	auditQuery(t, db, "checks", `
		SELECT
		  count(*) FILTER (WHERE input_tokens < 0 OR cache_read_tokens < 0
		    OR output_tokens < 0 OR reasoning_tokens < 0 OR billable_tokens < 0
		    OR cache_read_tokens > input_tokens),
		  count(*) FILTER (WHERE billable_tokens <>
		    greatest(input_tokens-cache_read_tokens+output_tokens, 0)),
		  count(*) FILTER (WHERE proxy_request_id='')
		FROM usage_events WHERE account_type='antigravity'`)
	auditQuery(t, db, "duplicates", `
		SELECT proxy_request_id, count(*), min(observed_at), max(observed_at),
		       sum(input_tokens), sum(output_tokens)
		FROM usage_events WHERE account_type='antigravity'
		GROUP BY proxy_request_id HAVING count(*) > 1
		ORDER BY count(*) DESC, max(observed_at) DESC LIMIT 30`)
	auditQuery(t, db, "latest", `
		SELECT observed_at, proxy_request_id, principal_id, model_normalized,
		       input_tokens, cache_read_tokens, output_tokens,
		       reasoning_tokens, billable_tokens
		FROM usage_events WHERE account_type='antigravity'
		ORDER BY observed_at DESC LIMIT 100`)
	auditQuery(t, db, "largest", `
		SELECT observed_at, proxy_request_id, principal_id, model_normalized,
		       input_tokens, cache_read_tokens, output_tokens,
		       reasoning_tokens, billable_tokens
		FROM usage_events WHERE account_type='antigravity'
		ORDER BY input_tokens DESC LIMIT 50`)
	auditQuery(t, db, "jumps", `
		WITH ordered AS (
		  SELECT observed_at, proxy_request_id, principal_id, model_normalized,
		         input_tokens, output_tokens,
		         lag(input_tokens) OVER (
		           PARTITION BY principal_id, model_normalized ORDER BY observed_at
		         ) previous_input
		  FROM usage_events WHERE account_type='antigravity'
		)
		SELECT observed_at, proxy_request_id, principal_id, model_normalized,
		       previous_input, input_tokens,
		       round(input_tokens::DOUBLE/nullif(previous_input, 0), 2),
		       output_tokens
		FROM ordered
		WHERE previous_input > 0 AND input_tokens >= previous_input*2
		ORDER BY observed_at DESC LIMIT 50`)
}

func TestLegacyAnalyticsAudit(t *testing.T) {
	requireAuditFile(t, "/tmp/codex-pool-analytics-audit.db")
	db, err := sql.Open("sqlite", "file:/tmp/codex-pool-analytics-audit.db?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	auditQuery(t, db, "legacy_providers", `
		SELECT account_type, count(*), sum(input_tokens), sum(cached_tokens),
		       sum(output_tokens), sum(reasoning_tokens)
		FROM request_costs GROUP BY account_type ORDER BY account_type`)
	auditQuery(t, db, "legacy_antigravity_latest", `
		SELECT timestamp, account_id, user_id, model, input_tokens,
		       cached_tokens, output_tokens, reasoning_tokens
		FROM request_costs WHERE account_type='antigravity'
		ORDER BY timestamp DESC LIMIT 100`)
}

func TestBoltAntigravityAudit(t *testing.T) {
	requireAuditFile(t, "/tmp/codex-pool-proxy-audit.db")
	db, err := bbolt.Open("/tmp/codex-pool-proxy-audit.db", 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var all []RequestUsage
	err = db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketUsageRequests))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, value []byte) error {
			var usage RequestUsage
			if err := json.Unmarshal(value, &usage); err == nil {
				all = append(all, usage)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[AccountType]int{}
	for _, usage := range all {
		counts[usage.AccountType]++
	}
	t.Logf("bolt providers=%v total=%d", counts, len(all))
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp.Before(all[j].Timestamp) })
	var antigravity []RequestUsage
	for _, usage := range all {
		if usage.AccountType == AccountTypeAntigravity {
			antigravity = append(antigravity, usage)
		}
	}
	t.Logf("bolt antigravity rows=%d", len(antigravity))
	start := 0
	if len(antigravity) > 100 {
		start = len(antigravity) - 100
	}
	for _, usage := range antigravity[start:] {
		t.Logf("bolt_antigravity time=%s request=%s user=%s model=%s input=%d cached=%d output=%d reasoning=%d billable=%d",
			usage.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z"),
			usage.RequestID, usage.UserID, usage.Model, usage.InputTokens,
			usage.CachedInputTokens, usage.OutputTokens, usage.ReasoningTokens,
			usage.BillableTokens)
	}
}

func TestAgyConversationSchemaAudit(t *testing.T) {
	requireAuditFile(t, "/tmp/agy-conversation-audit.db")
	db, err := sql.Open("sqlite", "file:/tmp/agy-conversation-audit.db?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	auditQuery(t, db, "agy_schema", `
		SELECT type, name, sql FROM sqlite_master
		WHERE type IN ('table','view') ORDER BY name`)
}

func auditQuery(t *testing.T, db *sql.DB, label, query string) {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s columns=%v", label, columns)
	values, dest := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %s", label, fmt.Sprint(values))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
