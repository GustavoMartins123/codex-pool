package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func fetchRunnerCatalog(ctx context.Context, options Options) (runnerproto.RunnerCatalog, error) {
	return fetchRunnerCatalogSince(ctx, options, runnerproto.RunnerCatalog{})
}

func fetchRunnerCatalogSince(ctx context.Context, options Options, previous runnerproto.RunnerCatalog) (runnerproto.RunnerCatalog, error) {
	var catalog runnerproto.RunnerCatalog
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, options.Config.PoolURL+"/api/runner/catalog", nil)
	if err != nil {
		return catalog, err
	}
	r.Header.Set("Authorization", "Bearer "+options.Config.Token)
	if previous.Revision != "" {
		r.Header.Set("If-None-Match", `"`+previous.Revision+`"`)
	}
	response, err := httpClient(options.HTTPClient).Do(r)
	if err != nil {
		return catalog, errors.New("runner model catalog unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && previous.Revision != "" {
		return previous, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if response.StatusCode != http.StatusOK || err != nil || len(raw) > 8<<20 || json.Unmarshal(raw, &catalog) != nil ||
		runnerproto.ValidateAgentModels(catalog.Models) != nil || !json.Valid([]byte(catalog.CodexCatalog)) {
		return catalog, errors.New("invalid runner model catalog")
	}
	revision := catalog.Revision
	catalog.Revision = ""
	expected := runnerproto.ContentRevision(catalog)
	if revision != "" && revision != expected {
		return catalog, errors.New("runner catalog revision mismatch")
	}
	catalog.Revision = expected
	return catalog, nil
}

func agentFingerprint(executables map[string]string) string {
	values := []string{}
	for agent, path := range executables {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			values = append(values, agent+":missing")
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil {
			values = append(values, agent+":missing")
			continue
		}
		values = append(values, fmt.Sprintf("%s|%s|%d|%d|%d", agent, resolved, info.Size(), info.ModTime().UnixNano(), info.Mode()))
	}
	slices.Sort(values)
	return runnerproto.ContentRevision(values)
}

func (d *daemon) refreshModels() error {
	d.catalogMu.Lock()
	defer d.catalogMu.Unlock()
	d.mu.Lock()
	previous, fingerprint := d.modelCatalog, d.agentFingerprint
	d.mu.Unlock()
	currentFingerprint := agentFingerprint(d.options.Executables)
	catalog, err := fetchRunnerCatalogSince(d.ctx, d.options, previous)
	if err != nil {
		if previous.Revision == "" || fingerprint == currentFingerprint {
			return err
		}
		// A failed network refresh must not keep a removed or replaced executable
		// advertised as usable. Recheck it with the last authorized catalog.
		catalog = previous
	}
	if catalog.Revision == previous.Revision && fingerprint == currentFingerprint {
		return nil
	}
	models, probeErr := prepareAgentModels(d.ctx, d.options, catalog)
	if probeErr != nil {
		log.Printf("runner model discovery: %v; unconfirmed models will not be offered", probeErr)
	}
	capabilities := runnerproto.ModelCapabilities{Revision: runnerproto.ModelsRevision(models), Models: models}
	d.mu.Lock()
	changed := d.modelsRevision != capabilities.Revision
	d.modelCatalog, d.models, d.modelsRevision = catalog, models, capabilities.Revision
	// Failed probes are retried even when the binary and source catalog did not change.
	if probeErr == nil {
		d.agentFingerprint = currentFingerprint
	} else {
		d.agentFingerprint = ""
	}
	if d.catalogGenerations == nil {
		d.catalogGenerations = map[string]string{}
	}
	if _, exists := d.catalogGenerations[capabilities.Revision]; !exists {
		d.catalogOrder = append(d.catalogOrder, capabilities.Revision)
	}
	d.catalogGenerations[capabilities.Revision] = catalog.CodexCatalog
	if len(d.catalogOrder) > 8 {
		delete(d.catalogGenerations, d.catalogOrder[0])
		d.catalogOrder = d.catalogOrder[1:]
	}
	d.mu.Unlock()
	if changed {
		select {
		case d.catalogNotify <- struct{}{}:
		default:
		}
	}
	return nil
}

func (d *daemon) watchModels() {
	interval := d.options.modelsRefreshInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if err := d.refreshModels(); err != nil && d.ctx.Err() == nil {
				log.Printf("runner catalog refresh: %v", err)
			}
		}
	}
}

func prepareAgentModels(ctx context.Context, options Options, catalog runnerproto.RunnerCatalog) ([]runnerproto.AgentModel, error) {
	confirmed := []runnerproto.AgentModel{}
	var claudeErr error
	claudeReady := false
	if exe := options.Executables["claude"]; exe != "" {
		lookup := AgentLookup{}
		lookup.defaults()
		_, claudeErr = lookup.validate(ctx, exe)
		claudeReady = claudeErr == nil
	}
	for _, model := range catalog.Models {
		// Claude Code accepts explicit model IDs through the Anthropic API;
		// Codex needs its own catalog loaded and checked through model/list.
		if model.Agent == "claude" && claudeReady {
			confirmed = append(confirmed, model)
		}
	}
	if options.Executables["codex"] == "" {
		return confirmed, claudeErr
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
	return confirmed, claudeErr
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
