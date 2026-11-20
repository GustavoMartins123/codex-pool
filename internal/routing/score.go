package routing

import "sort"

type Signals struct {
	QuotaHeadroom  float64 `toml:"quota_headroom" json:"quota_headroom"`
	ProjectedQuota float64 `toml:"projected_quota" json:"projected_quota"`
	RemainingQuota float64 `toml:"remaining_quota" json:"remaining_quota"`
	ResetUrgency   float64 `toml:"reset_urgency" json:"reset_urgency"`
	ResetTiming    float64 `toml:"reset_timing" json:"reset_timing"`
	ResetProximity float64 `toml:"reset_proximity" json:"reset_proximity"`
	Health         float64 `toml:"health" json:"health"`
	TTFT           float64 `toml:"ttft" json:"ttft"`
	Throughput     float64 `toml:"throughput" json:"throughput"`
	Performance    float64 `toml:"performance" json:"performance"`
	Affinity       float64 `toml:"affinity" json:"affinity"`
	Inflight       float64 `toml:"inflight" json:"inflight"`
	RecentFailure  float64 `toml:"recent_failure" json:"recent_failure"`
}

type Weights Signals

func (w Weights) Total() float64 {
	return w.QuotaHeadroom + w.ProjectedQuota + w.RemainingQuota +
		w.ResetUrgency + w.ResetTiming + w.ResetProximity +
		w.Health + w.TTFT + w.Throughput + w.Performance +
		w.Affinity + w.Inflight + w.RecentFailure
}

func (w Weights) Normalized() Weights {
	total := w.Total()
	if total <= 0 {
		return w
	}
	scale := func(value float64) float64 {
		if value < 0 {
			return 0
		}
		return value / total
	}
	w.QuotaHeadroom = scale(w.QuotaHeadroom)
	w.ProjectedQuota = scale(w.ProjectedQuota)
	w.RemainingQuota = scale(w.RemainingQuota)
	w.ResetUrgency = scale(w.ResetUrgency)
	w.ResetTiming = scale(w.ResetTiming)
	w.ResetProximity = scale(w.ResetProximity)
	w.Health = scale(w.Health)
	w.TTFT = scale(w.TTFT)
	w.Throughput = scale(w.Throughput)
	w.Performance = scale(w.Performance)
	w.Affinity = scale(w.Affinity)
	w.Inflight = scale(w.Inflight)
	w.RecentFailure = scale(w.RecentFailure)
	return w
}

func WeightedScore(weights Weights, signals Signals) float64 {
	score := signals.QuotaHeadroom*weights.QuotaHeadroom +
		signals.ProjectedQuota*weights.ProjectedQuota +
		signals.RemainingQuota*weights.RemainingQuota +
		signals.ResetUrgency*weights.ResetUrgency +
		signals.ResetTiming*weights.ResetTiming +
		signals.ResetProximity*weights.ResetProximity +
		signals.Health*weights.Health +
		signals.TTFT*weights.TTFT +
		signals.Throughput*weights.Throughput +
		signals.Performance*weights.Performance +
		signals.Affinity*weights.Affinity +
		signals.Inflight*weights.Inflight +
		signals.RecentFailure*weights.RecentFailure
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func Reasons(profile string, weights Weights, signals Signals) []string {
	type reason struct {
		name  string
		value float64
	}
	values := []reason{
		{"quota_headroom", signals.QuotaHeadroom * weights.QuotaHeadroom},
		{"projected_quota", signals.ProjectedQuota * weights.ProjectedQuota},
		{"reset_urgency", signals.ResetUrgency*weights.ResetUrgency + signals.ResetProximity*weights.ResetProximity},
		{"health", signals.Health * weights.Health},
		{"ttft", signals.TTFT * weights.TTFT},
		{"throughput", signals.Throughput * weights.Throughput},
		{"conversation_affinity", signals.Affinity * weights.Affinity},
		{"inflight", signals.Inflight * weights.Inflight},
		{"recent_failure", signals.RecentFailure * weights.RecentFailure},
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].value == values[j].value {
			return values[i].name < values[j].name
		}
		return values[i].value > values[j].value
	})
	out := []string{"profile:" + profile}
	for _, item := range values {
		if item.value > 0 {
			out = append(out, item.name)
		}
		if len(out) == 4 {
			break
		}
	}
	return out
}
