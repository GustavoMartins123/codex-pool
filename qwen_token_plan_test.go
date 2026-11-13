package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQwenPersonalCreditsUseActualSubscriptionAllowance(t *testing.T) {
	now := time.Now().UTC()
	for _, total := range []float64{10000, 25000, 100000} {
		plan := &QwenTokenPlanConfig{Mode: qwenPersonalManual, Plan: "essential", TotalCredits: total, Remaining: total * .625, CycleStart: now.Add(-24 * time.Hour), CycleEnd: now.Add(29 * 24 * time.Hour), UpdatedAt: now.Add(-time.Minute)}
		h := &proxyHandler{}
		credits, err := h.queryQwenCredits(context.Background(), plan, now)
		if err != nil || credits.Total != total || credits.Used != total*.375 || credits.Source != "manual" {
			t.Fatalf("incorrect Personal balance: %+v %v", credits, err)
		}
		usage := qwenCreditUsage(credits, now)
		if usage.PrimaryUsedPercent != .375 || usage.PrimaryWindowMinutes != 30*24*60 || !usage.PrimaryUsageReported || usage.SecondaryUsageReported {
			t.Fatalf("subscription percentage or cycle incorrect: %+v", usage)
		}
	}
	for _, remaining := range []float64{-1, 10001, math.NaN(), math.Inf(1)} {
		plan := &QwenTokenPlanConfig{Mode: qwenPersonalManual, TotalCredits: 10000, Remaining: remaining, CycleStart: now.Add(-time.Hour), CycleEnd: now.Add(time.Hour)}
		if validateQwenTokenPlan(plan, now) == nil {
			t.Fatalf("accepted invalid remaining credits %v", remaining)
		}
	}
	if policy := resetWindowPolicy(AccountTypeQwen, "token_plan_personal"); policy.Primary != ResetWindowMonthly || policy.Secondary != ResetWindowNone {
		t.Fatalf("Qwen exposed a non-monthly quota: %+v", policy)
	}
}

func qwenSeatFixture(now time.Time, total, remaining string) string {
	return fmt.Sprintf(`{"Success":true,"Data":{"Items":[{"SeatId":"other-seat","SpecType":"max","Status":"NORMAL","EquityList":[]},{"SeatId":"seat-test","SpecType":"pro","Status":"NORMAL","EquityList":[{"EquityType":"CREDITS","CycleStartTime":%d,"CycleEndTime":%d,"CycleTotalValue":%s,"CycleSurplusValue":%s}]}]}}`, now.Add(-24*time.Hour).UnixMilli(), now.Add(29*24*time.Hour).UnixMilli(), total, remaining)
}

func TestQwenTeamCreditsUseSignedSeatSpecificAPI(t *testing.T) {
	now := time.Now().UTC()
	plan := &QwenTokenPlanConfig{Mode: qwenTeamAPI, AccessKeyID: "synthetic-id", AccessKeySecret: "synthetic-secret", SecurityToken: "synthetic-sts", SeatID: "seat-test"}
	h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "modelstudio.ap-southeast-1.aliyuncs.com" || req.URL.Path != "/tokenplan/subscription/seat-detail" || req.URL.Query().Get("SeatId") != "seat-test" {
			t.Fatalf("wrong seat credit endpoint: %s", req.URL)
		}
		if !strings.HasPrefix(req.Header.Get("Authorization"), "ACS3-HMAC-SHA256 Credential=synthetic-id,") || req.Header.Get("x-acs-action") != "GetSubscriptionSeatDetails" || req.Header.Get("x-acs-security-token") != "synthetic-sts" || !strings.Contains(req.Header.Get("Authorization"), "x-acs-security-token;") {
			t.Fatal("Team credit query did not use signed Alibaba Cloud authentication")
		}
		return qwenJSONResponse(200, qwenSeatFixture(now, "25000.5", "5000.1")), nil
	}))
	credits, err := h.queryQwenCredits(context.Background(), plan, now)
	if err != nil || credits.Edition != "team" || credits.Total != 25000.5 || credits.Remaining != 5000.1 || credits.Source != "alibaba-api" {
		t.Fatalf("invalid Team credits: %+v %v", credits, err)
	}
	if math.Abs(qwenCreditUsage(credits, now).PrimaryUsedPercent-.8) > .0000001 {
		t.Fatal("credit percentage did not use the actual seat allowance")
	}
	for _, body := range []string{`{"Success":false}`, `{"Success":true}`, qwenSeatFixture(now, "0", "0"), qwenSeatFixture(now, "25000", "25001"), qwenSeatFixture(now, "25000", "null")} {
		if _, err := parseQwenTeamCredits([]byte(body), "seat-test", now); err == nil {
			t.Fatalf("accepted invalid Team credit response: %s", body)
		}
	}
	if _, err := parseQwenTeamCredits([]byte(qwenSeatFixture(now, "25000", "0")), "unknown-seat", now); err == nil {
		t.Fatal("used another seat's balance")
	}
}

func TestQwenUsageIncludesCacheAndReasoning(t *testing.T) {
	provider := NewQwenProvider(mustParse(defaultQwenBase))
	for _, body := range []string{
		`{"model":"qwen3.8-max","usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":10}}}`,
		`{"response":{"model":"qwen3.8-max","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":10}}}}`,
	} {
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatal(err)
		}
		usage := provider.ParseUsage(obj)
		if usage == nil || usage.InputTokens != 100 || usage.OutputTokens != 20 || usage.CachedInputTokens != 30 || usage.ReasoningTokens != 10 || usage.BillableTokens != 90 || usage.Model != "qwen3.8-max" {
			t.Fatalf("incorrect token accounting: %+v", usage)
		}
	}
}
