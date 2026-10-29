package runnerproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// ModelsRevision ignores report order and never exposes the command key.
func ModelsRevision(models []AgentModel) string {
	canonical := append([]AgentModel{}, models...)
	for i := range canonical {
		canonical[i].ReasoningEfforts = append([]string{}, canonical[i].ReasoningEfforts...)
		slices.Sort(canonical[i].ReasoningEfforts)
	}
	slices.SortFunc(canonical, func(a, b AgentModel) int { return strings.Compare(a.Agent+"|"+a.Model, b.Agent+"|"+b.Model) })
	return ContentRevision(canonical)
}

func ContentRevision(value any) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func ValidateCapabilities(c *ModelCapabilities) error {
	if c == nil || ValidateAgentModels(c.Models) != nil || c.Revision != ModelsRevision(c.Models) {
		return errors.New("invalid runner catalog revision")
	}
	return nil
}

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
