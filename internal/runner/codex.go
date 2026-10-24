package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/google/uuid"
)

type codexApproval struct {
	requestID   json.RawMessage
	method      string
	permissions json.RawMessage
}
type codexProcess struct {
	io                      *jsonProcess
	session                 runnerproto.Session
	emit                    func(runnerproto.Message) error
	mu                      sync.Mutex
	thread, turn, agentTurn string
	started                 time.Time
	tokens                  int64
	permissions             map[string]codexApproval
	textItems               map[string]bool
}

func startCodex(ctx context.Context, exe, workspace, control, origin, token string, session runnerproto.Session, emit func(runnerproto.Message) error, onExit func(int)) (agentProcess, error) {
	if session.PermissionMode == "gated" {
		if err := checkCodexSandbox(ctx, exe, workspace, control); err != nil {
			return nil, err
		}
	}
	settings := []string{
		`model_provider="pool"`, `model_providers.pool.name="codex-pool"`,
		"model_providers.pool.base_url=" + strconv.Quote(origin+"/v1"),
		`model_providers.pool.env_key="POOL_SESSION_TOKEN"`, `model_providers.pool.wire_api="responses"`,
		`model_providers.pool.requires_openai_auth=false`, `cli_auth_credentials_store="ephemeral"`,
		`mcp_servers={}`, `features.apps=false`, `features.remote_skills=false`, `features.shell_snapshot=false`,
		`model_providers.pool.request_max_retries=0`, `model_providers.pool.stream_max_retries=0`,
	}
	if runtime.GOOS == "windows" && session.PermissionMode == "gated" {
		settings = append(settings, `windows.sandbox="mxc"`)
	}
	args := []string{"app-server", "--listen", "stdio://"}
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	command := exec.Command(exe, args...)
	command.Dir = workspace
	command.Env = cleanEnvironment(map[string]string{"CODEX_HOME": control, "POOL_SESSION_TOKEN": token, "OTEL_SDK_DISABLED": "true", "GIT_TERMINAL_PROMPT": "0"})
	process, err := spawnJSONProcess(ctx, command)
	if err != nil {
		return nil, err
	}
	a := &codexProcess{io: process, session: session, emit: emit, permissions: map[string]codexApproval{}, textItems: map[string]bool{}}
	go process.read(a.handle, onExit)
	ready := false
	defer func() {
		if !ready {
			process.close()
			<-process.done
		}
	}()
	if _, err := a.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "codex_pool_runner", "version": "1"}}); err != nil {
		process.close()
		return nil, err
	}
	if err := process.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		process.close()
		return nil, err
	}
	policy := "untrusted"
	sandbox := "workspace-write"
	if session.PermissionMode == "auto" {
		policy = "never"
		sandbox = "danger-full-access"
	}
	result, err := a.call("thread/start", map[string]any{"model": session.Model, "modelProvider": "pool", "cwd": workspace, "approvalPolicy": policy, "approvalsReviewer": "user", "sandbox": sandbox, "ephemeral": true, "serviceName": "codex-pool-runner"})
	if err != nil {
		process.close()
		return nil, err
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(result, &response) != nil || response.Thread.ID == "" {
		process.close()
		return nil, errors.New("invalid Codex thread response")
	}
	a.mu.Lock()
	a.thread = response.Thread.ID
	a.mu.Unlock()
	ready = true
	return a, nil
}

func (a *codexProcess) call(method string, params any) (json.RawMessage, error) {
	id := a.io.nextID()
	return a.io.call(id, map[string]any{"id": id, "method": method, "params": params})
}
func (a *codexProcess) Close() { a.io.close() }
func (a *codexProcess) StartTurn(id, input string) error {
	a.mu.Lock()
	if a.turn == id {
		a.mu.Unlock()
		return nil
	}
	if a.turn != "" {
		a.mu.Unlock()
		return errors.New("agent turn already running")
	}
	a.turn = id
	a.agentTurn = ""
	a.tokens = 0
	a.started = time.Now()
	a.textItems = map[string]bool{}
	thread := a.thread
	a.mu.Unlock()
	m := runnerproto.Frame("turn.started")
	m.TurnID = id
	if err := a.emit(m); err != nil {
		return err
	}
	result, err := a.call("turn/start", map[string]any{"threadId": thread, "input": []map[string]any{{"type": "text", "text": input}}})
	if err != nil {
		return err
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &response) != nil || response.Turn.ID == "" {
		return errors.New("invalid Codex turn response")
	}
	a.mu.Lock()
	if a.turn == id {
		a.agentTurn = response.Turn.ID
	}
	a.mu.Unlock()
	return nil
}
func (a *codexProcess) Cancel(id string) error {
	a.mu.Lock()
	thread, turn, active := a.thread, a.agentTurn, a.turn == id
	a.mu.Unlock()
	if !active {
		return nil
	}
	if turn == "" {
		return errors.New("Codex turn identifier unavailable")
	}
	_, err := a.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}
