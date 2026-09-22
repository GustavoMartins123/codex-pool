package main

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RouteTrace records the routing decision made for a single request.
type RouteTrace struct {
	RequestID    string             `json:"request_id"`
	Timestamp    time.Time          `json:"timestamp"`
	Policy       string             `json:"policy"`
	Selected     RouteTarget        `json:"selected"`
	Score          float64            `json:"score"`
	Reasons        []string           `json:"reasons"`
	Alternatives   []RouteAlternative `json:"alternatives"`
	FallbackFrom   string             `json:"fallback_from,omitempty"`
	FallbackReason string             `json:"fallback_reason,omitempty"`
	CircuitState   string             `json:"circuit_state,omitempty"`

	// Sensitive fields (visible only to operators)
	AccountID      string              `json:"account_id,omitempty"`
	ScoreBreakdown *ScoreBreakdownView `json:"score_breakdown,omitempty"`
	ClientIP       string              `json:"client_ip,omitempty"`
	UserID         string              `json:"user_id,omitempty"`
	Attempts       int                 `json:"attempts,omitempty"`
	DurationMs     float64             `json:"duration_ms,omitempty"`
	TTFTMs         float64             `json:"ttft_ms,omitempty"`
	ConnectMs      float64             `json:"connect_ms,omitempty"`
	StatusCode     int                 `json:"status_code,omitempty"`
	Error          string              `json:"error,omitempty"`
}

// RouteTarget identifies the provider and model chosen.
type RouteTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// RouteAlternative records an alternative account/route considered during selection.
type RouteAlternative struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Score    float64  `json:"score"`
	Reasons  []string `json:"reasons,omitempty"`
	// Sensitive fields (visible only to operators)
	AccountID      string              `json:"account_id,omitempty"`
	ScoreBreakdown *ScoreBreakdownView `json:"score_breakdown,omitempty"`
}

// ScoreBreakdownView exposes the explainable breakdown of account scoring.
type ScoreBreakdownView struct {
	Score                float64                `json:"score"`
	Profile              string                 `json:"profile,omitempty"`
	QuotaScore           float64                `json:"quota_score"`
	ProjectedQuotaScore  float64                `json:"projected_quota_score,omitempty"`
	RemainingQuotaScore  float64                `json:"remaining_quota_score,omitempty"`
	ResetUrgency         float64                `json:"reset_urgency"`
	ResetProximityScore  float64                `json:"reset_proximity_score,omitempty"`
	InflightPenalty      float64                `json:"inflight_penalty"`
	InflightScore        float64                `json:"inflight_score,omitempty"`
	LatencyScore         float64                `json:"latency_score"`
	ThroughputScore      float64                `json:"throughput_score,omitempty"`
	AffinityScore        float64                `json:"affinity_score,omitempty"`
	HealthScore          float64                `json:"health_score"`
	RecentFailurePenalty float64                `json:"recent_failure_penalty"`
	RecentFailureScore   float64                `json:"recent_failure_score,omitempty"`
	Weights              *RoutingProfileWeights `json:"weights,omitempty"`
	BaseWindow           string                 `json:"base_window,omitempty"`
	PrimaryUsed          float64                `json:"primary_used,omitempty"`
	SecondaryUsed        float64                `json:"secondary_used,omitempty"`
	PrimaryPaceBonus     float64                `json:"primary_pace_bonus,omitempty"`
	CreditBonus          float64                `json:"credit_bonus,omitempty"`
}

func newRoutingBreakdownView(score routingScore) *ScoreBreakdownView {
	weights := score.Weights
	return &ScoreBreakdownView{
		Score:               score.Score,
		Profile:             string(score.Profile),
		QuotaScore:          score.Signals.QuotaHeadroom,
		ProjectedQuotaScore: score.Signals.ProjectedQuota,
		RemainingQuotaScore: score.Signals.RemainingQuota,
		ResetUrgency:        score.Signals.ResetUrgency,
		ResetProximityScore: score.Signals.ResetProximity,
		InflightScore:       score.Signals.Inflight,
		LatencyScore:        score.Signals.TTFT,
		ThroughputScore:     score.Signals.Throughput,
		AffinityScore:       score.Signals.Affinity,
		HealthScore:         score.Signals.Health,
		RecentFailureScore:  score.Signals.RecentFailure,
		Weights:             &weights,
	}
}

