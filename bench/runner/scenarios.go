package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// LoadScenarios reads all scenario JSON files from the provided directory.
func LoadScenarios(dir string) ([]Scenario, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read scenarios dir %q: %w", dir, err)
	}

	var scenarios []Scenario
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, f.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read scenario %q: %w", path, err)
		}
		var sc Scenario
		if err := json.Unmarshal(data, &sc); err != nil {
			return nil, fmt.Errorf("parse scenario %q: %w", path, err)
		}
		scenarios = append(scenarios, sc)
	}
	return scenarios, nil
}

// ExecuteScenario executes a Scenario according to its configuration.
func ExecuteScenario(ctx context.Context, exec ExecutionContext, sc Scenario) (ScenarioMetrics, error) {
	concurrency := sc.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	iterations := sc.Iterations
	if iterations <= 0 {
		iterations = 1
	}

	// Multi-step scenario (e.g., model switch, provider switch, long conversation)
	if len(sc.Steps) > 0 {
		var combined ScenarioMetrics
		for i, step := range sc.Steps {
			method := step.Method
			if method == "" {
				method = "POST"
			}
			path := step.Path
			if path == "" {
				path = "/v1/chat/completions"
			}
			stepMetrics, err := ExecuteSingleRequest(ctx, exec, method, path, step.Headers, step.Body)
			if err != nil {
				combined.Errors++
			}
			AccumulateMetrics(&combined, stepMetrics)
			if i == 0 {
				combined.TTFTMs = stepMetrics.TTFTMs
			}
		}
		FinalizeAverages(&combined, len(sc.Steps))
		return combined, nil
	}

	// Concurrent scenario
	if concurrency > 1 {
		var wg sync.WaitGroup
		resultsChan := make(chan ScenarioMetrics, concurrency*iterations)
		totalRequests := concurrency * iterations

		method := sc.Method
		if method == "" {
			method = "POST"
		}
		path := sc.Path
		if path == "" {
			path = "/v1/chat/completions"
		}

		for c := 0; c < concurrency; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for it := 0; it < iterations; it++ {
					m, _ := ExecuteSingleRequest(ctx, exec, method, path, sc.Headers, sc.Body)
					resultsChan <- m
				}
			}()
		}
		wg.Wait()
		close(resultsChan)

		var total ScenarioMetrics
		for m := range resultsChan {
			AccumulateMetrics(&total, m)
		}
		FinalizeAverages(&total, totalRequests)
		return total, nil
	}

	// Single request repeated iterations times
	method := sc.Method
	if method == "" {
		method = "POST"
	}
	path := sc.Path
	if path == "" {
		path = "/v1/chat/completions"
	}

	var total ScenarioMetrics
	for it := 0; it < iterations; it++ {
		m, err := ExecuteSingleRequest(ctx, exec, method, path, sc.Headers, sc.Body)
		if err != nil {
			total.Errors++
		}
		AccumulateMetrics(&total, m)
	}
	FinalizeAverages(&total, iterations)
	return total, nil
}
