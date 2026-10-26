package runnerproto

import (
	"errors"
	"strings"
)

func ValidateAgentModels(models []AgentModel) error {
	if len(models) > 512 {
		return errors.New("too many runner models")
	}
	seen := map[string]bool{}
	for _, model := range models {
		key := model.Agent + "|" + model.Model
		if (model.Agent != "codex" && model.Agent != "claude") || len(model.Model) == 0 || len(model.Model) > 256 ||
			strings.TrimSpace(model.Model) != model.Model || strings.ContainsAny(model.Model, "\x00\r\n") || seen[key] || len(model.ReasoningEfforts) > 8 {
			return errors.New("invalid runner model")
		}
		seen[key] = true
		efforts := map[string]bool{}
		for _, effort := range model.ReasoningEfforts {
			if !ValidReasoningEffort(effort) || effort == "" || efforts[effort] || model.Agent == "claude" && (effort == "none" || effort == "minimal") {
				return errors.New("invalid runner model effort")
			}
			efforts[effort] = true
		}
		if model.DefaultReasoningEffort != "" && !efforts[model.DefaultReasoningEffort] {
			return errors.New("invalid runner model default")
		}
	}
	return nil
}

func SupportsModel(models []AgentModel, agent, model, effort string) bool {
	for _, candidate := range models {
		if candidate.Agent != agent || candidate.Model != model {
			continue
		}
		if effort == "" {
			return true
		}
		for _, level := range candidate.ReasoningEfforts {
			if effort == level {
				return true
			}
		}
	}
	return false
}
