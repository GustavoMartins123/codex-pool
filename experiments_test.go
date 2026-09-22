package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
