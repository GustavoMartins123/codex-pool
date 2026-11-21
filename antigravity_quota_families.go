package main

import "time"

// AccountQuotaWindow is one reset window of a model family's quota.
type AccountQuotaWindow struct {
	UsedPct       float64 `json:"used_pct"`
	ResetMinutes  int     `json:"reset_minutes"`
	WindowMinutes int     `json:"window_minutes"`
}

// AccountQuotaFamily reports the quota a model family draws from. The
// account-level windows blend every family into the most consumed bucket, so
// an exhausted Claude/GPT bucket hides a full Gemini one.
type AccountQuotaFamily struct {
	ID       string              `json:"id"`
	Label    string              `json:"label"`
	FiveHour *AccountQuotaWindow `json:"five_hour,omitempty"`
	Weekly   *AccountQuotaWindow `json:"weekly,omitempty"`
}

var antigravityQuotaFamilyLabels = []struct{ id, label string }{
	{antigravityQuotaFamilyGemini, "Gemini"},
	{antigravityQuotaFamilyThirdParty, "Claude / GPT"},
}

// antigravityQuotaFamilies lists the families a quota summary describes, in a
// stable order. A bucket whose reset has passed counts as refilled, the same
// reading familyExhaustedUntil applies to routing.
func antigravityQuotaFamilies(summary *AntigravityQuotaSummary, now time.Time) []AccountQuotaFamily {
	if summary == nil {
		return nil
	}
	var families []AccountQuotaFamily
	for _, known := range antigravityQuotaFamilyLabels {
		buckets := summary.familyBuckets(known.id)
		if len(buckets) == 0 {
			continue
		}
		family := AccountQuotaFamily{
			ID:       known.id,
			Label:    known.label,
			FiveHour: antigravityFamilyWindow(buckets, "5h", 5*60, now),
			Weekly:   antigravityFamilyWindow(buckets, "weekly", 7*24*60, now),
		}
		if family.FiveHour != nil || family.Weekly != nil {
			families = append(families, family)
		}
	}
	return families
}

func antigravityFamilyWindow(buckets []AntigravityQuotaBucket, window string, windowMinutes int, now time.Time) *AccountQuotaWindow {
	remaining, reset := antigravityBucketsWindow(buckets, window)
	if remaining == nil {
		return nil
	}
	quota := &AccountQuotaWindow{WindowMinutes: windowMinutes}
	if !reset.IsZero() && !reset.After(now) {
		return quota
	}
	quota.UsedPct = antigravityUsedFraction(remaining) * 100
	if !reset.IsZero() {
		quota.ResetMinutes = int(reset.Sub(now).Minutes())
	}
	return quota
}
