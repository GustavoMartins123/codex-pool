package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/sse"
)

// aistudioTestSignature looks like a native Gemini signature, which the
// translation layer keeps instead of replacing it with the skip value.
var aistudioTestSignature = aistudioTestSignatureFor(0)

func aistudioTestSignatureFor(n int) string {
	return base64.StdEncoding.EncodeToString(append([]byte{0x12, byte(n)}, bytes.Repeat([]byte("s"), 30)...))
}

func aistudioTestAccount(id, key string) *Account {
	return &Account{ID: id, Type: AccountTypeAIStudio, AccessToken: key, PlanType: "api_key", Models: map[string]DiscoveredModel{
		"aistudio/gemini-3.8-flash": {ID: "aistudio/gemini-3.8-flash", Protocol: "gemini", ContextWindow: 1048576, Reasoning: true},
	}}
}

func aistudioTestHandler(t *testing.T, upstream string, accounts ...*Account) *proxyHandler {
	t.Helper()
	h := qwenTestHandler(t, http.DefaultTransport, accounts...)
	other := mustParse("https://unused.example")
	h.registry = NewProviderRegistry(NewCodexProvider(other, other, nil), NewClaudeProvider(other), NewGeminiProvider(other, other), NewAIStudioProvider(mustParse(upstream)))
	return h
}

func aistudioProxyRequest(t *testing.T, h *proxyHandler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", generateClaudePoolToken("aistudio-test-secret", "aistudio-client"))
	rr := httptest.NewRecorder()
	testPoolProxyRequest(t, h, rr, req, "aistudio-proxy")
	return rr
}

// aistudioGeminiRequest decodes one upstream call and checks it uses the public
// Gemini API shape rather than the Code Assist envelope.
func aistudioGeminiRequest(t *testing.T, r *http.Request, key string) map[string]any {
	t.Helper()
	if r.URL.Path != "/v1beta/models/gemini-3.8-flash:streamGenerateContent" || r.URL.RawQuery != "alt=sse" {
		t.Errorf("AI Studio upstream URL = %s", r.URL)
	}
	if r.Header.Get("x-goog-api-key") != key || r.Header.Get("Authorization") != "" {
		t.Errorf("AI Studio credentials = key %q authorization %q", r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization"))
	}
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"request", "project", "sessionId", "model", "requestType"} {
		if _, ok := request[field]; ok {
			t.Errorf("Code Assist field %q leaked to the Gemini API: %v", field, request)
		}
	}
	if len(anySlice(request["contents"])) == 0 {
		t.Errorf("Gemini request has no contents: %v", request)
	}
	return request
}

// aistudioSignedCall finds the model functionCall part and its signature.
func aistudioSignedCall(request map[string]any) (map[string]any, string) {
	for _, raw := range anySlice(request["contents"]) {
		for _, rawPart := range anySlice(mapValue(raw)["parts"]) {
			part := mapValue(rawPart)
			if call := mapValue(part["functionCall"]); call != nil {
				return call, stringValue(part["thoughtSignature"])
			}
		}
	}
	return nil, ""
}

func aistudioFunctionResponse(request map[string]any) map[string]any {
	for _, raw := range anySlice(request["contents"]) {
		for _, rawPart := range anySlice(mapValue(raw)["parts"]) {
			if response := mapValue(mapValue(rawPart)["functionResponse"]); response != nil {
				return response
			}
		}
	}
	return nil
}

func writeGeminiSSE(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range chunks {
		fmt.Fprintf(w, "data: %s\r\n\r\n", chunk)
	}
}

// aistudioToolUpstream answers the first turn with a signed function call and
// the second turn with text, failing the test if the signature was not replayed.
func aistudioToolUpstream(t *testing.T, tool string, args map[string]any) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := aistudioGeminiRequest(t, r, "AIza-tools")
		switch calls.Add(1) {
		case 1:
			declared := false
			for _, raw := range anySlice(request["tools"]) {
				for _, declaration := range anySlice(mapValue(raw)["functionDeclarations"]) {
					declared = declared || mapValue(declaration)["name"] == tool
				}
			}
			if !declared {
				t.Errorf("tool %s was not declared to Gemini: %v", tool, request["tools"])
			}
			encoded, _ := json.Marshal(args)
			writeGeminiSSE(w,
				`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"`+tool+`","args":`+string(encoded)+`},"thoughtSignature":"`+aistudioTestSignature+`"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":5}}`)
		case 2:
			call, signature := aistudioSignedCall(request)
			if call == nil || call["name"] != tool || signature != aistudioTestSignature {
				t.Errorf("signed function call was not replayed: call=%v signature=%q", call, signature)
			}
			if response := aistudioFunctionResponse(request); response == nil || response["name"] != tool || !strings.Contains(fmt.Sprint(response["response"]), "TOOL_RESULT") {
				t.Errorf("function response = %v", response)
			}
			writeGeminiSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"AISTUDIO_DONE"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":60,"candidatesTokenCount":3}}`)
		default:
			t.Error("unexpected extra Gemini call")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return server, &calls
}

