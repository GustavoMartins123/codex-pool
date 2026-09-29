package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	Canary map[string]CanaryConfig `toml:"canary"`
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

// maybeStartShadow records a ROUTING shadow: it evaluates what the candidate
// model of the canary rule would resolve to — provider metadata and whether
// the pool currently has a usable account for it — without any upstream
// call, quota consumption, or runtime mutation. Sending real traffic as an
// experiment requires the separate traffic-shadow configuration.
func (h *proxyHandler) maybeStartShadow(r *http.Request, body []byte, assignment *experimentAssignment, reqID string) {
	if h == nil || assignment == nil || !assignment.Rule.Shadow || assignment.Variant == "canary" ||
		r.Header.Get("X-Pool-Shadow") != "" || !shadowSafeRequest(r, body) {
		return
	}
	shadowBody := rewriteModelInBody(body, assignment.Rule.Candidate)
	if shadowBody == nil {
		return
	}
	status := http.StatusOK
	if !h.poolHasAccountForModel(assignment.Rule.Candidate) {
		status = http.StatusServiceUnavailable
	}
	h.experiments.Record(assignment.Name, "shadow", ExperimentObservation{
			Status:      status,
		ToolRequest: requestUsesTools(shadowBody),
	})
}

func (h *proxyHandler) poolHasAccountForModel(model string) bool {
	meta, ok := lookupModelMetadata(model, h.pool)
	if !ok || meta.Provider == "" {
		return false
	}
	h.pool.mu.RLock()
	defer h.pool.mu.RUnlock()
	now := time.Now()
	for _, a := range h.pool.accounts {
		if a.Type != meta.Provider || a.Dead || a.Disabled {
			continue
		}
		a.mu.Lock()
		usable := !a.RateLimitUntil.After(now) && accountPrimaryUsageLocked(a) < primaryHardExcludeThreshold && accountSecondaryUsageLocked(a) < secondaryHardExcludeThreshold
		a.mu.Unlock()
		if usable {
			return true
		}
	}
	return false
}

func ioNopCloserBytes(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}

func (a *experimentAssignment) String() string {
	if a == nil {
		return ""
	}
	return fmt.Sprintf("%s:%s", a.Name, a.Variant)
}
