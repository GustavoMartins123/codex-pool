package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"strings"

	"codex-pool-proxy/internal/wire"
)

// detectRequestFormat determines the API format from the request path.
func detectRequestFormat(path string) wire.RequestFormat {
	switch {
	case path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages?"):
		return wire.FormatClaude
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return wire.FormatOpenAI
	case strings.HasPrefix(path, "/v1/completions"):
		return wire.FormatOpenAI
	default:
		return wire.FormatUnknown
	}
}

// providerTargetFormat returns the format the provider expects.
func providerTargetFormat(accountType AccountType) wire.RequestFormat {
	switch capabilitiesFor(accountType).WireFormat {
	case WireAnthropic:
		return wire.FormatClaude
	case WireResponses, WireOpenAI:
		return wire.FormatOpenAI
	default:
		return wire.FormatUnknown
	}
}

func shouldTranslateResponsesToAnthropic(path string, targetFormat wire.RequestFormat) bool {
	if targetFormat != wire.FormatClaude {
		return false
	}
	return strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/responses")
}

func chatMessagesResponseDirection(requestDir wire.TranslateDirection) wire.TranslateDirection {
	switch requestDir {
	case wire.TranslateClaudeToOAI:
		return wire.TranslateOAIToClaude
	case wire.TranslateOAIToClaude:
		return wire.TranslateClaudeToOAI
	default:
		return wire.TranslateNone
	}
}

// isOpenAIModel returns true if the model name looks like an OpenAI model.
func isOpenAIModel(model string) bool {
	m := strings.ToLower(model)
	// Strip thinking suffix e.g. "gpt-4o(16384)"
	if idx := strings.LastIndex(m, "("); idx > 0 {
		m = m[:idx]
	}
	prefixes := []string{
		"gpt-", "o1", "o3", "o4", "chatgpt-",
		"text-davinci", "text-embedding", "dall-e",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	// Codex CLI models
	if strings.Contains(m, "codex") {
		return true
	}
	return false
}

// claudeCanonicalModel maps short Claude model names to their full API model IDs.
func claudeCanonicalModel(model string) string {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return model
	}
	lower := strings.ToLower(raw)
	if idx := strings.LastIndex(lower, "("); idx > 0 {
		raw = strings.TrimSpace(raw[:idx])
		lower = strings.TrimSpace(lower[:idx])
	}

	has1M := strings.Contains(lower, "[1m]")
	baseRaw := strings.TrimSpace(strings.ReplaceAll(raw, "[1m]", ""))
	baseLower := strings.TrimSpace(strings.ReplaceAll(lower, "[1m]", ""))

	var canonical string
	if strings.HasPrefix(baseLower, "claude") {
		canonical = baseRaw
	} else {
		switch baseLower {
		case "opus":
			canonical = "claude-opus-5-5"
		case "fable":
			canonical = "claude-fable-5-1"
		case "sonnet":
			canonical = "claude-sonnet-5-5"
		case "haiku":
			canonical = "claude-haiku-4-5-20251001"
		default:
			return model
		}
	}

	if has1M {
		return canonical + " [1m]"
	}
	return canonical
}

