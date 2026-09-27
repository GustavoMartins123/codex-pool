package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Runner manages the execution and reporting of the benchmark suite.
type Runner struct {
	Exec        ExecutionContext
	ScenarioDir string
	BaselineDir string
	ReportDir   string
}

// NewRunner creates an initialized benchmark runner.
func NewRunner(exec ExecutionContext, scenarioDir, baselineDir, reportDir string) *Runner {
	if scenarioDir == "" {
		scenarioDir = "bench/scenarios"
	}
	if baselineDir == "" {
		baselineDir = "bench/baselines"
	}
	if reportDir == "" {
		reportDir = "bench/reports"
	}
	return &Runner{
		Exec:        exec,
		ScenarioDir: scenarioDir,
		BaselineDir: baselineDir,
		ReportDir:   reportDir,
	}
}

// Run executes all loaded scenarios and compiles a report.
func (r *Runner) Run(ctx context.Context) (*BenchmarkReport, error) {
	scenarios, err := LoadScenarios(r.ScenarioDir)
	if err != nil {
		return nil, fmt.Errorf("load scenarios: %w", err)
	}

	report := &BenchmarkReport{
		Timestamp:  time.Now().UTC(),
		Target:     r.Exec.BaseURL,
		Scenarios:  make(map[string]ScenarioMetrics),
		SmokeTests: make(map[string]ScenarioMetrics),
	}

	for _, sc := range scenarios {
		metrics, err := ExecuteScenario(ctx, r.Exec, sc)
		if err != nil {
			metrics.Errors++
		}
		report.Scenarios[sc.Name] = metrics
		AccumulateMetrics(&report.Summary, metrics)
	}

	if len(scenarios) > 0 {
		FinalizeAverages(&report.Summary, len(scenarios))
	}

	return report, nil
}

// LoadBaseline reads the baseline report from disk.
func (r *Runner) LoadBaseline() (*BenchmarkReport, error) {
	path := filepath.Join(r.BaselineDir, "baseline.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baseline %q: %w", path, err)
	}
	var baseline BenchmarkReport
	if err := json.Unmarshal(data, &baseline); err != nil {
		return nil, fmt.Errorf("parse baseline %q: %w", path, err)
	}
	return &baseline, nil
}

// SaveBaseline writes the current report as the persistent baseline.
func (r *Runner) SaveBaseline(report *BenchmarkReport) error {
	if err := os.MkdirAll(r.BaselineDir, 0755); err != nil {
		return err
	}
	path := filepath.Join(r.BaselineDir, "baseline.json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// SaveReport writes a timestamped report to the reports directory.
func (r *Runner) SaveReport(report *BenchmarkReport) (string, error) {
	if err := os.MkdirAll(r.ReportDir, 0755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("report_%s.json", report.Timestamp.Format("20060102_150405"))
	path := filepath.Join(r.ReportDir, filename)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0644)
}

// CompareAgainstBaseline compares the current report with the baseline.
func (r *Runner) CompareAgainstBaseline(current, baseline *BenchmarkReport) BenchmarkComparison {
	comp := BenchmarkComparison{
		Timestamp:  time.Now().UTC(),
		Scenarios:  make(map[string]ScenarioComparison),
		SmokeTests: make(map[string]ScenarioComparison),
	}

	for name, curMetrics := range current.Scenarios {
		baseMetrics, hasBase := baseline.Scenarios[name]
		scComp := ScenarioComparison{
			Scenario: name,
			Metrics:  make(map[string]MetricDelta),
		}

		// Check total duration
		scComp.Metrics["total_duration_ms"] = computeDelta(baseMetrics.TotalDurationMs, curMetrics.TotalDurationMs, hasBase, true)
		// Check TTFT
		scComp.Metrics["ttft_ms"] = computeDelta(baseMetrics.TTFTMs, curMetrics.TTFTMs, hasBase, true)
		// Check tokens per second (higher is better)
		scComp.Metrics["tokens_per_second"] = computeDelta(baseMetrics.TokensPerSecond, curMetrics.TokensPerSecond, hasBase, false)
		// Check errors
		scComp.Metrics["errors"] = computeDelta(float64(baseMetrics.Errors), float64(curMetrics.Errors), hasBase, true)

		for _, delta := range scComp.Metrics {
			if delta.Regressed {
				comp.HasRegression = true
			}
		}

		comp.Scenarios[name] = scComp
	}

	return comp
}

func computeDelta(base, cur float64, hasBase bool, lowerIsBetter bool) MetricDelta {
	if !hasBase || base == 0 {
		// No usable baseline: report the metric as unjudgeable instead of
		// silently passing it. A zero baseline means the baseline was
		// recorded without timing signal (mock mode), and treating that as
		// "within threshold" is how a real regression slips through.
		return MetricDelta{Baseline: base, Current: cur, NoBaseline: true}
	}
	delta := cur - base
	pct := (delta / base) * 100.0
	regressed := false
	if lowerIsBetter {
		// Regressed if latency/errors grew by more than 20%
		regressed = pct > 20.0
	} else {
		// Regressed if throughput dropped by more than 20%
		regressed = pct < -20.0
	}
	return MetricDelta{
		Baseline:  base,
		Current:   cur,
		Delta:     delta,
		DeltaPct:  pct,
		Regressed: regressed,
	}
}

// FormatComparison produces a terminal-friendly comparison table.
func FormatComparison(comp BenchmarkComparison) string {
	var sb strings.Builder
	sb.WriteString("\n=== BENCHMARK COMPARISON REPORT ===\n")
	sb.WriteString(fmt.Sprintf("%-20s | %-12s | %-12s | %-10s | %s\n", "SCENARIO", "BASELINE (ms)", "CURRENT (ms)", "DELTA %", "STATUS"))
	sb.WriteString(strings.Repeat("-", 70) + "\n")

	keys := make([]string, 0, len(comp.Scenarios))
	for k := range comp.Scenarios {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		sc := comp.Scenarios[k]
		dur := sc.Metrics["total_duration_ms"]
		status := "OK"
		switch {
		case dur.NoBaseline:
			status = "NO BASELINE"
		case dur.Regressed:
			status = "REGRESSION"
		}
		sb.WriteString(fmt.Sprintf("%-20s | %12.1f | %12.1f | %+9.1f%% | %s\n",
			k, dur.Baseline, dur.Current, dur.DeltaPct, status))
	}
	sb.WriteString(strings.Repeat("-", 70) + "\n")
	if comp.HasRegression {
		sb.WriteString("WARNING: Regressions detected (>20% difference) against baseline!\n")
	} else if hasUnjudgeable(comp) {
		sb.WriteString("WARNING: No usable baseline for at least one metric: those rows were NOT verified. Re-record the baseline against a real target.\n")
	} else {
		sb.WriteString("SUCCESS: All scenarios within baseline performance thresholds.\n")
	}
	return sb.String()
}

// hasUnjudgeable reports whether any compared metric lacked a usable baseline.
func hasUnjudgeable(comp BenchmarkComparison) bool {
	for _, sc := range comp.Scenarios {
		for _, delta := range sc.Metrics {
			if delta.NoBaseline {
				return true
			}
		}
	}
	return false
}
