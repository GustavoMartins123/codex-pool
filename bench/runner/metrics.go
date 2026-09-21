package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ExecutionContext wraps options for a scenario execution.
type ExecutionContext struct {
	BaseURL    string
	AuthToken  string
	HTTPClient *http.Client
}

// ExecuteSingleRequest runs an HTTP request, measures all required metrics, and parses token counts.
func ExecuteSingleRequest(ctx context.Context, exec ExecutionContext, method, path string, headers map[string]string, body []byte) (ScenarioMetrics, error) {
	if exec.HTTPClient == nil {
		exec.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}

	targetURL := strings.TrimRight(exec.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(body))
	if err != nil {
		return ScenarioMetrics{Errors: 1}, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if exec.AuthToken != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+exec.AuthToken)
	}

	metrics := ScenarioMetrics{}
	start := time.Now()

	resp, err := exec.HTTPClient.Do(req)
	if err != nil {
		metrics.TotalDurationMs = float64(time.Since(start).Milliseconds())
		metrics.Errors = 1
		return metrics, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		metrics.Status429 = 1
		metrics.Errors = 1
	} else if resp.StatusCode >= 500 {
		metrics.Status5xx = 1
		metrics.Errors = 1
	} else if resp.StatusCode >= 400 {
		metrics.Errors = 1
	}

	// Check retry header if proxy sent attempts
	if attemptStr := resp.Header.Get("X-Pool-Attempt"); attemptStr != "" {
		if attempt, err := strconv.Atoi(attemptStr); err == nil && attempt > 1 {
			metrics.Retries = int64(attempt - 1)
		}
	}

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	var firstTokenTime time.Time
	var outputTokensCount int64

	if isSSE {
		scanner := bufio.NewScanner(resp.Body)
		// Large buffer for SSE chunks
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "data:") {
				continue
			}
			if firstTokenTime.IsZero() {
				firstTokenTime = time.Now()
			}

			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}

			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				// Parse delta tokens / content
				if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]any); ok {
						if delta, ok := choice["delta"].(map[string]any); ok {
							if content, ok := delta["content"].(string); ok && len(content) > 0 {
								outputTokensCount++
							}
						}
					}
				}
				// Parse usage if emitted in SSE stream
				if usage, ok := chunk["usage"].(map[string]any); ok {
					parseUsageIntoMetrics(usage, &metrics)
				}
			}
		}

		if err := scanner.Err(); err != nil {
			metrics.Errors++
		}
	} else {
		// Non-streaming response
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			metrics.Errors++
		}
		if firstTokenTime.IsZero() {
			firstTokenTime = time.Now()
		}

		var obj map[string]any
		if json.Unmarshal(bodyBytes, &obj) == nil {
			if usage, ok := obj["usage"].(map[string]any); ok {
				parseUsageIntoMetrics(usage, &metrics)
			}
		}
	}

	metrics.TotalDurationMs = float64(time.Since(start).Milliseconds())
	if !firstTokenTime.IsZero() {
		metrics.TTFTMs = float64(firstTokenTime.Sub(start).Milliseconds())
	} else {
		metrics.TTFTMs = metrics.TotalDurationMs
	}

	if metrics.OutputTokens == 0 && outputTokensCount > 0 {
		metrics.OutputTokens = outputTokensCount
	}

	if metrics.TotalDurationMs > 0 && metrics.OutputTokens > 0 {
		metrics.TokensPerSecond = float64(metrics.OutputTokens) / (metrics.TotalDurationMs / 1000.0)
	}

	// Quota consumed estimation (billable tokens / primary window fraction)
	if metrics.OutputTokens > 0 || metrics.InputTokens > 0 {
		metrics.QuotaConsumed = float64(metrics.InputTokens-metrics.CachedInputTokens+metrics.OutputTokens) / 1000.0
	}

	return metrics, nil
}

func parseUsageIntoMetrics(usage map[string]any, metrics *ScenarioMetrics) {
	if v, ok := usage["prompt_tokens"].(float64); ok {
		metrics.InputTokens = int64(v)
	} else if v, ok := usage["input_tokens"].(float64); ok {
		metrics.InputTokens = int64(v)
	}

	if v, ok := usage["completion_tokens"].(float64); ok {
		metrics.OutputTokens = int64(v)
	} else if v, ok := usage["output_tokens"].(float64); ok {
		metrics.OutputTokens = int64(v)
	}

	// Prompt cache read tokens
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := details["cached_tokens"].(float64); ok {
			metrics.CachedInputTokens = int64(v)
		}
	} else if v, ok := usage["cache_read_input_tokens"].(float64); ok {
		metrics.CachedInputTokens = int64(v)
	}

	// Reasoning tokens
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if v, ok := details["reasoning_tokens"].(float64); ok {
			metrics.ReasoningTokens = int64(v)
		}
	}
}

// AccumulateMetrics sums two scenario metric structs.
func AccumulateMetrics(dst *ScenarioMetrics, src ScenarioMetrics) {
	dst.TotalDurationMs += src.TotalDurationMs
	if dst.TTFTMs == 0 || (src.TTFTMs > 0 && src.TTFTMs < dst.TTFTMs) {
		dst.TTFTMs = src.TTFTMs
	}
	dst.InputTokens += src.InputTokens
	dst.CachedInputTokens += src.CachedInputTokens
	dst.OutputTokens += src.OutputTokens
	dst.ReasoningTokens += src.ReasoningTokens
	dst.Errors += src.Errors
	dst.Retries += src.Retries
	dst.Status429 += src.Status429
	dst.Status5xx += src.Status5xx
	dst.WebSocketAbnormalClosures += src.WebSocketAbnormalClosures
	dst.QuotaConsumed += src.QuotaConsumed
}

// FinalizeAverages averages rates and durations across iterations.
func FinalizeAverages(m *ScenarioMetrics, count int) {
	if count <= 0 {
		return
	}
	n := float64(count)
	m.TotalDurationMs = m.TotalDurationMs / n
	m.TTFTMs = m.TTFTMs / n
	if m.TotalDurationMs > 0 && m.OutputTokens > 0 {
		m.TokensPerSecond = float64(m.OutputTokens) / (m.TotalDurationMs / 1000.0)
	}
}
