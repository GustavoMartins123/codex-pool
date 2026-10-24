package main

import (
	"strings"

	"codex-pool-proxy/internal/runnerproto"
)

// Capability information belongs to the pool, never to a mobile model-name heuristic.
func modelReasoningEfforts(pool *poolState, model poolModelDescriptor) []string {
	result := []string{}
	if !model.Capabilities["reasoning"] {
		return result
	}
	if model.Provider == string(AccountTypeCodex) && pool != nil {
		base := strings.TrimSuffix(model.ID, "[1m]")
		first := true
		for _, account := range pool.allAccounts() {
			if account.Type != AccountTypeCodex || (pool.catalogAccountAllows != nil && !pool.catalogAccountAllows(account, model.ID)) {
				continue
			}
			account.mu.Lock()
			discovered, ok := accountDiscoveredModel(account, base)
			levels := append([]string(nil), discovered.ReasoningEfforts...)
			account.mu.Unlock()
			if !ok {
				continue
			}
			if first {
				for _, level := range levels {
					if runnerproto.ValidReasoningEffort(level) && level != "" && !supportsAgent(result, level) {
						result = append(result, level)
					}
				}
				first = false
			} else {
				kept := result[:0]
				for _, level := range result {
					if supportsAgent(levels, level) {
						kept = append(kept, level)
					}
				}
				result = kept
			}
		}
		return result
	}
	id := strings.TrimPrefix(model.ID, "antigravity/")
	if model.Provider == string(AccountTypeClaude) || model.Provider == string(AccountTypeAntigravity) {
		// https://code.claude.com/docs/en/model-config#adjust-effort-level
		switch id {
		case "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-opus-5", "claude-sonnet-5", "claude-fable-5-1", "claude-fable-5", "claude-opus-4-8", "claude-opus-4-7":
			return []string{"low", "medium", "high", "xhigh", "max"}
		case "claude-opus-4-6", "claude-sonnet-4-6":
			return []string{"low", "medium", "high", "max"}
		}
	}
	if model.Provider == string(AccountTypeGemini) || model.Provider == string(AccountTypeAntigravity) {
		// https://ai.google.dev/gemini-api/docs/thinking
		switch id {
		case "gemini-4-argon", "gemini-3.8-flash", "gemini-3.1-pro-preview":
			return []string{"low", "medium", "high"}
		case "gemini-3.6-flash", "gemini-3.5-flash-lite", "gemini-3-flash-preview":
			return []string{"minimal", "low", "medium", "high"}
		case "gemini-3-pro-preview":
			return []string{"low", "high"}
		}
	}
	return result
}

func modelDefaultReasoningEffort(pool *poolState, model poolModelDescriptor) string {
	if model.Provider != string(AccountTypeCodex) || pool == nil {
		return ""
	}
	base := strings.TrimSuffix(model.ID, "[1m]")
	value := ""
	for _, account := range pool.allAccounts() {
		if account.Type != AccountTypeCodex || (pool.catalogAccountAllows != nil && !pool.catalogAccountAllows(account, model.ID)) {
			continue
		}
		account.mu.Lock()
		discovered, ok := accountDiscoveredModel(account, base)
		account.mu.Unlock()
		if !ok {
			continue
		}
		if !supportsAgent(model.ReasoningEfforts, discovered.DefaultReasoningEffort) || discovered.DefaultReasoningEffort == "" {
			return ""
		}
		if value != "" && value != discovered.DefaultReasoningEffort {
			return ""
		}
		value = discovered.DefaultReasoningEffort
	}
	return value
}