// isClaudeModel returns true if the model name looks like an Anthropic Claude model.
func isClaudeModel(model string) bool {
	m := strings.ToLower(model)
	if idx := strings.LastIndex(m, "("); idx > 0 {
		m = m[:idx]
	}
	if strings.HasPrefix(m, "claude") {
		return true
	}
	// Match tier keywords (sonnet, opus, fable, haiku) that are Claude-specific
	for _, kw := range []string{"sonnet", "opus", "fable", "haiku"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

func toInt64(v any) int64 {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// logTranslation logs format translation at debug level.
func logTranslation(reqID string, direction wire.TranslateDirection, debug bool) {
	if !debug {
		return
	}
	switch direction {
	case wire.TranslateClaudeToOAI:
		log.Printf("[%s] format translation: claude -> openai", reqID)
	case wire.TranslateOAIToClaude:
		log.Printf("[%s] format translation: openai -> claude", reqID)
	case wire.TranslateChatToResponses:
		log.Printf("[%s] format translation: chat completions -> responses api", reqID)
	case wire.TranslateCompletionsToResponses:
		log.Printf("[%s] format translation: completions -> responses api", reqID)
	case wire.TranslateResponsesToClaude:
		log.Printf("[%s] format translation: responses api -> claude", reqID)
	case wire.TranslateClaudeToResponses:
		log.Printf("[%s] format translation: claude -> responses api", reqID)
	}
}

func claudeModelEntry(slug, displayName string, contextWindow int) map[string]any {
	return map[string]any{
		"slug":                                 slug,
		"display_name":                         displayName,
		"description":                          displayName,
		"prefer_websockets":                    false,
		"support_verbosity":                    false,
		"default_verbosity":                    nil,
		"default_service_tier":                 nil,
		"default_reasoning_level":              "high",
		"default_reasoning_summary":            "none",
		"apply_patch_tool_type":                "freeform",
		"web_search_tool_type":                 "text",
		"shell_type":                           "shell_command",
		"input_modalities":                     []string{"text", "image"},
		"supports_image_detail_original":       false,
		"supports_parallel_tool_calls":         true,
		"supports_reasoning_summaries":         false,
		"supports_search_tool":                 false,
		"supported_in_api":                     true,
		"supported_reasoning_levels":           []any{},
		"experimental_supported_tools":         []any{},
		"truncation_policy":                    map[string]any{"mode": "tokens", "limit": 10000},
		"context_window":                       contextWindow,
		"max_context_window":                   contextWindow,
		"priority":                             100,
		"visibility":                           "list",
		"availability_nux":                     nil,
		"available_in_plans":                   []string{"plus", "pro", "team", "enterprise", "business"},
		"available_access_programs":            map[string]any{"cyber": []string{"standard"}},
		"minimal_client_version":               "0.1.0",
		"reasoning_summary_format":             "none",
		"model_messages":                       nil,
		"base_instructions":                    "",
		"comp_hash":                            "3000",
		"additional_speed_tiers":               []any{},
		"service_tiers":                        []any{},
		"guardian":                             nil,
		"model_specialty":                      nil,
		"tool_mode":                            nil,
		"multi_agent_version":                  nil,
		"multi_agent_reasoning_effort":         nil,
		"node_repl_disabled":                   false,
		"node_repl_auto_review_required":       false,
		"requires_sandboxed_review":            false,
		"include_apps_usage_instructions":      false,
		"include_plugin_usage_instructions":    false,
		"include_skills_usage_instructions":    false,
		"supports_experimental_context":        false,
		"supports_reasoning_effort_updates":    true,
		"supports_reasoning_summary_parameter": true,
		"use_responses_lite":                   true,
		"upgrade":                              nil,
	}
}

// injectClaudeModels adds local pool model entries to the Codex model catalog response
// so the Codex CLI can select models that the pool routes but upstream omits.
func injectClaudeModels(body []byte) []byte {
	// Decompress gzip if needed (upstream may return compressed data when
	// client's Accept-Encoding header is forwarded)
	actualBody := body
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err == nil {
			decompressed, err := io.ReadAll(gr)
			gr.Close()
			if err == nil {
				actualBody = decompressed
			}
		}
	}

	var catalog map[string]any
	if err := json.Unmarshal(actualBody, &catalog); err != nil {
		return body
	}
	models, ok := catalog["models"].([]any)
	if !ok {
		return body
	}

	existing := make(map[string]bool, len(models))
	for _, raw := range models {
		if model, ok := raw.(map[string]any); ok {
			if slug, _ := model["slug"].(string); slug != "" {
				existing[slug] = true
			}
		}
	}
	for _, model := range modelsForProvider(AccountTypeClaude) {
		if !existing[model.ID] {
			models = append(models, claudeModelEntry(model.ID, model.DisplayName, model.ContextWindow))
		}
	}
	for _, model := range antigravityModels.Models(nil) {
		slug := "antigravity/" + model.ID
		if !existing[slug] {
			models = append(models, claudeModelEntry(slug, model.DisplayName, model.MaxTokens))
		}
	}
	catalog["models"] = models

	out, err := json.Marshal(catalog)
	if err != nil {
		return body
	}
	return out
}
