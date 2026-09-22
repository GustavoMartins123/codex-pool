package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// RequestCapabilities describes the functional and resource requirements of a request.
type RequestCapabilities struct {
	ContextTokens    int      `json:"context_tokens"`
	MaxOutputTokens  int      `json:"max_output_tokens"`
	Modalities       []string `json:"modalities"` // "text", "image", "video", "audio"
	Tools            bool     `json:"tools"`
	WebSearch        bool     `json:"web_search"`
	Reasoning        bool     `json:"reasoning"`
	RequiredProtocol string   `json:"required_protocol,omitempty"`
}

// extractRequestCapabilities inspects the endpoint path, body, and headers
// to identify what capabilities the request strictly requires.
func extractRequestCapabilities(path string, body []byte, headers http.Header) RequestCapabilities {
	caps := RequestCapabilities{
		Modalities: []string{"text"},
	}

	if len(body) == 0 {
		return caps
	}

	// Approximate input token count (4 chars ~= 1 token)
	caps.ContextTokens = len(body) / 4

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return caps
	}

	// Check max tokens / max completion tokens
	if maxT, ok := obj["max_tokens"].(float64); ok && maxT > 0 {
		caps.MaxOutputTokens = int(maxT)
	} else if maxCT, ok := obj["max_completion_tokens"].(float64); ok && maxCT > 0 {
		caps.MaxOutputTokens = int(maxCT)
	}

	// Check tools / functions
	if tools, ok := obj["tools"].([]any); ok && len(tools) > 0 {
		caps.Tools = true
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				name, _ := tm["name"].(string)
				tType, _ := tm["type"].(string)
				if strings.Contains(strings.ToLower(name), "search") || strings.Contains(strings.ToLower(tType), "search") {
					caps.WebSearch = true
				}
				if fn, ok := tm["function"].(map[string]any); ok {
					fnName, _ := fn["name"].(string)
					if strings.Contains(strings.ToLower(fnName), "search") {
						caps.WebSearch = true
					}
				}
			}
		}
	}
	if funcs, ok := obj["functions"].([]any); ok && len(funcs) > 0 {
		caps.Tools = true
	}

	// Check web search explicit flags or parameters
	if ws, ok := obj["web_search"].(bool); ok && ws {
		caps.WebSearch = true
	}

	// Check reasoning / thinking
	if _, ok := obj["reasoning_effort"]; ok {
		caps.Reasoning = true
	}
	if thinking, ok := obj["thinking"].(map[string]any); ok && len(thinking) > 0 {
		caps.Reasoning = true
	}

	// Check for multimodal image inputs
	hasImage := false
	bodyStr := string(body)
	if strings.Contains(bodyStr, `"image_url"`) ||
		strings.Contains(bodyStr, `"data:image/`) ||
		strings.Contains(bodyStr, `"input_image"`) ||
		strings.Contains(bodyStr, `"type":"image"`) ||
		strings.Contains(bodyStr, `"type": "image"`) {
		hasImage = true
	}

	if hasImage {
		caps.Modalities = append(caps.Modalities, "image")
	}

	return caps
}

// ModelMetadata represents standard capability and limit metadata for a candidate model.
type ModelMetadata struct {
	ID            string
	Provider      AccountType
	ContextWindow int
	MaxTokens     int
	Reasoning     bool
	WebSearch     bool
	SupportsImage bool
	SupportsTools bool
}

