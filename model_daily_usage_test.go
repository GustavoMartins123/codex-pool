package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestModelDailyUsageUTCBoundsAndTotals(t *testing.T) {
	s, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 1, 1, 0, 0, 0, time.FixedZone("east", 3*3600))
	for _, date := range []string{"2026-09-28", "2026-09-29", "2026-09-30", "2026-10-01"} {
		for _, id := range []string{"a", "b"} {
			_, err := s.db.Exec(`INSERT INTO daily_costs(date,account_id,account_type,model,input_tokens,cached_tokens,cache_creation_tokens,output_tokens,reasoning_tokens,request_count,cost_usd) VALUES(?,?,'kimi','model',10,2,3,4,1,2,0.5)`, date, id)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, ts := range []string{"2026-09-29T23:59:59Z", "2026-09-30T00:00:00Z", "2026-09-30T23:59:59Z", "2026-10-01T00:00:00Z"} {
		_, err := s.db.Exec(`INSERT INTO request_costs(timestamp,account_id,account_type,model,input_tokens,cached_tokens,cache_creation_tokens,output_tokens,reasoning_tokens,cost_usd) VALUES(?,'a','kimi',NULL,5,1,2,3,1,0.25)`, ts)
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.getModelDailyUsageAt(1, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	want := []ModelDailyUsageEntry{
		{Date: "2026-09-29", AccountType: "kimi", Model: "model", InputTokens: 20, CachedTokens: 4, CacheCreationTokens: 6, OutputTokens: 8, ReasoningTokens: 2, RequestCount: 4, CostUSD: 1},
		{Date: "2026-09-30", AccountType: "kimi", InputTokens: 10, CachedTokens: 2, CacheCreationTokens: 4, OutputTokens: 6, ReasoningTokens: 2, RequestCount: 2, CostUSD: 0.5},
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d: %+v", i, rows[i])
		}
	}
	if _, err := s.getModelDailyUsageAt(0, now); err == nil {
		t.Fatal("invalid window accepted")
	}
	plan, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT date,account_type,model,SUM(input_tokens),SUM(cached_tokens),SUM(cache_creation_tokens),SUM(output_tokens),SUM(reasoning_tokens),SUM(request_count),SUM(cost_usd) FROM daily_costs WHERE date >= ? AND date < ? GROUP BY date,account_type,model`, "2026-09-29", "2026-09-30")
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	covered := false
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		covered = covered || strings.Contains(detail, "COVERING INDEX idx_daily_costs_model_usage")
		if strings.Contains(detail, "TEMP B-TREE FOR GROUP") {
			t.Fatal(detail)
		}
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatal("historical query did not use covering index")
	}
}

func TestModelDailyUsageRejectsPartialResults(t *testing.T) {
	s, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	today := time.Now().UTC().Format("2006-01-02")
	if _, err := s.db.Exec(`INSERT INTO request_costs(timestamp,account_id,account_type,input_tokens) VALUES(?,'a','kimi',NULL)`, today+"T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.getModelDailyUsage(1); err == nil || rows != nil {
		t.Fatalf("partial result: %+v, %v", rows, err)
	}
}
