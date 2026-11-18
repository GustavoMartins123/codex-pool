package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
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
	catalog := runnerCatalog(poolModelsForClients(visible))
	catalog.Revision = runnerproto.ContentRevision(catalog)
	w.Header().Set("ETag", `"`+catalog.Revision+`"`)
	if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	respondJSON(w, catalog)
}

func runnerSupportsSession(r *connectedRunner, s *sessionRecord) bool {
	if s.Capabilities != nil {
		return runnerproto.SupportsModel(s.Capabilities.Models, s.Agent, s.Model, s.ReasoningEffort)
	}
	return !r.hello.ModelsKnown || runnerproto.SupportsModel(r.hello.Models, s.Agent, s.Model, s.ReasoningEffort)
}

func (b *sessionBroker) updateRunnerModels(r *connectedRunner, capabilities *runnerproto.ModelCapabilities) error {
	if err := runnerproto.ValidateCapabilities(capabilities); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return sessionError(409, "runner connection replaced")
	}
	for _, model := range capabilities.Models {
		if !supportsAgent(r.hello.Agents, model.Agent) {
			return sessionError(400, "unavailable runner agent")
		}
	}
	if r.hello.CatalogRevision == capabilities.Revision {
		return nil
	}
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketRunners))
		var stored runnerRecord
		if err := json.Unmarshal(bucket.Get([]byte(r.id)), &stored); err != nil {
			return err
		}
		stored.Runner.ModelsKnown, stored.Runner.Models, stored.Runner.CatalogRevision = true, capabilities.Models, capabilities.Revision
		return putJSON(bucket, r.id, stored)
	}); err != nil {
		return err
	}
	r.hello.ModelsKnown, r.hello.Models, r.hello.CatalogRevision = true, capabilities.Models, capabilities.Revision
	b.signalCatalogLocked()
	for _, session := range b.sessions {
		if session.PrincipalID == r.principal && session.State == "pending_runner" {
			b.offerLocked(session)
		}
	}
	return nil
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
			var upstream map[string]any
			if agent == "codex" && model.Provider == string(AccountTypeCodex) && json.Unmarshal(model.CodexCatalogEntry, &upstream) != nil {
				continue
			}
			result.Models = append(result.Models, runnerproto.AgentModel{Agent: agent, Model: model.ID, ReasoningEfforts: efforts, DefaultReasoningEffort: defaultEffort})
			if agent != "codex" {
				continue
			}
			if upstream != nil {
				if stringValue(upstream["slug"]) != model.ID {
					upstream["slug"], upstream["display_name"], upstream["description"] = model.ID, model.Name, model.Description
					upstream["context_window"], upstream["max_context_window"] = model.ContextWindow, model.ContextWindow
				}
				native = append(native, upstream)
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

// finalizeCodexModelCatalog adds discovered Responses models and orders the
// combined catalog for the Codex model picker.
func finalizeCodexModelCatalog(body []byte, models []poolModelDescriptor) ([]byte, error) {
	var catalog map[string]any
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode Codex model catalog: %w", err)
	}
	existing, ok := catalog["models"].([]any)
	if !ok {
		return nil, fmt.Errorf("Codex model catalog has no models array")
	}

	var responses []poolModelDescriptor
	for _, model := range models {
		if supportsAgent(model.Protocols, "responses") || model.Provider == string(AccountTypeCodex) {
			responses = append(responses, model)
		}
	}
	if len(responses) > 0 {
		var generated struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal([]byte(runnerCatalog(responses).CodexCatalog), &generated); err != nil {
			return nil, fmt.Errorf("decode discovered Codex models: %w", err)
		}
		seen := make(map[string]bool, len(existing))
		for _, raw := range existing {
			if model, ok := raw.(map[string]any); ok {
				seen[stringValue(model["slug"])] = true
			}
		}
		for _, model := range generated.Models {
			if slug := stringValue(model["slug"]); !seen[slug] {
				seen[slug] = true
				existing = append(existing, model)
			}
		}
	}

	if err := orderCodexCatalogModels(existing); err != nil {
		return nil, err
	}
	catalog["models"] = existing
	return json.Marshal(catalog)
}

type codexModelRank struct {
	gpt     bool
	version []int
	tier    int
	base    string
	context string
}

func rankCodexCatalogModel(slug string) codexModelRank {
	name, gpt := strings.CutPrefix(strings.ToLower(slug), "gpt-")
	if !gpt {
		return codexModelRank{}
	}
	base, context, _ := strings.Cut(name, "[")
	release, suffix, _ := strings.Cut(base, "-")
	var version []int
	for part := range strings.SplitSeq(release, ".") {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return codexModelRank{gpt: true}
		}
		version = append(version, number)
	}
	variant, _, _ := strings.Cut(suffix, "-")
	tiers := [...]string{"astra", "sol", "terra", "luna", "pro", "", "mini", "nano"}
	tier := slices.Index(tiers[:], variant)
	if tier < 0 {
		tier = len(tiers)
	}
	return codexModelRank{gpt: true, version: version, tier: tier, base: base, context: context}
}

func orderCodexCatalogModels(models []any) error {
	type rankedModel struct {
		entry map[string]any
		rank  codexModelRank
	}
	ranked := make([]rankedModel, len(models))
	for i, raw := range models {
		entry, _ := raw.(map[string]any)
		slug := stringValue(entry["slug"])
		if strings.TrimSpace(slug) == "" {
			return fmt.Errorf("Codex model catalog entry %d has no model slug", i+1)
		}
		ranked[i] = rankedModel{entry: entry, rank: rankCodexCatalogModel(slug)}
	}
	slices.SortStableFunc(ranked, func(left, right rankedModel) int {
		a, b := left.rank, right.rank
		if a.gpt != b.gpt {
			if a.gpt {
				return -1
			}
			return 1
		}
		if order := slices.Compare(b.version, a.version); order != 0 {
			return order
		}
		if order := cmp.Compare(a.tier, b.tier); order != 0 {
			return order
		}
		if order := cmp.Compare(a.base, b.base); order != 0 {
			return order
		}
		return cmp.Compare(a.context, b.context)
	})
	for i, model := range ranked {
		// Codex orders its picker by ascending priority.
		model.entry["priority"] = i + 1
		models[i] = model.entry
	}
	return nil
}