// lookupModelMetadata retrieves canonical model specifications across all catalogs.
func lookupModelMetadata(modelID string, pool *poolState) (ModelMetadata, bool) {
	modelID = strings.TrimSpace(modelID)
	lower := strings.ToLower(modelID)

	// 1. Models configured or discovered on provider accounts.
	if pool != nil {
		for _, account := range pool.allAccounts() {
			account.mu.Lock()
			discovered, ok := accountDiscoveredModel(account, modelID)
			accountType := account.Type
			account.mu.Unlock()
			if !ok {
				continue
			}
			return ModelMetadata{
				ID: discovered.ID, Provider: accountType,
				ContextWindow: discovered.ContextWindow, MaxTokens: discovered.MaxOutputTokens,
				Reasoning: discovered.Reasoning, WebSearch: discovered.WebSearch,
				SupportsImage: containsString(discovered.Modalities, "image"), SupportsTools: true,
			}, true
		}
	}

	// 2. Antigravity models
	if strings.HasPrefix(lower, "antigravity/") || strings.Contains(lower, "gemini") {
		canonical := strings.TrimPrefix(lower, "antigravity/")
		if pool != nil {
			for _, m := range antigravityModels.Models(pool) {
				if strings.EqualFold(m.ID, canonical) || strings.EqualFold("antigravity/"+m.ID, modelID) {
					return ModelMetadata{
						ID:            "antigravity/" + m.ID,
						Provider:      AccountTypeAntigravity,
						ContextWindow: m.MaxTokens,
						MaxTokens:     m.MaxOutputTokens,
						Reasoning:     m.SupportsThinking,
						WebSearch:     m.WebSearch,
						SupportsImage: m.SupportsImages,
						SupportsTools: m.SupportsTools,
					}, true
				}
			}
		}
		// Default Antigravity Gemini specs
		return ModelMetadata{
			ID:            modelID,
			Provider:      AccountTypeAntigravity,
			ContextWindow: 1048576,
			MaxTokens:     65536,
			Reasoning:     true,
			WebSearch:     true,
			SupportsImage: true,
			SupportsTools: true,
		}, true
	}

	// 3. Main catalog (poolModels)
	for _, m := range poolModels {
		if strings.EqualFold(m.ID, modelID) {
			supportsImg := false
			for _, inp := range m.Input {
				if inp == "image" {
					supportsImg = true
					break
				}
			}
			return ModelMetadata{
				ID:            m.ID,
				Provider:      m.AccountType,
				ContextWindow: m.ContextWindow,
				MaxTokens:     m.MaxTokens,
				Reasoning:     m.Reasoning,
				WebSearch:     m.WebSearch,
				SupportsImage: supportsImg,
				SupportsTools: true,
			}, true
		}
		for _, alias := range m.Aliases {
			if strings.EqualFold(alias, modelID) {
				supportsImg := false
				for _, inp := range m.Input {
					if inp == "image" {
						supportsImg = true
						break
					}
				}
				return ModelMetadata{
					ID:            m.ID,
					Provider:      m.AccountType,
					ContextWindow: m.ContextWindow,
					MaxTokens:     m.MaxTokens,
					Reasoning:     m.Reasoning,
					WebSearch:     m.WebSearch,
					SupportsImage: supportsImg,
					SupportsTools: true,
				}, true
			}
		}
	}

	// 4. Grok catalog
	for _, g := range grokModelCatalog {
		if strings.EqualFold(g.ID, modelID) || strings.EqualFold(g.Name, modelID) {
			return ModelMetadata{
				ID:            g.ID,
				Provider:      AccountTypeGrok,
				ContextWindow: g.ContextWindow,
				MaxTokens:     g.MaxTokens,
				Reasoning:     g.Reasoning,
				WebSearch:     g.WebSearch,
				SupportsImage: true,
				SupportsTools: true,
			}, true
		}
	}

	// 5. Default fallback for standard OpenAI models
	if isOpenAIModel(modelID) {
		return ModelMetadata{
			ID:            modelID,
			Provider:      AccountTypeCodex,
			ContextWindow: 272000,
			MaxTokens:     128000,
			Reasoning:     true,
			WebSearch:     true,
			SupportsImage: true,
			SupportsTools: true,
		}, true
	}

	return ModelMetadata{}, false
}

// isModelCompatible validates all capability gates for a target model.
// If any required capability is missing, it returns false with a descriptive reason.
func isModelCompatible(modelID string, caps RequestCapabilities, pool *poolState) (bool, string) {
	meta, ok := lookupModelMetadata(modelID, pool)
	if !ok {
		// If model is unknown, cannot guarantee compatibility
		return false, fmt.Sprintf("model_%s_unknown", modelID)
	}

	// Gate 1: Context window
	if caps.ContextTokens > 0 && meta.ContextWindow > 0 {
		if caps.ContextTokens > meta.ContextWindow {
			return false, fmt.Sprintf("context_window_too_small(%d>%d)", caps.ContextTokens, meta.ContextWindow)
		}
	}

	// Gate 2: Image modality
	requiresImage := false
	for _, mod := range caps.Modalities {
		if mod == "image" {
			requiresImage = true
			break
		}
	}
	if requiresImage && !meta.SupportsImage {
		return false, "image_modality_unsupported"
	}

	// Gate 3: Tools / Function calling
	if caps.Tools && !meta.SupportsTools {
		return false, "tools_unsupported"
	}

	// Gate 4: Web search
	if caps.WebSearch && !meta.WebSearch {
		return false, "web_search_unsupported"
	}

	// Gate 5: Reasoning
	if caps.Reasoning && !meta.Reasoning {
		return false, "reasoning_unsupported"
	}

	// Gate 6: Max output tokens
	if caps.MaxOutputTokens > 0 && meta.MaxTokens > 0 {
		if caps.MaxOutputTokens > meta.MaxTokens {
			return false, fmt.Sprintf("max_output_tokens_exceeded(%d>%d)", caps.MaxOutputTokens, meta.MaxTokens)
		}
	}

	return true, "compatible"
}

// isPoolAutoModel reports whether the given model name requests automatic pool orchestration.
func isPoolAutoModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	return lower == "pool/auto" ||
		strings.HasPrefix(lower, "pool/auto-") ||
		strings.HasPrefix(lower, "pool/auto:") ||
		lower == "auto"
}

// parsePoolAutoProfile extracts the requested profile name (e.g. "fast", "quality").
func parsePoolAutoProfile(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "fast"):
		return "fast"
	case strings.Contains(lower, "quality"):
		return "quality"
	case strings.Contains(lower, "efficient"):
		return "efficient"
	case strings.Contains(lower, "long-context") || strings.Contains(lower, "1m"):
		return "long-context"
	default:
		return "balanced"
	}
}
