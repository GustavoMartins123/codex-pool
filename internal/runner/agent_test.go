package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == "-c" && os.Args[3] == "sandbox" {
		os.Exit(0)
	}
	if len(os.Args) > 1 && (os.Args[1] == "app-server" || os.Args[1] == "-p") {
		fakeAgent(os.Args[1] == "app-server")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// An independent subprocess implements only the documented CLI messages needed by
// these contract tests. It never calls an LLM or executes the proposed command.
func fakeAgent(codex bool) {
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		os.Exit(2)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	turn := 0
	model, effort := "synthetic-model", ""
	control := os.Getenv("CLAUDE_CONFIG_DIR")
	nativeID := ""
	resume := false
	if codex {
		control = os.Getenv("CODEX_HOME")
		nativeID = "thread"
	} else {
		for i, arg := range os.Args {
			if (arg == "--session-id" || arg == "--resume") && i+1 < len(os.Args) {
				nativeID = os.Args[i+1]
				resume = arg == "--resume"
			}
			if arg == "--no-session-persistence" {
				os.Exit(20)
			}
		}
	}
	nativeLogID := nativeID
	if codex {
		nativeLogID = "thread-log"
	}
	nativePath := filepath.Join(control, "sessions", nativeLogID+".jsonl")
	persist := func() {
		if os.MkdirAll(filepath.Dir(nativePath), 0700) != nil {
			os.Exit(21)
		}
		raw, _ := json.Marshal(turn)
		if os.WriteFile(nativePath, raw, 0600) != nil {
			os.Exit(22)
		}
	}
	load := func() {
		raw, err := os.ReadFile(nativePath)
		if err != nil || json.Unmarshal(raw, &turn) != nil {
			os.Exit(23)
		}
	}
	if !codex {
		if resume {
			load()
		} else {
			persist()
		}
	}
	send := func(value any) {
		if encoder.Encode(value) != nil {
			os.Exit(3)
		}
	}
	complete := func(text string, interrupted bool) {
		if codex {
			send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"itemId": "item", "delta": text}})
			send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"last": map[string]any{"totalTokens": 7}}}})
			status := "completed"
			if interrupted {
				status = "interrupted"
			}
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "agent-turn", "status": status}}})
		} else {
			send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}}})
			send(map[string]any{"type": "result", "is_error": false, "usage": map[string]any{"input_tokens": 3, "output_tokens": 4}})
		}
	}
	for scanner.Scan() {
		var frame map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(4)
		}
		if codex {
			method := rawString(frame["method"])
			switch method {
			case "initialize":
				send(map[string]any{"id": frame["id"], "result": map[string]any{}})
			case "initialized":
			case "thread/start", "thread/resume":
				var params map[string]any
				_ = json.Unmarshal(frame["params"], &params)
				if (params["model"] != "synthetic-model" && params["model"] != "gpt-5.6-sol") || params["approvalPolicy"] != "untrusted" || params["sandbox"] != "workspace-write" {
					os.Exit(5)
				}
				if method == "thread/resume" {
					if params["threadId"] != nativeID {
						os.Exit(24)
					}
					load()
				} else {
					if params["ephemeral"] != false {
						os.Exit(25)
					}
					persist()
				}
				send(map[string]any{"id": frame["id"], "result": map[string]any{"thread": map[string]any{"id": "thread", "sessionId": nativeLogID}}})
			case "turn/start":
				var params struct {
					Model  string `json:"model"`
					Effort string `json:"effort"`
					Input  []struct {
						Text string `json:"text"`
					} `json:"input"`
				}
				if json.Unmarshal(frame["params"], &params) != nil || params.Model == "" {
					os.Exit(26)
				}
				model, effort = params.Model, params.Effort
				turn++
				persist()
				send(map[string]any{"id": frame["id"], "result": map[string]any{"turn": map[string]any{"id": "agent-turn"}}})
				send(map[string]any{"method": "turn/started", "params": map[string]any{"turn": map[string]any{"id": "agent-turn"}}})
				if len(params.Input) == 1 && params.Input[0].Text == "report-settings" {
					complete(model+":"+effort, false)
					continue
				}
				send(map[string]any{"id": "approval-request", "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "echo synthetic", "reason": "test permission"}})
			case "turn/interrupt":
				send(map[string]any{"id": frame["id"], "result": map[string]any{}})
				complete("cancelled", true)
			case "":
				var decision struct {
					Decision string `json:"decision"`
				}
				_ = json.Unmarshal(frame["result"], &decision)
				if decision.Decision == "accept" {
					complete("approved", false)
				} else if decision.Decision == "decline" {
					complete("denied", false)
				} else {
					os.Exit(6)
				}
			default:
				os.Exit(7)
			}
		} else {
			switch rawString(frame["type"]) {
			case "control_request":
				var request struct {
					Subtype  string `json:"subtype"`
					Settings struct {
						Model  string `json:"model"`
						Effort string `json:"effortLevel"`
					} `json:"settings"`
				}
				_ = json.Unmarshal(frame["request"], &request)
				if request.Subtype == "apply_flag_settings" {
					model, effort = request.Settings.Model, request.Settings.Effort
				}
				send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": rawString(frame["request_id"]), "response": map[string]any{}}})
				if request.Subtype == "interrupt" {
					complete("cancelled", true)
				}
			case "user":
				turn++
				persist()
				var user struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				}
				_ = json.Unmarshal(frame["message"], &user.Message)
				if user.Message.Content == "report-settings" {
					complete(model+":"+effort, false)
					continue
				}
				send(map[string]any{"type": "control_request", "request_id": "permission-request", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "echo synthetic"}}})
			case "control_response":
				var response struct {
					Response struct {
						Behavior string `json:"behavior"`
					} `json:"response"`
				}
				_ = json.Unmarshal(frame["response"], &response)
				if response.Response.Behavior == "allow" {
					complete("approved", false)
				} else if response.Response.Behavior == "deny" {
					complete("denied", false)
				} else {
					os.Exit(8)
				}
			default:
				os.Exit(9)
			}
		}
	}
	_ = turn
}

