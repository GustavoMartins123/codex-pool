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
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestQwenResponsesRealCodexCLI(t *testing.T) {
	binary := os.Getenv("QWEN_CODEX_CLI_PATH")
	if binary == "" {
		t.Skip("QWEN_CODEX_CLI_PATH is required for the Codex CLI integration test")
	}
	// Codex sends turns over HTTP or over its Responses websocket; the pool
	// must route the model to its provider either way.
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) { runQwenResponsesRealCodexCLI(t, binary, transport == "websocket") })
	}
}

func runQwenResponsesRealCodexCLI(t *testing.T, binary string, websockets bool) {
	t.Setenv("POOL_JWT_SECRET", "qwen-cli-test-secret")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(workingDirectory, ".qwen-cli-test-*")
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
	patch := "*** Begin Patch\n*** Add File: qwen-output.txt\n+QWEN_TOOL_OUTPUT\n*** End Patch"
	var calls atomic.Int32
	var shellName, patchName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := calls.Add(1)
		var request map[string]any
		if r.URL.Path != "/compatible-mode/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-sp-cli-test" || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("Codex did not reach the Qwen Chat Completions endpoint")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request["stream"] != true || request["input"] != nil {
			t.Error("Codex Responses request was not converted to streaming Chat Completions")
		}
		if turn == 1 {
			var names []string
			for _, raw := range anySlice(request["tools"]) {
				function := mapValue(mapValue(raw)["function"])
				name := stringValue(function["name"])
				names = append(names, name)
				if strings.HasPrefix(name, "functions_exec_command_") {
					shellName = name
				}
				if name == "apply_patch" || strings.HasPrefix(name, "functions_apply_patch_") {
					patchName = name
				}
			}
			if shellName == "" || patchName == "" {
				t.Errorf("Codex tools were not available to Qwen: %v", names)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		if turn > 1 {
			wantID, wantOutput := "call-cli-shell", "QWEN_TOOL_INPUT"
			if turn == 3 {
				wantID, wantOutput = "call-cli-patch", "qwen-output.txt"
			}
			found := false
			for _, raw := range anySlice(request["messages"]) {
				message := mapValue(raw)
				if message["role"] == "tool" && message["tool_call_id"] == wantID && strings.Contains(stringValue(message["content"]), wantOutput) {
					found = true
				}
			}
			if !found {
				t.Errorf("Codex did not execute and return %s; tool results: %s", wantID, qwenCLIToolResults(request["messages"]))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		delta := map[string]any{"role": "assistant"}
		finish := "tool_calls"
		switch turn {
		case 1:
			command := "cat qwen-input.txt"
			if runtime.GOOS == "windows" {
				command = "Get-Content -LiteralPath qwen-input.txt"
			}
			args, _ := json.Marshal(map[string]any{"cmd": command, "workdir": workspace, "tty": true, "yield_time_ms": 1000})
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-cli-shell", "type": "function", "function": map[string]any{"name": shellName, "arguments": string(args)}}}
		case 2:
			args, _ := json.Marshal(map[string]string{"input": patch})
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-cli-patch", "type": "function", "function": map[string]any{"name": patchName, "arguments": string(args)}}}
		case 3:
			delta["content"], finish = "QWEN_CODEX_COMPLETE", "stop"
		default:
			t.Error("Codex sent an unexpected extra model request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, choice := range []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
			data, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("chat-cli-%d", turn), "choices": []any{choice}})
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	account := &Account{ID: "qwen-cli", Type: AccountTypeQwen, AccessToken: "sk-sp-cli-test", Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai", ContextWindow: 128000}}}
	accounts := []*Account{account}
	codexUpstream := mustParse("https://unused.example")
	if websockets {
		// The websocket handshake binds a Codex account; its upstream must
		// never receive a Qwen turn.
		upstream := newFakeCodexUpstream(t)
		upstream.on("acct_cli_ws", func(ctx context.Context, conn *websocket.Conn) {
			if _, data, err := conn.Read(ctx); err == nil {
				t.Errorf("Qwen turn reached the ChatGPT websocket upstream: %s", clipDiagnostic(string(data), 200))
			}
		})
		codexUpstream = mustParse(upstream.server.URL)
		accounts = append(accounts, &Account{Type: AccountTypeCodex, ID: "cli-ws", AccessToken: "cli-ws-token", AccountID: "acct_cli_ws", PlanType: "pro"})
	}
	h := qwenTestHandler(t, http.DefaultTransport, accounts...)
	base := mustParse(upstream.URL + "/compatible-mode/v1")
	h.cfg.qwenBase = base
	other := mustParse("https://unused.example")
	h.registry = NewProviderRegistry(NewCodexProvider(codexUpstream, codexUpstream, codexUpstream), NewClaudeProvider(other), NewGeminiProvider(other, other), NewQwenProvider(base))
	h.cfg.websocketReadLimit = 64 << 20
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()
	catalog := filepath.Join(control, "models.json")
	if err := os.WriteFile(catalog, []byte(runnerCatalog(discoveredModelsForPool(h.pool)).CodexCatalog), 0600); err != nil {
		t.Fatal(err)
	}
	lastMessage := filepath.Join(control, "last-message.txt")
	args := []string{"exec", "--ignore-user-config", "--ephemeral", "--skip-git-repo-check", "--json", "--color", "never", "-s", "workspace-write", "-C", workspace, "-m", "qwen3.8-max", "--output-last-message", lastMessage}
	settings := []string{
		`model_provider="qwen-test"`, `model_providers.qwen-test.name="Qwen test"`,
		"model_providers.qwen-test.base_url=" + strconv.Quote(proxy.URL+"/v1"),
		`model_providers.qwen-test.env_key="QWEN_CODEX_TEST_TOKEN"`, `model_providers.qwen-test.wire_api="responses"`,
		`model_providers.qwen-test.requires_openai_auth=false`, `model_providers.qwen-test.request_max_retries=0`, `model_providers.qwen-test.stream_max_retries=0`,
		"model_catalog_json=" + strconv.Quote(catalog), `cli_auth_credentials_store="ephemeral"`,
		`mcp_servers={}`, `features.apps=false`, `features.shell_snapshot=false`,
	}
	if websockets {
		settings = append(settings, `model_providers.qwen-test.supports_websockets=true`, `model_providers.qwen-test.features.responses_websockets_v2=true`)
	} else {
		settings = append(settings, `model_providers.qwen-test.supports_websockets=false`)
	}
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	args = append(args, "Read qwen-input.txt with exec_command, then create qwen-output.txt with apply_patch containing QWEN_TOOL_OUTPUT. End with QWEN_CODEX_COMPLETE.")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workspace
	command.Env = append(os.Environ(), "CODEX_HOME="+control, "QWEN_CODEX_TEST_TOKEN="+generateClaudePoolToken("qwen-cli-test-secret", "codex-cli"), "OTEL_SDK_DISABLED=true")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex CLI tool round trip failed: %v\n%s", err, codexCLIOutputTail(output))
	}
	result, err := os.ReadFile(lastMessage)
	if err != nil || strings.TrimSpace(string(result)) != "QWEN_CODEX_COMPLETE" || calls.Load() != 3 {
		t.Fatalf("Codex did not finish the tool conversation: calls=%d last=%q err=%v\n%s", calls.Load(), result, err, codexCLIOutputTail(output))
	}
	created, err := os.ReadFile(filepath.Join(workspace, "qwen-output.txt"))
	if err != nil || strings.TrimSpace(string(created)) != "QWEN_TOOL_OUTPUT" {
		t.Fatalf("Codex did not apply the Qwen patch: %q %v\n%s", created, err, codexCLIOutputTail(output))
	}
}

const codexCLIDiagnosticLines, codexCLIDiagnosticWidth = 12, 240

// qwenCLIToolResults summarizes only the tool messages so a failed turn does
// not dump the whole converted conversation history.
func qwenCLIToolResults(messages any) string {
	var results []string
	for _, raw := range anySlice(messages) {
		message := mapValue(raw)
		if message["role"] == "tool" {
			results = append(results, fmt.Sprintf("%s=%q", stringValue(message["tool_call_id"]), clipDiagnostic(stringValue(message["content"]), 120)))
		}
	}
	if len(results) == 0 {
		return "none"
	}
	return strings.Join(results, ", ")
}

// codexCLIOutputTail keeps the last JSON events of the CLI run, where the
// failure is reported, and clips each one to a readable width.
func codexCLIOutputTail(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	omitted := 0
	if len(lines) > codexCLIDiagnosticLines {
		omitted, lines = len(lines)-codexCLIDiagnosticLines, lines[len(lines)-codexCLIDiagnosticLines:]
	}
	for i, line := range lines {
		lines[i] = clipDiagnostic(line, codexCLIDiagnosticWidth)
	}
	if omitted > 0 {
		lines = append([]string{fmt.Sprintf("... %d earlier output lines omitted", omitted)}, lines...)
	}
	return strings.Join(lines, "\n")
}

func clipDiagnostic(value string, width int) string {
	if len(value) <= width {
		return value
	}
	return value[:width] + fmt.Sprintf("... (%d bytes)", len(value))
}
