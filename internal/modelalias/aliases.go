// Package modelalias maps client-facing model names to upstream model IDs.
package modelalias

import (
	"log"
	"strings"
	"sync"

	"codex-pool-proxy/internal/requestjson"
)

// defaults are always registered unless overridden by config.toml
// [model_aliases]. Keys are matched case-insensitively at resolve time.
var defaults = map[string]string{
	"sonnet": "claude-sonnet-5-5",
	"opus":   "claude-opus-5-5",
	// GPT-5.6 series short name → Sol (default variant).
	"gpt-5.6": "gpt-5.6-sol",
	// The 1M choices are client-facing context profiles. Upstream uses the
	// corresponding base model ID.
	"gpt-6-astra[1m]":   "gpt-6-astra",
	"gpt-6.1-sol[1m]":   "gpt-6.1-sol",
	"gpt-5.6-sol[1m]":   "gpt-5.6-sol",
	"gpt-5.6-terra[1m]": "gpt-5.6-terra",
	"gpt-5.6-luna[1m]":  "gpt-5.6-luna",
}

// Aliases manages model name aliases. It is safe for concurrent use and
// hot-reload.
type Aliases struct {
	mu      sync.RWMutex
	aliases map[string]string // short name -> full upstream model name
}

func merge(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(defaults)+len(cfg))
	for k, v := range defaults {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	for k, v := range cfg {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		// Config overrides built-ins.
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

// New returns the built-in aliases overlaid with cfg, which may be nil.
func New(cfg map[string]string) *Aliases {
	return &Aliases{aliases: merge(cfg)}
}

// Resolve returns the upstream model name for a given alias, or the
// original name if no alias is defined. The second return value indicates
// whether an alias was applied.
func (m *Aliases) Resolve(model string) (string, bool) {
	if m == nil || model == "" {
		return model, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if target, ok := m.aliases[strings.ToLower(model)]; ok {
		return target, true
	}
	return model, false
}

// Reload replaces the alias map (used by hot-reload). Always re-applies
// built-in defaults, then overlays cfg (which may be nil).
func (m *Aliases) Reload(cfg map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aliases = merge(cfg)
}

// Apply resolves the model alias and rewrites the body's model field when an
// alias applies, using requestjson.RewriteModel.
func Apply(aliases *Aliases, model string, body []byte, debug bool, reqID string) (string, []byte) {
	resolved, aliased := aliases.Resolve(model)
	if !aliased {
		return model, body
	}
	if rewritten := requestjson.RewriteModel(body, resolved); rewritten != nil {
		if debug {
			log.Printf("[%s] model alias: %s -> %s", reqID, model, resolved)
		}
		return resolved, rewritten
	}
	// Body rewrite failed, but still use the resolved name for routing.
	if debug {
		log.Printf("[%s] model alias: %s -> %s (body rewrite failed, routing only)", reqID, model, resolved)
	}
	return resolved, body
}
