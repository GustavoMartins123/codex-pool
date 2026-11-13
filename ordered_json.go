package main

import (
	"codex-pool-proxy/internal/orderedjson"
)

// claudeBodyKeyOrder is the canonical key order for Claude Messages API
// requests, matching what real Claude Code sends over the wire.
var claudeBodyKeyOrder = []string{
	"model", "messages", "system", "tools", "metadata",
	"max_tokens", "thinking", "context_management",
	"output_config", "stream",
}

// claudeOrderedBody marshals a Claude request body with the correct key order.
func claudeOrderedBody(bodyObj map[string]any) ([]byte, error) {
	return orderedjson.Marshal(bodyObj, claudeBodyKeyOrder)
}
