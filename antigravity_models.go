package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	known    map[string]bool
}

var antigravityModels = &antigravityModelRegistry{accounts: make(map[string]AntigravityAccountSnapshot), known: make(map[string]bool)}

func (r *antigravityModelRegistry) Reset() {
	r.mu.Lock()
	r.accounts = make(map[string]AntigravityAccountSnapshot)
	r.known = make(map[string]bool)
	r.mu.Unlock()
}

func (r *antigravityModelRegistry) MarkAccount(accountID string) {
	if strings.TrimSpace(accountID) == "" {
		return
	}
	r.mu.Lock()
	if r.known == nil {
		r.known = make(map[string]bool)
	}
	r.known[accountID] = true
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
	if r.known == nil {
		r.known = make(map[string]bool)
	}
	r.known[accountID] = true
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
		return true // allow cold-start discovery/fallback accounts to be tried
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
		return true, time.Time{}
	}
	info, ok := snapshot.Models[model]
	if !ok {
		return false, time.Time{}
	}
	if info.Quota.RemainingFraction != nil && *info.Quota.RemainingFraction <= 0 && info.Quota.ResetTime.After(now) {
		return false, info.Quota.ResetTime
	}
	return true, time.Time{}
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
	for _, fallback := range antigravityFallbackModels {
		if fallback.ID == model {
			return model, true
		}
	}
	return model, false
}

