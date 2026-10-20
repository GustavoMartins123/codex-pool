package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClaudeResponsesHTTPToolRoundTrip(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			t.Setenv("POOL_JWT_SECRET", "test-secret")
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "sk-ant-api-synthetic" || body["model"] != "claude-sonnet-5-5" {
					t.Error("wrong provider path, authentication, or model")
					w.WriteHeader(400)
					return
				}
				messages, ok := body["messages"].([]any)
				if !ok || len(messages) == 0 {
					t.Error("easy input message was lost")
					w.WriteHeader(400)
					return
				}
				assertClaudeMessageText(t, messages[0], "user", "Look up 7")
				choice, _ := body["tool_choice"].(map[string]any)
				tools, _ := body["tools"].([]any)
				if len(tools) != 1 {
					t.Error("tool declaration was lost")
					w.WriteHeader(400)
					return
				}
				name, _ := tools[0].(map[string]any)["name"].(string)
				content := map[string]any{"type": "tool_use", "id": "tool_synthetic", "name": name, "input": map[string]any{"value": 7}}
				stop := "tool_use"
				if calls.Load() == 1 {
					if choice["type"] != "tool" || choice["name"] != name || choice["disable_parallel_tool_use"] != true {
						t.Errorf("forced function contract changed: %#v", choice)
					}
				} else {
					if choice["type"] != "none" || len(messages) != 3 {
						t.Errorf("tool continuation contract changed: %#v", body)
						w.WriteHeader(400)
						return
					}
					assertClaudeToolBlocks(t, messages[1], "assistant", "tool_use", []string{"tool_synthetic"})
					assertClaudeToolBlocks(t, messages[2], "user", "tool_result", []string{"tool_synthetic"})
					blocks := messages[2].(map[string]any)["content"].([]any)
					if blocks[0].(map[string]any)["content"] != "value:7" {
						t.Error("tool output was changed")
					}
					content = map[string]any{"type": "text", "text": "DONE"}
					stop = "end_turn"
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					write := func(event map[string]any) {
						data, _ := json.Marshal(event)
						fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
					}
					write(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_synthetic", "model": "claude-sonnet-5-5", "usage": map[string]any{"input_tokens": 1}}})
					block := map[string]any{"type": "text", "text": ""}
					delta := map[string]any{"type": "text_delta", "text": "DONE"}
					if stop == "tool_use" {
						block = map[string]any{"type": "tool_use", "id": "tool_synthetic", "name": name, "input": map[string]any{}}
						delta = map[string]any{"type": "input_json_delta", "partial_json": `{"value":7}`}
					}
					write(map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
					write(map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
					write(map[string]any{"type": "content_block_stop", "index": 0})
					write(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": 1}})
					write(map[string]any{"type": "message_stop"})
				} else {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"id": "msg_synthetic", "type": "message", "role": "assistant", "model": "claude-sonnet-5-5", "content": []any{content}, "stop_reason": stop, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
				}
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			fx := newCodexProxyFixture(t, base, []*Account{{ID: "synthetic", Type: AccountTypeClaude, PlanType: "max", AccessToken: "sk-ant-api-synthetic"}})
			fx.handler.aliases = newModelAliases(nil)
			input := []any{map[string]any{"role": "user", "content": "Look up 7"}}
			payload := map[string]any{"model": "claude-sonnet-5-5", "input": input, "stream": streaming, "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}}}}, "tool_choice": map[string]any{"type": "function", "name": "lookup"}, "parallel_tool_calls": false}
			send := func() map[string]any {
				data, _ := json.Marshal(payload)
				req, _ := http.NewRequest("POST", fx.server.URL+"/v1/responses", bytes.NewReader(data))
				req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				data, _ = io.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					t.Fatalf("status %d: %s", resp.StatusCode, data)
				}
				var result map[string]any
				if streaming {
					for _, line := range strings.Split(string(data), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.completed" {
							result, _ = event["response"].(map[string]any)
						}
					}
				} else if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if result == nil {
					t.Fatalf("missing response: %s", data)
				}
				return result
			}
			first := send()
			output, _ := first["output"].([]any)
			if len(output) != 1 {
				t.Fatalf("unexpected tool response: %#v", first)
			}
			call, _ := output[0].(map[string]any)
			if call["type"] != "function_call" || call["name"] != "lookup" || call["call_id"] != "tool_synthetic" || call["arguments"] != `{"value":7}` {
				t.Fatalf("tool call changed: %#v", call)
			}
			payload["input"] = append(input, call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "value:7"})
			payload["tool_choice"] = "none"
			second := send()
			encoded, _ := json.Marshal(second)
			if !bytes.Contains(encoded, []byte(`"text":"DONE"`)) || calls.Load() != 2 {
				t.Fatalf("continuation failed: %s, calls=%d", encoded, calls.Load())
			}
		})
	}
}
