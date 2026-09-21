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

	// Save baseline and reload
	if err := runner.SaveBaseline(report); err != nil {
		t.Fatalf("SaveBaseline failed: %v", err)
	}

	baseline, err := runner.LoadBaseline()
	if err != nil {
		t.Fatalf("LoadBaseline failed: %v", err)
	}

	comp := runner.CompareAgainstBaseline(report, baseline)
	if comp.HasRegression {
		t.Errorf("comparison against identical baseline reported regressions")
	}

	formatted := FormatComparison(comp)
	if !strings.Contains(formatted, "BENCHMARK COMPARISON REPORT") {
		t.Errorf("unexpected comparison format output: %s", formatted)
	}
}
