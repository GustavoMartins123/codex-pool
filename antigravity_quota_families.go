package main

import (
	"strings"
	"time"
)

// AccountQuotaWindow is one reset window of a model family's quota.
type AccountQuotaWindow struct {
	UsedPct       float64 `json:"used_pct"`
	ResetMinutes  int     `json:"reset_minutes"`
	WindowMinutes int     `json:"window_minutes"`
}

// AccountQuotaFamily reports the quota a group of models draws from. The
// account-level windows blend every group into the most consumed bucket, so
// an exhausted Claude/GPT bucket hides a full Gemini one.
type AccountQuotaFamily struct {
	ID       string              `json:"id"`
	Label    string              `json:"label"`
	FiveHour *AccountQuotaWindow `json:"five_hour,omitempty"`
	Weekly   *AccountQuotaWindow `json:"weekly,omitempty"`
}

const antigravityQuotaFamilyArgon = "argon"

var antigravityQuotaFamilyLabels = map[string]string{
	antigravityQuotaFamilyGemini:     "Gemini",
	antigravityQuotaFamilyArgon:      "Gemini 4 Argon",
	antigravityQuotaFamilyThirdParty: "Claude / GPT",
}

// Gemini leads because it is the pool's main model family; unknown groups
// follow in the order the summary lists them.
var antigravityQuotaFamilyOrder = []string{antigravityQuotaFamilyGemini, antigravityQuotaFamilyArgon, antigravityQuotaFamilyThirdParty}

// antigravityGroupFamily names the family a quota group describes. Paid Pro
// plans report Gemini 4 Argon as its own group with a separate weekly limit,
// while Ultra folds it into the Gemini group, so the group, not the bucket ID
// prefix, is what separates it.
func antigravityGroupFamily(group AntigravityQuotaGroup) (id, label string) {
	name := strings.ToLower(group.DisplayName)
	switch {
	case strings.Contains(name, "argon"):
		id = antigravityQuotaFamilyArgon
	case strings.Contains(name, "claude"), strings.Contains(name, "gpt"):
		id = antigravityQuotaFamilyThirdParty
	case strings.Contains(name, "gemini"):
		id = antigravityQuotaFamilyGemini
	default:
		for _, bucket := range group.Buckets {
			if family := antigravityBucketFamily(bucket); antigravityQuotaFamilyLabels[family] != "" {
				id = family
				break
			}
		}
	}
	if id == "" {
		if strings.TrimSpace(group.DisplayName) == "" {
			return "", ""
		}
		return strings.Join(strings.Fields(name), "-"), strings.TrimSpace(group.DisplayName)
	}
	return id, antigravityQuotaFamilyLabels[id]
}

// antigravityQuotaFamilies lists the quota families a summary describes. A
// bucket whose reset has passed counts as refilled, the same reading
// familyExhaustedUntil applies to routing.
func antigravityQuotaFamilies(summary *AntigravityQuotaSummary, now time.Time) []AccountQuotaFamily {
	if summary == nil {
		return nil
	}
	labels := map[string]string{}
	buckets := map[string][]AntigravityQuotaBucket{}
	var seen []string
	add := func(id, label string, more []AntigravityQuotaBucket) {
		if id == "" {
			return
		}
		if _, ok := buckets[id]; !ok {
			seen = append(seen, id)
			labels[id] = label
		}
		buckets[id] = append(buckets[id], more...)
	}
	for _, group := range summary.Groups {
		id, label := antigravityGroupFamily(group)
		add(id, label, group.Buckets)
	}
	for _, bucket := range summary.Buckets {
		if family := antigravityBucketFamily(bucket); antigravityQuotaFamilyLabels[family] != "" {
			add(family, antigravityQuotaFamilyLabels[family], []AntigravityQuotaBucket{bucket})
		}
	}

	ordered := make([]string, 0, len(seen))
	for _, id := range antigravityQuotaFamilyOrder {
		if _, ok := buckets[id]; ok {
			ordered = append(ordered, id)
		}
	}
	for _, id := range seen {
		if antigravityQuotaFamilyLabels[id] == "" {
			ordered = append(ordered, id)
		}
	}

	var families []AccountQuotaFamily
	for _, id := range ordered {
		family := AccountQuotaFamily{
			ID:       id,
			Label:    labels[id],
			FiveHour: antigravityFamilyWindow(buckets[id], "5h", 5*60, now),
			Weekly:   antigravityFamilyWindow(buckets[id], "weekly", 7*24*60, now),
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
