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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAIStudioResponsesRealCodexCLI drives the real Codex CLI through the pool
// to a simulated Gemini API that requires thought signatures on tool calls.
func TestAIStudioResponsesRealCodexCLI(t *testing.T) {
	binary := os.Getenv("AISTUDIO_CODEX_CLI_PATH")
	if binary == "" {
		t.Skip("AISTUDIO_CODEX_CLI_PATH is required for the Codex CLI integration test")
	}
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) { runAIStudioResponsesRealCodexCLI(t, binary, transport == "websocket") })
	}
}

func runAIStudioResponsesRealCodexCLI(t *testing.T, binary string, websockets bool) {
	t.Setenv("POOL_JWT_SECRET", "aistudio-cli-test-secret")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(workingDirectory, ".aistudio-cli-test-*")
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
	if err := os.WriteFile(filepath.Join(workspace, "aistudio-input.txt"), []byte("AISTUDIO_TOOL_INPUT"), 0600); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Add File: aistudio-output.txt\n+AISTUDIO_TOOL_OUTPUT\n*** End Patch"
	var calls atomic.Int32
	var mu sync.Mutex
	var trace []string
	// Gemini signs every call differently; each must come back in place.
	signed := []string{aistudioTestSignatureFor(1), aistudioTestSignatureFor(2)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := aistudioGeminiRequest(t, r, "AIza-cli-test")
		turn := calls.Add(1)
		var shellName, patchName string
		for _, raw := range anySlice(request["tools"]) {
			for _, declaration := range anySlice(mapValue(raw)["functionDeclarations"]) {
				name := stringValue(mapValue(declaration)["name"])
				if strings.Contains(name, "exec_command") {
					shellName = name
				}
				if strings.Contains(name, "apply_patch") {
					patchName = name
				}
			}
		}
		var signatures []string
		var results []string
		for _, rawContent := range anySlice(request["contents"]) {
			for _, rawPart := range anySlice(mapValue(rawContent)["parts"]) {
				part := mapValue(rawPart)
				if mapValue(part["functionCall"]) != nil {
					signatures = append(signatures, stringValue(part["thoughtSignature"]))
				}
				if response := mapValue(part["functionResponse"]); response != nil {
					results = append(results, fmt.Sprint(response["response"]))
				}
			}
		}
		mu.Lock()
		trace = append(trace, fmt.Sprintf("turn=%d shell=%q patch=%q signatures=%d results=%d", turn, shellName, patchName, len(signatures), len(results)))
		mu.Unlock()
		if int(turn) <= len(signed)+1 && strings.Join(signatures, ",") != strings.Join(signed[:turn-1], ",") {
			t.Errorf("turn %d replayed signatures %v, want %v", turn, signatures, signed[:turn-1])
		}
		var part string
		switch turn {
		case 1:
			if shellName == "" || patchName == "" {
				t.Errorf("Codex tools were not declared to Gemini")
			}
			args, _ := json.Marshal(map[string]any{"cmd": "cat aistudio-input.txt", "workdir": workspace, "yield_time_ms": 1000})
			part = `{"functionCall":{"name":"` + shellName + `","args":` + string(args) + `},"thoughtSignature":"` + signed[0] + `"}`
		case 2:
			if len(results) != 1 || !strings.Contains(results[0], "AISTUDIO_TOOL_INPUT") {
				t.Errorf("Codex did not return the command output: %d results", len(results))
			}
			args, _ := json.Marshal(map[string]any{"input": patch})
			part = `{"functionCall":{"name":"` + patchName + `","args":` + string(args) + `},"thoughtSignature":"` + signed[1] + `"}`
		case 3:
			if len(results) != 2 {
				t.Errorf("Codex did not return the patch result: %d results", len(results))
			}
			part = `{"text":"AISTUDIO_CODEX_COMPLETE"}`
		default:
			t.Error("Codex sent an unexpected extra model request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeGeminiSSE(w, `{"candidates":[{"content":{"role":"model","parts":[`+part+`]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20}}`)
	}))
	defer upstream.Close()
	accounts := []*Account{aistudioTestAccount("aistudio-cli", "AIza-cli-test")}
	codexUpstream := mustParse("https://unused.example")
	if websockets {
		// The websocket handshake binds a Codex account; its upstream must
		// never receive an AI Studio turn.
		fake := newFakeCodexUpstream(t)
		fake.on("acct_aistudio_ws", func(ctx context.Context, conn *websocket.Conn) {
			if _, data, err := conn.Read(ctx); err == nil {
				t.Errorf("AI Studio turn reached the ChatGPT websocket upstream: %s", clipDiagnostic(string(data), 200))
			}
		})
		codexUpstream = mustParse(fake.server.URL)
		accounts = append(accounts, &Account{Type: AccountTypeCodex, ID: "aistudio-ws", AccessToken: "ws-token", AccountID: "acct_aistudio_ws", PlanType: "pro"})
	}
	h := aistudioTestHandler(t, upstream.URL, accounts...)
	h.registry = NewProviderRegistry(NewCodexProvider(codexUpstream, codexUpstream, codexUpstream), NewClaudeProvider(codexUpstream), NewGeminiProvider(codexUpstream, codexUpstream), NewAIStudioProvider(mustParse(upstream.URL)))
	h.cfg.websocketReadLimit = 64 << 20
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()
	catalog := filepath.Join(control, "models.json")
	if err := os.WriteFile(catalog, []byte(runnerCatalog(discoveredModelsForPool(h.pool)).CodexCatalog), 0600); err != nil {
		t.Fatal(err)
	}
	lastMessage := filepath.Join(control, "last-message.txt")
	args := []string{"exec", "--ignore-user-config", "--ephemeral", "--skip-git-repo-check", "--json", "--color", "never", "-s", "workspace-write", "-C", workspace, "-m", "aistudio/gemini-3.8-flash", "--output-last-message", lastMessage}
	settings := []string{
		`model_provider="aistudio-test"`, `model_providers.aistudio-test.name="AI Studio test"`,
		"model_providers.aistudio-test.base_url=" + strconv.Quote(proxy.URL+"/v1"),
		`model_providers.aistudio-test.env_key="AISTUDIO_CODEX_TEST_TOKEN"`, `model_providers.aistudio-test.wire_api="responses"`,
		`model_providers.aistudio-test.requires_openai_auth=false`, `model_providers.aistudio-test.request_max_retries=0`, `model_providers.aistudio-test.stream_max_retries=0`,
		"model_catalog_json=" + strconv.Quote(catalog), `cli_auth_credentials_store="ephemeral"`,
		`mcp_servers={}`, `features.apps=false`, `features.shell_snapshot=false`,
	}
	if websockets {
		settings = append(settings, `model_providers.aistudio-test.supports_websockets=true`, `model_providers.aistudio-test.features.responses_websockets_v2=true`)
	} else {
		settings = append(settings, `model_providers.aistudio-test.supports_websockets=false`)
	}
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	args = append(args, "Read aistudio-input.txt with exec_command, then create aistudio-output.txt with apply_patch. End with AISTUDIO_CODEX_COMPLETE.")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workspace
	command.Env = append(os.Environ(), "CODEX_HOME="+control, "AISTUDIO_CODEX_TEST_TOKEN="+generateClaudePoolToken("aistudio-cli-test-secret", "codex-cli"), "OTEL_SDK_DISABLED=true")
	output, err := command.CombinedOutput()
	mu.Lock()
	turns := strings.Join(trace, "\n")
	mu.Unlock()
	if err != nil {
		t.Fatalf("Codex CLI tool round trip failed: %v\nupstream:\n%s\n%s", err, turns, codexCLIOutputTail(output))
	}
	result, err := os.ReadFile(lastMessage)
	if err != nil || strings.TrimSpace(string(result)) != "AISTUDIO_CODEX_COMPLETE" || calls.Load() != 3 {
		t.Fatalf("Codex did not finish the tool conversation: calls=%d last=%q err=%v\nupstream:\n%s\n%s", calls.Load(), result, err, turns, codexCLIOutputTail(output))
	}
	created, err := os.ReadFile(filepath.Join(workspace, "aistudio-output.txt"))
	if err != nil || strings.TrimSpace(string(created)) != "AISTUDIO_TOOL_OUTPUT" {
		t.Fatalf("Codex did not apply the patch: %q %v\nupstream:\n%s\n%s", created, err, turns, codexCLIOutputTail(output))
	}
	t.Logf("upstream turns:\n%s", turns)
}