func (r *antigravityModelRegistry) Models(pool *poolState) []AntigravityCatalogModel {
	r.mu.RLock()
	snapshots := make(map[string]AntigravityAccountSnapshot, len(r.accounts))
	for id, snapshot := range r.accounts {
		snapshots[id] = snapshot
	}
	knownAccounts := len(r.known)
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
	if len(merged) == 0 && knownAccounts > 0 {
		for _, fallback := range antigravityFallbackModels {
			copy := fallback
			copy.Stale = true
			merged[copy.ID] = &copy
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
		if account.Dead || account.Disabled || account.NeedsVerification {
			return false, time.Time{}
		}
		now := time.Now()
		until := account.ModelRateLimits[model]
		discoveryAvailable, discoveryReset := antigravityModels.DiscoveryAvailability(accountID, model, now)
		if discoveryReset.After(until) {
			until = discoveryReset
		}
		return !until.After(now) && discoveryAvailable, until
	}
	return false, time.Time{}
}

var antigravityFallbackModels = []AntigravityCatalogModel{
	{AntigravityModelInfo: AntigravityModelInfo{ID: "claude-opus-4-6-thinking", DisplayName: "Claude Opus 4.6 Thinking", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3-flash", DisplayName: "Gemini 3 Flash", SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3-flash-agent", DisplayName: "Gemini 3.5 Flash (High)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.1-flash-image", DisplayName: "Gemini 3.1 Flash Image", MaxTokens: 131072, MaxOutputTokens: 32768, SupportsImages: true, SupportsThinking: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-pro-agent", DisplayName: "Gemini 3.1 Pro (High)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.1-pro-low", DisplayName: "Gemini 3.1 Pro (Low)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gpt-oss-120b-medium", DisplayName: "GPT OSS 120B (Medium)", SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.1-flash-lite", DisplayName: "Gemini 3.1 Flash Lite", SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.5-flash-low", DisplayName: "Gemini 3.5 Flash (Medium)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.5-flash-extra-low", DisplayName: "Gemini 3.5 Flash (Low)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.6-flash-high", DisplayName: "Gemini 3.6 Flash (High)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.6-flash-medium", DisplayName: "Gemini 3.6 Flash (Medium)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.6-flash-low", DisplayName: "Gemini 3.6 Flash (Low)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.6-flash-tiered", DisplayName: "Gemini 3.6 Flash (Tiered)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.7-flash-high", DisplayName: "Gemini 3.7 Flash (High)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.7-flash-medium", DisplayName: "Gemini 3.7 Flash (Medium)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.7-flash-low", DisplayName: "Gemini 3.7 Flash (Low)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.7-flash-tiered", DisplayName: "Gemini 3.7 Flash (Tiered)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.8-flash-medium", DisplayName: "Gemini 3.8 Flash (Medium)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.8-flash-low", DisplayName: "Gemini 3.8 Flash (Low)", SupportsThinking: true, SupportsTools: true}},
	{AntigravityModelInfo: AntigravityModelInfo{ID: "gemini-3.8-flash-tiered", DisplayName: "Gemini 3.8 Flash (Tiered)", SupportsThinking: true, SupportsTools: true}},
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

func fetchAntigravityModels(ctx context.Context, transport http.RoundTripper, account *Account, bases ...*url.URL) (AntigravityAccountSnapshot, error) {
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
			return AntigravityAccountSnapshot{}, err
		}
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", antigravityUserAgent())
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
			return AntigravityAccountSnapshot{}, err
		}
		if summary, summaryErr := fetchAntigravityQuotaSummary(ctx, transport, account, bases...); summaryErr == nil {
			snapshot.QuotaSummary = &summary
		}
		return snapshot, nil
	}
	if lastErr == nil {
		lastErr = errors.New("fetchAvailableModels has no configured upstream")
	}
	return AntigravityAccountSnapshot{}, lastErr
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

func fetchAntigravityQuotaSummary(ctx context.Context, transport http.RoundTripper, account *Account, bases ...*url.URL) (AntigravityQuotaSummary, error) {
	requestBody, err := json.Marshal(map[string]string{"project": account.ProjectID})
	if err != nil {
		return AntigravityQuotaSummary{}, err
	}
	var lastErr error
	for _, base := range bases {
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
		req.Header.Set("User-Agent", antigravityUserAgent())
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

func antigravitySummaryWindow(summary *AntigravityQuotaSummary, window string) (*float64, time.Time) {
	if summary == nil {
		return nil, time.Time{}
	}
	var remaining *float64
	var reset time.Time
	consider := func(bucket AntigravityQuotaBucket) {
		if bucket.Disabled || bucket.RemainingFraction == nil || antigravityQuotaWindow(bucket) != window {
			return
		}
		value := *bucket.RemainingFraction
		betterReset := !bucket.ResetTime.IsZero() && (reset.IsZero() || bucket.ResetTime.Before(reset))
		if remaining == nil || value < *remaining || (value == *remaining && betterReset) {
			copy := value
			remaining = &copy
			reset = bucket.ResetTime
		}
	}
	for _, bucket := range summary.Buckets {
		consider(bucket)
	}
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			consider(bucket)
		}
	}
	return remaining, reset
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

func syncAntigravityModels(ctx context.Context, transport http.RoundTripper, account *Account, bases ...*url.URL) error {
	previous, hadPrevious := antigravityModels.AccountSnapshot(account.ID)
	snapshot, err := fetchAntigravityModels(ctx, transport, account, bases...)
	if err != nil {
		return err
	}
	if snapshot.QuotaSummary == nil && hadPrevious && previous.QuotaSummary != nil {
		snapshot.QuotaSummary = previous.QuotaSummary
	}
	antigravityModels.ReplaceAccount(account.ID, snapshot)
	if account != nil {
		account.mu.Lock()
		account.Usage = extractAntigravityAccountUsage(snapshot)
		account.mu.Unlock()
	}
	if hadPrevious && antigravitySnapshotsEquivalent(previous, snapshot) {
		return nil
	}
	return saveAccount(account)
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

func isAntigravityModel(model string) bool {
	_, ok := antigravityModels.Canonical(model)
	return ok
}

func antigravityCanonicalModel(model string) string {
	canonical, _ := antigravityModels.Canonical(model)
	return canonical
}

func (p *poolState) candidateForAntigravityModel(conversationID string, exclude map[string]bool, model, clientIP string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	model = antigravityCanonicalModel(model)
	now := time.Now()
	pinKey := "antigravity:" + model + ":" + conversationID
	if conversationID != "" {
		// If conversation was previously pinned to an account of another provider, unpin it
		if pinnedID := p.convPin[conversationID]; pinnedID != "" {
			for _, account := range p.accounts {
				if account.ID == pinnedID && account.Type != AccountTypeAntigravity {
					delete(p.convPin, conversationID)
					break
				}
			}
		}
		if pinnedID := p.convPin[pinKey]; pinnedID != "" && (exclude == nil || !exclude[pinnedID]) {
			for _, account := range p.accounts {
				if account.ID != pinnedID || account.Type != AccountTypeAntigravity || !antigravityModels.Supports(account.ID, model) {
					continue
				}
				account.mu.Lock()
				until := account.ModelRateLimits[model]
				discoveryAvailable, _ := antigravityModels.DiscoveryAvailability(account.ID, model, now)
				eligible := !account.Dead && !account.Disabled && !account.NeedsVerification && accountAllowsClientIPLocked(account, clientIP) && !until.After(now) && discoveryAvailable
				account.mu.Unlock()
				if eligible {
					return account
				}
			}
			delete(p.convPin, pinKey)
		} else if pinnedID := p.convPin[conversationID]; pinnedID != "" && (exclude == nil || !exclude[pinnedID]) {
			for _, account := range p.accounts {
				if account.ID != pinnedID || account.Type != AccountTypeAntigravity || !antigravityModels.Supports(account.ID, model) {
					continue
				}
				account.mu.Lock()
				until := account.ModelRateLimits[model]
				discoveryAvailable, _ := antigravityModels.DiscoveryAvailability(account.ID, model, now)
				eligible := !account.Dead && !account.Disabled && !account.NeedsVerification && accountAllowsClientIPLocked(account, clientIP) && !until.After(now) && discoveryAvailable
				account.mu.Unlock()
				if eligible {
					p.convPin[pinKey] = account.ID
					return account
				}
			}
		}
	}
	var best *Account
	bestScore := -1e9
	for _, account := range p.accounts {
		if account.Type != AccountTypeAntigravity || (exclude != nil && exclude[account.ID]) || !antigravityModels.Supports(account.ID, model) {
			continue
		}
		if p.circuitBreakers != nil {
			if allowed, _, _ := p.circuitBreakers.AllowTarget(string(AccountTypeAntigravity), account.ID, model, nil); !allowed {
				continue
			}
		}
		account.mu.Lock()
		until := account.ModelRateLimits[model]
		discoveryAvailable, _ := antigravityModels.DiscoveryAvailability(account.ID, model, now)
		eligible := !account.Dead && !account.Disabled && !account.NeedsVerification && accountAllowsClientIPLocked(account, clientIP) && !until.After(now) && discoveryAvailable
		score := scoreAccountLocked(account, now) - float64(atomic.LoadInt64(&account.Inflight))*0.02
		account.mu.Unlock()
		if eligible && (best == nil || score > bestScore) {
			best, bestScore = account, score
		}
	}
	if best != nil && conversationID != "" {
		p.convPin[pinKey] = best.ID
		p.convPin[conversationID] = best.ID
	}
	return best
}

func setAntigravityModelCooldown(account *Account, model string, until time.Time) {
	if account == nil || until.IsZero() {
		return
	}
	account.mu.Lock()
	if account.ModelRateLimits == nil {
		account.ModelRateLimits = make(map[string]time.Time)
	}
	model = antigravityCanonicalModel(model)
	if until.After(account.ModelRateLimits[model]) {
		account.ModelRateLimits[model] = until
	}
	account.mu.Unlock()
	_ = saveAccount(account)
}

func clearAntigravityModelCooldown(account *Account, model string) {
	if account == nil {
		return
	}
	account.mu.Lock()
	canonical := antigravityCanonicalModel(model)
	_, existed := account.ModelRateLimits[canonical]
	delete(account.ModelRateLimits, canonical)
	account.mu.Unlock()
	if existed {
		_ = saveAccount(account)
	}
}

func (h *proxyHandler) startAntigravityModelPoller() {
	if h == nil {
		return
	}
	syncAll := func() {
		provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
		if provider == nil {
			return
		}
		for _, account := range h.pool.allAccounts() {
			if account.Type != AccountTypeAntigravity {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if h.needsRefresh(account) {
				_ = h.refreshAccount(ctx, account)
			}
			_ = syncAntigravityModels(ctx, h.transport, account, provider.DailyURL(), provider.ProductionURL())
			cancel()
		}
	}
	go func() {
		syncAll()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			syncAll()
		}
	}()
}

func (p *poolState) candidateForAntigravityModelWithTrace(conversationID string, exclude map[string]bool, model, clientIP string) (*Account, string, []string, float64, []RouteAlternative, *ScoreBreakdownView) {
	acc := p.candidateForAntigravityModel(conversationID, exclude, model, clientIP)
	if acc == nil {
		return nil, "none", nil, 0, nil, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	policy := "balanced"
	reasons := []string{"healthy"}

	canonical := antigravityCanonicalModel(model)
	pinKey := "antigravity:" + canonical + ":" + conversationID
	if conversationID != "" && (p.convPin[pinKey] == acc.ID || p.convPin[conversationID] == acc.ID) {
		policy = "pinned"
		reasons = append(reasons, "conversation_pin")
	}

	acc.mu.Lock()
	sb := scoreAccountBreakdownLocked(acc, now)
	inflight := atomic.LoadInt64(&acc.Inflight)
	hadHealthError := acc.NeedsVerification || acc.HealthError != ""
	acc.mu.Unlock()

	score := sb.Score - float64(inflight)*0.02
	reasons = append(reasons, "quota_headroom")

	breakdownView := newScoreBreakdownView(sb, inflight, hadHealthError)

	var alternatives []RouteAlternative
	for _, a := range p.accounts {
		if a == nil || a.ID == acc.ID || a.Type != AccountTypeAntigravity || a.Dead || a.Disabled {
			continue
		}
		a.mu.Lock()
		altSB := scoreAccountBreakdownLocked(a, now)
		altInflight := atomic.LoadInt64(&a.Inflight)
		altHealthErr := a.NeedsVerification || a.HealthError != ""
		a.mu.Unlock()

		altScore := altSB.Score - float64(altInflight)*0.02
		altReasons := []string{"healthy", "quota_headroom"}
		altView := newScoreBreakdownView(altSB, altInflight, altHealthErr)

		alternatives = append(alternatives, RouteAlternative{
			Provider:       string(a.Type),
			Model:          model,
			Score:          altScore,
			Reasons:        altReasons,
			AccountID:      a.ID,
			ScoreBreakdown: altView,
		})
	}

	return acc, policy, reasons, score, alternatives, breakdownView
}