func TestAIStudioResponsesToolRoundTripReplaysSignature(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "aistudio-test-secret")
	upstream, calls := aistudioToolUpstream(t, "exec_command", map[string]any{"cmd": "cat input.txt"})
	defer upstream.Close()
	h := aistudioTestHandler(t, upstream.URL, aistudioTestAccount("key-1", "AIza-tools"))
	tools := `[{"type":"function","name":"exec_command","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]`
	first := aistudioProxyRequest(t, h, "/v1/responses", `{"model":"aistudio/gemini-3.8-flash","stream":true,"prompt_cache_key":"aistudio-conv-1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read input.txt"}]}],"tools":`+tools+`}`)
	if first.Code != 200 {
		t.Fatalf("first turn status=%d body=%s", first.Code, first.Body.String())
	}
	var output []any
	for _, frame := range strings.Split(first.Body.String(), "\n\n") {
		kind, data := sse.ParseEvent([]byte(frame))
		var event map[string]any
		if kind == "response.completed" && json.Unmarshal(data, &event) == nil {
			output = anySlice(mapValue(event["response"])["output"])
		}
	}
	var callID string
	for _, raw := range output {
		if item := mapValue(raw); item["type"] == "function_call" && item["name"] == "exec_command" {
			callID = stringValue(item["call_id"])
		}
	}
	if callID == "" {
		t.Fatalf("no function_call in Responses output: %s", first.Body.String())
	}
	// Codex resends the prior output items, then the tool result.
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "read input.txt"}}}}
	input = append(input, output...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": "TOOL_RESULT"})
	encodedInput, _ := json.Marshal(input)
	second := aistudioProxyRequest(t, h, "/v1/responses", `{"model":"aistudio/gemini-3.8-flash","stream":true,"prompt_cache_key":"aistudio-conv-1","input":`+string(encodedInput)+`,"tools":`+tools+`}`)
	if second.Code != 200 || !strings.Contains(second.Body.String(), "AISTUDIO_DONE") || calls.Load() != 2 {
		t.Fatalf("second turn status=%d calls=%d body=%.600s", second.Code, calls.Load(), second.Body.String())
	}
}

func TestAIStudioMessagesToolRoundTripReplaysSignature(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "aistudio-test-secret")
	upstream, calls := aistudioToolUpstream(t, "Bash", map[string]any{"command": "cat input.txt"})
	defer upstream.Close()
	h := aistudioTestHandler(t, upstream.URL, aistudioTestAccount("key-1", "AIza-tools"))
	tools := `[{"name":"Bash","description":"Run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]`
	user := `{"role":"user","content":"read input.txt"}`
	first := aistudioProxyRequest(t, h, "/v1/messages", `{"model":"aistudio/gemini-3.8-flash","max_tokens":1024,"metadata":{"user_id":"session_aistudio"},"messages":[`+user+`],"tools":`+tools+`}`)
	if first.Code != 200 {
		t.Fatalf("first turn status=%d body=%s", first.Code, first.Body.String())
	}
	var message map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	var toolID string
	for _, raw := range anySlice(message["content"]) {
		if block := mapValue(raw); block["type"] == "tool_use" && block["name"] == "Bash" {
			toolID = stringValue(block["id"])
		}
	}
	if toolID == "" || message["stop_reason"] != "tool_use" {
		t.Fatalf("no Bash tool_use in Claude response: %s", first.Body.String())
	}
	assistant, _ := json.Marshal(map[string]any{"role": "assistant", "content": message["content"]})
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"TOOL_RESULT"}]}`
	second := aistudioProxyRequest(t, h, "/v1/messages", `{"model":"aistudio/gemini-3.8-flash","max_tokens":1024,"metadata":{"user_id":"session_aistudio"},"messages":[`+user+`,`+string(assistant)+`,`+result+`],"tools":`+tools+`}`)
	if second.Code != 200 || !strings.Contains(second.Body.String(), "AISTUDIO_DONE") || calls.Load() != 2 {
		t.Fatalf("second turn status=%d calls=%d body=%.600s", second.Code, calls.Load(), second.Body.String())
	}
}

func TestAIStudioRateLimitedModelMovesToAnotherKey(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "aistudio-test-secret")
	var limited, served atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("x-goog-api-key") == "AIza-limited" {
			limited.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"Quota exceeded. Please retry in 30s.","status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		served.Add(1)
		writeGeminiSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`)
	}))
	defer upstream.Close()
	limitedKey, healthyKey := aistudioTestAccount("key-limited", "AIza-limited"), aistudioTestAccount("key-healthy", "AIza-healthy")
	h := aistudioTestHandler(t, upstream.URL, limitedKey, healthyKey)
	body := `{"model":"aistudio/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 3; i++ {
		rr := aistudioProxyRequest(t, h, "/v1/chat/completions", body)
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "hello") {
			t.Fatalf("request %d status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if limited.Load() > 1 || served.Load() != 3 {
		t.Fatalf("rate-limited key was retried: limited=%d served=%d", limited.Load(), served.Load())
	}
	if !aistudioModelCoolingDown(limitedKey, "gemini-3.8-flash", time.Now()) {
		t.Fatal("rate-limited model has no cooldown")
	}
}

func TestAIStudioRejectedRequestIsNotRetriedOnOtherKeys(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "aistudio-test-secret")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"Function call is missing a thought_signature","status":"INVALID_ARGUMENT"}}`)
	}))
	defer upstream.Close()
	h := aistudioTestHandler(t, upstream.URL, aistudioTestAccount("key-1", "AIza-1"), aistudioTestAccount("key-2", "AIza-2"))
	rr := aistudioProxyRequest(t, h, "/v1/messages", `{"model":"aistudio/gemini-3.8-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != http.StatusBadRequest || calls.Load() != 1 || !strings.Contains(rr.Body.String(), "thought_signature") {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, calls.Load(), rr.Body.String())
	}
}
