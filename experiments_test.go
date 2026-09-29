package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCanaryAssignmentIsDeterministic(t *testing.T) {
	tracker, err := newExperimentTracker(nil, ExperimentsConfig{Canary: map[string]CanaryConfig{
		"gpt-5.6-sol": {Candidate: "gpt-6-astra", Percent: 100},
	}})
	if err != nil {
		t.Fatal(err)
	}
	first := tracker.Assign("gpt-5.6-sol", "conversation-1")
	second := tracker.Assign("gpt-5.6-sol", "conversation-1")
	if first == nil || second == nil || first.Model != "gpt-6-astra" || first.Variant != "canary" || *first != *second {
		t.Fatalf("assignments = %#v %#v", first, second)
	}

	control, err := newExperimentTracker(nil, ExperimentsConfig{Canary: map[string]CanaryConfig{
		"gpt-5.6-sol": {Candidate: "gpt-6-astra", Percent: 0},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if assignment := control.Assign("gpt-5.6-sol", "conversation-1"); assignment.Model != "gpt-5.6-sol" || assignment.Variant != "control" {
		t.Fatalf("control assignment = %#v", assignment)
	}
}

func TestExperimentMetricsArePersistent(t *testing.T) {
	store := testUsageStore(t)
	tracker, err := newExperimentTracker(store.db, ExperimentsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tracker.Record("sol->astra", "canary", ExperimentObservation{
		Status: http.StatusOK, Duration: 2 * time.Second, TTFT: 100 * time.Millisecond,
		ResponseBytes: 400, ToolRequest: true,
	})
	reloaded, err := newExperimentTracker(store.db, ExperimentsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := reloaded.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
	got := metrics[0]
	if got.Requests != 1 || got.Successes != 1 || got.ToolSuccesses != 1 ||
		got.EstimatedOutputTokens != 100 || got.TotalTTFTMs != 100 ||
		got.EstimatedTokensPerSecond != 50 {
		t.Fatalf("metrics = %#v", got)
	}
}

func TestShadowBenchmarkRequiresSafeExplicitRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	safe := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	if !shadowSafeRequest(request, safe) {
		t.Fatal("safe request was rejected")
	}
	withTools := []byte(`{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"read"}],"input":"hello"}`)
	if shadowSafeRequest(request, withTools) {
		t.Fatal("tool request was accepted for shadowing")
	}
	image := []byte(`{"model":"gpt-5.6-sol","tools":[{"type":"image_generation"}],"input":"hello"}`)
	if shadowSafeRequest(request, image) {
		t.Fatal("image request was accepted for shadowing")
	}
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	if shadowSafeRequest(request, safe) {
		t.Fatal("websocket request was accepted for shadowing")
	}
}

func TestExperimentResponseWriterMeasuresTTFTAndBytes(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &experimentResponseWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusAccepted)
	_, _ = writer.Write([]byte(strings.Repeat("x", 16)))
	if writer.status != http.StatusAccepted || writer.bytes != 16 || writer.firstWrite.IsZero() {
		t.Fatalf("writer = %#v", writer)
	}
}

// P1-03 routing shadow: the shadow leg must not touch the upstream, consume
// quota, or mutate runtime — it only records the candidate's viability.
func TestRoutingShadowSendsNoUpstreamTraffic(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	store := testUsageStore(t)
	tracker, err := newExperimentTracker(store.db, ExperimentsConfig{Canary: map[string]CanaryConfig{
		"gpt-5.6-sol": {Candidate: "claude-sonnet-5", Percent: 0, Shadow: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	codexBase, _ := url.Parse("https://codex.mock")
	pool := newPoolState([]*Account{
		{ID: "codex", Type: AccountTypeCodex, AccessToken: "codex-token", PlanType: "pro"},
		{ID: "claude", Type: AccountTypeClaude, AccessToken: "claude-token", PlanType: "pro"},
	}, false)
	upstreamCalls := int32(0)
	h := &proxyHandler{
		cfg:         &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, requestTimeout: 5 * time.Second, streamTimeout: 5 * time.Second},
		pool:        pool,
		registry:    NewProviderRegistry(NewCodexProvider(codexBase, codexBase, nil), nil, nil),
		metrics:     newMetrics(),
		recent:      newRecentErrors(5),
		experiments: tracker,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&upstreamCalls, 1)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1","output":[]}`)), Request: req}, nil
		}),
	}
	body := `{"model":"gpt-5.6-sol","conversation_id":"shadow-zero-egress","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 1 {
		t.Fatalf("routing shadow must not call the upstream: calls=%d", got)
	}
	metrics, err := tracker.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	var shadow *ExperimentMetrics
	for i := range metrics {
		if metrics[i].Variant == "shadow" {
			shadow = &metrics[i]
		}
	}
	if shadow == nil {
		t.Fatalf("no shadow row recorded: %#v", metrics)
	}
	if shadow.Requests != 1 || shadow.Successes != 1 {
		t.Fatalf("shadow row = %#v", shadow)
	}
	if shadow.ResponseBytes != 0 || shadow.EstimatedOutputTokens != 0 {
		t.Fatalf("shadow row implies upstream bytes: %#v", shadow)
	}
}

func trafficShadowFixture(t *testing.T, traffic TrafficShadowConfig, poolAccounts []*Account) (*proxyHandler, *experimentTracker, *int32, *[]string) {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	store := testUsageStore(t)
	tracker, err := newExperimentTracker(store.db, ExperimentsConfig{
		Canary:  map[string]CanaryConfig{"gpt-5.6-sol": {Candidate: "gpt-5.6-sol", Percent: 0, Shadow: true}},
		Traffic: traffic,
	})
	if err != nil {
		t.Fatal(err)
	}
	codexBase, _ := url.Parse("https://codex.mock")
	pool := newPoolState(poolAccounts, false)
	calls := int32(0)
	var seenAccounts []string
	h := &proxyHandler{
		cfg:           &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, requestTimeout: 5 * time.Second, streamTimeout: 5 * time.Second},
		pool:          pool,
		registry:      NewProviderRegistry(NewCodexProvider(codexBase, codexBase, nil), nil, nil),
		metrics:       newMetrics(),
		recent:        newRecentErrors(5),
		experiments:   tracker,
		trafficShadow: newTrafficShadowRuntime(TrafficShadowConfig{}),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			seenAccounts = append(seenAccounts, req.Header.Get("ChatGPT-Account-ID"))
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1","output":[]}`)), Request: req}, nil
		}),
	}
	for i, principal := range traffic.Principals {
		if principal == "user" {
			traffic.Principals[i] = testPoolIdentity(t, h, "user")
		}
	}
	h.trafficShadow.Update(traffic)
	return h, tracker, &calls, &seenAccounts
}

