package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const bucketExperimentMetrics = "experiment_metrics"

type CanaryConfig struct {
	Candidate string  `toml:"candidate" json:"candidate"`
	Percent   float64 `toml:"percent" json:"percent"`
	Shadow    bool    `toml:"shadow" json:"shadow"`
}

type ExperimentsConfig struct {
	Canary  map[string]CanaryConfig `toml:"canary"`
	Traffic TrafficShadowConfig     `toml:"traffic"`
}

type experimentAssignment struct {
	Name      string
	BaseModel string
	Model     string
	Variant   string
	Rule      CanaryConfig
}

type ExperimentMetrics struct {
	Experiment               string    `json:"experiment"`
	Variant                  string    `json:"variant"`
	Requests                 int64     `json:"requests"`
	Successes                int64     `json:"successes"`
	Failures                 int64     `json:"failures"`
	StreamErrors             int64     `json:"stream_errors"`
	ToolRequests             int64     `json:"tool_requests"`
	ToolSuccesses            int64     `json:"tool_successes"`
	ResponseBytes            int64     `json:"response_bytes"`
	EstimatedOutputTokens    int64     `json:"estimated_output_tokens"`
	TotalDurationMs          float64   `json:"total_duration_ms"`
	TotalTTFTMs              float64   `json:"total_ttft_ms"`
	EstimatedTokensPerSecond float64   `json:"estimated_tokens_per_second"`
	LastStatus               int       `json:"last_status"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type ExperimentObservation struct {
	Status        int
	Duration      time.Duration
	TTFT          time.Duration
	ResponseBytes int64
	StreamError   bool
	ToolRequest   bool
}

type experimentTracker struct {
	db     *bbolt.DB
	mu     sync.RWMutex
	canary map[string]CanaryConfig
}

func newExperimentTracker(db *bbolt.DB, cfg ExperimentsConfig) (*experimentTracker, error) {
	tracker := &experimentTracker{db: db, canary: make(map[string]CanaryConfig)}
	tracker.Configure(cfg)
	if db != nil {
		if err := db.Update(func(tx *bbolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte(bucketExperimentMetrics))
			return err
		}); err != nil {
			return nil, err
		}
	}
	return tracker, nil
}

func (t *experimentTracker) Configure(cfg ExperimentsConfig) {
	if t == nil {
		return
	}
	canary := make(map[string]CanaryConfig)
	for model, rule := range cfg.Canary {
		model = strings.ToLower(strings.TrimSpace(model))
		rule.Candidate = strings.TrimSpace(rule.Candidate)
		if model == "" || rule.Candidate == "" {
			continue
		}
		if rule.Percent < 0 {
			rule.Percent = 0
		}
		if rule.Percent > 100 {
			rule.Percent = 100
		}
		canary[model] = rule
	}
	t.mu.Lock()
	t.canary = canary
	t.mu.Unlock()
}

func stableExperimentBucket(key string) float64 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))
	return float64(hash.Sum32()%10000) / 100
}

func (t *experimentTracker) Assign(model, key string) *experimentAssignment {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	rule, ok := t.canary[strings.ToLower(strings.TrimSpace(model))]
	t.mu.RUnlock()
	if !ok {
		return nil
	}
	assignment := &experimentAssignment{
		Name: model + "->" + rule.Candidate, BaseModel: model,
		Model: model, Variant: "control", Rule: rule,
	}
	if stableExperimentBucket(key+"|"+assignment.Name) < rule.Percent {
		assignment.Model = rule.Candidate
		assignment.Variant = "canary"
	}
	return assignment
}

func experimentMetricKey(experiment, variant string) string {
	return experiment + "\x00" + variant
}

func (t *experimentTracker) Record(experiment, variant string, observation ExperimentObservation) {
	if t == nil || t.db == nil || experiment == "" || variant == "" {
		return
	}
	_ = t.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketExperimentMetrics))
		key := experimentMetricKey(experiment, variant)
		var metrics ExperimentMetrics
		if raw := bucket.Get([]byte(key)); raw != nil {
			_ = json.Unmarshal(raw, &metrics)
		}
		metrics.Experiment = experiment
		metrics.Variant = variant
		metrics.Requests++
		if observation.Status >= 200 && observation.Status < 400 && !observation.StreamError {
			metrics.Successes++
		} else {
			metrics.Failures++
		}
		if observation.StreamError {
			metrics.StreamErrors++
		}
		if observation.ToolRequest {
			metrics.ToolRequests++
			if observation.Status >= 200 && observation.Status < 400 && !observation.StreamError {
				metrics.ToolSuccesses++
			}
		}
		estimatedTokens := observation.ResponseBytes / 4
		metrics.ResponseBytes += observation.ResponseBytes
		metrics.EstimatedOutputTokens += estimatedTokens
		metrics.TotalDurationMs += float64(observation.Duration.Milliseconds())
		metrics.TotalTTFTMs += float64(observation.TTFT.Milliseconds())
		if seconds := metrics.TotalDurationMs / 1000; seconds > 0 {
			metrics.EstimatedTokensPerSecond = float64(metrics.EstimatedOutputTokens) / seconds
		}
		metrics.LastStatus = observation.Status
		metrics.UpdatedAt = time.Now().UTC()
		return putJSON(bucket, key, &metrics)
	})
}

func (t *experimentTracker) Metrics() ([]ExperimentMetrics, error) {
	if t == nil || t.db == nil {
		return nil, nil
	}
	var metrics []ExperimentMetrics
	err := t.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketExperimentMetrics)).ForEach(func(_, raw []byte) error {
			var item ExperimentMetrics
			if json.Unmarshal(raw, &item) == nil {
				metrics = append(metrics, item)
			}
			return nil
		})
	})
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Experiment == metrics[j].Experiment {
			return metrics[i].Variant < metrics[j].Variant
		}
		return metrics[i].Experiment < metrics[j].Experiment
	})
	return metrics, err
}

func (h *proxyHandler) serveExperimentMetrics(w http.ResponseWriter) {
	if h == nil || h.experiments == nil {
		respondJSON(w, map[string]any{"metrics": []ExperimentMetrics{}})
		return
	}
	metrics, err := h.experiments.Metrics()
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "experiment metrics unavailable")
		return
	}
	respondJSON(w, map[string]any{"metrics": metrics})
}

type shadowResponseWriter struct {
	header     http.Header
	status     int
	bytes      int64
	firstWrite time.Time
}

func (w *shadowResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *shadowResponseWriter) WriteHeader(status int) {
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
	w.status = status
}
func (w *shadowResponseWriter) Write(body []byte) (int, error) {
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.bytes += int64(len(body))
	return len(body), nil
}
func (w *shadowResponseWriter) Flush() {
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
}

type experimentResponseWriter struct {
	http.ResponseWriter
	status     int
	bytes      int64
	firstWrite time.Time
}

func (w *experimentResponseWriter) WriteHeader(status int) {
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *experimentResponseWriter) Write(body []byte) (int, error) {
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += int64(n)
	return n, err
}
func (w *experimentResponseWriter) Flush() {
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (w *experimentResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func shadowSafeRequest(r *http.Request, body []byte) bool {
	if r == nil || r.Method != http.MethodPost || len(body) == 0 || len(body) > 1<<20 || isWebSocketUpgradeRequest(r) {
		return false
	}
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		return false
	}
	if requestObjectUsesTools(object) {
		return false
	}
	return !valueHasImageGenerationTool(object)
}

func requestUsesTools(body []byte) bool {
	var object map[string]any
	return json.Unmarshal(body, &object) == nil && requestObjectUsesTools(object)
}

func requestObjectUsesTools(object map[string]any) bool {
	tools, exists := object["tools"]
	if !exists || tools == nil {
		return false
	}
	if list, ok := tools.([]any); ok {
		return len(list) > 0
	}
	return true
}

// maybeStartShadow records a routing-only shadow observation, or dispatches
// a traffic-experiment leg when explicitly configured.
func (h *proxyHandler) maybeStartShadow(r *http.Request, body []byte, assignment *experimentAssignment, principal, reqID string) {
	if h == nil || assignment == nil || !assignment.Rule.Shadow || assignment.Variant == "canary" ||
		r.Header.Get("X-Pool-Shadow") != "" {
		return
	}
	if h.trafficShadow.enabledFor(assignment.Name, principal) {
		if !shadowSafeRequest(r, body) {
			return
		}
		h.startTrafficShadow(r, body, assignment, principal, reqID)
		return
	}
	h.recordRoutingShadow(r, assignment)
}

// recordRoutingShadow runs the real routing decision for the candidate model
// — same provider resolution, plan requirement, IP restrictions and scoring
// as a live request — with an empty conversation id so no pin or conversation
// state is written. Circuit-breaker probes are the one side effect, the same
// allowance every selection already makes.
func (h *proxyHandler) recordRoutingShadow(r *http.Request, assignment *experimentAssignment) {
	provider, _, _ := h.modelRouteOverride(r.URL.Path, assignment.Rule.Candidate, nil)
	status := http.StatusOK
	var selected *Account
	if provider != nil {
		accountType := provider.Type()
		requiredPlan := requiredPlanForRequest(accountType, r, assignment.Rule.Candidate)
		selected, _, _, _, _, _ = h.pool.candidateWithRoutingTraceForUser("shadow-probe", "", nil, accountType, requiredPlan, getClientIP(r), assignment.Rule.Candidate, "")
	}
	if provider == nil || selected == nil {
		status = http.StatusServiceUnavailable
	}
	h.experiments.Record(assignment.Name, "shadow", ExperimentObservation{Status: status})
}

type TrafficShadowConfig struct {
	Enabled          bool     `toml:"enabled" json:"enabled"`
	Experiments      []string `toml:"experiments" json:"experiments"`
	Accounts         []string `toml:"accounts" json:"accounts"`
	Principals       []string `toml:"principals" json:"principals"`
	MaxInflight      int      `toml:"max_inflight" json:"max_inflight"`
	DailyBudget      int      `toml:"daily_request_budget" json:"daily_request_budget"`
	TimeoutSeconds   int      `toml:"timeout_seconds" json:"timeout_seconds"`
	DetachFromClient bool     `toml:"detach_from_client" json:"detach_from_client"`
}

type trafficShadowRuntime struct {
	mu       sync.Mutex
	cfg      TrafficShadowConfig
	inflight int
	day      string
	spent    int
}

func newTrafficShadowRuntime(cfg TrafficShadowConfig) *trafficShadowRuntime {
	r := &trafficShadowRuntime{}
	r.Update(cfg)
	return r
}

func (r *trafficShadowRuntime) Update(cfg TrafficShadowConfig) {
	trimmed := func(list []string) []string {
		var out []string
		for _, item := range list {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	cfg.Experiments = trimmed(cfg.Experiments)
	cfg.Accounts = trimmed(cfg.Accounts)
	cfg.Principals = trimmed(cfg.Principals)
	if !cfg.Enabled {
		cfg = TrafficShadowConfig{}
	} else {
		if cfg.MaxInflight < 1 {
			cfg.MaxInflight = 1
		}
		if cfg.TimeoutSeconds < 1 {
			cfg.TimeoutSeconds = 120
		}
		if cfg.DailyBudget < 1 || len(cfg.Experiments) == 0 || len(cfg.Accounts) == 0 || len(cfg.Principals) == 0 {
			cfg.Enabled = false
		}
	}
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

func (r *trafficShadowRuntime) enabledFor(experiment, principal string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cfg.Enabled || !containsString(r.cfg.Experiments, experiment) || !containsString(r.cfg.Principals, principal) {
		return false
	}
	return len(r.cfg.Accounts) > 0
}

func (r *trafficShadowRuntime) begin(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cfg.Enabled {
		return false
	}
	day := now.UTC().Format("2006-01-02")
	if r.day != day {
		r.day, r.spent = day, 0
	}
	if r.spent >= r.cfg.DailyBudget || r.inflight >= r.cfg.MaxInflight {
		return false
	}
	r.spent++
	r.inflight++
	return true
}

func (r *trafficShadowRuntime) end() {
	r.mu.Lock()
	if r.inflight > 0 {
		r.inflight--
	}
	r.mu.Unlock()
}

func (r *trafficShadowRuntime) timeout() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Duration(r.cfg.TimeoutSeconds) * time.Second
}

func (r *trafficShadowRuntime) detachFromClient() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.DetachFromClient
}

func (r *trafficShadowRuntime) accountAllowed(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return containsString(r.cfg.Accounts, id)
}

// markShadowAccountExclusions restricts a traffic-shadow leg to the
// explicitly authorized accounts: every other pool account is excluded from
// candidate selection.
func (h *proxyHandler) markShadowAccountExclusions(exclude map[string]bool) {
	if h.trafficShadow == nil {
		return
	}
	h.pool.mu.RLock()
	ids := make([]string, 0, len(h.pool.accounts))
	for _, a := range h.pool.accounts {
		ids = append(ids, a.ID)
	}
	h.pool.mu.RUnlock()
	for _, id := range ids {
		if !h.trafficShadow.accountAllowed(id) {
			exclude[id] = true
		}
	}
}

// startTrafficShadow sends the candidate model to a real upstream under the
// traffic-experiment guardrails: explicit opt-in, experiment/account/
// principal allowlists, daily budget, inflight cap, its own deadline, a
// "-traffic-shadow" request id for separate accounting, and — unless
// detach_from_client is set — cancellation together with the client request.
func (h *proxyHandler) startTrafficShadow(r *http.Request, body []byte, assignment *experimentAssignment, principal, reqID string) {
	shadowBody := rewriteModelInBody(body, assignment.Rule.Candidate)
	if shadowBody == nil || !h.trafficShadow.begin(time.Now()) {
		return
	}
	base := r.Context()
	if h.trafficShadow.detachFromClient() {
		base = context.WithoutCancel(base)
	}
	ctx, cancel := context.WithTimeout(base, h.trafficShadow.timeout())
	request := r.Clone(ctx)
	request.Body = ioNopCloserBytes(shadowBody)
	request.ContentLength = int64(len(shadowBody))
	request.Header = r.Header.Clone()
	request.Header.Set("X-Pool-Shadow", assignment.Name)
	request.Header.Set("X-Pool-Canary-Bypass", "1")
	request.Header.Del("Content-Length")
	go func() {
		defer cancel()
		defer h.trafficShadow.end()
		started := time.Now()
		writer := &shadowResponseWriter{}
		h.proxyRequest(writer, request, reqID+"-traffic-shadow")
		status := writer.status
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		ttft := time.Duration(0)
		if !writer.firstWrite.IsZero() {
			ttft = writer.firstWrite.Sub(started)
		}
		h.experiments.Record(assignment.Name, "traffic-shadow", ExperimentObservation{
			Status: status, Duration: time.Since(started), TTFT: ttft,
			ResponseBytes: writer.bytes, StreamError: status >= 500,
		})
	}()
}

func ioNopCloserBytes(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}
