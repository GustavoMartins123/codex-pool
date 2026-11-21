package main

import (
	"testing"
	"time"
)

const (
	argonModel  = "gemini-4-argon-high"
	geminiModel = "gemini-3.8-flash-high"
)

// bucket builds a quota bucket; remaining < 0 means the fraction is unreported.
func quotaBucket(id, window string, remaining float64, reset time.Time) AntigravityQuotaBucket {
	bucket := AntigravityQuotaBucket{BucketID: id, Window: window, ResetTime: reset}
	if remaining >= 0 {
		bucket.RemainingFraction = &remaining
	}
	return bucket
}

// paidProSummary mirrors a paid Pro account: Argon reports its own weekly
// bucket, whose ID still starts with "gemini-".
func paidProSummary(now time.Time, geminiWeekly, gemini5h, argonWeekly, thirdPartyWeekly float64) *AntigravityQuotaSummary {
	weekly, fiveHour := now.Add(5*24*time.Hour), now.Add(3*time.Hour)
	return &AntigravityQuotaSummary{FetchedAt: now, Groups: []AntigravityQuotaGroup{
		{DisplayName: "Gemini Models", Buckets: []AntigravityQuotaBucket{
			quotaBucket("gemini-weekly", "weekly", geminiWeekly, weekly),
			quotaBucket("gemini-5h", "5h", gemini5h, fiveHour),
		}},
		{DisplayName: "Claude and GPT models", Buckets: []AntigravityQuotaBucket{
			quotaBucket("3p-weekly", "weekly", thirdPartyWeekly, weekly),
			quotaBucket("3p-5h", "5h", 1, fiveHour),
		}},
		{DisplayName: "Gemini 4 Argon", Buckets: []AntigravityQuotaBucket{
			quotaBucket("gemini-argon-wtu-weekly", "weekly", argonWeekly, weekly),
		}},
	}}
}

func TestAntigravityArgonBucketIsNotAGeminiBucket(t *testing.T) {
	argon := AntigravityQuotaBucket{BucketID: "gemini-argon-wtu-weekly"}
	if got := antigravityBucketFamily(argon); got != "argon" {
		t.Fatalf("argon bucket family = %q", got)
	}
	if got := antigravityBucketFamily(AntigravityQuotaBucket{BucketID: "gemini-weekly"}); got != "gemini" {
		t.Fatalf("gemini bucket family = %q", got)
	}
	for model, want := range map[string]string{
		argonModel:                       "argon",
		"antigravity/gemini-4-argon-low": "argon",
		"gemini-pro-orchestrator-argon":  "argon",
		geminiModel:                      "gemini",
		"gemini-4-neon-high":             "gemini",
		"claude-opus-5-5-thinking":       "3p",
	} {
		if got := antigravityQuotaFamily(model); got != want {
			t.Errorf("family of %q = %q, want %q", model, got, want)
		}
	}
}

func TestAntigravityExhaustedArgonDoesNotBlockGemini(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	summary := paidProSummary(now, 1, 1, 0, 1)
	if got := summary.exhaustedUntil(geminiModel, now); !got.IsZero() {
		t.Fatalf("an exhausted Argon bucket blocked a Gemini model until %s", got)
	}
	if got := summary.exhaustedUntil(argonModel, now); !got.After(now) {
		t.Fatalf("exhausted Argon must block Argon models, got %s", got)
	}
}

func TestAntigravityExhaustedGeminiDoesNotBlockArgonOnPaidPro(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	// A real paid Pro reading: the Gemini 5-hour bucket is spent while Argon has
	// 19% of its own weekly quota left.
	summary := paidProSummary(now, 0.706, 0, 0.191, 0)
	if got := summary.exhaustedUntil(geminiModel, now); !got.After(now) {
		t.Fatalf("spent Gemini 5h bucket must block Gemini models, got %s", got)
	}
	if got := summary.exhaustedUntil(argonModel, now); !got.IsZero() {
		t.Fatalf("Argon has its own quota but was blocked until %s", got)
	}
}

func TestAntigravityArgonFallsBackToGeminiBucketWithoutOwnBucket(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	spent := 0.0
	// Ultra reports no Argon bucket: Argon draws from the Gemini one.
	summary := &AntigravityQuotaSummary{Groups: []AntigravityQuotaGroup{{DisplayName: "Gemini Models", Buckets: []AntigravityQuotaBucket{
		quotaBucket("gemini-weekly", "weekly", spent, now.Add(48*time.Hour)),
	}}}}
	if got := summary.exhaustedUntil(argonModel, now); !got.After(now) {
		t.Fatalf("Argon must follow the Gemini bucket on Ultra, got %s", got)
	}
}

func TestAntigravityAccountIsExhaustedOnlyWhenEveryFamilyIs(t *testing.T) {
	now := time.Now().UTC()
	rt := newAntigravityRuntime()
	store := func(summary *AntigravityQuotaSummary) {
		rt.models.ReplaceAccount("pro", AntigravityAccountSnapshot{
			FetchedAt:    now,
			Models:       map[string]AntigravityModelInfo{geminiModel: {ID: geminiModel}},
			QuotaSummary: summary,
		})
	}

	// Gemini and Claude/GPT are spent, but Argon still has quota to serve.
	store(paidProSummary(now, 0, 1, 1, 0))
	if exhausted, known := rt.models.AllFamiliesExhausted("pro", now); !known || exhausted {
		t.Fatalf("account with Argon quota left reported exhausted=%v known=%v", exhausted, known)
	}
	store(paidProSummary(now, 0, 1, 0, 0))
	if exhausted, known := rt.models.AllFamiliesExhausted("pro", now); !known || !exhausted {
		t.Fatalf("account with every family spent reported exhausted=%v known=%v", exhausted, known)
	}
}