// routeTraceStore stores recent route traces in a bounded ring buffer for fast, low-overhead retrieval.
type routeTraceStore struct {
	mu      sync.RWMutex
	traces  map[string]*RouteTrace
	order   []string
	maxSize int
	head    int
}

func newRouteTraceStore(maxSize int) *routeTraceStore {
	if maxSize <= 0 {
		maxSize = 2048
	}
	return &routeTraceStore{
		traces:  make(map[string]*RouteTrace, maxSize),
		order:   make([]string, maxSize),
		maxSize: maxSize,
	}
}

func (s *routeTraceStore) Record(trace *RouteTrace) {
	if s == nil || trace == nil || trace.RequestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// If slot has old ID, evict it
	oldID := s.order[s.head]
	if oldID != "" && oldID != trace.RequestID {
		delete(s.traces, oldID)
	}

	s.order[s.head] = trace.RequestID
	s.traces[trace.RequestID] = trace
	s.head = (s.head + 1) % s.maxSize
}

func (s *routeTraceStore) Get(requestID string) (*RouteTrace, bool) {
	if s == nil || requestID == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	trace, ok := s.traces[requestID]
	if !ok {
		return nil, false
	}
	return trace, true
}

func (s *routeTraceStore) SanitizeForClient(trace *RouteTrace) *RouteTrace {
	if trace == nil {
		return nil
	}
	clean := *trace
	clean.AccountID = ""
	clean.ScoreBreakdown = nil
	clean.ClientIP = ""
	clean.UserID = ""

	if len(clean.Alternatives) > 0 {
		sanitizedAlts := make([]RouteAlternative, len(clean.Alternatives))
		for i, alt := range clean.Alternatives {
			sanitizedAlts[i] = RouteAlternative{
				Provider: alt.Provider,
				Model:    alt.Model,
				Score:    alt.Score,
				Reasons:  alt.Reasons,
			}
		}
		clean.Alternatives = sanitizedAlts
	}
	return &clean
}

func newScoreBreakdownView(sb scoreBreakdown, inflight int64, healthError bool) *ScoreBreakdownView {
	inflightPen := float64(inflight) * 0.02
	healthScore := 1.0
	if healthError {
		healthScore = 0.0
	} else if sb.PenaltyApplied > 0 {
		healthScore = math.Max(0.0, 1.0-(sb.PenaltyApplied/10.0))
	}
	latencyScore := 1.0 + sb.RecentUseBonus

	return &ScoreBreakdownView{
		Score:                sb.Score - inflightPen,
		QuotaScore:           sb.BaseHeadroom,
		ResetUrgency:         sb.DrainMultiplier,
		InflightPenalty:      inflightPen,
		LatencyScore:         latencyScore,
		HealthScore:          healthScore,
		RecentFailurePenalty: sb.PenaltyApplied,
		BaseWindow:           sb.BaseWindow,
		PrimaryUsed:          sb.PrimaryUsed,
		SecondaryUsed:        sb.SecondaryUsed,
		PrimaryPaceBonus:     sb.PrimaryPaceBonus,
		CreditBonus:          sb.CreditBonus,
	}
}

func setPoolRouteHeaders(h http.Header, provider, model, route, reason string, attempt int, reqID string) {
	if provider != "" {
		h.Set("X-Pool-Provider", provider)
	}
	if model != "" {
		h.Set("X-Pool-Model", model)
	}
	if route != "" {
		h.Set("X-Pool-Route", route)
	}
	if attempt > 0 {
		h.Set("X-Pool-Attempt", strconv.Itoa(attempt))
	}
	if reason != "" {
		h.Set("X-Pool-Reason", reason)
	}
	if reqID != "" {
		h.Set("X-Pool-Request-Id", reqID)
	}
}
