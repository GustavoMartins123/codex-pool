package orderedjson

import (
	"encoding/json"
	"testing"
)

func TestMarshal(t *testing.T) {
	m := map[string]any{
		"stream":     true,
		"model":      "claude-sonnet-4-5",
		"max_tokens": 1024,
		"messages":   []any{},
	}
	order := []string{"model", "messages", "max_tokens", "stream"}
	out, err := Marshal(m, order)
	if err != nil {
		t.Fatal(err)
	}

	// Verify key order by parsing and checking positions
	s := string(out)
	positions := map[string]int{}
	for _, key := range order {
		pos := -1
		// Find "key": in the output
		search := `"` + key + `":`
		for i := 0; i <= len(s)-len(search); i++ {
			if s[i:i+len(search)] == search {
				pos = i
				break
			}
		}
		if pos < 0 {
			t.Fatalf("key %q not found in output: %s", key, s)
		}
		positions[key] = pos
	}

	// Verify ordering
	for i := 1; i < len(order); i++ {
		prev := order[i-1]
		curr := order[i]
		if positions[prev] > positions[curr] {
			t.Errorf("key %q (pos %d) should come before %q (pos %d)", prev, positions[prev], curr, positions[curr])
		}
	}
}

func TestMarshalRemainingKeys(t *testing.T) {
	m := map[string]any{
		"model":  "test",
		"extra":  "value",
		"alpha":  "first",
		"stream": true,
	}

	order := []string{"model", "stream"}
	out, err := Marshal(m, order)
	if err != nil {
		t.Fatal(err)
	}

	s := string(out)
	// "alpha" should come before "extra" (alphabetical) and both after ordered keys
	alphaPos := -1
	extraPos := -1
	streamPos := -1
	for i := 0; i <= len(s)-10; i++ {
		if s[i:i+8] == `"alpha":` {
			alphaPos = i
		}
		if s[i:i+8] == `"extra":` {
			extraPos = i
		}
		if s[i:i+9] == `"stream":` {
			streamPos = i
		}
	}

	if alphaPos < 0 || extraPos < 0 || streamPos < 0 {
		t.Fatalf("missing keys in output: %s", s)
	}
	if streamPos > alphaPos {
		t.Errorf("ordered key 'stream' should come before remaining key 'alpha'")
	}
	if alphaPos > extraPos {
		t.Errorf("remaining key 'alpha' should come before 'extra'")
	}
}

func TestMarshalNull(t *testing.T) {
	out, err := Marshal(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "null" {
		t.Errorf("expected null, got %s", out)
	}
}

func TestMarshalEmpty(t *testing.T) {
	out, err := Marshal(map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "{}" {
		t.Errorf("expected {}, got %s", out)
	}
}

func TestMarshalPreservesValues(t *testing.T) {
	m := map[string]any{
		"model": "claude-sonnet-4-5",
		"nested": map[string]any{
			"deep": []any{1, 2, 3},
		},
		"flag": true,
	}

	out, err := Marshal(m, []string{"model", "nested", "flag"})
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if parsed["model"] != "claude-sonnet-4-5" {
		t.Errorf("model = %v", parsed["model"])
	}
	if parsed["flag"] != true {
		t.Errorf("flag = %v", parsed["flag"])
	}
	nested, ok := parsed["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested is not a map: %T", parsed["nested"])
	}
	deep, ok := nested["deep"].([]any)
	if !ok {
		t.Fatalf("deep is not a slice: %T", nested["deep"])
	}
	if len(deep) != 3 {
		t.Errorf("deep has %d elements, want 3", len(deep))
	}
}
