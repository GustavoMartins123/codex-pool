package wire

import (
	"encoding/json"
	"testing"
)

func TestClaudeOrderedBody(t *testing.T) {
	m := map[string]any{
		"stream":   true,
		"model":    "claude-sonnet-4-5",
		"messages": []any{},
		"system":   "hello",
	}

	out, err := MarshalAnthropicRequest(m)
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's valid JSON
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Verify model comes before messages in the output
	s := string(out)
	modelPos := -1
	messagesPos := -1
	for i := 0; i <= len(s)-12; i++ {
		if s[i:i+8] == `"model":` {
			modelPos = i
		}
		if s[i:i+11] == `"messages":` {
			messagesPos = i
		}
	}
	if modelPos > messagesPos {
		t.Errorf("model should come before messages, got model@%d messages@%d", modelPos, messagesPos)
	}
}
