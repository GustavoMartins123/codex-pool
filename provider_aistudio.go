package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// Google AI Studio API keys call the public Gemini API
// (generativelanguage.googleapis.com) with the native generateContent wire
// format. Requests reuse the Antigravity Gemini translation so thought
// signatures survive tool calls, which Gemini 3 and later require.
const defaultAIStudioBase = "https://generativelanguage.googleapis.com"

// aistudioModelPrefix keeps AI Studio routes apart from the bare Gemini names
// that the Antigravity and Gemini CLI providers already own.
const aistudioModelPrefix = "aistudio/"

type AIStudioProvider struct {
	base *url.URL
}

func NewAIStudioProvider(base *url.URL) *AIStudioProvider { return &AIStudioProvider{base: base} }
func (p *AIStudioProvider) Type() AccountType             { return AccountTypeAIStudio }

type AIStudioAuthJSON struct {
	APIKey   string                 `json:"api_key"`
	Dead     bool                   `json:"dead"`
	Disabled bool                   `json:"disabled"`
	Models   *providerModelSnapshot `json:"provider_model_snapshot,omitempty"`
}

func (p *AIStudioProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var auth AIStudioAuthJSON
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	key := strings.TrimSpace(auth.APIKey)
	if key == "" {
		return nil, nil
	}
	account := &Account{
		Type: AccountTypeAIStudio, ID: strings.TrimSuffix(name, filepath.Ext(name)),
		File: path, AccessToken: key, PlanType: "api_key",
		Dead: auth.Dead, Disabled: auth.Disabled,
	}
	if auth.Models == nil || len(auth.Models.Models) == 0 {
		account.HealthError = "AI Studio model catalog is unavailable; model discovery is required"
	}
	return account, nil
}

func (p *AIStudioProvider) SetAuthHeaders(req *http.Request, acc *Account) {
	req.Header.Del("Authorization")
	req.Header.Set("x-goog-api-key", acc.AccessToken)
}

func (p *AIStudioProvider) RefreshToken(context.Context, *Account, http.RoundTripper) error {
	return nil
}

func (p *AIStudioProvider) ParseUsage(obj map[string]any) *RequestUsage {
	return (&AntigravityProvider{}).ParseUsage(obj)
}

func (p *AIStudioProvider) ParseUsageHeaders(*Account, http.Header) {}
func (p *AIStudioProvider) UpstreamURL(string) *url.URL             { return p.base }
func (p *AIStudioProvider) MatchesPath(string) bool                 { return false }
func (p *AIStudioProvider) NormalizePath(path string) string        { return path }
func (p *AIStudioProvider) DetectsSSE(path, contentType string) bool {
	return strings.Contains(path, "streamGenerateContent") || strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func isAIStudioModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), aistudioModelPrefix)
}

// aistudioUpstreamModel returns the Gemini API model id behind a public route.
func aistudioUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if isAIStudioModel(model) {
		model = model[len(aistudioModelPrefix):]
	}
	return strings.TrimPrefix(model, "models/")
}

func aistudioModelsURL(base *url.URL) *url.URL {
	target := *base
	target.Path = singleJoin(target.Path, "/v1beta/models")
	query := target.Query()
	query.Set("pageSize", "1000")
	target.RawQuery = query.Encode()
	return &target
}

// aistudioUnsupportedFamilies are not text coding models: media generation,
// embeddings, realtime audio, transcription, and agents that need their own
// tools or API (deep research, computer use, robotics).
var aistudioUnsupportedFamilies = []string{
	"embedding", "imagen", "veo", "tts", "aqa", "audio", "live", "lyria", "image", "nano-banana",
	"transcribe", "deep-research", "computer-use", "robotics",
}

// parseAIStudioModels reads the native Gemini models list:
// {"models":[{"name":"models/<id>","inputTokenLimit":...,"outputTokenLimit":...,
// "supportedGenerationMethods":["generateContent",...],"thinking":true}]}.
func parseAIStudioModels(body []byte) (map[string]DiscoveredModel, error) {
	var payload struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			Description                string   `json:"description"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			OutputTokenLimit           int      `json:"outputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			Thinking                   bool     `json:"thinking"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid AI Studio model list: %w", err)
	}
	names := make(map[string]int, len(payload.Models))
	for _, row := range payload.Models {
		names[strings.TrimSpace(row.DisplayName)]++
	}
	models := make(map[string]DiscoveredModel)
	for _, row := range payload.Models {
		id := strings.TrimPrefix(strings.TrimSpace(row.Name), "models/")
		if id == "" || !containsString(row.SupportedGenerationMethods, "generateContent") {
			continue
		}
		lower := strings.ToLower(id)
		unsupported := false
		for _, family := range aistudioUnsupportedFamilies {
			if strings.Contains(lower, family) {
				unsupported = true
				break
			}
		}
		if unsupported {
			continue
		}
		public := aistudioModelPrefix + id
		// Antigravity publishes the same Google display names, and AI Studio
		// reuses one name for several preview ids; the picker must tell them apart.
		display := strings.TrimSpace(row.DisplayName)
		if display == "" || names[display] > 1 {
			display = id
		}
		models[public] = DiscoveredModel{
			ID: public, DisplayName: display + " · AI Studio", Description: row.Description,
			ContextWindow: row.InputTokenLimit, MaxOutputTokens: row.OutputTokenLimit,
			Reasoning: row.Thinking, Protocol: "gemini", Modalities: []string{"text", "image"},
		}
	}
	if len(models) == 0 {
		return nil, errors.New("AI Studio returned no text generation models")
	}
	return models, nil
}
