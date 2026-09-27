package main

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderSmokeTests(t *testing.T) {
	providers := []struct {
		name     string
		model    string
		endpoint string
	}{
		{"codex", "gpt-5.4", "/v1/chat/completions"},
		{"claude", "claude-sonnet-4-5", "/v1/chat/completions"},
		{"gemini", "gemini-2.5-pro", "/v1/chat/completions"},
		{"antigravity", "antigravity/gemini-3.8-flash-high", "/v1/chat/completions"},
		{"kimi", "kimi-k2", "/v1/chat/completions"},
		{"minimax", "minimax-text-01", "/v1/chat/completions"},
		{"zai", "zai-glm-4", "/v1/chat/completions"},
		{"xiaomi", "mimo-v1", "/v1/chat/completions"},
		{"grok", "grok-3", "/v1/chat/completions"},
		{"adverserial", "adverserial-deep-1", "/v1/chat/completions"},
		{"opencode_go", "opencode-go", "/v1/chat/completions"},
	}

	for _, p := range providers {
		t.Run(p.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Pool-Provider", p.name)
				w.Header().Set("X-Pool-Model", p.model)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
			})

			exec := ExecutionContext{
				BaseURL: "http://mock-proxy.local",
				HTTPClient: &http.Client{
					Transport: &handlerTransport{handler: handler},
					Timeout:   5 * time.Second,
				},
			}

			body := []byte(`{"model":"` + p.model + `","messages":[{"role":"user","content":"smoke test"}]}`)
			metrics, err := ExecuteSingleRequest(context.Background(), exec, "POST", p.endpoint, nil, body)
			if err != nil {
				t.Fatalf("provider %s smoke test error: %v", p.name, err)
			}
			if metrics.Errors > 0 {
				t.Fatalf("provider %s smoke test failed with %d errors", p.name, metrics.Errors)
			}
			if metrics.InputTokens != 5 || metrics.OutputTokens != 2 {
				t.Fatalf("provider %s smoke test tokens mismatch: got in=%d out=%d, want in=5 out=2",
					p.name, metrics.InputTokens, metrics.OutputTokens)
			}
		})
	}
}

func TestBenchmarkRunnerAndBaselineComparison(t *testing.T) {
	handler := mockHandler()
	scenariosDir, _ := filepath.Abs("../scenarios")
	exec := ExecutionContext{
		BaseURL: "http://mock-proxy.local",
		HTTPClient: &http.Client{
			Transport: &handlerTransport{handler: handler},
			Timeout:   10 * time.Second,
		},
	}

	runner := NewRunner(exec, scenariosDir, t.TempDir(), t.TempDir())
	ctx := context.Background()

	report, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("runner.Run() failed: %v", err)
	}

	if len(report.Scenarios) < 11 {
		t.Fatalf("expected at least 11 scenarios, got %d", len(report.Scenarios))
	}

	// Verify required scenarios are present
	requiredScenarios := []string{
		"simple_request",
		"large_request",
		"streaming",
		"non_streaming",
		"tools",
		"web_search",
		"image_input",
		"model_switch",
		"provider_switch",
		"long_conversation",
		"concurrency",
	}

	for _, name := range requiredScenarios {
		sc, ok := report.Scenarios[name]
		if !ok {
			t.Errorf("missing scenario: %s", name)
			continue
		}
		if sc.Errors > 0 {
			t.Errorf("scenario %s had errors: %d", name, sc.Errors)
		}
	}
}

// TestCommittedBaselineIsLoadableAndComplete guards the versioned baseline
// itself. The previous version of this test saved the fresh report into a temp
// dir and compared it against itself, which can never report a regression.
func TestCommittedBaselineIsLoadableAndComplete(t *testing.T) {
	runner := NewRunner(ExecutionContext{}, "../scenarios", "../baselines", t.TempDir())
	baseline, err := runner.LoadBaseline()
	if err != nil {
		t.Fatalf("committed baseline must load: %v", err)
	}
	required := []string{
		"simple_request", "large_request", "streaming", "non_streaming",
		"tools", "web_search", "image_input", "model_switch",
		"provider_switch", "long_conversation", "concurrency",
	}
	for _, name := range required {
		if _, ok := baseline.Scenarios[name]; !ok {
			t.Errorf("committed baseline is missing scenario %q", name)
		}
	}

	// A fresh report must be comparable against the committed baseline and
	// every scenario must produce the four compared metrics.
	report := &BenchmarkReport{Target: "mock", Scenarios: map[string]ScenarioMetrics{}}
	for _, name := range required {
		report.Scenarios[name] = ScenarioMetrics{TotalDurationMs: 1, TTFTMs: 1, TokensPerSecond: 1}
	}
	comp := runner.CompareAgainstBaseline(report, baseline)
	if len(comp.Scenarios) != len(required) {
		t.Fatalf("comparison covered %d scenarios, want %d", len(comp.Scenarios), len(required))
	}
	for name, sc := range comp.Scenarios {
		for _, metric := range []string{"total_duration_ms", "ttft_ms", "tokens_per_second", "errors"} {
			if _, ok := sc.Metrics[metric]; !ok {
				t.Errorf("%s: comparison missing metric %q", name, metric)
			}
		}
	}
}

