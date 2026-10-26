package main

import (
	"encoding/json"
	"net/http"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func (h *proxyHandler) handleRunnerCatalog(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	runner, err := b.runnerCredential(r)
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	if h.pool == nil || h.cfg == nil || h.pool.accountAuthority != h.passport {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	visible, err := h.memberQuotaSnapshot(runner.PrincipalID, time.Now().UTC())
	if err != nil {
		respondJSONError(w, 503, "catalog authorization unavailable")
		return
	}
	respondJSON(w, runnerCatalog(poolModelsForClients(visible)))
}

func runnerCatalog(models []poolModelDescriptor) runnerproto.RunnerCatalog {
	result := runnerproto.RunnerCatalog{Models: []runnerproto.AgentModel{}}
	native := []map[string]any{}
	for _, model := range models {
		for _, agent := range []string{"codex", "claude"} {
			protocols := append(append([]string{}, model.Protocols...), model.Protocol)
			if agent == "codex" && !supportsAgent(protocols, "responses") && !supportsAgent(protocols, "openai") ||
				agent == "claude" && !supportsAgent(protocols, "anthropic") {
				continue
			}
			efforts := []string{}
			for _, effort := range model.ReasoningEfforts {
				if runnerproto.ValidReasoningEffort(effort) && effort != "" && (agent != "claude" || effort != "none" && effort != "minimal") {
					efforts = append(efforts, effort)
				}
			}
			defaultEffort := model.DefaultReasoningEffort
			if !supportsAgent(efforts, defaultEffort) {
				defaultEffort = ""
			}
			result.Models = append(result.Models, runnerproto.AgentModel{Agent: agent, Model: model.ID, ReasoningEfforts: efforts, DefaultReasoningEffort: defaultEffort})
			if agent != "codex" {
				continue
			}
			contextWindow := model.ContextWindow
			if contextWindow <= 0 {
				contextWindow = 128000
			}
			entry := claudeModelEntry(model.ID, model.Name, contextWindow)
			levels := []map[string]string{}
			for _, effort := range efforts {
				levels = append(levels, map[string]string{"effort": effort, "description": effort})
			}
			entry["supported_reasoning_levels"] = levels
			entry["default_reasoning_level"] = defaultEffort
			if defaultEffort == "" {
				entry["default_reasoning_level"] = "medium"
			}
			native = append(native, entry)
		}
	}
	raw, _ := json.Marshal(map[string]any{"models": native})
	result.CodexCatalog = string(raw)
	return result
}
