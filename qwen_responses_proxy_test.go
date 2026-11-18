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

	"codex-pool-proxy/internal/jsonvalue"
	"codex-pool-proxy/internal/sse"
)

func qwenResponsesEvents(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, frame := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
		kind, data := sse.ParseEvent([]byte(frame))
		if len(data) == 0 {
			continue
		}
		var event map[string]any
		if json.Unmarshal(data, &event) != nil || event["type"] != kind || event["sequence_number"] != float64(len(events)) {
			t.Fatalf("invalid Responses event at index %d: %s", len(events), data)
		}
		events = append(events, event)
	}
	return events
}

func TestQwenCodexResponsesHTTPToolRoundTrip(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses", "/api/codex/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				t.Setenv("POOL_JWT_SECRET", "qwen-tools-test-secret")
				patch := "*** Begin Patch\n*** Add File: café.txt\n+Olá mundo\n*** End Patch"
				patchArgs, _ := json.Marshal(map[string]string{"input": patch})
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					turn := calls.Add(1)
					var request map[string]any
					if r.URL.Path != "/compatible-mode/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-sp-qwen-tools" || json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("Codex request did not reach the Qwen Chat Completions endpoint with provider credentials")
						w.WriteHeader(400)
						return
					}
					if request["model"] != "qwen3.8-max" || request["input"] != nil || request["previous_response_id"] != nil || request["stream"] != stream {
						t.Errorf("Responses envelope leaked to Qwen: %#v", request)
					}
					tools := jsonvalue.Slice(request["tools"])
					if len(tools) != 2 {
						t.Error("Codex additional_tools declarations were lost")
						w.WriteHeader(400)
						return
					}
					execName := jsonvalue.String(jsonvalue.Map(jsonvalue.Map(tools[0])["function"])["name"])
					patchName := jsonvalue.String(jsonvalue.Map(jsonvalue.Map(tools[1])["function"])["name"])
					if !qwenFunctionNamePattern.MatchString(execName) || patchName != "apply_patch" {
						t.Error("upstream tool names do not match the Qwen contract")
					}
					messages := jsonvalue.Slice(request["messages"])
					if turn == 2 {
						if len(messages) != 4 || request["tool_choice"] != "none" {
							t.Errorf("continuation lost history: %#v", request)
							w.WriteHeader(400)
							return
						}
						assistant := jsonvalue.Map(messages[1])
						toolCalls := jsonvalue.Slice(assistant["tool_calls"])
						if assistant["reasoning_content"] != "Need tools." || len(toolCalls) != 2 || jsonvalue.Map(jsonvalue.Map(toolCalls[0])["function"])["name"] != execName || jsonvalue.Map(jsonvalue.Map(toolCalls[1])["function"])["arguments"] != string(patchArgs) {
							t.Error("reasoning, namespace or raw patch changed during replay")
						}
						if jsonvalue.Map(messages[2])["tool_call_id"] != "call-patch" || jsonvalue.Map(messages[2])["content"] != "File created." || jsonvalue.Map(messages[3])["tool_call_id"] != "call-command" || jsonvalue.Map(messages[3])["content"] != "/workspace" {
							t.Error("out-of-order tool results lost their original call IDs")
						}
					}
					toolCalls := []any{
						map[string]any{"id": "call-command", "type": "function", "function": map[string]any{"name": execName, "arguments": `{"cmd":"pwd"}`}},
						map[string]any{"id": "call-patch", "type": "function", "function": map[string]any{"name": patchName, "arguments": string(patchArgs)}},
					}
					finish := "tool_calls"
					message := map[string]any{"role": "assistant", "reasoning_content": "Need tools.", "tool_calls": toolCalls, "content": nil}
					if turn == 2 {
						finish = "stop"
						message = map[string]any{"role": "assistant", "content": "Arquivo criado."}
					}
					usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 30, "prompt_tokens_details": map[string]any{"cached_tokens": 40}, "completion_tokens_details": map[string]any{"reasoning_tokens": 10}}
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(map[string]any{"id": "chat-qwen", "model": "qwen3.8-max", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
						return
					}
					if jsonvalue.Map(request["stream_options"])["include_usage"] != true {
						t.Error("streaming usage was not requested")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					write := func(delta map[string]any, finish any) {
						encoded, _ := json.Marshal(map[string]any{"id": "chat-qwen", "model": "qwen3.8-max", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
						frame := []byte("data: " + string(encoded) + "\r\n\r\n")
						for len(frame) > 0 {
							size := min(13, len(frame))
							w.Write(frame[:size])
							w.(http.Flusher).Flush()
							frame = frame[size:]
						}
					}
					if turn == 1 {
						write(map[string]any{"role": "assistant", "reasoning_content": "Need tools."}, nil)
						write(map[string]any{"tool_calls": []any{
							map[string]any{"index": 0, "id": "call-command", "type": "function", "function": map[string]any{"name": execName, "arguments": `{"cmd":`}},
							map[string]any{"index": 1, "id": "call-patch", "type": "function", "function": map[string]any{"name": patchName, "arguments": string(patchArgs[:20])}},
						}}, nil)
						write(map[string]any{"tool_calls": []any{map[string]any{"index": 1, "function": map[string]any{"arguments": string(patchArgs[20:])}}, map[string]any{"index": 0, "function": map[string]any{"arguments": `"pwd"}`}}}}, nil)
					} else {
						write(map[string]any{"content": "Arquivo "}, nil)
						write(map[string]any{"content": "criado."}, nil)
					}
					write(map[string]any{}, finish)
					encoded, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": usage})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
				}))
				defer upstream.Close()
				account := &Account{ID: "qwen-tools", Type: AccountTypeQwen, AccessToken: "sk-sp-qwen-tools", PlanType: "token_plan", Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai"}}}
				h := qwenTestHandler(t, http.DefaultTransport, account)
				base, _ := url.Parse(upstream.URL + "/compatible-mode/v1")
				h.cfg.qwenBase = base
				other := mustParse("https://unused.example")
				h.registry = NewProviderRegistry(NewCodexProvider(other, other, nil), NewClaudeProvider(other), NewGeminiProvider(other, other), NewQwenProvider(base))
				proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
				defer proxy.Close()
				input := []any{map[string]any{"type": "additional_tools", "tools": qwenCodexToolDeclarations()}, map[string]any{"role": "user", "content": "Create the file."}}
				payload := map[string]any{"model": "qwen3.8-max", "stream": stream, "input": input, "parallel_tool_calls": true}
				send := func(chunked bool) (map[string]any, []map[string]any) {
					data, _ := json.Marshal(payload)
					req, _ := http.NewRequest("POST", proxy.URL+path, bytes.NewReader(data))
					if chunked {
						req.ContentLength = -1
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("qwen-tools-test-secret", "codex-client"))
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					data, _ = io.ReadAll(resp.Body)
					if resp.StatusCode != 200 {
						t.Fatalf("proxy returned %d: %s", resp.StatusCode, data)
					}
					if !stream {
						var response map[string]any
						if json.Unmarshal(data, &response) != nil || response["object"] != "response" {
							t.Fatalf("invalid Responses body: %s", data)
						}
						return response, nil
					}
					events := qwenResponsesEvents(t, data)
					if len(events) < 3 || events[0]["type"] != "response.created" || events[len(events)-1]["type"] != "response.completed" {
						t.Fatalf("missing terminal Responses event: %s", data)
					}
					return jsonvalue.Map(events[len(events)-1]["response"]), events
				}
				first, events := send(false)
				output := jsonvalue.Slice(first["output"])
				if len(output) != 3 || jsonvalue.Map(output[1])["type"] != "function_call" || jsonvalue.Map(output[1])["namespace"] != "functions" || jsonvalue.Map(output[1])["name"] != "exec_command" || jsonvalue.Map(output[1])["arguments"] != `{"cmd":"pwd"}` || jsonvalue.Map(output[2])["type"] != "custom_tool_call" || jsonvalue.Map(output[2])["input"] != patch {
					t.Fatalf("tool calls changed across the proxy: %#v", first)
				}
				usage := jsonvalue.Map(first["usage"])
				if usage["input_tokens"] != float64(100) || jsonvalue.Map(usage["input_tokens_details"])["cached_tokens"] != float64(40) {
					t.Fatal("usage-only stream chunk was not included in the completed response")
				}
				if stream {
					var arguments, custom strings.Builder
					items := map[string]bool{}
					for _, event := range events {
						if event["type"] == "response.output_item.added" {
							items[jsonvalue.String(jsonvalue.Map(event["item"])["id"])] = true
						}
						if id := jsonvalue.String(event["item_id"]); id != "" && !items[id] {
							t.Fatal("tool delta references an output item that was never opened")
						}
						if event["type"] == "response.function_call_arguments.delta" {
							arguments.WriteString(jsonvalue.String(event["delta"]))
						}
						if event["type"] == "response.custom_tool_call_input.delta" {
							custom.WriteString(jsonvalue.String(event["delta"]))
						}
					}
					if arguments.String() != `{"cmd":"pwd"}` || custom.String() != patch {
						t.Fatal("fragmented arguments or raw patch deltas were changed")
					}
				}
				payload["input"] = append(input, append(output, map[string]any{"type": "custom_tool_call_output", "call_id": "call-patch", "output": "File created."}, map[string]any{"type": "function_call_output", "call_id": "call-command", "output": "/workspace"})...)
				payload["tool_choice"], payload["previous_response_id"] = "none", first["id"]
				second, _ := send(true)
				if calls.Load() != 2 || jsonvalue.Map(jsonvalue.Slice(jsonvalue.Map(jsonvalue.Slice(second["output"])[0])["content"])[0])["text"] != "Arquivo criado." {
					t.Fatalf("tool continuation failed: %#v", second)
				}
			})
		}
	}
}
