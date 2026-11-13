package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestQwenMessagesRealClaudeCodeCLI drives the real Claude Code CLI through the
// pool's Anthropic Messages route to a simulated Qwen Chat Completions upstream.
func TestQwenMessagesRealClaudeCodeCLI(t *testing.T) {
	binary := os.Getenv("QWEN_CLAUDE_CLI_PATH")
	if binary == "" {
		t.Skip("QWEN_CLAUDE_CLI_PATH is required for the Claude Code CLI integration test")
	}
	t.Setenv("POOL_JWT_SECRET", "qwen-claude-cli-test-secret")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(workingDirectory, ".qwen-claude-cli-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	workspace, control := filepath.Join(root, "workspace"), filepath.Join(root, "control")
	for _, directory := range []string{workspace, control} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "qwen-input.txt"), []byte("QWEN_TOOL_INPUT"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(workspace, "qwen-output.txt")
	var toolTurns atomic.Int32
	var mu sync.Mutex
	var trace []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if r.URL.Path != "/compatible-mode/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-sp-claude-cli-test" || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Errorf("Claude Code did not reach the Qwen Chat Completions endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		tools := map[string]bool{}
		for _, raw := range anySlice(request["tools"]) {
			tools[stringValue(mapValue(mapValue(raw)["function"])["name"])] = true
		}
		results := map[string]string{}
		for _, raw := range anySlice(request["messages"]) {
			message := mapValue(raw)
			if message["role"] == "tool" {
				results[stringValue(message["tool_call_id"])] = stringValue(message["content"])
			}
		}
		delta := map[string]any{"role": "assistant"}
		finish := "stop"
		switch {
		case !tools["Bash"] || !tools["Write"]:
			// Auxiliary requests (titles, summaries) carry no agent tools.
			delta["content"] = "Qwen auxiliary reply"
		case len(results) == 0:
			toolTurns.Add(1)
			args, _ := json.Marshal(map[string]any{"command": "cat qwen-input.txt", "description": "Read the input fixture"})
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-claude-bash", "type": "function", "function": map[string]any{"name": "Bash", "arguments": string(args)}}}
			finish = "tool_calls"
		case len(results) == 1 && strings.Contains(results["call-claude-bash"], "QWEN_TOOL_INPUT"):
			toolTurns.Add(1)
			args, _ := json.Marshal(map[string]any{"file_path": output, "content": "QWEN_TOOL_OUTPUT\n"})
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-claude-write", "type": "function", "function": map[string]any{"name": "Write", "arguments": string(args)}}}
			finish = "tool_calls"
		case len(results) == 2 && results["call-claude-write"] != "":
			toolTurns.Add(1)
			delta["content"] = "QWEN_CLAUDE_COMPLETE"
		default:
			t.Errorf("Claude Code returned unexpected tool results: %s", qwenCLIToolResults(request["messages"]))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		trace = append(trace, fmt.Sprintf("tools=%d results=%d finish=%s", len(tools), len(results), finish))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, choice := range []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
			data, _ := json.Marshal(map[string]any{"id": "chat-claude-cli", "model": "qwen3.8-max", "choices": []any{choice}})
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	account := &Account{ID: "qwen-claude-cli", Type: AccountTypeQwen, AccessToken: "sk-sp-claude-cli-test", Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai", ContextWindow: 128000}}}
	h := qwenTestHandler(t, http.DefaultTransport, account)
	base := mustParse(upstream.URL + "/compatible-mode/v1")
	h.cfg.qwenBase = base
	other := mustParse("https://unused.example")
	h.registry = NewProviderRegistry(NewCodexProvider(other, other, nil), NewClaudeProvider(other), NewGeminiProvider(other, other), NewQwenProvider(base))
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()

	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--model", "qwen3.8-max",
		"--allowedTools", "Bash(cat qwen-input.txt)", "Write", "--permission-mode", "acceptEdits", "--strict-mcp-config", "--setting-sources", "",
		"Read qwen-input.txt with Bash, then create qwen-output.txt with Write containing QWEN_TOOL_OUTPUT. End with QWEN_CLAUDE_COMPLETE."}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workspace
	// The child must not inherit this machine's Anthropic credentials or the
	// parent agent session; it only talks to the local proxy.
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE") || name == "HOME" {
			continue
		}
		env = append(env, entry)
	}
	command.Env = append(env, "HOME="+control, "CLAUDE_CONFIG_DIR="+control,
		"ANTHROPIC_BASE_URL="+proxy.URL, "ANTHROPIC_API_KEY="+generateClaudePoolToken("qwen-claude-cli-test-secret", "claude-cli"),
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=qwen3.8-max", "ANTHROPIC_SMALL_FAST_MODEL=qwen3.8-max",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1")
	result, err := command.CombinedOutput()
	mu.Lock()
	turns := strings.Join(trace, "; ")
	mu.Unlock()
	if err != nil {
		t.Fatalf("Claude Code CLI tool round trip failed: %v\nupstream: %s\n%s", err, turns, codexCLIOutputTail(result))
	}
	if toolTurns.Load() != 3 || !strings.Contains(string(result), "QWEN_CLAUDE_COMPLETE") {
		t.Fatalf("Claude Code did not finish the tool conversation: turns=%d\nupstream: %s\n%s", toolTurns.Load(), turns, codexCLIOutputTail(result))
	}
	created, err := os.ReadFile(output)
	if err != nil || strings.TrimSpace(string(created)) != "QWEN_TOOL_OUTPUT" {
		t.Fatalf("Claude Code did not write the Qwen file: %q %v\n%s", created, err, codexCLIOutputTail(result))
	}
	// Claude Code sizes its context from the reported input tokens.
	var reported bool
	for _, line := range strings.Split(string(result), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["type"] == "result" {
			reported = mapValue(event["usage"])["input_tokens"] == float64(300)
		}
	}
	if !reported {
		t.Fatalf("Claude Code did not receive the Qwen prompt usage:\n%s", codexCLIOutputTail(result))
	}
	t.Logf("upstream turns: %s", turns)
}
