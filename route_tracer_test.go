package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRouteTraceRecordingAndRedaction(t *testing.T) {
	store := newRouteTraceStore(10)

	breakdown := &ScoreBreakdownView{
		Score:                0.82,
		QuotaScore:           0.85,
		ResetUrgency:         1.0,
		InflightPenalty:      0.02,
		LatencyScore:         1.1,
		HealthScore:          1.0,
		RecentFailurePenalty: 0.0,
		BaseWindow:           "7d",
		PrimaryUsed:          0.15,
		SecondaryUsed:        0.15,
		CreditBonus:          1.0,
	}

	trace := &RouteTrace{
		RequestID: "req_test_123",
		Timestamp: time.Now().UTC(),
		Policy:    "balanced",
		Selected: RouteTarget{
			Provider: "codex",
			Model:    "gpt-5.6-sol",
		},
		Score:   0.82,
		Reasons: []string{"conversation_pin", "healthy", "weekly_headroom"},
		Alternatives: []RouteAlternative{
			{
				Provider:       "codex",
				Model:          "gpt-5.6-sol",
				Score:          0.75,
				Reasons:        []string{"healthy", "weekly_headroom"},
				AccountID:      "sensitive_acc_2",
				ScoreBreakdown: breakdown,
			},
		},
		AccountID:      "sensitive_acc_1",
		ScoreBreakdown: breakdown,
		ClientIP:       "192.168.1.50",
		UserID:         "user_vip",
		Attempts:       1,
		DurationMs:     45.2,
		StatusCode:     200,
	}

	store.Record(trace)

	// Verify retrieval
	retrieved, ok := store.Get("req_test_123")
	if !ok || retrieved == nil {
		t.Fatalf("expected to retrieve trace req_test_123")
	}

	// Verify operator view includes sensitive fields
	if retrieved.AccountID != "sensitive_acc_1" {
		t.Errorf("operator view should have AccountID, got %q", retrieved.AccountID)
	}
	if retrieved.ScoreBreakdown == nil || retrieved.ScoreBreakdown.QuotaScore != 0.85 {
		t.Errorf("operator view should have ScoreBreakdown, got %+v", retrieved.ScoreBreakdown)
	}
	if retrieved.Alternatives[0].AccountID != "sensitive_acc_2" {
		t.Errorf("operator view should have alternative AccountID, got %q", retrieved.Alternatives[0].AccountID)
	}

	// Verify client/member sanitized view strips sensitive fields
	sanitized := store.SanitizeForClient(retrieved)
	if sanitized.AccountID != "" {
		t.Errorf("client view must NOT include AccountID, got %q", sanitized.AccountID)
	}
	if sanitized.ScoreBreakdown != nil {
		t.Errorf("client view must NOT include ScoreBreakdown, got %+v", sanitized.ScoreBreakdown)
	}
	if sanitized.ClientIP != "" {
		t.Errorf("client view must NOT include ClientIP, got %q", sanitized.ClientIP)
	}
	if sanitized.UserID != "" {
		t.Errorf("client view must NOT include UserID, got %q", sanitized.UserID)
	}
	if sanitized.Alternatives[0].AccountID != "" {
		t.Errorf("client view must NOT include alternative AccountID, got %q", sanitized.Alternatives[0].AccountID)
	}
	if sanitized.Alternatives[0].ScoreBreakdown != nil {
		t.Errorf("client view must NOT include alternative ScoreBreakdown, got %+v", sanitized.Alternatives[0].ScoreBreakdown)
	}

	// Public fields must remain intact
	if sanitized.RequestID != "req_test_123" || sanitized.Policy != "balanced" || sanitized.Selected.Model != "gpt-5.6-sol" {
		t.Errorf("sanitized public fields corrupted: %+v", sanitized)
	}
}

