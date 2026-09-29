package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
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
type trafficShadowContextKey struct{}

type trafficShadowIdentity struct {
	experiment string
	principal  string
	accounts   []string
}

func trafficShadowFromRequest(r *http.Request) (trafficShadowIdentity, bool) {
	shadow, ok := r.Context().Value(trafficShadowContextKey{}).(trafficShadowIdentity)
	return shadow, ok
}

func (h *proxyHandler) maybeStartShadow(r *http.Request, body []byte, assignment *experimentAssignment, principal, reqID string) {
	_, internalShadow := trafficShadowFromRequest(r)
	if h == nil || assignment == nil || !assignment.Rule.Shadow || assignment.Variant == "canary" ||
		internalShadow {
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

const bucketTrafficShadowBudget = "traffic_shadow_budget"

type trafficShadowRuntime struct {
	mu       sync.Mutex
	cfg      TrafficShadowConfig
	db       *bbolt.DB
	inflight int
}

func validateTrafficShadowConfig(cfg TrafficShadowConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.MaxInflight < 1 || cfg.DailyBudget < 1 || cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 3600 {
		return errors.New("enabled traffic shadow requires positive concurrency, daily budget and timeout (at most 3600 seconds)")
	}
	for name, list := range map[string][]string{"experiments": cfg.Experiments, "accounts": cfg.Accounts, "principals": cfg.Principals} {
		if len(list) == 0 {
			return fmt.Errorf("traffic shadow requires explicit %s", name)
		}
		for _, value := range list {
			if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
				return fmt.Errorf("traffic shadow %s contains an empty or untrimmed entry", name)
			}
		}
	}
	return nil
}
func newTrafficShadowRuntime(db *bbolt.DB, cfg TrafficShadowConfig) (*trafficShadowRuntime, error) {
	r := &trafficShadowRuntime{db: db}
	if err := r.Update(cfg); err != nil {
		return nil, err
	}
	return r, nil
}
func (r *trafficShadowRuntime) Update(cfg TrafficShadowConfig) error {
	if err := validateTrafficShadowConfig(cfg); err != nil {
		return err
	}
	if cfg.Enabled && r.db == nil {
		return errors.New("traffic shadow requires a persistent budget database")
	}
	cfg.Experiments = append([]string(nil), cfg.Experiments...)
	cfg.Accounts = append([]string(nil), cfg.Accounts...)
	cfg.Principals = append([]string(nil), cfg.Principals...)
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	return nil
}
func (r *trafficShadowRuntime) enabledFor(experiment, principal string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.Enabled && containsString(r.cfg.Experiments, experiment) && containsString(r.cfg.Principals, principal)
}

// begin persists the reservation before dispatch. Every runtime sharing the
// database reads and increments the authoritative counter in one transaction.
func (r *trafficShadowRuntime) begin(experiment, principal string, now time.Time) (TrafficShadowConfig, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.cfg
	if !cfg.Enabled || !containsString(cfg.Experiments, experiment) || !containsString(cfg.Principals, principal) || r.inflight >= cfg.MaxInflight {
		return cfg, false, nil
	}
	if r.db == nil {
		return cfg, false, errors.New("traffic shadow budget database unavailable")
	}
	day := now.UTC().Format("2006-01-02")
	admitted := false
	err := r.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(bucketTrafficShadowBudget))
		if err != nil {
			return err
		}
		spent := 0
		if raw := bucket.Get([]byte(day)); raw != nil {
			var count *int
			if err := json.Unmarshal(raw, &count); err != nil {
				return fmt.Errorf("invalid traffic shadow budget: %w", err)
			}
			if count == nil || *count < 0 {
				return errors.New("invalid traffic shadow budget counter")
			}
			spent = *count
		}
		if spent >= cfg.DailyBudget {
			return nil
		}
		encoded, err := json.Marshal(spent + 1)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(day), encoded); err != nil {
			return err
		}
		admitted = true
		return nil
	})
	if err != nil {
		return cfg, false, fmt.Errorf("traffic shadow budget admission failed: %w", err)
	}
	if admitted {
		r.inflight++
	}
	return cfg, admitted, nil
}

func (r *trafficShadowRuntime) end() {
	r.mu.Lock()
	if r.inflight > 0 {
		r.inflight--
	}
	r.mu.Unlock()
}

// markShadowAccountExclusions restricts a traffic-shadow leg to the
// explicitly authorized accounts: every other pool account is excluded from
// candidate selection.
func (h *proxyHandler) markShadowAccountExclusions(exclude map[string]bool, accounts []string) {
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
		if !containsString(accounts, id) {
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
	if shadowBody == nil {
		return
	}
	cfg, admitted, err := h.trafficShadow.begin(assignment.Name, principal, time.Now())
	if err != nil {
		log.Printf("%s: %v", reqID, err)
		h.experiments.Record(assignment.Name, "traffic-shadow", ExperimentObservation{Status: http.StatusServiceUnavailable})
		return
	}
	if !admitted {
		return
	}
	base := r.Context()
	if cfg.DetachFromClient {
		base = context.WithoutCancel(base)
	}
	ctx, cancel := context.WithTimeout(base, time.Duration(cfg.TimeoutSeconds)*time.Second)
	ctx = context.WithValue(ctx, trafficShadowContextKey{}, trafficShadowIdentity{experiment: assignment.Name, principal: principal, accounts: cfg.Accounts})
	request := r.Clone(ctx)
	request.Body = ioNopCloserBytes(shadowBody)
	request.ContentLength = int64(len(shadowBody))
	request.Header = r.Header.Clone()
	request.Header.Del("X-Pool-Shadow")
	request.Header.Del("X-Pool-Canary-Bypass")
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
