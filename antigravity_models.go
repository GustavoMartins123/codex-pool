package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/routing"
)

type AntigravityQuotaInfo struct {
	RemainingFraction *float64  `json:"remaining_fraction,omitempty"`
	ResetTime         time.Time `json:"reset_time,omitempty"`
}

type AntigravityModelInfo struct {
	ID                 string                     `json:"id"`
	DisplayName        string                     `json:"display_name,omitempty"`
	MaxTokens          int                        `json:"max_tokens,omitempty"`
	MaxOutputTokens    int                        `json:"max_output_tokens,omitempty"`
	SupportsImages     bool                       `json:"supports_images,omitempty"`
	SupportsThinking   bool                       `json:"supports_thinking,omitempty"`
	SupportsTools      bool                       `json:"supports_tools,omitempty"`
	ThinkingBudget     int                        `json:"thinking_budget,omitempty"`
	Recommended        bool                       `json:"recommended,omitempty"`
	SupportedMimeTypes []string                   `json:"supported_mime_types,omitempty"`
	WebSearch          bool                       `json:"web_search,omitempty"`
	Quota              AntigravityQuotaInfo       `json:"quota,omitempty"`
	Raw                map[string]json.RawMessage `json:"raw,omitempty"`
}

type AntigravityQuotaBucket struct {
	BucketID          string    `json:"bucket_id,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	Description       string    `json:"description,omitempty"`
	Window            string    `json:"window,omitempty"`
	RemainingFraction *float64  `json:"remaining_fraction,omitempty"`
	RemainingAmount   *int64    `json:"remaining_amount,omitempty"`
	Disabled          bool      `json:"disabled,omitempty"`
	ResetTime         time.Time `json:"reset_time,omitempty"`
}

type AntigravityQuotaGroup struct {
	DisplayName string                     `json:"display_name,omitempty"`
	Description string                     `json:"description,omitempty"`
	Buckets     []AntigravityQuotaBucket   `json:"buckets,omitempty"`
	Raw         map[string]json.RawMessage `json:"raw,omitempty"`
}

type AntigravityQuotaSummary struct {
	FetchedAt   time.Time                  `json:"fetched_at"`
	Description string                     `json:"description,omitempty"`
	Buckets     []AntigravityQuotaBucket   `json:"buckets,omitempty"`
	Groups      []AntigravityQuotaGroup    `json:"groups,omitempty"`
	Raw         map[string]json.RawMessage `json:"raw,omitempty"`
}

type AntigravityAccountSnapshot struct {
	FetchedAt    time.Time                       `json:"fetched_at"`
	Models       map[string]AntigravityModelInfo `json:"models"`
	Deprecated   map[string]string               `json:"deprecated_model_ids,omitempty"`
	QuotaSummary *AntigravityQuotaSummary        `json:"quota_summary,omitempty"`
	Raw          map[string]json.RawMessage      `json:"raw,omitempty"`
}

type AntigravityCatalogModel struct {
	AntigravityModelInfo
	Aliases            []string  `json:"aliases,omitempty"`
	Replacement        string    `json:"replacement,omitempty"`
	SupportingAccounts int       `json:"supporting_accounts"`
	AvailableAccounts  int       `json:"available_accounts"`
	AvailableNow       bool      `json:"available_now"`
	Stale              bool      `json:"stale"`
	NextResetAt        time.Time `json:"next_reset_at,omitempty"`
}

type antigravityModelRegistry struct {
	mu       sync.RWMutex
	accounts map[string]AntigravityAccountSnapshot
}

// antigravityRuntime is the per-server Antigravity state shared by pool, provider and handler.
type antigravityRuntime struct {
	models    *antigravityModelRegistry
	versions  *antigravityVersionState
	saveLocks sync.Map
}

func newAntigravityRuntime() *antigravityRuntime {
	return &antigravityRuntime{
		models:   &antigravityModelRegistry{accounts: make(map[string]AntigravityAccountSnapshot)},
		versions: newAntigravityVersionState(antigravityFallbackClientVersion, antigravityVersionCacheTTL),
	}
}

func (r *antigravityModelRegistry) Reset() {
	r.mu.Lock()
	r.accounts = make(map[string]AntigravityAccountSnapshot)
	r.mu.Unlock()
}

func (r *antigravityModelRegistry) ReplaceAccount(accountID string, snapshot AntigravityAccountSnapshot) {
	if strings.TrimSpace(accountID) == "" || len(snapshot.Models) == 0 {
		return
	}
	if snapshot.FetchedAt.IsZero() {
		snapshot.FetchedAt = time.Now().UTC()
	}
	r.mu.Lock()
	r.accounts[accountID] = snapshot
	r.mu.Unlock()
}

func (r *antigravityModelRegistry) AccountSnapshot(accountID string) (AntigravityAccountSnapshot, bool) {
	r.mu.RLock()
	snapshot, ok := r.accounts[accountID]
	r.mu.RUnlock()
	return snapshot, ok
}

func (r *antigravityModelRegistry) Supports(accountID, model string) bool {
	model, _ = r.Canonical(model)
	r.mu.RLock()
	snapshot, ok := r.accounts[accountID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	_, ok = snapshot.Models[model]
	return ok
}

func (r *antigravityModelRegistry) DiscoveryAvailability(accountID, model string, now time.Time) (bool, time.Time) {
	model, _ = r.Canonical(model)
	r.mu.RLock()
	snapshot, ok := r.accounts[accountID]
	r.mu.RUnlock()
	if !ok {
		return false, time.Time{}
	}
	info, ok := snapshot.Models[model]
	if !ok {
		return false, time.Time{}
	}
	var reset time.Time
	if info.Quota.RemainingFraction != nil && *info.Quota.RemainingFraction <= 0 {
		reset = info.Quota.ResetTime
	}
	if familyReset := snapshot.QuotaSummary.exhaustedUntil(model, now); familyReset.After(reset) {
		reset = familyReset
	}
	if reset.After(now) {
		return false, reset
	}
	return true, time.Time{}
}

// Antigravity enforces quota per model family rather than per model: every
// Gemini model shares one 5-hour and one weekly bucket, and Claude/GPT-OSS
// share another pair. fetchAvailableModels only mirrors the 5-hour bucket, so
// the weekly limit is visible only through retrieveUserQuotaSummary.
const (
	antigravityQuotaFamilyGemini     = "gemini"
	antigravityQuotaFamilyThirdParty = "3p"
)

func antigravityQuotaFamily(model string) string {
	lower := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(model), "antigravity/"))
	switch {
	case strings.HasPrefix(lower, "claude"), strings.HasPrefix(lower, "gpt"):
		return antigravityQuotaFamilyThirdParty
	// Gemini 4 Argon has its own weekly bucket on paid Pro plans, so it must not
	// share the Gemini family.
	case strings.HasPrefix(lower, "gemini") && strings.Contains(lower, "argon"):
		return antigravityQuotaFamilyArgon
	case strings.HasPrefix(lower, "gemini") && !strings.Contains(lower, "image"):
		return antigravityQuotaFamilyGemini
	default:
		return ""
	}
}

// antigravityBucketFamily names the family a bucket belongs to from its ID. The
// Argon bucket is "gemini-argon-wtu-weekly": its prefix says Gemini, but it is
// a separate limit.
func antigravityBucketFamily(bucket AntigravityQuotaBucket) string {
	id := strings.ToLower(strings.TrimSpace(bucket.BucketID))
	if strings.Contains(id, antigravityQuotaFamilyArgon) {
		return antigravityQuotaFamilyArgon
	}
	family, _, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	return family
}

// AllFamiliesExhausted reports whether every model family the account's quota
// summary describes is at its hard-exclude threshold. known is false when no
// summary identifies a family, so callers fall back to aggregate usage.
func (r *antigravityModelRegistry) AllFamiliesExhausted(accountID string, now time.Time) (exhausted, known bool) {
	r.mu.RLock()
	snapshot, ok := r.accounts[accountID]
	r.mu.RUnlock()
	if !ok || snapshot.QuotaSummary == nil {
		return false, false
	}
	for _, family := range []string{antigravityQuotaFamilyGemini, antigravityQuotaFamilyArgon, antigravityQuotaFamilyThirdParty} {
		if len(snapshot.QuotaSummary.familyBuckets(family)) == 0 {
			continue
		}
		known = true
		if !snapshot.QuotaSummary.familyExhaustedUntil(family, now).After(now) {
			return false, true
		}
	}
	return known, known
}

// familyBuckets returns the buckets a model family draws from, identified by
// the family prefix of their bucket IDs ("gemini-weekly", "3p-5h").
func (s *AntigravityQuotaSummary) familyBuckets(family string) []AntigravityQuotaBucket {
	if s == nil || family == "" {
		return nil
	}
	var buckets []AntigravityQuotaBucket
	for _, bucket := range s.Buckets {
		if antigravityBucketFamily(bucket) == family {
			buckets = append(buckets, bucket)
		}
	}
	for _, group := range s.Groups {
		for _, bucket := range group.Buckets {
			if antigravityBucketFamily(bucket) == family {
				buckets = append(buckets, bucket)
			}
		}
	}
	return buckets
}

// exhaustedUntil returns the latest reset among the family buckets at or past
// the pool's hard-exclude thresholds; a model is usable again only once every
// bucket it draws from has refilled. Another family's buckets never block it.
func (s *AntigravityQuotaSummary) exhaustedUntil(model string, now time.Time) time.Time {
	family := antigravityQuotaFamily(model)
	// Ultra plans fold Argon into the larger Gemini bucket and report no bucket
	// of its own.
	if family == antigravityQuotaFamilyArgon && len(s.familyBuckets(family)) == 0 {
		family = antigravityQuotaFamilyGemini
	}
	return s.familyExhaustedUntil(family, now)
}

func (s *AntigravityQuotaSummary) familyExhaustedUntil(family string, now time.Time) time.Time {
	var until time.Time
	for _, bucket := range s.familyBuckets(family) {
		if bucket.Disabled || bucket.RemainingFraction == nil || !bucket.ResetTime.After(now) {
			continue
		}
		threshold := 1.0
		switch antigravityQuotaWindow(bucket) {
		case "5h":
			threshold = primaryHardExcludeThreshold
		case "weekly":
			threshold = secondaryHardExcludeThreshold
		}
		if 1-*bucket.RemainingFraction >= threshold && bucket.ResetTime.After(until) {
			until = bucket.ResetTime
		}
	}
	return until
}

func (r *antigravityModelRegistry) Canonical(model string) (string, bool) {
	model = strings.TrimSpace(strings.TrimPrefix(model, "antigravity/"))
	if model == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	found := false
	for _, snapshot := range r.accounts {
		if replacement, ok := snapshot.Deprecated[model]; ok {
			return replacement, true
		}
		if _, ok := snapshot.Models[model]; ok {
			found = true
		}
	}
	if found {
		return model, true
	}
	return model, false
}

func (r *antigravityModelRegistry) Models(pool *poolState) []AntigravityCatalogModel {
	r.mu.RLock()
	snapshots := make(map[string]AntigravityAccountSnapshot, len(r.accounts))
	for id, snapshot := range r.accounts {
		snapshots[id] = snapshot
	}
	r.mu.RUnlock()

	merged := make(map[string]*AntigravityCatalogModel)
	aliases := make(map[string]map[string]bool)
	deprecatedIDs := make(map[string]bool)
	fresh := make(map[string]bool)
	metadataTime := make(map[string]time.Time)
	accountIDs := make([]string, 0, len(snapshots))
	for accountID := range snapshots {
		accountIDs = append(accountIDs, accountID)
		for oldID := range snapshots[accountID].Deprecated {
			deprecatedIDs[oldID] = true
		}
	}
	sort.Strings(accountIDs)
	for _, accountID := range accountIDs {
		snapshot := snapshots[accountID]
		for oldID, replacement := range snapshot.Deprecated {
			if aliases[replacement] == nil {
				aliases[replacement] = make(map[string]bool)
			}
			aliases[replacement][oldID] = true
		}
		for id, model := range snapshot.Models {
			if deprecatedIDs[id] {
				continue
			}
			entry := merged[id]
			if entry == nil {
				copy := AntigravityCatalogModel{AntigravityModelInfo: model}
				entry = &copy
				merged[id] = entry
				metadataTime[id] = snapshot.FetchedAt
			} else if snapshot.FetchedAt.After(metadataTime[id]) {
				entry.AntigravityModelInfo = model
				metadataTime[id] = snapshot.FetchedAt
			}
			entry.SupportingAccounts++
			available, reset := antigravityAccountModelAvailable(pool, accountID, id)
			if available {
				entry.AvailableAccounts++
				entry.AvailableNow = true
			} else if !reset.IsZero() && (entry.NextResetAt.IsZero() || reset.Before(entry.NextResetAt)) {
				entry.NextResetAt = reset
			}
			if time.Since(snapshot.FetchedAt) <= 24*time.Hour {
				fresh[id] = true
			}
		}
	}
	result := make([]AntigravityCatalogModel, 0, len(merged))
	for id, entry := range merged {
		entry.Stale = !fresh[id]
		entry.Aliases = []string{"antigravity/" + id}
		for alias := range aliases[id] {
			entry.Aliases = append(entry.Aliases, alias)
		}
		sort.Strings(entry.Aliases)
		result = append(result, *entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func antigravityAccountModelAvailable(pool *poolState, accountID, model string) (bool, time.Time) {
	if pool == nil {
		return false, time.Time{}
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for _, account := range pool.accounts {
		if account.Type != AccountTypeAntigravity || account.ID != accountID {
			continue
		}
		account.mu.Lock()
		defer account.mu.Unlock()
		if account.Dead || account.Disabled || accountHealthBlockedLocked(account) {
			return false, time.Time{}
		}
		now := time.Now()
		until := antigravityModelCooldownLocked(account, model)
		discoveryAvailable, discoveryReset := pool.antigravity.models.DiscoveryAvailability(accountID, model, now)
		if discoveryReset.After(until) {
			until = discoveryReset
		}
		return !until.After(now) && discoveryAvailable, until
	}
	return false, time.Time{}
}

func parseAntigravityModelSnapshot(body []byte, fetchedAt time.Time) (AntigravityAccountSnapshot, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return AntigravityAccountSnapshot{}, err
	}
	var models map[string]map[string]json.RawMessage
	if err := json.Unmarshal(root["models"], &models); err != nil || len(models) == 0 {
		return AntigravityAccountSnapshot{}, fmt.Errorf("fetchAvailableModels returned no models")
	}
	webSearch := make(map[string]bool)
	var webSearchIDs []string
	_ = json.Unmarshal(root["webSearchModelIds"], &webSearchIDs)
	for _, id := range webSearchIDs {
		webSearch[id] = true
	}
	var webSearchMap map[string]bool
	if json.Unmarshal(root["webSearchModelIds"], &webSearchMap) == nil {
		for id, enabled := range webSearchMap {
			if enabled {
				webSearch[id] = true
			}
		}
	}
	snapshot := AntigravityAccountSnapshot{
		FetchedAt:  fetchedAt.UTC(),
		Models:     make(map[string]AntigravityModelInfo, len(models)),
		Deprecated: make(map[string]string),
		Raw:        root,
	}
	for id, raw := range models {
		id = strings.TrimSpace(id)
		if id == "" || strings.ContainsAny(id, " \t\r\n") {
			continue
		}
		if antigravityHiddenModelIDs[id] {
			continue
		}
		model := AntigravityModelInfo{ID: id, Raw: raw, WebSearch: webSearch[id], SupportsTools: true}
		decodeRaw(raw, "displayName", &model.DisplayName)
		if displayName := antigravityCorrectedDisplayNames[id]; displayName != "" {
			model.DisplayName = displayName
		}
		decodeRaw(raw, "maxTokens", &model.MaxTokens)
		decodeRaw(raw, "maxOutputTokens", &model.MaxOutputTokens)
		decodeRaw(raw, "supportsImages", &model.SupportsImages)
		decodeRaw(raw, "supportsThinking", &model.SupportsThinking)
		decodeRaw(raw, "thinkingBudget", &model.ThinkingBudget)
		decodeRaw(raw, "recommended", &model.Recommended)
		applyAntigravityModelCorrections(&model)
		var mimeMap map[string]bool
		if json.Unmarshal(raw["supportedMimeTypes"], &mimeMap) == nil {
			for mime, enabled := range mimeMap {
				if enabled {
					model.SupportedMimeTypes = append(model.SupportedMimeTypes, mime)
				}
			}
			sort.Strings(model.SupportedMimeTypes)
		}
		if len(model.SupportedMimeTypes) == 0 {
			_ = json.Unmarshal(raw["supportedMimeTypes"], &model.SupportedMimeTypes)
			sort.Strings(model.SupportedMimeTypes)
		}
		var quota struct {
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
		}
		if json.Unmarshal(raw["quotaInfo"], &quota) == nil {
			model.Quota.RemainingFraction = quota.RemainingFraction
			model.Quota.ResetTime, _ = time.Parse(time.RFC3339Nano, quota.ResetTime)
			// proto3 JSON omits zero values: a reset without a fraction is an
			// exhausted model, not an unknown one.
			if quota.RemainingFraction == nil && !model.Quota.ResetTime.IsZero() {
				model.Quota.RemainingFraction = new(float64)
			}
		}
		snapshot.Models[id] = model
	}
	var deprecated map[string]struct {
		NewModelID string `json:"newModelId"`
	}
	_ = json.Unmarshal(root["deprecatedModelIds"], &deprecated)
	for oldID, replacement := range deprecated {
		if replacement.NewModelID != "" {
			snapshot.Deprecated[oldID] = replacement.NewModelID
		}
	}
	var deprecatedStrings map[string]string
	if json.Unmarshal(root["deprecatedModelIds"], &deprecatedStrings) == nil {
		for oldID, replacement := range deprecatedStrings {
			if replacement != "" {
				snapshot.Deprecated[oldID] = replacement
			}
		}
	}
	return snapshot, nil
}

var antigravityHiddenModelIDs = map[string]bool{
	"chat_20706":                  true,
	"chat_23310":                  true,
	"tab_flash_lite_preview":      true,
	"tab_jump_flash_lite_preview": true,
	"gemini-2.5-flash-thinking":   true,
	"gemini-2.5-pro":              true,
}

var antigravityCorrectedDisplayNames = map[string]string{
	"gemini-2.5-flash":        "Gemini 2.5 Flash",
	"gemini-2.5-flash-lite":   "Gemini 2.5 Flash Lite",
	"gemini-3.6-flash-tiered": "Gemini 3.6 Flash (Tiered)",
	"gemini-3.7-flash-tiered": "Gemini 3.7 Flash (Tiered)",
	"gemini-3.8-flash-tiered": "Gemini 3.8 Flash (Tiered)",
}

func applyAntigravityModelCorrections(model *AntigravityModelInfo) {
	if model == nil {
		return
	}
	if displayName := antigravityCorrectedDisplayNames[model.ID]; displayName != "" {
		model.DisplayName = displayName
	}
	if model.ID == "gemini-3.1-flash-image" {
		model.MaxTokens = 131072
		model.MaxOutputTokens = 32768
		model.SupportsImages = true
		model.SupportsThinking = true
		model.SupportsTools = false
	}
}

func decodeRaw(raw map[string]json.RawMessage, key string, target any) {
	if value, ok := raw[key]; ok {
		_ = json.Unmarshal(value, target)
	}
}

func fetchAntigravityModels(ctx context.Context, rt *antigravityRuntime, transport http.RoundTripper, account *Account, bases ...*url.URL) (AntigravityAccountSnapshot, error) {
	snapshot, _, err := fetchAntigravityModelsWithQuota(ctx, rt, transport, account, bases...)
	return snapshot, err
}

// fetchAntigravityModelsWithQuota also reports why the quota summary is
// missing, so callers can act on a daily-host verification demand.
func fetchAntigravityModelsWithQuota(ctx context.Context, rt *antigravityRuntime, transport http.RoundTripper, account *Account, bases ...*url.URL) (AntigravityAccountSnapshot, error, error) {
	body := []byte(`{}`)
	var lastErr error
	for _, base := range bases {
		if base == nil {
			continue
		}
		u := *base
		u.Path = singleJoin(u.Path, "/v1internal:fetchAvailableModels")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return AntigravityAccountSnapshot{}, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", antigravityUserAgent(rt))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			lastErr = err
			continue
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("fetchAvailableModels failed: %s: %s", resp.Status, safeText(responseBody))
			continue
		}
		snapshot, err := parseAntigravityModelSnapshot(responseBody, time.Now())
		if err != nil {
			return AntigravityAccountSnapshot{}, nil, err
		}
		summary, summaryErr := fetchAntigravityQuotaSummary(ctx, rt, transport, account, bases...)
		if summaryErr == nil {
			snapshot.QuotaSummary = &summary
		}
		return snapshot, summaryErr, nil
	}
	if lastErr == nil {
		lastErr = errors.New("fetchAvailableModels has no configured upstream")
	}
	return AntigravityAccountSnapshot{}, nil, lastErr
}

// antigravityForbiddenError is a daily-host 403 from the quota summary. That
// endpoint enforces Google's account verification for the host that serves
// generation, so its answer describes the account state.
type antigravityForbiddenError struct {
	body []byte
}

func (e *antigravityForbiddenError) Error() string {
	return "retrieveUserQuotaSummary forbidden: " + safeText(e.body)
}

func parseAntigravityQuotaSummary(body []byte, fetchedAt time.Time) (AntigravityQuotaSummary, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return AntigravityQuotaSummary{}, err
	}
	type wireBucket struct {
		BucketID          string   `json:"bucketId"`
		DisplayName       string   `json:"displayName"`
		Description       string   `json:"description"`
		Window            string   `json:"window"`
		RemainingFraction *float64 `json:"remainingFraction"`
		RemainingAmount   *int64   `json:"remainingAmount"`
		Disabled          bool     `json:"disabled"`
		ResetTime         string   `json:"resetTime"`
	}
	decodeBucket := func(raw wireBucket) AntigravityQuotaBucket {
		reset, _ := time.Parse(time.RFC3339Nano, raw.ResetTime)
		// proto3 JSON omits zero values, so an enabled bucket without a
		// fraction or amount is exhausted, not unknown.
		if raw.RemainingFraction == nil && raw.RemainingAmount == nil && !raw.Disabled {
			raw.RemainingFraction = new(float64)
		}
		return AntigravityQuotaBucket{
			BucketID:          raw.BucketID,
			DisplayName:       raw.DisplayName,
			Description:       raw.Description,
			Window:            raw.Window,
			RemainingFraction: raw.RemainingFraction,
			RemainingAmount:   raw.RemainingAmount,
			Disabled:          raw.Disabled,
			ResetTime:         reset,
		}
	}

	var description string
	_ = json.Unmarshal(root["description"], &description)
	summary := AntigravityQuotaSummary{FetchedAt: fetchedAt.UTC(), Description: description, Raw: root}
	var buckets []wireBucket
	_ = json.Unmarshal(root["buckets"], &buckets)
	for _, bucket := range buckets {
		summary.Buckets = append(summary.Buckets, decodeBucket(bucket))
	}
	var groupObjects []map[string]json.RawMessage
	_ = json.Unmarshal(root["groups"], &groupObjects)
	for _, raw := range groupObjects {
		var displayName, groupDescription string
		var groupBuckets []wireBucket
		_ = json.Unmarshal(raw["displayName"], &displayName)
		_ = json.Unmarshal(raw["description"], &groupDescription)
		_ = json.Unmarshal(raw["buckets"], &groupBuckets)
		group := AntigravityQuotaGroup{DisplayName: displayName, Description: groupDescription, Raw: raw}
		for _, bucket := range groupBuckets {
			group.Buckets = append(group.Buckets, decodeBucket(bucket))
		}
		summary.Groups = append(summary.Groups, group)
	}
	if len(summary.Buckets) == 0 && len(summary.Groups) == 0 {
		return AntigravityQuotaSummary{}, errors.New("retrieveUserQuotaSummary returned no quota buckets")
	}
	return summary, nil
}

func fetchAntigravityQuotaSummary(ctx context.Context, rt *antigravityRuntime, transport http.RoundTripper, account *Account, bases ...*url.URL) (AntigravityQuotaSummary, error) {
	requestBody, err := json.Marshal(map[string]string{"project": account.google.ProjectID})
	if err != nil {
		return AntigravityQuotaSummary{}, err
	}
	var lastErr error
	for index, base := range bases {
		if base == nil {
			continue
		}
		u := *base
		u.Path = singleJoin(u.Path, "/v1internal:retrieveUserQuotaSummary")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(requestBody))
		if err != nil {
			return AntigravityQuotaSummary{}, err
		}
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", antigravityUserAgent(rt))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			lastErr = err
			continue
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		// Production can demand verification for an account the daily host
		// still serves; only the daily host's 403 describes the account.
		if resp.StatusCode == http.StatusForbidden && index == 0 {
			return AntigravityQuotaSummary{}, &antigravityForbiddenError{body: responseBody}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("retrieveUserQuotaSummary failed: %s: %s", resp.Status, safeText(responseBody))
			continue
		}
		return parseAntigravityQuotaSummary(responseBody, time.Now())
	}
	if lastErr == nil {
		lastErr = errors.New("retrieveUserQuotaSummary has no configured upstream")
	}
	return AntigravityQuotaSummary{}, lastErr
}

func antigravityQuotaWindow(bucket AntigravityQuotaBucket) string {
	value := strings.ToLower(strings.Join([]string{bucket.Window, bucket.BucketID, bucket.DisplayName}, " "))
	switch {
	case strings.Contains(value, "weekly") || strings.Contains(value, "week"):
		return "weekly"
	case strings.Contains(value, "5h") || strings.Contains(value, "five hour") || strings.Contains(value, "5-hour"):
		return "5h"
	default:
		return ""
	}
}

// allBuckets returns every bucket of the summary, top-level ones first.
func (s *AntigravityQuotaSummary) allBuckets() []AntigravityQuotaBucket {
	if s == nil {
		return nil
	}
	buckets := append([]AntigravityQuotaBucket(nil), s.Buckets...)
	for _, group := range s.Groups {
		buckets = append(buckets, group.Buckets...)
	}
	return buckets
}

// antigravityBucketsWindow returns the most consumed bucket of one window,
// with the reset it reports.
func antigravityBucketsWindow(buckets []AntigravityQuotaBucket, window string) (*float64, time.Time) {
	var remaining *float64
	var reset time.Time
	for _, bucket := range buckets {
		if bucket.Disabled || bucket.RemainingFraction == nil || antigravityQuotaWindow(bucket) != window {
			continue
		}
		value := *bucket.RemainingFraction
		betterReset := !bucket.ResetTime.IsZero() && (reset.IsZero() || bucket.ResetTime.Before(reset))
		if remaining == nil || value < *remaining || (value == *remaining && betterReset) {
			copy := value
			remaining = &copy
			reset = bucket.ResetTime
		}
	}
	return remaining, reset
}

func antigravitySummaryWindow(summary *AntigravityQuotaSummary, window string) (*float64, time.Time) {
	return antigravityBucketsWindow(summary.allBuckets(), window)
}

func antigravityModelQuotaFallback(snapshot AntigravityAccountSnapshot) (*float64, time.Time) {
	var remaining *float64
	var reset time.Time
	for _, model := range snapshot.Models {
		if model.Quota.RemainingFraction == nil {
			continue
		}
		value := *model.Quota.RemainingFraction
		betterReset := !model.Quota.ResetTime.IsZero() && (reset.IsZero() || model.Quota.ResetTime.Before(reset))
		if remaining == nil || value < *remaining || (value == *remaining && betterReset) {
			copy := value
			remaining = &copy
			reset = model.Quota.ResetTime
		}
	}
	return remaining, reset
}

func antigravityUsedFraction(remaining *float64) float64 {
	if remaining == nil {
		return 0
	}
	used := 1 - *remaining
	if used < 0 {
		return 0
	}
	if used > 1 {
		return 1
	}
	return used
}

func extractAntigravityAccountUsage(snapshot AntigravityAccountSnapshot) UsageSnapshot {
	fiveHourRemaining, fiveHourReset := antigravitySummaryWindow(snapshot.QuotaSummary, "5h")
	weeklyRemaining, weeklyReset := antigravitySummaryWindow(snapshot.QuotaSummary, "weekly")
	source := "antigravity-quota-summary"
	if fiveHourRemaining == nil {
		fiveHourRemaining, fiveHourReset = antigravityModelQuotaFallback(snapshot)
		source = "antigravity-model-quota"
	}
	fiveHourUsed := antigravityUsedFraction(fiveHourRemaining)
	weeklyUsed := antigravityUsedFraction(weeklyRemaining)
	usage := UsageSnapshot{
		PrimaryUsed:            fiveHourUsed,
		PrimaryUsedPercent:     fiveHourUsed,
		PrimaryUsageReported:   fiveHourRemaining != nil,
		PrimaryResetAt:         fiveHourReset,
		SecondaryUsed:          weeklyUsed,
		SecondaryUsedPercent:   weeklyUsed,
		SecondaryUsageReported: weeklyRemaining != nil,
		SecondaryResetAt:       weeklyReset,
		RetrievedAt:            snapshot.FetchedAt,
		Source:                 source,
		primarySet:             fiveHourRemaining != nil,
		secondarySet:           weeklyRemaining != nil,
	}
	if fiveHourRemaining != nil {
		usage.PrimaryWindowMinutes = 5 * 60
	}
	if weeklyRemaining != nil {
		usage.SecondaryWindowMinutes = 7 * 24 * 60
	}
	return usage
}

// syncAntigravityModels refreshes one account's model inventory and quota.
// The quota summary doubles as the account health probe: a daily-host success
// clears verification markers that only a served request could clear before,
// and a daily-host verification demand sets them.
func syncAntigravityModels(ctx context.Context, rt *antigravityRuntime, transport http.RoundTripper, account *Account, bases ...*url.URL) error {
	previous, hadPrevious := rt.models.AccountSnapshot(account.ID)
	snapshot, quotaErr, err := fetchAntigravityModelsWithQuota(ctx, rt, transport, account, bases...)
	if err != nil {
		return err
	}
	var forbidden *antigravityForbiddenError
	switch {
	case quotaErr == nil:
		clearAntigravityHealth(rt, account)
	case errors.As(quotaErr, &forbidden):
		if needsVerification, banned, _ := classifyAntigravityForbidden(forbidden.body); needsVerification || banned {
			recordAntigravityForbidden(rt, account, forbidden.body)
		}
	default:
		log.Printf("antigravity quota summary %s failed: %v", account.ID, quotaErr)
	}
	if snapshot.QuotaSummary == nil && hadPrevious && previous.QuotaSummary != nil {
		snapshot.QuotaSummary = previous.QuotaSummary
	}
	rt.models.ReplaceAccount(account.ID, snapshot)
	if account != nil {
		account.mu.Lock()
		account.Usage = extractAntigravityAccountUsage(snapshot)
		account.mu.Unlock()
	}
	if !hadPrevious || !antigravitySnapshotsEquivalent(previous, snapshot) {
		if err := saveAccount(rt, account); err != nil {
			return err
		}
	}
	if forbidden != nil {
		return fmt.Errorf("quota summary: %w", quotaErr)
	}
	return nil
}

func antigravitySnapshotsEquivalent(left, right AntigravityAccountSnapshot) bool {
	left.FetchedAt, right.FetchedAt = time.Time{}, time.Time{}
	if left.QuotaSummary != nil {
		summary := *left.QuotaSummary
		summary.FetchedAt = time.Time{}
		left.QuotaSummary = &summary
	}
	if right.QuotaSummary != nil {
		summary := *right.QuotaSummary
		summary.FetchedAt = time.Time{}
		right.QuotaSummary = &summary
	}
	return reflect.DeepEqual(left, right)
}

func isAntigravityModel(rt *antigravityRuntime, model string) bool {
	_, ok := rt.models.Canonical(model)
	return ok
}

func antigravityCanonicalModel(rt *antigravityRuntime, model string) string {
	canonical, _ := rt.models.Canonical(model)
	return canonical
}

func (p *poolState) selectAntigravityAccount(req routing.Request) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	model := antigravityCanonicalModel(p.antigravity, req.Model)
	conversationID, exclude := req.ConversationID, req.Exclude
	now := time.Now()
	p.evictOldestPinsLocked(now)
	pinKey := "antigravity:" + model + ":" + conversationID
	if conversationID != "" {
		// If conversation was previously pinned to an account of another provider, unpin it
		if pinnedID := p.convPin[conversationID]; pinnedID != "" {
			for _, account := range p.accounts {
				if account.ID == pinnedID && account.Type != AccountTypeAntigravity {
					p.unpinLocked(conversationID)
					break
				}
			}
		}
		if pinnedID := p.convPin[pinKey]; pinnedID != "" && (exclude == nil || !exclude[pinnedID]) {
			for _, account := range p.accounts {
				if account.ID != pinnedID || account.Type != AccountTypeAntigravity || !p.antigravity.models.Supports(account.ID, model) {
					continue
				}
				account.mu.Lock()
				until := antigravityModelCooldownLocked(account, model)
				discoveryAvailable, _ := p.antigravity.models.DiscoveryAvailability(account.ID, model, now)
				eligible := accountMatchesRouteLocked(account, req) && !until.After(now) && discoveryAvailable
				account.mu.Unlock()
				if eligible {
					if allowed, _, _ := p.circuitBreakers.AllowTarget(req.Provider, account.ID, model, nil); allowed {
						return account
					}
				}
			}
			p.unpinLocked(pinKey)
		} else if pinnedID := p.convPin[conversationID]; pinnedID != "" && (exclude == nil || !exclude[pinnedID]) {
			for _, account := range p.accounts {
				if account.ID != pinnedID || account.Type != AccountTypeAntigravity || !p.antigravity.models.Supports(account.ID, model) {
					continue
				}
				account.mu.Lock()
				until := antigravityModelCooldownLocked(account, model)
				discoveryAvailable, _ := p.antigravity.models.DiscoveryAvailability(account.ID, model, now)
				eligible := accountMatchesRouteLocked(account, req) && !until.After(now) && discoveryAvailable
				account.mu.Unlock()
				if eligible {
					if allowed, _, _ := p.circuitBreakers.AllowTarget(req.Provider, account.ID, model, nil); !allowed {
						continue
					}
					if err := p.pinForUserLocked(p.convOwner[conversationID], pinKey, account.ID, now); err != nil {
						return nil
					}
					return account
				}
			}
		}
	}
	var best *Account
	bestScore := -1e9
	for _, account := range p.accounts {
		if account.Type != AccountTypeAntigravity || (exclude != nil && exclude[account.ID]) || !p.antigravity.models.Supports(account.ID, model) {
			continue
		}
		if p.circuitBreakers != nil {
			if allowed, _, _ := p.circuitBreakers.CanAllowTarget(string(AccountTypeAntigravity), account.ID, model, nil); !allowed {
				continue
			}
		}
		account.mu.Lock()
		until := antigravityModelCooldownLocked(account, model)
		discoveryAvailable, _ := p.antigravity.models.DiscoveryAvailability(account.ID, model, now)
		eligible := accountMatchesRouteLocked(account, req) && !until.After(now) && discoveryAvailable
		score := scoreAccountLocked(account, now) - float64(atomic.LoadInt64(&account.Inflight))*0.02
		account.mu.Unlock()
		if eligible && (best == nil || score > bestScore) {
			best, bestScore = account, score
		}
	}
	if best != nil {
		if allowed, _, _ := p.circuitBreakers.AllowTarget(req.Provider, best.ID, model, nil); !allowed {
			return nil
		}
	}
	if best != nil && conversationID != "" {
		if err := p.pinForUserLocked(p.convOwner[conversationID], pinKey, best.ID, now); err != nil {
			return nil
		}
		if err := p.pinForUserLocked(p.convOwner[conversationID], conversationID, best.ID, now); err != nil {
			p.unpinLocked(pinKey)
			return nil
		}
	}
	return best
}

func setAntigravityModelCooldown(rt *antigravityRuntime, account *Account, model string, until time.Time) {
	setAntigravityCooldown(rt, account, antigravityCanonicalModel(rt, model), until)
}

// setAntigravityQuotaCooldown cools down the whole model family when the
// rate-limit body names a shared family bucket, and only the model otherwise.
func setAntigravityQuotaCooldown(rt *antigravityRuntime, account *Account, model string, body []byte, until time.Time) {
	setAntigravityCooldown(rt, account, antigravityRateLimitScope(rt, model, body), until)
}

func setAntigravityCooldown(rt *antigravityRuntime, account *Account, key string, until time.Time) {
	if account == nil || until.IsZero() {
		return
	}
	account.mu.Lock()
	if account.google.ModelRateLimits == nil {
		account.google.ModelRateLimits = make(map[string]time.Time)
	}
	if until.After(account.google.ModelRateLimits[key]) {
		account.google.ModelRateLimits[key] = until
	}
	account.mu.Unlock()
	_ = saveAccount(rt, account)
}

func clearAntigravityModelCooldown(rt *antigravityRuntime, account *Account, model string) {
	if account == nil {
		return
	}
	account.mu.Lock()
	canonical := antigravityCanonicalModel(rt, model)
	_, existed := account.google.ModelRateLimits[canonical]
	delete(account.google.ModelRateLimits, canonical)
	// A served request proves its family bucket has quota again.
	if family := antigravityQuotaFamily(canonical); family != "" {
		_, familyExisted := account.google.ModelRateLimits[antigravityFamilyCooldownKey(family)]
		delete(account.google.ModelRateLimits, antigravityFamilyCooldownKey(family))
		existed = existed || familyExisted
	}
	account.mu.Unlock()
	if existed {
		_ = saveAccount(rt, account)
	}
}

func antigravityFamilyCooldownKey(family string) string {
	return "family:" + family
}

// antigravityModelCooldownLocked returns when a model may be retried on the
// account, honoring both its own cooldown and its family's. Callers hold
// account.mu.
func antigravityModelCooldownLocked(account *Account, model string) time.Time {
	until := account.google.ModelRateLimits[model]
	if family := antigravityQuotaFamily(model); family != "" {
		if familyUntil := account.google.ModelRateLimits[antigravityFamilyCooldownKey(family)]; familyUntil.After(until) {
			until = familyUntil
		}
	}
	return until
}

// antigravityRateLimitScope picks the cooldown key for a 429. Only a
// structured provider-emitted bucket identifier establishes that the limit is
// shared by the family; generic RESOURCE_EXHAUSTED and free-form messages
// stay scoped to the exact model.
func antigravityRateLimitScope(rt *antigravityRuntime, model string, body []byte) string {
	model = antigravityCanonicalModel(rt, model)
	family := antigravityQuotaFamily(model)
	var root any
	if family != "" && json.Unmarshal(body, &root) == nil && antigravityBodyIdentifiesQuotaFamily(root, family) {
		return antigravityFamilyCooldownKey(family)
	}
	return model
}

func antigravityBodyIdentifiesQuotaFamily(value any, family string) bool {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			lowerKey := strings.ToLower(key)
			if text, ok := child.(string); ok && (strings.Contains(lowerKey, "quota") || strings.Contains(lowerKey, "bucket") || strings.Contains(lowerKey, "limit")) {
				identifier := strings.ToLower(strings.TrimSpace(text))
				if identifier == family+"-weekly" || identifier == family+"-5h" {
					return true
				}
			}
			if antigravityBodyIdentifiesQuotaFamily(child, family) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if antigravityBodyIdentifiesQuotaFamily(child, family) {
				return true
			}
		}
	}
	return false
}

func (h *proxyHandler) startAntigravityModelPoller(ctx context.Context) {
	syncAll := func() {
		provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
		if provider == nil {
			return
		}
		for _, account := range h.pool.allAccounts() {
			if account.Type != AccountTypeAntigravity {
				continue
			}
			checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if h.needsRefresh(account) {
				if err := h.refreshAccount(checkCtx, account); err != nil {
					log.Printf("antigravity refresh %s failed: %v", account.ID, err)
				}
			}
			if err := syncAntigravityModels(checkCtx, h.pool.antigravity, h.transport, account, provider.DailyURL(), provider.ProductionURL()); err != nil {
				log.Printf("antigravity sync %s failed: %v", account.ID, err)
			}
			if accountPlanCheckDue(account, time.Now()) {
				if err := h.checkAntigravityPlan(checkCtx, account); err != nil {
					log.Printf("antigravity plan check %s failed: %v", account.ID, err)
				}
			}
			cancel()
		}
	}
	go func() {
		syncAll()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncAll()
			}
		}
	}()
}
