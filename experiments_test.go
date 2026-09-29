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
