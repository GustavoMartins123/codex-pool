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
				{"account_daily", func() error { _, err := h.analyticsStore.getAllAccountDailyCosts(); return err }},
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
	analytics, err := newAnalyticsStore(filepath.Join(dir, "analytics.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := analytics.Close(); err != nil {
			b.Error(err)
		}
	})
	usage, err := newUsageStore(filepath.Join(dir, "usage.db"), 90)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := usage.Close(); err != nil {
			b.Error(err)
		}
	})
	now := time.Now().UTC()
	tx, err := analytics.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	daily, err := tx.Prepare(`INSERT INTO daily_costs
		(date, account_id, account_type, model, input_tokens, output_tokens, request_count, cost_usd)
		VALUES (?, ?, ?, ?, 100000, 20000, 100, 0.01)`)
	if err != nil {
		b.Fatal(err)
	}
	defer daily.Close()
	live, err := tx.Prepare(`INSERT INTO request_costs
		(timestamp, account_id, account_type, model, input_tokens, output_tokens, cost_usd)
		VALUES (?, ?, ?, ?, 1000, 200, 0.01)`)
	if err != nil {
		b.Fatal(err)
	}
	defer live.Close()
	accounts := make([]*Account, 0, accountCount*4/5)
	for i := range accountCount {
		id := fmt.Sprintf("account-%03d", i)
		if i < accountCount*4/5 {
			accounts = append(accounts, &Account{ID: id, Type: AccountTypeCodex, PlanType: "pro"})
		}
		for day := 1; day <= 365; day++ {
			date := now.AddDate(0, 0, -day).Format("2006-01-02")
			for model := range 3 {
				if _, err := daily.Exec(date, id, string(AccountTypeCodex), fmt.Sprintf("model-%d", model)); err != nil {
					b.Fatal(err)
				}
			}
		}
		for request := range 100 {
			if _, err := live.Exec(now.Format(time.RFC3339), id, string(AccountTypeCodex), fmt.Sprintf("model-%d", request%3)); err != nil {
				b.Fatal(err)
			}
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
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return &proxyHandler{pool: newPoolState(accounts, false), store: usage, analyticsStore: analytics}, now
}
