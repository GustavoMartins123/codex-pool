package main

import (
	"github.com/fsnotify/fsnotify"
	"fmt"
	"path/filepath"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsServe(t *testing.T) {
	m := newMetrics()
	m.inc("200", "acct1")
	m.inc("429", "acct1")
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.serve(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if len(body) == 0 {
		t.Fatalf("expected metrics output")
	}
}

// TestCyberPolicyMetricsExposed locks down the (account, action)
// counters that operators rely on to alert when suppressions fire
// without successful swaps. Each known action is counted with a
// distinct label so dashboards can break down by what the proxy did.
func TestCyberPolicyMetricsExposed(t *testing.T) {
	m := newMetrics()
	m.incCyberPolicy("shiv_1", "suppressed_ws")
	m.incCyberPolicy("shiv_1", "suppressed_ws")
	m.incCyberPolicy("darv", "swap_succeeded")
	m.incCyberPolicy("shiv_1", "swap_no_candidate")
	m.incCyberPolicy("shiv_1", "suppressed_sse")
	m.incCyberPolicy("shiv_1", "suppressed_buffered")
	m.incCyberPolicy("shiv_1", "retry_buffered")
	m.incCyberPolicy("shiv_1", "retry_4xx")

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.serve(w, req)
	body := w.Body.String()

	want := []string{
		`codexpool_cyber_policy_actions_total{account="darv",action="swap_succeeded"} 1`,
		`codexpool_cyber_policy_actions_total{account="shiv_1",action="suppressed_ws"} 2`,
		`codexpool_cyber_policy_actions_total{account="shiv_1",action="swap_no_candidate"} 1`,
		`codexpool_cyber_policy_actions_total{account="shiv_1",action="suppressed_sse"} 1`,
		`codexpool_cyber_policy_actions_total{account="shiv_1",action="suppressed_buffered"} 1`,
		`codexpool_cyber_policy_actions_total{account="shiv_1",action="retry_buffered"} 1`,
		`codexpool_cyber_policy_actions_total{account="shiv_1",action="retry_4xx"} 1`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("missing metric line %q\nbody:\n%s", line, body)
		}
	}
}

func TestCyberPolicyMetricsSnapshotCopiesMap(t *testing.T) {
	m := newMetrics()
	m.incCyberPolicy("a", "suppressed_ws")
	snap := m.cyberPolicySnapshot()
	if snap[cyberPolicyKey{"a", "suppressed_ws"}] != 1 {
		t.Fatalf("expected 1, got %d", snap[cyberPolicyKey{"a", "suppressed_ws"}])
	}
	// Mutating the snapshot must not affect future reads.
	snap[cyberPolicyKey{"a", "suppressed_ws"}] = 999
	again := m.cyberPolicySnapshot()
	if again[cyberPolicyKey{"a", "suppressed_ws"}] != 1 {
		t.Fatalf("snapshot mutation leaked into source: got %d", again[cyberPolicyKey{"a", "suppressed_ws"}])
	}
}

func TestCyberPolicyMetricsIgnoresEmptyAction(t *testing.T) {
	m := newMetrics()
	m.incCyberPolicy("acct", "")
	if got := len(m.cyberPolicySnapshot()); got != 0 {
		t.Fatalf("empty action should not increment, got snapshot len %d", got)
	}
}

func TestAntigravityInputResidualP99Metric(t *testing.T) {
	m := newMetrics()
	for value := int64(1); value <= 100; value++ {
		m.recordAntigravityInputResidual(value)
	}
	summary := m.performanceSummary(nil)
	if summary.AntigravityInputResidualP99 != 99 {
		t.Fatalf("unexpected Antigravity residual p99: %v", summary.AntigravityInputResidualP99)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	m.serve(w, req)
	if !strings.Contains(w.Body.String(), "codexpool_antigravity_input_residual_p99 99") {
		t.Fatalf("missing Antigravity residual metric: %s", w.Body.String())
	}
}

func TestComputeCyberPolicyStatsHealthSignals(t *testing.T) {
	cyberLive := &Account{ID: "cyber-live", Type: AccountTypeCodex, CyberAccess: true}
	cyberDead := &Account{ID: "cyber-dead", Type: AccountTypeCodex, CyberAccess: true, Dead: true}
	plain := &Account{ID: "plain", Type: AccountTypeCodex}

	cases := []struct {
		name             string
		accounts         []*Account
		bumps            map[string]map[string]int // action -> account -> n
		wantHealthy      bool
		wantCandidates   int
		wantSuppressedWS int64
	}{
		{
			name:           "no suppressions, cyber candidate available -> healthy",
			accounts:       []*Account{plain, cyberLive},
			wantHealthy:    true,
			wantCandidates: 1,
		},
		{
			name:           "no suppressions and no cyber candidate -> degraded",
			accounts:       []*Account{plain},
			wantHealthy:    false,
			wantCandidates: 0,
		},
		{
			name:     "suppression paired with swap -> healthy",
			accounts: []*Account{plain, cyberLive},
			bumps: map[string]map[string]int{
				"suppressed_ws":  {"plain": 3},
				"swap_succeeded": {"cyber-live": 3},
			},
			wantHealthy:      true,
			wantCandidates:   1,
			wantSuppressedWS: 3,
		},
		{
			name:     "suppressions outpace resolutions -> degraded",
			accounts: []*Account{plain, cyberLive},
			bumps: map[string]map[string]int{
				"suppressed_ws":  {"plain": 5},
				"swap_succeeded": {"cyber-live": 2},
			},
			wantHealthy:      false,
			wantCandidates:   1,
			wantSuppressedWS: 5,
		},
		{
			name:           "dead cyber account doesn't count as candidate",
			accounts:       []*Account{plain, cyberDead},
			wantHealthy:    false,
			wantCandidates: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &proxyHandler{
				cfg:     &config{},
				metrics: newMetrics(),
				pool:    newPoolState(tc.accounts, false),
			}
			for action, byAcc := range tc.bumps {
				for acc, n := range byAcc {
					for i := 0; i < n; i++ {
						h.metrics.incCyberPolicy(acc, action)
					}
				}
			}
			got := h.computeCyberPolicyStats(tc.accounts)
			if got.Healthy != tc.wantHealthy {
				t.Errorf("Healthy = %v, want %v", got.Healthy, tc.wantHealthy)
			}
			if got.CyberCandidatesAvailable != tc.wantCandidates {
				t.Errorf("CyberCandidatesAvailable = %d, want %d", got.CyberCandidatesAvailable, tc.wantCandidates)
			}
			if got.Counters["suppressed_ws"] != tc.wantSuppressedWS {
				t.Errorf("suppressed_ws = %d, want %d", got.Counters["suppressed_ws"], tc.wantSuppressedWS)
			}
		})
	}
}


func TestMetricsBoundedSamplesAndModels(t *testing.T) {
	m := newMetrics()

	// Record 1200 performance samples (more than maxSamples 1000).
	for i := 0; i < 1200; i++ {
		m.recordPerformance("codex", fmt.Sprintf("model-%d", i), 10.0, 5.0, 2.0, 50.0, 200, 0, false)
	}

	m.mu.Lock()
	ttftLen := len(m.ttftSamples)
	connectLen := len(m.connectSamples)
	durationLen := len(m.durationSamples)
	tpsLen := len(m.tokensPerSecSamples)
	modelCount := len(m.modelRequests)
	m.mu.Unlock()

	if ttftLen > 1000 || connectLen > 1000 || durationLen > 1000 || tpsLen > 1000 {
		t.Fatalf("expected metric samples bounded to 1000, got ttft=%d connect=%d duration=%d tps=%d",
			ttftLen, connectLen, durationLen, tpsLen)
	}
	if modelCount > maxTrackedMetricModels {
		t.Fatalf("expected modelRequests bounded to %d, got %d", maxTrackedMetricModels, modelCount)
	}
}

func TestBruteForceTrackerBoundedAndSafeStop(t *testing.T) {
	bf := newBruteForceTracker()
	defer bf.stop()

	// Fill with more than maxTrackedBruteForceIPs.
	for i := 0; i < maxTrackedBruteForceIPs+500; i++ {
		bf.recordFailure(fmt.Sprintf("192.168.%d.%d", (i>>8)&255, i&255))
	}

	bf.mu.Lock()
	count := len(bf.attempts)
	bf.mu.Unlock()

	if count > maxTrackedBruteForceIPs {
		t.Fatalf("expected brute force attempts bounded to <= %d, got %d", maxTrackedBruteForceIPs, count)
	}

	// Multiple calls to stop() must not panic.
	bf.stop()
	bf.stop()
}

func TestPoolWatcherCloseStopsDebounceTimers(t *testing.T) {
	dir := t.TempDir()
	handler := &proxyHandler{pool: newPoolState(nil, false)}
	pw, err := newPoolWatcher(dir, "", handler)
	if err != nil {
		t.Fatal(err)
	}
	pw.handleEvent(fsnotify.Event{Name: filepath.Join(dir, "account.json"), Op: fsnotify.Create})
	pw.close()
}