// TestCompareAgainstBaselineDetectsRegression is the actual gate: a slower
// report than the baseline must be flagged.
func TestCompareAgainstBaselineDetectsRegression(t *testing.T) {
	baseline := &BenchmarkReport{Scenarios: map[string]ScenarioMetrics{
		"simple_request": {TotalDurationMs: 100, TTFTMs: 50, TokensPerSecond: 80},
	}}
	slower := &BenchmarkReport{Scenarios: map[string]ScenarioMetrics{
		"simple_request": {TotalDurationMs: 200, TTFTMs: 50, TokensPerSecond: 40},
	}}
	faster := &BenchmarkReport{Scenarios: map[string]ScenarioMetrics{
		"simple_request": {TotalDurationMs: 50, TTFTMs: 25, TokensPerSecond: 160},
	}}

	runner := NewRunner(ExecutionContext{}, t.TempDir(), t.TempDir(), t.TempDir())

	regressed := runner.CompareAgainstBaseline(slower, baseline)
	if !regressed.HasRegression {
		t.Fatal("doubling latency and halving throughput must be reported as a regression")
	}
	if !regressed.Scenarios["simple_request"].Metrics["total_duration_ms"].Regressed {
		t.Error("total_duration_ms regression not flagged")
	}
	if !regressed.Scenarios["simple_request"].Metrics["tokens_per_second"].Regressed {
		t.Error("tokens_per_second regression not flagged")
	}
	if regressed.Scenarios["simple_request"].Metrics["ttft_ms"].Regressed {
		t.Error("unchanged ttft must not be flagged as a regression")
	}

	improved := runner.CompareAgainstBaseline(faster, baseline)
	if improved.HasRegression {
		t.Error("a faster report must not be reported as a regression")
	}
}

// TestComparisonWithoutUsableBaselineIsNotReportedAsSuccess is the guard
// against a zero-valued (mock) baseline silently passing everything.
func TestComparisonWithoutUsableBaselineIsNotReportedAsSuccess(t *testing.T) {
	zero := &BenchmarkReport{Scenarios: map[string]ScenarioMetrics{
		"simple_request": {TotalDurationMs: 0, TTFTMs: 0, TokensPerSecond: 0},
	}}
	measured := &BenchmarkReport{Scenarios: map[string]ScenarioMetrics{
		"simple_request": {TotalDurationMs: 900, TTFTMs: 400, TokensPerSecond: 10},
	}}

	runner := NewRunner(ExecutionContext{}, t.TempDir(), t.TempDir(), t.TempDir())
	comp := runner.CompareAgainstBaseline(measured, zero)

	delta := comp.Scenarios["simple_request"].Metrics["total_duration_ms"]
	if !delta.NoBaseline {
		t.Error("a zero baseline must be marked as unjudgeable, not as passing")
	}
	if delta.Regressed {
		t.Error("an unjudgeable metric must not claim a regression verdict")
	}

	formatted := FormatComparison(comp)
	if strings.Contains(formatted, "SUCCESS: All scenarios within baseline") {
		t.Errorf("a run with no usable baseline must not print SUCCESS:\n%s", formatted)
	}
	if !strings.Contains(formatted, "NO BASELINE") {
		t.Errorf("comparison must flag the missing baseline:\n%s", formatted)
	}

	scenariosMissing := &BenchmarkReport{Scenarios: map[string]ScenarioMetrics{}}
	comp = runner.CompareAgainstBaseline(measured, scenariosMissing)
	if !hasUnjudgeable(comp) {
		t.Error("a scenario absent from the baseline must be unjudgeable")
	}
	if !strings.Contains(FormatComparison(comp), "REGRESSION") &&
		!strings.Contains(FormatComparison(comp), "NO BASELINE") {
		t.Error("comparison output must never silently claim success")
	}
}