func trafficShadowRequest(t *testing.T, h *proxyHandler) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"gpt-5.6-sol","conversation_id":"traffic-shadow","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user"))
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	return w
}

func trafficShadowAccounts() []*Account {
	return []*Account{
		{ID: "codex-a", Type: AccountTypeCodex, AccessToken: "token-a", AccountID: "acct_a", PlanType: "pro"},
		{ID: "codex-b", Type: AccountTypeCodex, AccessToken: "token-b", AccountID: "acct_b", PlanType: "pro"},
	}
}

func enabledTraffic() TrafficShadowConfig {
	return TrafficShadowConfig{
		Enabled:        true,
		Experiments:    []string{"gpt-5.6-sol->gpt-5.6-sol"},
		Accounts:       []string{"codex-a", "codex-b"},
		Principals:     []string{"user"},
		MaxInflight:    4,
		DailyBudget:    10,
		TimeoutSeconds: 5,
	}
}

func TestTrafficShadowOffByDefault(t *testing.T) {
	h, _, calls, _ := trafficShadowFixture(t, TrafficShadowConfig{}, trafficShadowAccounts())
	w := trafficShadowRequest(t, h)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("traffic must not run without explicit opt-in: calls=%d", got)
	}
}

func TestTrafficShadowSendsSecondCallAndSeparateMetric(t *testing.T) {
	h, tracker, calls, _ := trafficShadowFixture(t, enabledTraffic(), trafficShadowAccounts())
	w := trafficShadowRequest(t, h)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("traffic shadow must add exactly one upstream call: calls=%d", got)
	}
	for time.Now().Before(deadline) {
		metrics, _ := tracker.Metrics()
		found := false
		for _, m := range metrics {
			if m.Variant == "traffic-shadow" && m.Requests == 1 {
				found = true
			}
		}
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("traffic-shadow metric row missing")
}

func TestTrafficShadowRestrictedToAllowlistedAccounts(t *testing.T) {
	cfg := enabledTraffic()
	cfg.Accounts = []string{"codex-b"}
	accounts := trafficShadowAccounts()
	accounts[1].Usage.SecondaryUsedPercent = 0.99
	h, _, calls, _ := trafficShadowFixture(t, cfg, accounts)
	w := trafficShadowRequest(t, h)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	time.Sleep(700 * time.Millisecond)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("shadow must abort when only non-authorized accounts remain: calls=%d", got)
	}
}

func TestTrafficShadowBudgetExhausts(t *testing.T) {
	cfg := enabledTraffic()
	cfg.DailyBudget = 1
	h, _, calls, _ := trafficShadowFixture(t, cfg, trafficShadowAccounts())
	trafficShadowRequest(t, h)
	time.Sleep(700 * time.Millisecond)
	first := atomic.LoadInt32(calls)
	trafficShadowRequest(t, h)
	time.Sleep(700 * time.Millisecond)
	second := atomic.LoadInt32(calls)
	if first != 2 || second != 3 {
		t.Fatalf("daily budget must stop the second shadow leg: after-first=%d after-second=%d", first, second)
	}
}

func TestTrafficShadowPrincipalNotAllowed(t *testing.T) {
	cfg := enabledTraffic()
	cfg.Principals = []string{"someone-else"}
	h, _, calls, _ := trafficShadowFixture(t, cfg, trafficShadowAccounts())
	trafficShadowRequest(t, h)
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("principal outside the allowlist must not generate traffic: calls=%d", got)
	}
}

func TestTrafficShadowConfigNormalizes(t *testing.T) {
	r := newTrafficShadowRuntime(TrafficShadowConfig{Enabled: true})
	if r.cfg.Enabled {
		t.Fatal("config without allowlists/budget must disable itself")
	}
	r = newTrafficShadowRuntime(TrafficShadowConfig{Enabled: true, Experiments: []string{"e"}, Accounts: []string{"a"}, Principals: []string{"p"}, DailyBudget: 3})
	if r.cfg.MaxInflight != 1 || r.cfg.TimeoutSeconds != 120 {
		t.Fatalf("defaults missing: %#v", r.cfg)
	}
	if r.enabledFor("e", "p") == false || r.enabledFor("e", "q") || r.enabledFor("other", "p") {
		t.Fatal("gate semantics broken")
	}
	now := time.Now()
	if !r.begin(now) || r.begin(now) {
		t.Fatal("inflight cap of one must block a second begin")
	}
	r.end()
	if !r.begin(now) {
		t.Fatal("end must release the inflight slot")
	}
	r.end()
	r.day = ""
}