func (a *codexProcess) Decide(id, decision string) error {
	a.mu.Lock()
	permission, ok := a.permissions[id]
	if ok {
		delete(a.permissions, id)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	if decision != "approve" && decision != "deny" {
		return errors.New("invalid permission decision")
	}
	var result any
	if permission.method == "item/permissions/requestApproval" {
		permissions := json.RawMessage(`{}`)
		if decision == "approve" {
			permissions = permission.permissions
		}
		result = map[string]any{"permissions": permissions, "scope": "turn"}
	} else {
		value := "decline"
		if decision == "approve" {
			value = "accept"
		}
		result = map[string]any{"decision": value}
	}
	return a.io.write(map[string]any{"id": permission.requestID, "result": result})
}

func (a *codexProcess) delta(kind, content string) error {
	if content == "" {
		return nil
	}
	a.mu.Lock()
	turn := a.turn
	a.mu.Unlock()
	if turn == "" {
		return errors.New("Codex output outside a turn")
	}
	return emitChunks(a.emit, turn, kind, content)
}

func (a *codexProcess) handle(message map[string]json.RawMessage) error {
	method := rawString(message["method"])
	if method == "" {
		id := rawString(message["id"])
		a.io.deliver(id, message["result"], len(message["error"]) > 0)
		return nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(message["params"], &params); err != nil {
		return err
	}
	if requestID := message["id"]; len(requestID) > 0 {
		if method != "item/commandExecution/requestApproval" && method != "item/fileChange/requestApproval" && method != "item/permissions/requestApproval" {
			return a.io.write(map[string]any{"id": requestID, "error": map[string]any{"code": -32601, "message": "Runner does not support this client request"}})
		}
		a.mu.Lock()
		turn := a.turn
		aid := uuid.NewString()
		a.permissions[aid] = codexApproval{requestID, method, params["permissions"]}
		a.mu.Unlock()
		if turn == "" {
			return errors.New("approval outside a turn")
		}
		if a.session.PermissionMode == "auto" {
			return a.Decide(aid, "approve")
		}
		command := rawString(params["command"])
		if command == "" {
			command = method + " " + string(message["params"])
		}
		if len(command) > 32<<10 {
			return errors.New("approval description exceeds limit")
		}
		m := runnerproto.Frame("approval.request")
		m.TurnID = turn
		m.ApprovalID = aid
		m.Command = command
		m.Context = rawString(params["reason"])
		return a.emit(m)
	}
	switch method {
	case "turn/started":
		var turn struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(params["turn"], &turn); err != nil {
			return err
		}
		a.mu.Lock()
		a.agentTurn = turn.ID
		a.mu.Unlock()
	case "item/agentMessage/delta":
		a.mu.Lock()
		a.textItems[rawString(params["itemId"])] = true
		a.mu.Unlock()
		return a.delta("text", rawString(params["delta"]))
	case "item/reasoning/summaryTextDelta":
		return a.delta("thinking", rawString(params["delta"]))
	case "item/commandExecution/outputDelta":
		return a.delta("tool_result", rawString(params["delta"]))
	case "item/started", "item/completed":
		var item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Command string          `json:"command"`
			Status  string          `json:"status"`
			Changes json.RawMessage `json:"changes"`
		}
		if err := json.Unmarshal(params["item"], &item); err != nil {
			return err
		}
		if method == "item/started" {
			switch item.Type {
			case "commandExecution":
				return a.delta("tool_call", item.Command)
			case "fileChange":
				return a.delta("tool_call", string(item.Changes))
			case "mcpToolCall":
				return a.delta("tool_call", string(params["item"]))
			}
		}
		if method == "item/completed" {
			if item.Type == "agentMessage" {
				a.mu.Lock()
				seen := a.textItems[item.ID]
				a.mu.Unlock()
				if !seen {
					return a.delta("text", item.Text)
				}
			}
			if item.Type == "fileChange" || item.Type == "commandExecution" {
				return a.delta("tool_result", item.Type+": "+item.Status)
			}
		}
	case "thread/tokenUsage/updated":
		var usage struct {
			Last struct {
				Total int64 `json:"totalTokens"`
			} `json:"last"`
		}
		if err := json.Unmarshal(params["tokenUsage"], &usage); err != nil {
			return err
		}
		a.mu.Lock()
		a.tokens = usage.Last.Total
		a.mu.Unlock()
	case "turn/completed":
		var turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(params["turn"], &turn); err != nil {
			return err
		}
		a.mu.Lock()
		id := a.turn
		elapsed := time.Since(a.started).Milliseconds()
		tokens := a.tokens
		a.turn = ""
		a.agentTurn = ""
		a.permissions = map[string]codexApproval{}
		a.mu.Unlock()
		if id == "" {
			return errors.New("unexpected Codex completion")
		}
		m := runnerproto.Frame("turn.finished")
		m.TurnID = id
		m.DurationMS = elapsed
		m.Tokens = tokens
		switch turn.Status {
		case "completed":
		case "interrupted":
			m.ExitCode = 130
		case "failed":
			m.ExitCode = 1
		default:
			return errors.New("invalid Codex turn status")
		}
		return a.emit(m)
	case "model/rerouted":
		return errors.New("Codex attempted to change the selected model")
	case "serverRequest/resolved":
		requestID := params["requestId"]
		a.mu.Lock()
		cancelled := []string{}
		turn := a.turn
		for id, permission := range a.permissions {
			if string(permission.requestID) == string(requestID) {
				cancelled = append(cancelled, id)
				delete(a.permissions, id)
			}
		}
		a.mu.Unlock()
		for _, id := range cancelled {
			m := runnerproto.Frame("approval.cancelled")
			m.ApprovalID = id
			m.TurnID = turn
			if err := a.emit(m); err != nil {
				return err
			}
		}
	case "error":
		// The terminal turn event carries failure without exposing credential-bearing CLI diagnostics.
	}
	return nil
}
