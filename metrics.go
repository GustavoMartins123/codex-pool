package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

type metrics struct {
	mu                    sync.Mutex
	requests              map[string]int64            // status -> count
	accStatus             map[string]map[string]int64 // account -> status -> count
	webSocketTerminations map[webSocketTerminationKey]int64
	// cyberPolicy counts cyber_policy-related actions taken by the
	// proxy. The keys are `(account, action)` pairs. Action is one of:
	//   suppressed_ws            — WS cyber_policy frame seen
	//   suppressed_sse           — streaming SSE cyber_policy seen
	//   suppressed_buffered      — buffered translation hit
	//   swap_succeeded           — WS hot-swap to cyber upstream done
	//   swap_no_candidate        — saw cyber_policy but no cyber candidate
	//   retry_buffered           — buffered translation retried on cyber
	cyberPolicy map[cyberPolicyKey]int64
	passport    map[passportMetricKey]int64

	// Performance & Reliability metrics (F1)
	ttftSamples                     []float64 // rolling buffer in milliseconds
	connectSamples                  []float64 // rolling buffer in milliseconds
	durationSamples                 []float64 // rolling buffer in milliseconds
	tokensPerSecSamples             []float64 // rolling buffer in tokens/s
	retriesTotal                    int64
	streamInterrupts                int64
	providerRequests                map[string]map[string]int64 // provider -> status -> count
	modelRequests                   map[string]map[string]int64 // model -> status -> count
	antigravityInputResidualSamples []float64
}

type passportMetricKey struct {
	name  string
	label string
}

type webSocketTerminationKey struct {
	account string
	side    string
	code    int
	outcome string
}

type cyberPolicyKey struct {
	account string
	action  string
}

func newMetrics() *metrics {
	return &metrics{
		requests:                        make(map[string]int64),
		accStatus:                       make(map[string]map[string]int64),
		cyberPolicy:                     make(map[cyberPolicyKey]int64),
		passport:                        make(map[passportMetricKey]int64),
		webSocketTerminations:           make(map[webSocketTerminationKey]int64),
		ttftSamples:                     make([]float64, 0, 1000),
		connectSamples:                  make([]float64, 0, 1000),
		durationSamples:                 make([]float64, 0, 1000),
		tokensPerSecSamples:             make([]float64, 0, 1000),
		providerRequests:                make(map[string]map[string]int64),
		modelRequests:                   make(map[string]map[string]int64),
		antigravityInputResidualSamples: make([]float64, 0, 1000),
	}
}

func (m *metrics) incWebSocketTermination(account string, term webSocketTermination) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.webSocketTerminations[webSocketTerminationKey{account: account, side: term.Side, code: int(term.Code), outcome: term.Outcome}]++
	m.mu.Unlock()
}

func (m *metrics) webSocketTerminationCount(account, side string, code int, outcome string) int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.webSocketTerminations[webSocketTerminationKey{account: account, side: side, code: code, outcome: outcome}]
}

func (m *metrics) incPassport(name, label string) {
	if m == nil || name == "" || label == "" {
		return
	}
	m.mu.Lock()
	m.passport[passportMetricKey{name: name, label: label}]++
	m.mu.Unlock()
}

func (m *metrics) inc(status string, account string) {
	m.mu.Lock()
	m.requests[status]++
	if account != "" {
		mp, ok := m.accStatus[account]
		if !ok {
			mp = make(map[string]int64)
			m.accStatus[account] = mp
		}
		mp[status]++
	}
	m.mu.Unlock()
}

// incCyberPolicy bumps the (account, action) counter. account may be
// empty for actions that are not tied to a single account.
func (m *metrics) incCyberPolicy(account, action string) {
	if action == "" {
		return
	}
	m.mu.Lock()
	m.cyberPolicy[cyberPolicyKey{account: account, action: action}]++
	m.mu.Unlock()
}

// cyberPolicySnapshot returns a copy of the cyber_policy counter map
// for use by admin/status endpoints.
func (m *metrics) cyberPolicySnapshot() map[cyberPolicyKey]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[cyberPolicyKey]int64, len(m.cyberPolicy))
	for k, v := range m.cyberPolicy {
		out[k] = v
	}
	return out
}

// PerformanceSummary provides an explainable snapshot of proxy performance.
type PerformanceSummary struct {
	TTFTMsAvg                   float64         `json:"ttft_ms_avg"`
	ConnectMsAvg                float64         `json:"connect_ms_avg"`
	TotalDurationMsAvg          float64         `json:"total_duration_ms_avg"`
	TokensPerSecAvg             float64         `json:"tokens_per_second_avg"`
	SuccessRate                 float64         `json:"success_rate"`
	RateLimit429Rate            float64         `json:"rate_limit_429_rate"`
	ServerError5xxRate          float64         `json:"server_error_5xx_rate"`
	RetryCount                  int64           `json:"retry_count"`
	StreamInterruptions         int64           `json:"stream_interruptions"`
	AntigravityInputResidualP99 float64         `json:"antigravity_input_residual_p99"`
	ProviderAvailability        map[string]bool `json:"provider_availability,omitempty"`
	ModelAvailability           map[string]bool `json:"model_availability,omitempty"`
}

