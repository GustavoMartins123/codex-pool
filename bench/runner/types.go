package main

import (
	"encoding/json"
	"time"
)

// Scenario defines the configuration of a benchmark scenario.
type Scenario struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Method      string            `json:"method,omitempty"`
	Path        string            `json:"path,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        json.RawMessage   `json:"body,omitempty"`
	Concurrency int               `json:"concurrency,omitempty"`
	Iterations  int               `json:"iterations,omitempty"`
	Steps       []ScenarioStep    `json:"steps,omitempty"`
}

// ScenarioStep is a discrete step within a multi-turn or multi-model scenario.
type ScenarioStep struct {
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// ScenarioMetrics captures all measurements required for a benchmark scenario.
type ScenarioMetrics struct {
	TotalDurationMs            float64 `json:"total_duration_ms"`
	TTFTMs                     float64 `json:"ttft_ms"`
	TokensPerSecond            float64 `json:"tokens_per_second"`
	InputTokens                int64   `json:"input_tokens"`
	CachedInputTokens          int64   `json:"cached_input_tokens"`
	OutputTokens               int64   `json:"output_tokens"`
	ReasoningTokens            int64   `json:"reasoning_tokens"`
	Errors                     int64   `json:"errors"`
	Retries                    int64   `json:"retries"`
	Status429                  int64   `json:"status_429"`
	Status5xx                  int64   `json:"status_5xx"`
	WebSocketAbnormalClosures  int64   `json:"websocket_abnormal_closures"`
	QuotaConsumed              float64 `json:"quota_consumed"`
}

// BenchmarkReport contains full results of a benchmark execution.
type BenchmarkReport struct {
	Timestamp   time.Time                  `json:"timestamp"`
	Commit      string                     `json:"commit,omitempty"`
	Target      string                     `json:"target"`
	Scenarios   map[string]ScenarioMetrics `json:"scenarios"`
	SmokeTests  map[string]ScenarioMetrics `json:"smoke_tests"`
	Summary     ScenarioMetrics            `json:"summary"`
}

// MetricDelta describes the change between current run and baseline.
type MetricDelta struct {
	Baseline   float64 `json:"baseline"`
	Current    float64 `json:"current"`
	Delta      float64 `json:"delta"`
	DeltaPct   float64 `json:"delta_pct"`
	Regressed  bool    `json:"regressed"`
	// NoBaseline marks a metric that cannot be judged: either the scenario is
	// absent from the baseline or the recorded baseline is zero. Such metrics
	// are never "OK"; they carry no signal, and a run in mock mode produces
	// exactly that (no wall-clock timing at all).
	NoBaseline bool `json:"no_baseline"`
}

// ScenarioComparison captures differences for a single scenario.
type ScenarioComparison struct {
	Scenario string                 `json:"scenario"`
	Metrics  map[string]MetricDelta `json:"metrics"`
}

// BenchmarkComparison summarizes comparison against a baseline.
type BenchmarkComparison struct {
	Timestamp   time.Time                       `json:"timestamp"`
	Scenarios   map[string]ScenarioComparison  `json:"scenarios"`
	SmokeTests  map[string]ScenarioComparison  `json:"smoke_tests"`
	HasRegression bool                          `json:"has_regression"`
}
