package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"
)

type handlerTransport struct {
	handler http.Handler
}

func (t *handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	return resp, nil
}

func main() {
	target := flag.String("target", "http://127.0.0.1:8989", "Base URL of codex-pool proxy")
	token := flag.String("token", os.Getenv("POOL_TOKEN"), "Pool authorization token")
	scenarioDir := flag.String("scenarios", "bench/scenarios", "Directory containing scenario JSON files")
	baselineDir := flag.String("baseline", "bench/baselines", "Directory containing baseline JSON file")
	reportDir := flag.String("report", "bench/reports", "Directory to write run reports")
	saveBaseline := flag.Bool("save-baseline", false, "Overwrite baseline.json with current run results")
	mock := flag.Bool("mock", false, "Run against an in-process mock server instead of a live proxy")
	flag.Parse()

	baseURL := *target
	var client *http.Client = &http.Client{Timeout: 60 * time.Second}

	if *mock {
		handler := mockHandler()
		baseURL = "http://mock-proxy.local"
		client = &http.Client{
			Transport: &handlerTransport{handler: handler},
			Timeout:   60 * time.Second,
		}
		log.Printf("[bench] Running in mock mode against %s", baseURL)
	}

	exec := ExecutionContext{
		BaseURL:    baseURL,
		AuthToken:  *token,
		HTTPClient: client,
	}

	r := NewRunner(exec, *scenarioDir, *baselineDir, *reportDir)
	ctx := context.Background()

	log.Printf("[bench] Starting benchmark run against %s...", baseURL)
	report, err := r.Run(ctx)
	if err != nil {
		log.Fatalf("[bench] Benchmark run failed: %v", err)
	}

	log.Printf("[bench] Successfully executed %d scenarios", len(report.Scenarios))

	// Compare with baseline if available
	baseline, err := r.LoadBaseline()
	if err == nil {
		comp := r.CompareAgainstBaseline(report, baseline)
		fmt.Println(FormatComparison(comp))
	} else {
		log.Printf("[bench] No baseline found at %s: %v", *baselineDir, err)
	}

	// Save baseline if requested
	if *saveBaseline {
		if err := r.SaveBaseline(report); err != nil {
			log.Fatalf("[bench] Failed to save baseline: %v", err)
		}
		log.Printf("[bench] Baseline saved to %s/baseline.json", *baselineDir)
	}

	// Save report
	reportPath, err := r.SaveReport(report)
	if err != nil {
		log.Printf("[bench] Warning: Failed to save report: %v", err)
	} else {
		log.Printf("[bench] Report saved to %s", reportPath)
	}
}

// mockHandler returns an http.Handler that simulates proxy responses for benchmarking.
func mockHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Pool-Provider", "codex")
		w.Header().Set("X-Pool-Model", "gpt-5.4")
		w.Header().Set("X-Pool-Route", "balanced")
		w.Header().Set("X-Pool-Attempt", "1")
		w.Header().Set("X-Pool-Reason", "weekly-headroom")

		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || strings.Contains(r.URL.RawQuery, "stream=true") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world!\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		resp := `{
			"id": "chatcmpl-mock",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-5.4",
			"choices": [
				{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "Benchmark mock response content."
					},
					"finish_reason": "stop"
				}
			],
			"usage": {
				"prompt_tokens": 15,
				"completion_tokens": 8,
				"total_tokens": 23,
				"prompt_tokens_details": {
					"cached_tokens": 5
				},
				"completion_tokens_details": {
					"reasoning_tokens": 2
				}
			}
		}`
		_, _ = w.Write([]byte(resp))
	})
}