func TestRouteTraceEndpointAuth(t *testing.T) {
	h := &proxyHandler{
		cfg:         &config{adminToken: "super-secret-admin"},
		routeTraces: newRouteTraceStore(10),
	}

	trace := &RouteTrace{
		RequestID: "req_endpoint_abc",
		Timestamp: time.Now().UTC(),
		Policy:    "balanced",
		Selected: RouteTarget{
			Provider: "codex",
			Model:    "gpt-5.6-sol",
		},
		Score:          0.85,
		Reasons:        []string{"healthy", "weekly_headroom"},
		AccountID:      "operator_secret_account",
		ScoreBreakdown: &ScoreBreakdownView{QuotaScore: 0.90, Score: 0.85},
	}
	h.getRouteTraces().Record(trace)

	// 1. Unauthenticated client request
	req1 := httptest.NewRequest("GET", "/api/pool/routes/req_endpoint_abc", nil)
	rec1 := httptest.NewRecorder()
	h.handleRouteTrace(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for client trace, got %d", rec1.Code)
	}
	var clientResp map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &clientResp); err != nil {
		t.Fatal(err)
	}
	if clientResp["account_id"] != nil {
		t.Errorf("client response must not contain account_id, got %v", clientResp["account_id"])
	}
	if clientResp["score_breakdown"] != nil {
		t.Errorf("client response must not contain score_breakdown, got %v", clientResp["score_breakdown"])
	}
	if clientResp["request_id"] != "req_endpoint_abc" {
		t.Errorf("client response request_id mismatch: %v", clientResp["request_id"])
	}

	// 2. Operator request with X-Admin-Token
	req2 := httptest.NewRequest("GET", "/api/pool/routes/req_endpoint_abc", nil)
	req2.Header.Set("X-Admin-Token", "super-secret-admin")
	rec2 := httptest.NewRecorder()
	h.handleRouteTrace(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for operator trace, got %d", rec2.Code)
	}
	var opResp map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &opResp); err != nil {
		t.Fatal(err)
	}
	if opResp["account_id"] != "operator_secret_account" {
		t.Errorf("operator response must contain account_id, got %v", opResp["account_id"])
	}
	if opResp["score_breakdown"] == nil {
		t.Errorf("operator response must contain score_breakdown")
	}

	// 3. Not found request
	req3 := httptest.NewRequest("GET", "/api/pool/routes/non_existent_id", nil)
	rec3 := httptest.NewRecorder()
	h.handleRouteTrace(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown trace, got %d", rec3.Code)
	}
}

func TestRouteTraceHeaders(t *testing.T) {
	headers := make(http.Header)
	setPoolRouteHeaders(headers, "codex", "gpt-5.6-sol", "balanced", "weekly-headroom", 1, "req_123")

	if headers.Get("X-Pool-Provider") != "codex" {
		t.Errorf("X-Pool-Provider = %q, want codex", headers.Get("X-Pool-Provider"))
	}
	if headers.Get("X-Pool-Model") != "gpt-5.6-sol" {
		t.Errorf("X-Pool-Model = %q, want gpt-5.6-sol", headers.Get("X-Pool-Model"))
	}
	if headers.Get("X-Pool-Route") != "balanced" {
		t.Errorf("X-Pool-Route = %q, want balanced", headers.Get("X-Pool-Route"))
	}
	if headers.Get("X-Pool-Attempt") != "1" {
		t.Errorf("X-Pool-Attempt = %q, want 1", headers.Get("X-Pool-Attempt"))
	}
	if headers.Get("X-Pool-Reason") != "weekly-headroom" {
		t.Errorf("X-Pool-Reason = %q, want weekly-headroom", headers.Get("X-Pool-Reason"))
	}
	if headers.Get("X-Pool-Request-Id") != "req_123" {
		t.Errorf("X-Pool-Request-Id = %q, want req_123", headers.Get("X-Pool-Request-Id"))
	}
}

func TestPerformanceMetricsTracking(t *testing.T) {
	m := newMetrics()

	// Record HTTP request
	m.recordPerformance("codex", "gpt-5.4", 250.0, 100.0, 30.0, 45.0, 200, 0, false)
	m.inc("200", "acc1")

	// Record retried request
	m.recordPerformance("antigravity", "gemini-3.8-flash-high", 400.0, 150.0, 50.0, 60.0, 200, 1, false)
	m.inc("200", "acc2")

	// Record 429
	m.recordPerformance("codex", "gpt-5.4", 80.0, 80.0, 20.0, 0, 429, 0, false)
	m.inc("429", "acc1")

	// Record 500 with stream interruption
	m.recordPerformance("claude", "claude-sonnet-4-5", 500.0, 200.0, 40.0, 10.0, 500, 0, true)
	m.inc("500", "acc3")

	summary := m.performanceSummary(nil)

	if summary.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", summary.RetryCount)
	}
	if summary.StreamInterruptions != 1 {
		t.Errorf("StreamInterruptions = %d, want 1", summary.StreamInterruptions)
	}
	if summary.SuccessRate != 0.5 { // 2 out of 4 requests were 200
		t.Errorf("SuccessRate = %v, want 0.5", summary.SuccessRate)
	}
	if summary.RateLimit429Rate != 0.25 { // 1 out of 4 was 429
		t.Errorf("RateLimit429Rate = %v, want 0.25", summary.RateLimit429Rate)
	}
	if summary.ServerError5xxRate != 0.25 { // 1 out of 4 was 500
		t.Errorf("ServerError5xxRate = %v, want 0.25", summary.ServerError5xxRate)
	}
	if summary.TTFTMsAvg < 100.0 || summary.TTFTMsAvg > 200.0 {
		t.Errorf("TTFTMsAvg = %v, expected between 100 and 200", summary.TTFTMsAvg)
	}
}

func BenchmarkRouteTraceStoreOverhead(b *testing.B) {
	store := newRouteTraceStore(2048)
	trace := &RouteTrace{
		RequestID: "req_bench",
		Timestamp: time.Now(),
		Policy:    "balanced",
		Selected:  RouteTarget{Provider: "codex", Model: "gpt-5.4"},
		Score:     0.9,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Record(trace)
		_, _ = store.Get("req_bench")
	}
}