func (m *metrics) recordPerformance(provider, model string, durationMs, ttftMs, connectMs, tokensPerSec float64, statusCode int, retries int, streamInterrupted bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	const maxSamples = 1000
	if ttftMs > 0 {
		if len(m.ttftSamples) >= maxSamples {
			m.ttftSamples = m.ttftSamples[1:]
		}
		m.ttftSamples = append(m.ttftSamples, ttftMs)
	}
	if connectMs > 0 {
		if len(m.connectSamples) >= maxSamples {
			m.connectSamples = m.connectSamples[1:]
		}
		m.connectSamples = append(m.connectSamples, connectMs)
	}
	if durationMs > 0 {
		if len(m.durationSamples) >= maxSamples {
			m.durationSamples = m.durationSamples[1:]
		}
		m.durationSamples = append(m.durationSamples, durationMs)
	}
	if tokensPerSec > 0 {
		if len(m.tokensPerSecSamples) >= maxSamples {
			m.tokensPerSecSamples = m.tokensPerSecSamples[1:]
		}
		m.tokensPerSecSamples = append(m.tokensPerSecSamples, tokensPerSec)
	}
	if retries > 0 {
		m.retriesTotal += int64(retries)
	}
	if streamInterrupted {
		m.streamInterrupts++
	}

	statusStr := fmt.Sprintf("%d", statusCode)
	if provider != "" {
		mp, ok := m.providerRequests[provider]
		if !ok {
			mp = make(map[string]int64)
			m.providerRequests[provider] = mp
		}
		mp[statusStr]++
	}
	if model != "" {
		mp, ok := m.modelRequests[model]
		if !ok {
			mp = make(map[string]int64)
			m.modelRequests[model] = mp
		}
		mp[statusStr]++
	}
}

func (m *metrics) incRetry() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.retriesTotal++
	m.mu.Unlock()
}

func (m *metrics) incStreamInterruption() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.streamInterrupts++
	m.mu.Unlock()
}

func (m *metrics) recordAntigravityInputResidual(residual int64) {
	if m == nil || residual <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	const maxSamples = 1000
	if len(m.antigravityInputResidualSamples) >= maxSamples {
		m.antigravityInputResidualSamples = m.antigravityInputResidualSamples[1:]
	}
	m.antigravityInputResidualSamples = append(m.antigravityInputResidualSamples, float64(residual))
}