// TestAIStudioMessagesRealClaudeCodeCLI drives the real Claude Code CLI through
// the pool's Messages route to a simulated Gemini API.
func TestAIStudioMessagesRealClaudeCodeCLI(t *testing.T) {
	binary := os.Getenv("AISTUDIO_CLAUDE_CLI_PATH")
	if binary == "" {
		t.Skip("AISTUDIO_CLAUDE_CLI_PATH is required for the Claude Code CLI integration test")
	}
	t.Setenv("POOL_JWT_SECRET", "aistudio-claude-cli-secret")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(workingDirectory, ".aistudio-claude-cli-test-*")
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
	if err := os.WriteFile(filepath.Join(workspace, "aistudio-input.txt"), []byte("AISTUDIO_TOOL_INPUT"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(workspace, "aistudio-output.txt")
	signed := []string{aistudioTestSignatureFor(1), aistudioTestSignatureFor(2)}
	var toolTurns atomic.Int32
	var mu sync.Mutex
	var trace []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := aistudioGeminiRequest(t, r, "AIza-claude-cli")
		tools := map[string]bool{}
		for _, raw := range anySlice(request["tools"]) {
			for _, declaration := range anySlice(mapValue(raw)["functionDeclarations"]) {
				tools[stringValue(mapValue(declaration)["name"])] = true
			}
		}
		var signatures, results []string
		for _, rawContent := range anySlice(request["contents"]) {
			for _, rawPart := range anySlice(mapValue(rawContent)["parts"]) {
				part := mapValue(rawPart)
				if mapValue(part["functionCall"]) != nil {
					signatures = append(signatures, stringValue(part["thoughtSignature"]))
				}
				if response := mapValue(part["functionResponse"]); response != nil {
					results = append(results, fmt.Sprint(response["response"]))
				}
			}
		}
		var part string
		switch {
		case !tools["Bash"] || !tools["Write"]:
			part = `{"text":"auxiliary reply"}`
		case len(results) == 0:
			toolTurns.Add(1)
			part = `{"functionCall":{"name":"Bash","args":{"command":"cat aistudio-input.txt","description":"Read the fixture"}},"thoughtSignature":"` + signed[0] + `"}`
		case len(results) == 1 && strings.Contains(results[0], "AISTUDIO_TOOL_INPUT"):
			toolTurns.Add(1)
			args, _ := json.Marshal(map[string]any{"file_path": output, "content": "AISTUDIO_TOOL_OUTPUT\n"})
			part = `{"functionCall":{"name":"Write","args":` + string(args) + `},"thoughtSignature":"` + signed[1] + `"}`
		case len(results) == 2:
			toolTurns.Add(1)
			part = `{"text":"AISTUDIO_CLAUDE_COMPLETE"}`
		default:
			t.Errorf("unexpected tool results: %d", len(results))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(results) > 0 && strings.Join(signatures, ",") != strings.Join(signed[:len(results)], ",") {
			t.Errorf("Claude Code turn with %d results replayed signatures %v", len(results), signatures)
		}
		mu.Lock()
		trace = append(trace, fmt.Sprintf("tools=%d signatures=%d results=%d", len(tools), len(signatures), len(results)))
		mu.Unlock()
		writeGeminiSSE(w, `{"candidates":[{"content":{"role":"model","parts":[`+part+`]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20}}`)
	}))
	defer upstream.Close()
	h := aistudioTestHandler(t, upstream.URL, aistudioTestAccount("aistudio-claude", "AIza-claude-cli"))
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--model", "aistudio/gemini-3.8-flash",
		"--allowedTools", "Bash(cat aistudio-input.txt)", "Write", "--permission-mode", "acceptEdits", "--strict-mcp-config", "--setting-sources", "",
		"Read aistudio-input.txt with Bash, then create aistudio-output.txt with Write. End with AISTUDIO_CLAUDE_COMPLETE."}
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
		"ANTHROPIC_BASE_URL="+proxy.URL, "ANTHROPIC_API_KEY="+generateClaudePoolToken("aistudio-claude-cli-secret", "claude-cli"),
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=aistudio/gemini-3.8-flash", "ANTHROPIC_SMALL_FAST_MODEL=aistudio/gemini-3.8-flash",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1")
	result, err := command.CombinedOutput()
	mu.Lock()
	turns := strings.Join(trace, "; ")
	mu.Unlock()
	if err != nil || toolTurns.Load() != 3 || !strings.Contains(string(result), "AISTUDIO_CLAUDE_COMPLETE") {
		t.Fatalf("Claude Code tool round trip failed: err=%v turns=%d\nupstream: %s\n%s", err, toolTurns.Load(), turns, codexCLIOutputTail(result))
	}
	created, err := os.ReadFile(output)
	if err != nil || strings.TrimSpace(string(created)) != "AISTUDIO_TOOL_OUTPUT" {
		t.Fatalf("Claude Code did not write the file: %q %v\n%s", created, err, codexCLIOutputTail(result))
	}
	t.Logf("upstream turns: %s", turns)
}
