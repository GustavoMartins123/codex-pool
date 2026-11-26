package modelalias

import (
	"encoding/json"
	"testing"
)

func TestDefaultGPT56ToSol(t *testing.T) {
	t.Parallel()

	aliases := New(nil)
	got, ok := aliases.Resolve("gpt-5.6")
	if !ok || got != "gpt-5.6-sol" {
		t.Fatalf("resolve(gpt-5.6) = (%q, %v), want (gpt-5.6-sol, true)", got, ok)
	}
	// Case-insensitive.
	got, ok = aliases.Resolve("GPT-5.6")
	if !ok || got != "gpt-5.6-sol" {
		t.Fatalf("resolve(GPT-5.6) = (%q, %v), want (gpt-5.6-sol, true)", got, ok)
	}
	// Real variant names pass through.
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"} {
		got, ok = aliases.Resolve(model)
		if ok || got != model {
			t.Fatalf("resolve(%q) = (%q, %v), want passthrough", model, got, ok)
		}
	}

	for model, want := range map[string]string{
		"gpt-5.6-sol[1m]":   "gpt-5.6-sol",
		"gpt-5.6-terra[1m]": "gpt-5.6-terra",
		"gpt-5.6-luna[1m]":  "gpt-5.6-luna",
	} {
		got, ok = aliases.Resolve(model)
		if !ok || got != want {
			t.Fatalf("resolve(%q) = (%q, %v), want (%q, true)", model, got, ok, want)
		}
	}
}

func TestConfigOverridesDefault(t *testing.T) {
	t.Parallel()

	aliases := New(map[string]string{
		"gpt-5.6": "gpt-5.6-luna",
	})
	got, ok := aliases.Resolve("gpt-5.6")
	if !ok || got != "gpt-5.6-luna" {
		t.Fatalf("resolve with override = (%q, %v), want (gpt-5.6-luna, true)", got, ok)
	}
}

func TestReloadKeepsDefaults(t *testing.T) {
	t.Parallel()

	aliases := New(map[string]string{"spark": "gpt-5.3-codex-spark"})
	aliases.Reload(nil)
	got, ok := aliases.Resolve("gpt-5.6")
	if !ok || got != "gpt-5.6-sol" {
		t.Fatalf("after reload(nil) resolve(gpt-5.6) = (%q, %v)", got, ok)
	}
	// Custom alias from before reload is gone.
	if _, ok := aliases.Resolve("spark"); ok {
		t.Fatal("expected spark alias cleared after reload(nil)")
	}
}

func TestApplyRewritesBody(t *testing.T) {
	t.Parallel()

	aliases := New(nil)
	body := []byte(`{"model":"gpt-5.6","input":"hi"}`)
	resolved, out := Apply(aliases, "gpt-5.6", body, false, "req")
	if resolved != "gpt-5.6-sol" {
		t.Fatalf("resolved = %q, want gpt-5.6-sol", resolved)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	if got := parsed["model"]; got != "gpt-5.6-sol" {
		t.Fatalf("body model = %#v, want gpt-5.6-sol", got)
	}
}
