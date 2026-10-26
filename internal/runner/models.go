package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func fetchRunnerCatalog(ctx context.Context, options Options) (runnerproto.RunnerCatalog, error) {
	var catalog runnerproto.RunnerCatalog
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, options.Config.PoolURL+"/api/runner/catalog", nil)
	if err != nil {
		return catalog, err
	}
	r.Header.Set("Authorization", "Bearer "+options.Config.Token)
	response, err := httpClient(options.HTTPClient).Do(r)
	if err != nil {
		return catalog, errors.New("runner model catalog unavailable")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if response.StatusCode != http.StatusOK || err != nil || len(raw) > 8<<20 || json.Unmarshal(raw, &catalog) != nil ||
		runnerproto.ValidateAgentModels(catalog.Models) != nil || !json.Valid([]byte(catalog.CodexCatalog)) {
		return catalog, errors.New("invalid runner model catalog")
	}
	return catalog, nil
}

func prepareAgentModels(ctx context.Context, options Options, catalog runnerproto.RunnerCatalog) ([]runnerproto.AgentModel, error) {
	confirmed := []runnerproto.AgentModel{}
	for _, model := range catalog.Models {
		// Claude Code accepts explicit model IDs through the Anthropic API;
		// Codex needs its own catalog loaded and checked through model/list.
		if model.Agent == "claude" && options.Executables["claude"] != "" {
			confirmed = append(confirmed, model)
		}
	}
	if options.Executables["codex"] == "" {
		return confirmed, nil
	}
	control, err := os.MkdirTemp(options.WorkspaceRoot, ".model-probe-*")
	if err != nil {
		return confirmed, err
	}
	defer os.RemoveAll(control)
	if err := saveCodexCatalog(control, catalog.CodexCatalog); err != nil {
		return confirmed, err
	}
	models, err := probeCodexModels(ctx, options.Executables["codex"], control, options.Config.PoolURL)
	if err != nil {
		return confirmed, err
	}
	for _, advertised := range catalog.Models {
		if advertised.Agent != "codex" {
			continue
		}
		for _, native := range models {
			if native.Model != advertised.Model {
				continue
			}
			model := advertised
			model.ReasoningEfforts = []string{}
			for _, effort := range advertised.ReasoningEfforts {
				if slices.Contains(native.ReasoningEfforts, effort) {
					model.ReasoningEfforts = append(model.ReasoningEfforts, effort)
				}
			}
			if !slices.Contains(model.ReasoningEfforts, model.DefaultReasoningEffort) {
				model.DefaultReasoningEffort = ""
			}
			confirmed = append(confirmed, model)
			break
		}
	}
	return confirmed, nil
}

func saveCodexCatalog(control, catalog string) error {
	return writePrivateJSON(filepath.Join(control, "model_catalog.json"), json.RawMessage(catalog))
}

func probeCodexModels(ctx context.Context, exe, control, origin string) ([]runnerproto.AgentModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	process, err := spawnJSONProcess(ctx, codexCommand(exe, control, control, origin, "", "gated"))
	if err != nil {
		return nil, err
	}
	go process.read(func(message map[string]json.RawMessage) error {
		if id := rawString(message["id"]); id != "" {
			process.deliver(id, message["result"], len(message["error"]) > 0)
		}
		return nil
	}, func(int) {})
	defer func() { process.close(); <-process.done }()
	call := func(method string, params any) (json.RawMessage, error) {
		id := process.nextID()
		return process.call(id, map[string]any{"id": id, "method": method, "params": params})
	}
	if _, err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_pool_runner", "version": "1"}}); err != nil {
		return nil, err
	}
	if err := process.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	var cursor *string
	models := []runnerproto.AgentModel{}
	seen := map[string]bool{}
	for page := 0; page < 16; page++ {
		raw, err := call("model/list", map[string]any{"cursor": cursor, "limit": 100, "includeHidden": true})
		if err != nil {
			return nil, err
		}
		var result struct {
			Data []struct {
				Model     string `json:"model"`
				Supported []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &result) != nil {
			return nil, errors.New("invalid Codex model list")
		}
		for _, entry := range result.Data {
			model := runnerproto.AgentModel{Agent: "codex", Model: entry.Model}
			for _, effort := range entry.Supported {
				model.ReasoningEfforts = append(model.ReasoningEfforts, effort.Effort)
			}
			models = append(models, model)
		}
		if result.NextCursor == nil || *result.NextCursor == "" {
			return models, nil
		}
		if seen[*result.NextCursor] {
			break
		}
		seen[*result.NextCursor] = true
		cursor = result.NextCursor
	}
	return nil, errors.New("Codex model list did not terminate")
}
