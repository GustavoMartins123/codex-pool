package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// transitionFixture keeps the client wire format separate from the provider.
// Antigravity is reached through Responses here, just as the Codex CLI does.
type transitionFixture struct {
	name string
	from AccountType
	to   AccountType
}

var transitionFixtures = []transitionFixture{
	{"codex_to_antigravity", AccountTypeCodex, AccountTypeAntigravity},
	{"antigravity_to_codex", AccountTypeAntigravity, AccountTypeCodex},
	{"codex_to_zai", AccountTypeCodex, AccountTypeZAI},
	{"zai_to_codex", AccountTypeZAI, AccountTypeCodex},
	{"claude_to_antigravity", AccountTypeClaude, AccountTypeAntigravity},
	{"antigravity_to_claude", AccountTypeAntigravity, AccountTypeClaude},
	{"zai_to_antigravity", AccountTypeZAI, AccountTypeAntigravity},
	{"antigravity_to_zai", AccountTypeAntigravity, AccountTypeZAI},
}

func transitionFormat(provider AccountType) contextWireFormat {
	if provider == AccountTypeClaude || provider == AccountTypeZAI {
		return contextFormatClaude
	}
	return contextFormatResponses
}

func transitionBody(t *testing.T, format contextWireFormat, messages []Message) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(contextTestBody(format, "placeholder", true), &root); err != nil {
		t.Fatal(err)
	}
	renderConversationMessages(format, root, messages)
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func transitionText(role, value string) Message {
	return Message{Role: role, Parts: []MessagePart{{Type: "text", Text: value}}}
}

func transitionHistory(scenario string) []Message {
	messages := []Message{transitionText("user", "first turn"), transitionText("assistant", "first answer")}
	switch scenario {
	case "history_20", "history_50", "history_100":
		var turns int
		_, _ = fmt.Sscanf(scenario, "history_%d", &turns)
		for i := 0; i < turns; i++ {
			messages = append(messages, transitionText("user", fmt.Sprintf("question %03d", i)), transitionText("assistant", fmt.Sprintf("answer %03d", i)))
		}
	case "tool", "after_tool_result":
		messages = append(messages, transitionText("user", "trigger lookup"),
			Message{Role: "assistant", Parts: []MessagePart{{Type: "tool_call", ToolID: "call_foreign", ToolName: "lookup", Arguments: `{"q":"weather"}`}}},
			Message{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call_foreign", Text: "sunny"}}},
		)
		if scenario == "tool" {
			messages = append(messages, transitionText("assistant", "It is sunny."))
		}
	case "parallel_tools":
		messages = append(messages, transitionText("user", "trigger three lookups"))
		calls := Message{Role: "assistant"}
		results := Message{Role: "tool"}
		for i := 1; i <= 3; i++ {
			id := fmt.Sprintf("call_foreign_%d", i)
			calls.Parts = append(calls.Parts, MessagePart{Type: "tool_call", ToolID: id, ToolName: "lookup", Arguments: fmt.Sprintf(`{"n":%d}`, i)})
			results.Parts = append(results.Parts, MessagePart{Type: "tool_result", ToolID: id, Text: fmt.Sprintf("result %d", i)})
		}
		messages = append(messages, calls, results, transitionText("assistant", "Done."))
	case "compaction":
		messages = append(messages, transitionText("user", strings.Repeat("context payload ", 30000)))
	case "reasoning":
		messages = append(messages, transitionText("assistant", "Visible conclusion."))
	case "image":
		messages = append(messages, Message{Role: "user", Parts: []MessagePart{{Type: "image", ImageURL: "data:image/png;base64,aGVsbG8="}}})
	}
	return messages
}

func transitionToolPairs(messages []Message) (calls, results int, valid bool) {
	ids := make(map[string]int)
	valid = true
	for _, message := range messages {
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				calls++
				ids[part.ToolID]++
			case "tool_result":
				results++
				ids[part.ToolID]--
			}
		}
	}
	for _, balance := range ids {
		if balance != 0 {
			valid = false
		}
	}
	return
}