func TestAgentApprovalDecisionAndCancellationContracts(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-inherit")
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			events := make(chan runnerproto.Message, 64)
			exits := make(chan int, 1)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			session := runnerproto.Session{ID: "synthetic-session", Agent: kind, Model: "synthetic-model", PermissionMode: "gated"}
			emit := func(m runnerproto.Message) error { events <- m; return nil }
			exit := func(code int) { exits <- code }
			workspace := t.TempDir()
			control := t.TempDir()
			var agent agentProcess
			if kind == "codex" {
				agent, err = startCodex(ctx, exe, workspace, control, "https://pool.example", "synthetic-key", session, emit, exit)
			} else {
				agent, err = startClaude(ctx, exe, workspace, control, "https://pool.example", "synthetic-key", session, emit, exit)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			next := func(kind string) runnerproto.Message {
				t.Helper()
				for {
					select {
					case <-ctx.Done():
						t.Fatal("agent event timeout")
					case event := <-events:
						if event.Type == kind {
							return event
						}
					}
				}
			}
			for index, decision := range []string{"approve", "deny"} {
				id := []string{"turn-one", "turn-two"}[index]
				if err := agent.StartTurn(id, "synthetic", runnerproto.TurnSettings{Model: session.Model}); err != nil {
					t.Fatal(err)
				}
				request := next("approval.request")
				if request.TurnID != id || request.ApprovalID == "" {
					t.Fatal("invalid approval request")
				}
				if err := agent.Decide(request.ApprovalID, decision); err != nil {
					t.Fatal(err)
				}
				finished := next("turn.finished")
				if finished.TurnID != id || finished.Tokens != 7 || finished.ExitCode != 0 {
					t.Fatalf("finish=%+v", finished)
				}
			}
			if err := agent.StartTurn("turn-three", "hold", runnerproto.TurnSettings{Model: session.Model}); err != nil {
				t.Fatal(err)
			}
			next("approval.request")
			if err := agent.Cancel("turn-three"); err != nil {
				t.Fatal(err)
			}
			if finished := next("turn.finished"); finished.ExitCode != 130 {
				t.Fatalf("cancel exit=%d", finished.ExitCode)
			}
			agent.Close()
			select {
			case <-exits:
			case <-ctx.Done():
				t.Fatal("agent process was not terminated")
			}
		})
	}
}

