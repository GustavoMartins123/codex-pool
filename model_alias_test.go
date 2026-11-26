package main

import (
	"bytes"
	"testing"

	"codex-pool-proxy/internal/modelalias"
)

func TestApplyModelAliasToJSONFrameForWebSocket(t *testing.T) {
	t.Parallel()

	h := &proxyHandler{aliases: modelalias.New(nil)}
	in := []byte(`{"type":"response.create","model":"gpt-5.6","input":[]}`)
	out := applyModelAliasToJSONFrame(h, "req", in)
	if !bytes.Contains(out, []byte(`"model":"gpt-5.6-sol"`)) {
		t.Fatalf("expected gpt-5.6-sol in frame, got %s", out)
	}
	// Unaliased models pass through unchanged.
	passthrough := []byte(`{"type":"response.create","model":"gpt-5.6-terra"}`)
	if got := applyModelAliasToJSONFrame(h, "req", passthrough); !bytes.Equal(got, passthrough) {
		t.Fatalf("terra frame rewritten unexpectedly: %s", got)
	}
}
