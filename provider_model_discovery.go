package main

import (
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
	"time"
)

const (
	providerModelPollInterval = 15 * time.Minute
)

type DiscoveredModel struct {
	ID                     string          `json:"id"`
	DisplayName            string          `json:"display_name,omitempty"`
	Description            string          `json:"description,omitempty"`
	ContextWindow          int             `json:"context_window,omitempty"`
	MaxOutputTokens        int             `json:"max_output_tokens,omitempty"`
	Reasoning              bool            `json:"reasoning,omitempty"`
	WebSearch              bool            `json:"web_search,omitempty"`
	Modalities             []string        `json:"modalities,omitempty"`
	Protocol               string          `json:"protocol,omitempty"`
	ReasoningEfforts       []string        `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string          `json:"default_reasoning_effort,omitempty"`
	CatalogEntry           json.RawMessage `json:"catalog_entry,omitempty"`
}

type providerModelSnapshot struct {
	FetchedAt time.Time                  `json:"fetched_at"`
	Models    map[string]DiscoveredModel `json:"models"`
}

type providerModelHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *providerModelHTTPError) Error() string {
	return fmt.Sprintf("model discovery failed: %s: %s", e.Status, e.Body)
}

func parseProviderModels(body []byte) (map[string]DiscoveredModel, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}

	var rows []map[string]json.RawMessage
	for _, key := range []string{"models", "data"} {
		if json.Unmarshal(root[key], &rows) == nil && len(rows) > 0 {
			break
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("model endpoint returned no models")
	}

	models := make(map[string]DiscoveredModel, len(rows))
	for _, row := range rows {
		model := DiscoveredModel{}
		decodeFirstString(row, &model.ID, "id", "slug", "model")
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		decodeFirstString(row, &model.DisplayName, "display_name", "name")
		decodeFirstString(row, &model.Description, "description")
		decodeFirstInt(row, &model.ContextWindow, "context_window", "context_length", "inputTokenLimit", "max_input_tokens")
		decodeFirstInt(row, &model.MaxOutputTokens, "max_output_tokens", "outputTokenLimit", "max_tokens")
		decodeFirstBool(row, &model.Reasoning, "supports_reasoning", "supports_thinking", "supports_reasoning_effort", "reasoning")
		if !model.Reasoning {
			model.Reasoning = hasNonEmptyArray(row, "supported_reasoning_levels", "reasoning_efforts")
		}
		var levels []struct {
			Effort string `json:"effort"`
		}
		if raw, ok := row["supported_reasoning_levels"]; ok {
			if err := json.Unmarshal(raw, &levels); err != nil {
				return nil, fmt.Errorf("invalid reasoning levels for model %s", model.ID)
			}
			for _, level := range levels {
				model.ReasoningEfforts = append(model.ReasoningEfforts, level.Effort)
			}
		}
		if raw, ok := row["reasoning_efforts"]; ok {
			if err := json.Unmarshal(raw, &model.ReasoningEfforts); err != nil {
				return nil, fmt.Errorf("invalid reasoning efforts for model %s", model.ID)
			}
		}
		if raw, ok := row["default_reasoning_level"]; ok {
			if err := json.Unmarshal(raw, &model.DefaultReasoningEffort); err != nil {
				return nil, fmt.Errorf("invalid default reasoning effort for model %s", model.ID)
			}
		}
		decodeFirstBool(row, &model.WebSearch, "supports_backend_search", "supports_search_tool", "web_search")

		model.Modalities = decodeStringArray(row, "input_modalities", "modalities")
		if len(model.Modalities) == 0 {
			model.Modalities = []string{"text"}
		}
		appendBoolModality(row, &model.Modalities, "supports_image_in", "image")
		appendBoolModality(row, &model.Modalities, "supports_video_in", "video")
		appendBoolModality(row, &model.Modalities, "supports_audio_in", "audio")
		applyDiscoveredModelDefaults(&model)
		if model.DisplayName == "" {
			model.DisplayName = model.ID
		}
		models[model.ID] = model
	}
	if len(models) == 0 {
		return nil, errors.New("model endpoint returned no usable models")
	}
	return models, nil
}

func applyDiscoveredModelDefaults(model *DiscoveredModel) {
	if model == nil {
		return
	}
	switch model.ID {
	case "gpt-daybreak-blue-latest":
		if model.DisplayName == "" {
			model.DisplayName = "GPT Daybreak Blue"
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = 372000
		}
		if model.MaxOutputTokens == 0 {
			model.MaxOutputTokens = 128000
		}
		model.Reasoning = true
		model.WebSearch = true
		if len(model.Modalities) == 1 {
			model.Modalities = []string{"text", "image"}
		}
	case "codex-auto-review":
		if model.MaxOutputTokens == 0 {
			model.MaxOutputTokens = 128000
		}
		model.Reasoning = true
	}
}

func decodeFirstString(row map[string]json.RawMessage, target *string, keys ...string) {
	for _, key := range keys {
		if raw, ok := row[key]; ok && json.Unmarshal(raw, target) == nil && *target != "" {
			return
		}
	}
}

func decodeFirstInt(row map[string]json.RawMessage, target *int, keys ...string) {
	for _, key := range keys {
		if raw, ok := row[key]; ok && json.Unmarshal(raw, target) == nil && *target > 0 {
			return
		}
	}
}

func decodeFirstBool(row map[string]json.RawMessage, target *bool, keys ...string) {
	for _, key := range keys {
		if raw, ok := row[key]; ok && json.Unmarshal(raw, target) == nil {
			return
		}
	}
}

func hasNonEmptyArray(row map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		var values []json.RawMessage
		if raw, ok := row[key]; ok && json.Unmarshal(raw, &values) == nil && len(values) > 0 {
			return true
		}
	}
	return false
}

func decodeStringArray(row map[string]json.RawMessage, keys ...string) []string {
	for _, key := range keys {
		var values []string
		if raw, ok := row[key]; ok && json.Unmarshal(raw, &values) == nil && len(values) > 0 {
			return values
		}
	}
	return nil
}

func appendBoolModality(row map[string]json.RawMessage, modalities *[]string, key, modality string) {
	var supported bool
	if raw, ok := row[key]; !ok || json.Unmarshal(raw, &supported) != nil || !supported {
		return
	}
	*modalities = append(*modalities, modality)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ModelDiscoverer is implemented by providers with an account-specific model list.
type ModelDiscoverer interface {
	ModelDiscoveryURL() *url.URL
}

// ModelDiscoveryHeaders sets headers required only for a provider's model list.
type ModelDiscoveryHeaders interface {
	SetModelDiscoveryHeaders(*http.Request)
}

func modelDiscoveryURL(base *url.URL, path string) *url.URL {
	if base == nil {
		return nil
	}
	target := *base
	target.Path = singleJoin(target.Path, path)
	return &target
}

func providerModelsURL(provider Provider) (*url.URL, bool) {
	discovery, ok := provider.(ModelDiscoverer)
	if !ok {
		return nil, false
	}
	target := discovery.ModelDiscoveryURL()
	return target, target != nil
}

func fetchProviderModels(ctx context.Context, transport http.RoundTripper, provider ProviderDriver, account *Account) (providerModelSnapshot, error) {
	target, ok := providerModelsURL(provider)
	if !ok {
		return providerModelSnapshot{}, errors.New("provider has no model discovery endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return providerModelSnapshot{}, err
	}
	provider.SetAuthHeaders(req, account)
	req.Header.Set("Accept", "application/json")
	if discovery, ok := provider.(ModelDiscoveryHeaders); ok {
		discovery.SetModelDiscoveryHeaders(req)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		return providerModelSnapshot{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return providerModelSnapshot{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providerModelSnapshot{}, &providerModelHTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: safeText(body)}
	}
	var models map[string]DiscoveredModel
	if provider.Type() == AccountTypeAIStudio {
		models, err = parseAIStudioModels(body)
	} else {
		models, err = parseProviderModels(body)
	}
	if err != nil {
		return providerModelSnapshot{}, err
	}
	if provider.Type() == AccountTypeQwen {
		models, err = qwenDiscoveredModels(models)
		if err != nil {
			return providerModelSnapshot{}, err
		}
	}
	if provider.Type() == AccountTypeCodex {
		if err := attachCodexCatalogEntries(body, models); err != nil {
			return providerModelSnapshot{}, err
		}
	}
	return providerModelSnapshot{FetchedAt: time.Now().UTC(), Models: models}, nil
}

func attachCodexCatalogEntries(body []byte, models map[string]DiscoveredModel) error {
	var catalog struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return fmt.Errorf("decode Codex model catalog: %w", err)
	}
	for _, raw := range catalog.Models {
		var entry struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("decode Codex model catalog entry: %w", err)
		}
		if model, ok := models[strings.TrimSpace(entry.Slug)]; ok {
			model.CatalogEntry = raw
			models[model.ID] = model
		}
	}
	for id, model := range models {
		if len(model.CatalogEntry) == 0 {
			return fmt.Errorf("Codex model %s has no catalog entry slug", id)
		}
	}
	return nil
}

func syncProviderModels(ctx context.Context, rt *antigravityRuntime, transport http.RoundTripper, registry *ProviderRegistry, account *Account) error {
	if registry == nil || account == nil {
		return errors.New("missing provider model discovery dependency")
	}
	provider := registry.ForType(account.Type)
	if provider == nil {
		return errors.New("provider not registered")
	}
	snapshot, err := fetchProviderModels(ctx, transport, provider, account)
	if err != nil {
		if providerRequiresModelDiscovery(account.Type) {
			account.mu.Lock()
			account.Models = nil
			account.ModelsFetchedAt = time.Time{}
			account.HealthError = discoveryFailureMessage(account.Type)
			saveErr := saveAccount(rt, account)
			account.mu.Unlock()
			if saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		return err
	}
	account.mu.Lock()
	changed := !reflect.DeepEqual(account.Models, snapshot.Models)
	account.Models = snapshot.Models
	account.ModelsFetchedAt = snapshot.FetchedAt
	if providerRequiresModelDiscovery(account.Type) {
		account.HealthError = ""
		saveErr := saveAccount(rt, account)
		account.mu.Unlock()
		if saveErr == nil && account.Type == AccountTypeAIStudio {
			// Listing models authenticates the key, so it clears a stale dead flag.
			restoreValidatedAccount(rt, account, "AI Studio model discovery")
		}
		return saveErr
	}
	if account.Type == AccountTypeCodex {
		hasDaybreak := accountHasDaybreak(snapshot.Models)
		if account.codex.CyberAccess != hasDaybreak {
			account.codex.CyberAccess = hasDaybreak
			changed = true
		}
	}
	account.mu.Unlock()
	if !changed {
		return nil
	}
	return saveAccount(rt, account)
}

func accountHasDaybreak(models map[string]DiscoveredModel) bool {
	for id := range models {
		if strings.Contains(strings.ToLower(id), "daybreak") {
			return true
		}
	}
	return false
}

func (h *proxyHandler) startProviderModelPoller(ctx context.Context) {
	syncAll := func() {
		for _, account := range h.pool.allAccounts() {
			if _, ok := providerModelsURL(h.registry.ForType(account.Type)); !ok {
				continue
			}
			checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if h.needsRefresh(account) {
				_ = h.refreshAccount(checkCtx, account)
			}
			_ = syncProviderModels(checkCtx, h.pool.antigravity, h.transport, h.registry, account)
			cancel()
		}
	}
	go func() {
		syncAll()
		ticker := time.NewTicker(providerModelPollInterval)
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

func discoveredModelsForPool(pool *poolState) []poolModelDescriptor {
	if pool == nil {
		return nil
	}
	type aggregate struct {
		model      DiscoveredModel
		provider   AccountType
		supporting int
		available  int
	}
	aggregates := make(map[string]*aggregate)
	now := time.Now()
	for _, account := range pool.allAccounts() {
		account.mu.Lock()
		for id, model := range account.Models {
			if poolModelIDExists(id) {
				continue
			}
			key := string(account.Type) + "\x00" + strings.ToLower(id)
			entry := aggregates[key]
			if entry == nil {
				entry = &aggregate{model: model, provider: account.Type}
				aggregates[key] = entry
			}
			entry.supporting++
			if accountAvailableForRoutingLocked(pool.antigravity, account, now) {
				entry.available++
			}
		}
		account.mu.Unlock()
	}

	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	descriptors := make([]poolModelDescriptor, 0, len(keys))
	for _, key := range keys {
		entry := aggregates[key]
		protocol := "anthropic"
		if entry.provider == AccountTypeCodex || entry.provider == AccountTypeGrok || entry.provider == AccountTypeQwen || entry.provider == AccountTypeAIStudio {
			protocol = "openai"
		} else if entry.model.Protocol != "" {
			protocol = entry.model.Protocol
		} else if pool.registry.isGenericType(entry.provider) {
			protocol = "openai"
		}
		capabilities := map[string]bool{"reasoning": entry.model.Reasoning, "tools": true}
		protocols := []string{protocol}
		if entry.provider == AccountTypeCodex {
			protocols = []string{"openai", "responses"}
		} else if entry.provider == AccountTypeQwen || entry.provider == AccountTypeAIStudio {
			protocols = []string{"openai", "responses", "anthropic"}
		}
		if entry.model.WebSearch {
			capabilities["web_search"] = true
		}
		contextWindow := entry.model.ContextWindow
		if entry.provider == AccountTypeQwen {
			// Apply the client profile to both current and persisted discovery snapshots.
			contextWindow = qwenClientContextWindow
		}
		descriptors = append(descriptors, poolModelDescriptor{
			ID: entry.model.ID, Name: entry.model.DisplayName, Description: entry.model.Description,
			Protocol: protocol, Protocols: protocols, Provider: string(entry.provider), UpstreamID: entry.model.ID,
			ContextWindow: contextWindow, MaxOutputTokens: entry.model.MaxOutputTokens,
			ReasoningEfforts: append([]string(nil), entry.model.ReasoningEfforts...), DefaultReasoningEffort: entry.model.DefaultReasoningEffort,
			Modalities: append([]string(nil), entry.model.Modalities...), Capabilities: capabilities,
			NativeTools:        nativeWebSearchTools(entry.provider, entry.model.WebSearch),
			SupportingAccounts: entry.supporting, AvailableAccounts: entry.available, AvailableNow: entry.available > 0,
		})
		if entry.provider == AccountTypeCodex {
			descriptors[len(descriptors)-1].CodexCatalogEntry = codexCatalogEntry(pool, entry.model.ID)
		}
	}
	return descriptors
}

// providerRequiresModelDiscovery reports providers whose routable models come
// only from their live catalog, so a failed discovery stops routing to them.
func providerRequiresModelDiscovery(accountType AccountType) bool {
	return accountType == AccountTypeQwen || accountType == AccountTypeAIStudio
}

func discoveryFailureMessage(accountType AccountType) string {
	name := "Qwen"
	if accountType == AccountTypeAIStudio {
		name = "AI Studio"
	}
	return name + " model discovery failed; refresh the catalog before routing requests"
}