func TestProviderTransitionCompatibilityMatrix(t *testing.T) {
	scenarios := []string{"simple", "history_20", "history_50", "history_100", "tool", "parallel_tools", "compaction", "reasoning", "image", "after_tool_result"}
	for _, pair := range transitionFixtures {
		for _, scenario := range scenarios {
			t.Run(pair.name+"/"+scenario, func(t *testing.T) {
				store := newConversationHandoffStore()
				fromFormat, toFormat := transitionFormat(pair.from), transitionFormat(pair.to)
				initial := transitionBody(t, fromFormat, transitionHistory(scenario))
				if scenario == "reasoning" {
					initial = addContextOpaqueState(t, initial)
				}
				if _, result, err := store.Prepare("transition-probe", pair.from, contextTestPath(fromFormat), initial); err != nil || result.Switched {
					t.Fatalf("initial request: switched=%v err=%v", result.Switched, err)
				}
				// Capture the visible answer independently of upstream response IDs.
				store.RecordAssistantText("transition-probe", pair.from, "provider answer")
				next := addContextOpaqueState(t, transitionBody(t, toFormat, []Message{transitionText("user", "next question")}))
				var raw map[string]any
				_ = json.Unmarshal(next, &raw)
				raw["thoughtSignature"] = "foreign-signature"
				next, _ = json.Marshal(raw)
				out, result, err := store.Prepare("transition-probe", pair.to, contextTestPath(toFormat), next)
				if err != nil || !result.Switched {
					t.Fatalf("handoff: switched=%v err=%v", result.Switched, err)
				}
				if strings.Contains(string(out), "foreign-signature") || strings.Contains(string(out), "resp_previous_provider") || strings.Contains(string(out), "cache_previous_provider") {
					t.Fatalf("foreign state leaked into %s payload: %s", pair.to, out)
				}
				var normalized map[string]any
				if err := json.Unmarshal(out, &normalized); err != nil {
					t.Fatal(err)
				}
				messages := normalizeConversationMessages(toFormat, normalized)
				calls, results, paired := transitionToolPairs(messages)
				if !paired || calls != results {
					t.Fatalf("invalid tool history: calls=%d results=%d payload=%s", calls, results, out)
				}
				visible := normalizedContextText(t, contextTestPath(toFormat), out)
				if !strings.Contains(visible, "first turn") || !strings.Contains(visible, "next question") {
					t.Fatalf("visible conversation lost: %q", visible)
				}
				if scenario == "parallel_tools" && calls != 3 || (scenario == "tool" || scenario == "after_tool_result") && calls != 1 {
					t.Fatalf("tool count=%d, scenario=%s, payload=%s", calls, scenario, out)
				}
				t.Logf("from=%s to=%s scenario=%s handoff_status=200 messages=%d calls=%d results=%d compacted=%v warnings=%v payload_bytes=%d", pair.from, pair.to, scenario, len(messages), calls, results, result.Compacted, result.Warnings, len(out))
			})
		}
	}
}

// This validator models the protocol distinctions hidden by a session-ID-only
// mock. A malformed session, foreign signature, and capacity failure can all
// produce an HTTP error without implying that account quota was exhausted.
func strictAntigravityTransitionStatus(body []byte, failure string) (int, string) {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return http.StatusBadRequest, `{"error":{"message":"invalid JSON"}}`
	}
	request, _ := envelope["request"].(map[string]any)
	if !isAntigravitySessionID(stringValue(request["sessionId"])) {
		return http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid session identifier"}}`
	}
	encoded, _ := json.Marshal(request)
	if strings.Contains(string(encoded), "foreign-signature") || strings.Contains(string(encoded), "resp_previous_provider") {
		return http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid thought signature or context mismatch"}}`
	}
	switch failure {
	case "quota":
		return http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"reason":"QUOTA_EXCEEDED"}]}}`
	case "capacity":
		return http.StatusServiceUnavailable, `{"error":{"message":"No capacity available for model"}}`
	case "signature":
		return http.StatusTooManyRequests, `{"error":{"message":"invalid thought signature"}}`
	case "context":
		return http.StatusTooManyRequests, `{"error":{"message":"context mismatch"}}`
	case "session":
		return http.StatusTooManyRequests, `{"error":{"message":"invalid session"}}`
	}
	return http.StatusOK, `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`
}

func TestProviderTransitionAntigravityStrictUpstream(t *testing.T) {
	store := newConversationHandoffStore()
	from := contextTestBody(contextFormatResponses, "hello", true)
	if _, _, err := store.Prepare("strict-upstream", AccountTypeCodex, "/v1/responses", from); err != nil {
		t.Fatal(err)
	}
	next := addContextOpaqueState(t, contextTestBody(contextFormatResponses, "continue", true))
	clean, result, err := store.Prepare("strict-upstream", AccountTypeAntigravity, "/v1/responses", next)
	if err != nil || !result.Switched {
		t.Fatalf("handoff: switched=%v err=%v", result.Switched, err)
	}
	prepared, err := prepareAntigravityRequest("/v1/responses", clean, "antigravity/gemini-3.8-flash-high", "project", "strict-upstream")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		failure string
		want    int
	}{
		{"", 200}, {"quota", 429}, {"session", 429}, {"context", 429}, {"signature", 429}, {"capacity", 503},
	} {
		t.Run(test.failure, func(t *testing.T) {
			status, payload := strictAntigravityTransitionStatus(prepared.Body, test.failure)
			if status != test.want {
				t.Fatalf("status=%d want=%d payload=%s sent=%s", status, test.want, payload, prepared.Body)
			}
			t.Logf("status=%d error=%s sent=%s", status, payload, prepared.Body)
		})
	}
}