func (m *metrics) performanceSummary(pool *poolState) PerformanceSummary {
	if m == nil {
		return PerformanceSummary{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	avg := func(samples []float64) float64 {
		if len(samples) == 0 {
			return 0
		}
		var sum float64
		for _, s := range samples {
			sum += s
		}
		return sum / float64(len(samples))
	}

	var totalReqs, successReqs, rate429Reqs, server5xxReqs int64
	for status, count := range m.requests {
		totalReqs += count
		if len(status) == 3 {
			switch status[0] {
			case '2':
				successReqs += count
			case '5':
				server5xxReqs += count
			}
			if status == "429" {
				rate429Reqs += count
			}
		}
	}

	var successRate, rate429, server5xx float64
	if totalReqs > 0 {
		successRate = float64(successReqs) / float64(totalReqs)
		rate429 = float64(rate429Reqs) / float64(totalReqs)
		server5xx = float64(server5xxReqs) / float64(totalReqs)
	}

	var antigravityInputResidualP99 float64
	if len(m.antigravityInputResidualSamples) > 0 {
		residuals := append([]float64(nil), m.antigravityInputResidualSamples...)
		sort.Float64s(residuals)
		index := int(math.Ceil(0.99*float64(len(residuals)))) - 1
		if index < 0 {
			index = 0
		}
		if index >= len(residuals) {
			index = len(residuals) - 1
		}
		antigravityInputResidualP99 = residuals[index]
	}

	summary := PerformanceSummary{
		TTFTMsAvg:                   avg(m.ttftSamples),
		ConnectMsAvg:                avg(m.connectSamples),
		TotalDurationMsAvg:          avg(m.durationSamples),
		TokensPerSecAvg:             avg(m.tokensPerSecSamples),
		SuccessRate:                 successRate,
		RateLimit429Rate:            rate429,
		ServerError5xxRate:          server5xx,
		RetryCount:                  m.retriesTotal,
		StreamInterruptions:         m.streamInterrupts,
		AntigravityInputResidualP99: antigravityInputResidualP99,
	}

	if pool != nil {
		provs, mods := pool.providerModelAvailability()
		summary.ProviderAvailability = provs
		summary.ModelAvailability = mods
	}

	return summary
}

func (p *poolState) providerModelAvailability() (map[string]bool, map[string]bool) {
	if p == nil {
		return map[string]bool{}, map[string]bool{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	providers := make(map[string]bool)
	models := make(map[string]bool)

	for _, a := range p.accounts {
		a.mu.Lock()
		isAvail := !a.Dead && !a.Disabled && !a.NeedsVerification && (a.RateLimitUntil.IsZero() || !a.RateLimitUntil.After(now))
		provType := string(a.Type)
		if isAvail {
			providers[provType] = true
			for m := range a.Models {
				models[m] = true
			}
		}
		a.mu.Unlock()
	}
	return providers, models
}

func (m *metrics) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	summary := m.performanceSummary(nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	// overall
	statuses := make([]string, 0, len(m.requests))
	for s := range m.requests {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		fmt.Fprintf(w, "codexpool_requests_total{status=\"%s\"} %d\n", s, m.requests[s])
	}
	// per account
	accs := make([]string, 0, len(m.accStatus))
	for a := range m.accStatus {
		accs = append(accs, a)
	}
	sort.Strings(accs)
	for _, a := range accs {
		st := m.accStatus[a]
		sts := make([]string, 0, len(st))
		for s := range st {
			sts = append(sts, s)
		}
		sort.Strings(sts)
		for _, s := range sts {
			fmt.Fprintf(w, "codexpool_account_requests_total{account=\"%s\",status=\"%s\"} %d\n", a, s, st[s])
		}
	}

	terminationKeys := make([]webSocketTerminationKey, 0, len(m.webSocketTerminations))
	for key := range m.webSocketTerminations {
		terminationKeys = append(terminationKeys, key)
	}
	sort.Slice(terminationKeys, func(i, j int) bool {
		if terminationKeys[i].account != terminationKeys[j].account {
			return terminationKeys[i].account < terminationKeys[j].account
		}
		if terminationKeys[i].side != terminationKeys[j].side {
			return terminationKeys[i].side < terminationKeys[j].side
		}
		if terminationKeys[i].code != terminationKeys[j].code {
			return terminationKeys[i].code < terminationKeys[j].code
		}
		return terminationKeys[i].outcome < terminationKeys[j].outcome
	})
	for _, key := range terminationKeys {
		fmt.Fprintf(w, "codexpool_websocket_terminations_total{account=\"%s\",side=\"%s\",code=\"%d\",outcome=\"%s\"} %d\n",
			key.account, key.side, key.code, key.outcome, m.webSocketTerminations[key])
	}

	// cyber_policy actions: per-(account, action) counters so operators
	// can alert on suppressions firing without successful swaps.
	cyberKeys := make([]cyberPolicyKey, 0, len(m.cyberPolicy))
	for k := range m.cyberPolicy {
		cyberKeys = append(cyberKeys, k)
	}
	sort.Slice(cyberKeys, func(i, j int) bool {
		if cyberKeys[i].account != cyberKeys[j].account {
			return cyberKeys[i].account < cyberKeys[j].account
		}
		return cyberKeys[i].action < cyberKeys[j].action
	})
	for _, k := range cyberKeys {
		fmt.Fprintf(w, "codexpool_cyber_policy_actions_total{account=\"%s\",action=\"%s\"} %d\n", k.account, k.action, m.cyberPolicy[k])
	}

	passportKeys := make([]passportMetricKey, 0, len(m.passport))
	for key := range m.passport {
		passportKeys = append(passportKeys, key)
	}
	sort.Slice(passportKeys, func(i, j int) bool {
		if passportKeys[i].name != passportKeys[j].name {
			return passportKeys[i].name < passportKeys[j].name
		}
		return passportKeys[i].label < passportKeys[j].label
	})
	for _, key := range passportKeys {
		fmt.Fprintf(w, "codexpool_%s_total{result=\"%s\"} %d\n", key.name, key.label, m.passport[key])
	}

	// Performance & Reliability metrics
	fmt.Fprintf(w, "codexpool_ttft_seconds_avg %.6f\n", summary.TTFTMsAvg/1000.0)
	fmt.Fprintf(w, "codexpool_upstream_connect_duration_seconds_avg %.6f\n", summary.ConnectMsAvg/1000.0)
	fmt.Fprintf(w, "codexpool_request_duration_seconds_avg %.6f\n", summary.TotalDurationMsAvg/1000.0)
	fmt.Fprintf(w, "codexpool_tokens_per_second_avg %.2f\n", summary.TokensPerSecAvg)
	fmt.Fprintf(w, "codexpool_success_rate %.4f\n", summary.SuccessRate)
	fmt.Fprintf(w, "codexpool_429_rate %.4f\n", summary.RateLimit429Rate)
	fmt.Fprintf(w, "codexpool_5xx_rate %.4f\n", summary.ServerError5xxRate)
	fmt.Fprintf(w, "codexpool_retries_total %d\n", summary.RetryCount)
	fmt.Fprintf(w, "codexpool_stream_interruptions_total %d\n", summary.StreamInterruptions)
	fmt.Fprintf(w, "codexpool_antigravity_input_residual_p99 %.0f\n", summary.AntigravityInputResidualP99)
}
