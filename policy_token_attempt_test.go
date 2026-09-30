package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

const testTokenCeiling int64 = 1128000

func tokenBudgetAdmission(t *testing.T, budget int64) (*PassportStore, *ClientCredential, *policyAdmission) {
	t.Helper()
	p, client := testPolicyPassport(t)
	setPrincipalBudget(t, p, "policy-user", PolicyLimits{DailyTokens: budget, MonthlyTokens: budget, TokenReservation: 1})
	a, err := p.beginPolicyRequest("policy-user", client.ID, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return p, client, a
}

func principalTokenCounter(t *testing.T, p *PassportStore) policyUsageCounter {
	t.Helper()
	var counter policyUsageCounter
	if err := p.db.View(func(tx *bbolt.Tx) error {
		_, key, _ := policyUsageKeys("principal:policy-user", time.Now().UTC())
		var err error
		counter, err = readPolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return counter
}

func TestTokenCeilingRequiresEntireHeadroom(t *testing.T) {
	p, _, a := tokenBudgetAdmission(t, 100)
	_, err := a.reserveTokenAttempt(testTokenCeiling)
	requirePolicyCode(t, err, "policy_daily_tokens_exceeded")
	a.Release()
	if c := principalTokenCounter(t, p); c.ReservedTokens != 0 || c.Tokens != 0 {
		t.Fatalf("failed attempt charged tokens: %+v", c)
	}
}

func TestTokenCeilingSettlesConfirmedConsumptionAboveDeclaredReservation(t *testing.T) {
	p, _, a := tokenBudgetAdmission(t, testTokenCeiling)
	attempt, err := a.reserveTokenAttempt(testTokenCeiling)
	if err != nil {
		t.Fatal(err)
	}
	body := &policyCompletionBody{ReadCloser: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":80,"output_tokens":10}}`)), admission: a, attempt: attempt, json: true}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	a.recordAttemptUsage(90, true)
	a.Release()
	if c := principalTokenCounter(t, p); c.ReservedTokens != 0 || c.Tokens != 90 {
		t.Fatalf("settlement: %+v", c)
	}
}

func TestTokenCeilingUnknownUsageAndAccountingFailuresRemainReserved(t *testing.T) {
	for _, mode := range []string{"unknown", "storage-error", "incomplete", "spoofed-terminal"} {
		t.Run(mode, func(t *testing.T) {
			p, client, a := tokenBudgetAdmission(t, testTokenCeiling)
			attempt, err := a.reserveTokenAttempt(testTokenCeiling)
			if err != nil {
				t.Fatal(err)
			}
			payload := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":80,\"output_tokens\":10}}}\n\n"
			if mode == "incomplete" {
				payload = "data: {\"type\":\"response.created\"}\n\n"
			}
			if mode == "spoofed-terminal" {
				payload = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"event: message_stop\"}\n\n"
			}
			body := &policyCompletionBody{ReadCloser: io.NopCloser(strings.NewReader(payload)), admission: a, attempt: attempt}
			if _, err := io.ReadAll(body); err != nil {
				t.Fatal(err)
			}
			if mode != "unknown" {
				a.recordAttemptUsage(90, mode != "storage-error")
			}
			a.Release()
			if c := principalTokenCounter(t, p); c.ReservedTokens != testTokenCeiling {
				t.Fatalf("uncertain consumption released: %+v", c)
			}
			reloaded, err := newPassportStore(p.db)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reloaded.beginPolicyRequest("policy-user", client.ID, nil, time.Now().UTC())
			requirePolicyCode(t, err, "policy_daily_tokens_exceeded")
		})
	}
}

func TestTokenCeilingRetryAndConcurrentCredentialsShareHeadroom(t *testing.T) {
	p, _, first := tokenBudgetAdmission(t, testTokenCeiling+2)
	client, err := p.createClient("policy-user", "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.beginPolicyRequest("policy-user", client.ID, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for _, a := range []*policyAdmission{first, second} {
		wg.Go(func() {
			if _, err := a.reserveTokenAttempt(testTokenCeiling); err == nil {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted %d simultaneous attempts", admitted.Load())
	}
	if _, err := first.reserveTokenAttempt(testTokenCeiling); err == nil {
		t.Fatal("retry exceeded aggregate headroom")
	}
	first.Release()
	second.Release()
	if c := principalTokenCounter(t, p); c.ReservedTokens != testTokenCeiling {
		t.Fatalf("retry reservation: %+v", c)
	}
}

func TestBudgetedProxyStreamsAndRejectsInsufficientHeadroomBeforeUpstream(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-budget\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":80,\"output_tokens\":10},\"output\":[]}}\n\n")
	}))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	fx := newCodexProxyFixture(t, base, []*Account{{ID: "budget", Type: AccountTypeCodex, AccountID: "budget", AccessToken: "upstream", PlanType: "pro"}})
	h := fx.handler
	h.store = testUsageStore(t)
	h.passport = nil
	testPoolIdentity(t, h, "policy-user")
	h.aliases = newModelAliases(nil)
	h.cfg.maxAttempts = 1
	setPrincipalBudget(t, h.passport, "policy-user", PolicyLimits{DailyTokens: testTokenCeiling, TokenReservation: 1})
	request := func() int {
		r, _ := http.NewRequestWithContext(context.Background(), "POST", fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
		r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "policy-user"))
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status := request(); status != 200 {
		t.Fatalf("bounded stream: %d", status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for principalTokenCounter(t, h.passport).Tokens != 90 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c := principalTokenCounter(t, h.passport); c.Tokens != 90 || c.ReservedTokens != 0 {
		t.Fatalf("stream settlement: %+v", c)
	}
	if status := request(); status != 429 {
		t.Fatalf("insufficient headroom: %d", status)
	}
	if hits.Load() != 1 {
		t.Fatalf("blocked request reached upstream: %d", hits.Load())
	}
}

func TestTokenBoundRejectsUnknownModelsAndHostedTools(t *testing.T) {
	for _, payload := range []string{`{"model":"unknown"}`, `{"model":"gpt-5.5","n":2}`, `{"model":"gpt-5.5","background":true}`, `{"model":"gpt-5.5","tools":[{"type":"web_search"}]}`} {
		r, _ := http.NewRequest("POST", "https://example.com/responses", strings.NewReader(payload))
		if _, err := policyRequestTokenBound(r, AccountTypeCodex); err == nil {
			t.Fatalf("unbounded request accepted: %s", payload)
		}
	}
}

func TestTokenAttemptUsesItsOwnUTCAccountingWindow(t *testing.T) {
	p, client, initial := tokenBudgetAdmission(t, testTokenCeiling)
	initial.Release()
	before := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	a, err := p.beginPolicyRequest("policy-user", client.ID, nil, before)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := a.reserveTokenAttemptAt(testTokenCeiling, before.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	attempt.complete = true
	attempt.expected = 90
	a.recordAttemptUsage(90, true)
	a.Release()
	if err := p.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		_, oldDay, oldMonth := policyUsageKeys("principal:policy-user", before)
		_, newDay, newMonth := policyUsageKeys("principal:policy-user", before.Add(time.Second))
		for _, key := range []string{oldDay, oldMonth} {
			c, err := readPolicyCounter(b, key)
			if err != nil {
				return err
			}
			if c.Tokens != 0 || c.ReservedTokens != 0 {
				t.Fatalf("old window charged: %+v", c)
			}
		}
		for _, key := range []string{newDay, newMonth} {
			c, err := readPolicyCounter(b, key)
			if err != nil {
				return err
			}
			if c.Tokens != 90 || c.ReservedTokens != 0 {
				t.Fatalf("new window incorrect: %+v", c)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
