package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkSignalAnalytics(b *testing.B) {
	for _, accounts := range []int{10, 100} {
		b.Run(fmt.Sprintf("%d_accounts", accounts), func(b *testing.B) {
			h, now := newSignalBenchmark(b, accounts)
			accountIDs := make([]string, 0)
			for _, account := range h.pool.allAccounts() {
				accountIDs = append(accountIDs, account.ID)
			}
			hourly, err := h.store.getGlobalHourlyUsage(24 * 14)
			if err != nil || len(hourly) != 24*14 {
				b.Fatalf("unexpected hourly fixture: rows=%d, err=%v", len(hourly), err)
			}
			points, err := h.buildSignalEconomics(now)
			if err != nil {
				b.Fatal(err)
			}
			wantValue := float64(accounts*4/5) * (365*3*0.01 + 100*0.01)
			if len(points) != 366 || math.Abs(points[len(points)-1].CumulativeAPIValue-wantValue) > 1e-6 {
				b.Fatalf("unexpected economics fixture: days=%d, want value=%v", len(points), wantValue)
			}
			queries := []struct {
				name string
				run  func() error
			}{
				{"economics_query", func() error { _, err := h.analyticsStore.getSignalEconomics(accountIDs, now); return err }},
				{"account_totals", func() error { _, err := h.analyticsStore.getAllTimeAccountCostStats(); return err }},
				{"model_daily", func() error { _, err := h.analyticsStore.getModelDailyUsage(42); return err }},
				{"origin_weekly", func() error { _, err := h.store.getOriginWeeklyUsage(6); return err }},
				{"hourly", func() error { _, err := h.store.getGlobalHourlyUsage(24 * 14); return err }},
				{"economics", func() error { _, err := h.buildSignalEconomics(now); return err }},
			}
			for _, query := range queries {
				b.Run(query.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if err := query.run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			b.Run("handler", func(b *testing.B) {
				request := httptest.NewRequest(http.MethodGet, "/api/pool/signal", nil)
				b.ReportAllocs()
				for b.Loop() {
					response := httptest.NewRecorder()
					h.handleSignalAnalytics(response, request)
					if response.Code != http.StatusOK {
						b.Fatalf("status=%d: %s", response.Code, response.Body.String())
					}
				}
			})
		})
	}
}

func newSignalBenchmark(b *testing.B, accountCount int) (*proxyHandler, time.Time) {
	b.Helper()
	dir := b.TempDir()
	usage, err := newUsageStore(filepath.Join(dir, "usage.db"), 90)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := usage.Close(); err != nil {
			b.Error(err)
		}
	})
	analytics := testDuckCosts(b, usage)
	now := time.Now().UTC()
	if _, err := analytics.db.Exec(`
		INSERT INTO legacy_cost_days
		WITH fixture AS (
			SELECT CAST(? AS DATE) - CAST(day_offset AS INTEGER) AS day,
			       printf('account-%03d', account_index) AS account_id,
			       printf('model-%d', model_index) AS model
			FROM range(?) AS accounts(account_index), range(1, 366) AS days(day_offset), range(3) AS models(model_index)
		)
		SELECT day, account_id, 'codex', model, CAST(day AS TIMESTAMP) AT TIME ZONE 'UTC',
		       100000, 0, 0, 20000, 0, 100, 0.01 FROM fixture
	`, now.Format(time.DateOnly), accountCount); err != nil {
		b.Fatal(err)
	}
	if _, err := analytics.db.Exec(`
		INSERT INTO usage_events (
			event_id, proxy_request_id, usage_sequence, attempt_number, observed_at,
			principal_id, client_credential_id, account_id, account_type, model_normalized,
			normalization_version, input_tokens, cache_read_tokens, cache_creation_tokens,
			output_tokens, reasoning_tokens, billable_tokens, api_equivalent_cost_usd,
			pricing_version, usage_completeness, source, source_grain
		)
		WITH fixture AS (
			SELECT printf('bench-%d-%d', account_index, request_index) AS event_id,
			       printf('account-%03d', account_index) AS account_id,
			       printf('model-%d', request_index % 3) AS model
			FROM range(?) AS accounts(account_index), range(100) AS requests(request_index)
		)
		SELECT event_id, event_id, 0, 0, ?, '', '', account_id, 'codex', model,
		       'v1', 1000, 0, 0, 200, 0, 1200, 0.01, 'fixture', 'complete', 'live', 'request'
		FROM fixture
	`, accountCount, now); err != nil {
		b.Fatal(err)
	}
	accounts := make([]*Account, 0, accountCount*4/5)
	for i := range accountCount {
		id := fmt.Sprintf("account-%03d", i)
		if i < accountCount*4/5 {
			accounts = append(accounts, &Account{ID: id, Type: AccountTypeCodex, PlanType: "pro"})
		}
		for week := range 6 {
			if err := usage.record(RequestUsage{
				Timestamp: now.AddDate(0, 0, -week*7), AccountID: id, AccountType: AccountTypeCodex,
				OriginID: fmt.Sprintf("origin-%03d", i), InputTokens: 1000, OutputTokens: 200, BillableTokens: 1200,
			}); err != nil {
				b.Fatal(err)
			}
		}
	}
	for hour := range 24 * 14 {
		if err := usage.record(RequestUsage{
			Timestamp: now.Add(-time.Duration(hour) * time.Hour), AccountID: "account-000", AccountType: AccountTypeCodex,
			UserID: "benchmark-user", OriginID: "origin-000", InputTokens: 1000, OutputTokens: 200, BillableTokens: 1200,
		}); err != nil {
			b.Fatal(err)
		}
	}
	if err := analytics.drain(accountCount*6 + 24*14); err != nil {
		b.Fatal(err)
	}
	return &proxyHandler{pool: newPoolState(accounts, false), store: usage, analyticsStore: analytics}, now
}
