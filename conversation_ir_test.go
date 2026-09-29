package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConversationIRContainsOnlyPortableContent(t *testing.T) {
	messages := []Message{
		{Role: "system", Parts: []MessagePart{{Type: "text", Text: "system instruction"}}},
		{Role: "developer", Parts: []MessagePart{{Type: "text", Text: "developer instruction"}}},
		{Role: "user", Parts: []MessagePart{{Type: "text", Text: "look"}, {Type: "image", ImageURL: "data:image/png;base64,aGVsbG8=", MimeType: "image/png", Data: "aGVsbG8="}}},
		{Role: "assistant", Parts: []MessagePart{{Type: "reasoning_visible", Text: "visible step"}, {Type: "tool_call", ToolID: "call-1", ToolName: "lookup", Arguments: `{}`}}},
		{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call-1", Text: "found"}}},
	}
	ir := conversationIRFromMessages(messages, "conv", 4)
	if len(ir.System) != 2 || len(ir.Messages) != 3 || ir.Metadata.ConversationID != "conv" || ir.Metadata.Epoch != 4 {
		t.Fatalf("IR shape=%+v", ir)
	}
	if ir.System[1].Role != IRRoleDeveloper || ir.Messages[0].Content[1].Type != IRImage {
		t.Fatalf("role or image lost: %+v", ir)
	}
	roundTrip := ir.legacyMessages()
	if len(roundTrip) != len(messages) || roundTrip[1].Role != "developer" || roundTrip[2].Parts[1].ImageURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("IR roundtrip lost content: %+v", roundTrip)
	}
	encoded, err := json.Marshal(ir)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"previous_response_id", "thoughtSignature", "encrypted_content", "prompt_cache_key"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("provider-local field %q entered IR: %s", forbidden, encoded)
		}
	}
}

func TestConversationIRPreservesImagesAcrossSupportedProviders(t *testing.T) {
	for _, pair := range []transitionFixture{
		{"codex_to_antigravity", AccountTypeCodex, AccountTypeAntigravity},
		{"antigravity_to_codex", AccountTypeAntigravity, AccountTypeCodex},
		{"claude_to_antigravity", AccountTypeClaude, AccountTypeAntigravity},
		{"antigravity_to_claude", AccountTypeAntigravity, AccountTypeClaude},
	} {
		t.Run(pair.name, func(t *testing.T) {
			store := newConversationHandoffStore()
			fromFormat, toFormat := transitionFormat(pair.from), transitionFormat(pair.to)
			image := Message{Role: "user", Parts: []MessagePart{{Type: "text", Text: "describe image"}, {Type: "image", ImageURL: "data:image/png;base64,aGVsbG8=", MimeType: "image/png", Data: "aGVsbG8="}}}
			first := transitionBody(t, fromFormat, []Message{image, transitionText("assistant", "it is an image")})
			if _, _, err := store.Prepare(conversationScopedKey("user", "image-conversation"), pair.from, contextTestPath(fromFormat), first); err != nil {
				t.Fatal(err)
			}
			next := transitionBody(t, toFormat, []Message{transitionText("user", "continue")})
			out, result, err := store.Prepare(conversationScopedKey("user", "image-conversation"), pair.to, contextTestPath(toFormat), next)
			if err != nil || !result.Switched {
				t.Fatalf("handoff=%+v err=%v", result, err)
			}
			var root map[string]any
			if err := json.Unmarshal(out, &root); err != nil {
				t.Fatal(err)
			}
			ir := normalizeConversationIR(toFormat, root, "image-conversation", 1)
			found := false
			for _, message := range ir.Messages {
				for _, part := range message.Content {
					if part.Type == IRImage && part.Data == "aGVsbG8=" && part.MimeType == "image/png" {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("image lost during %s handoff: %s", pair.name, out)
			}
		})
	}
}