func TestRunnerConfigAndWorkspaceBoundaries(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.json")
	unlock, err := LockState(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if another, err := LockState(lockPath); err == nil {
		another()
		t.Fatal("runner state locked twice")
	}
	unlock()
	for _, origin := range []string{"http://pool.example", "https://user:password@pool.example", "https://pool.example/v1", "https://pool.example?token=secret"} {
		if ValidateOrigin(origin) == nil {
			t.Fatal("unsafe origin accepted")
		}
	}
	file := filepath.Join(t.TempDir(), "runner.json")
	cfg := Config{PoolURL: "https://pool.example", RunnerID: "id", Token: "pool-runner-id.synthetic"}
	if err := WriteConfig(file, cfg); err != nil {
		t.Fatal(err)
	}
	actual, err := ReadConfig(file)
	if err != nil || actual != cfg {
		t.Fatal("runner state did not round trip")
	}
	root := t.TempDir()
	outside := t.TempDir()
	session := runnerproto.Session{ID: "session", Workspace: runnerproto.Workspace{Mode: "path", Target: outside}, PermissionMode: "auto"}
	if _, err := prepareWorkspace(context.Background(), root, []string{outside}, session); err == nil {
		t.Fatal("auto path workspace accepted")
	}
	session.PermissionMode = "gated"
	if _, err := prepareWorkspace(context.Background(), root, nil, session); err == nil {
		t.Fatal("unlisted path accepted")
	}
	session.Workspace = runnerproto.Workspace{Mode: "ephemeral"}
	path, err := prepareWorkspace(context.Background(), root, nil, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupWorkspace(root, outside, "ephemeral"); err == nil {
		t.Fatal("outside cleanup accepted")
	}
	if err := cleanupWorkspace(root, path, "ephemeral"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("ephemeral workspace retained")
	}
	if err := cleanupWorkspace(root, outside, "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("repo/path workspace removed")
	}
}

func TestInstalledCLIInitialization(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) {
			exe := os.Getenv("RUNNER_TEST_" + map[string]string{"codex": "CODEX", "claude": "CLAUDE"}[kind] + "_EXE")
			if exe == "" {
				t.Skip("set explicit native executable to verify installed CLI handshake")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			exits := make(chan int, 1)
			session := runnerproto.Session{ID: "69238fb9-7fb3-440a-b375-02d28c575b11", Agent: kind, Model: "synthetic-model", PermissionMode: "gated"}
			control := t.TempDir()
			emit := func(runnerproto.Message) error { return nil }
			exit := func(code int) { exits <- code }
			var agent agentProcess
			var err error
			if kind == "codex" {
				agent, err = startCodex(ctx, exe, t.TempDir(), control, "https://pool.invalid", "synthetic-key", session, emit, exit)
			} else {
				agent, err = startClaude(ctx, exe, t.TempDir(), control, "https://pool.invalid", "synthetic-key", session, emit, exit)
			}
			if err != nil {
				t.Fatal(err)
			}
			agent.Close()
			select {
			case <-exits:
			case <-ctx.Done():
				t.Fatal("installed CLI did not terminate")
			}
			_ = filepath.WalkDir(control, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					data, _ := os.ReadFile(path)
					if strings.Contains(string(data), "synthetic-key") {
						t.Fatal("credential persisted")
					}
				}
				return nil
			})
		})
	}
}
