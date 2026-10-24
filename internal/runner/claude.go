package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/google/uuid"
)

type claudeApproval struct {
	requestID string
	input     json.RawMessage
}
type claudeProcess struct {
	io          *jsonProcess
	session     runnerproto.Session
	emit        func(runnerproto.Message) error
	mu          sync.Mutex
	turn        string
	started     time.Time
	cancelled   bool
	permissions map[string]claudeApproval
}

func startClaude(ctx context.Context, exe, workspace, control, origin, token string, session runnerproto.Session, emit func(runnerproto.Message) error, onExit func(int)) (agentProcess, error) {
	mode := "default"
	if session.PermissionMode == "auto" {
		mode = "bypassPermissions"
	}
	args := []string{"-p", "--output-format", "stream-json", "--input-format", "stream-json", "--verbose", "--include-partial-messages", "--bare", "--model", session.Model, "--permission-mode", mode, "--permission-prompt-tool", "stdio", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	if session.ResumeRequested {
		if session.Checkpoint == nil || session.Checkpoint.Version != 1 || session.Checkpoint.NativeSessionID != session.ID || session.Checkpoint.NativeLogID != session.ID {
			return nil, errors.New("Claude resume requires its original native session checkpoint")
		}
		args = append(args, "--resume", session.Checkpoint.NativeSessionID)
	} else {
		args = append(args, "--session-id", session.ID)
	}
	command := exec.Command(exe, args...)
	command.Dir = workspace
	command.Env = cleanEnvironment(map[string]string{"CLAUDE_CONFIG_DIR": control, "ANTHROPIC_BASE_URL": origin, "ANTHROPIC_API_KEY": token, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_ENABLE_TELEMETRY": "0", "CLAUDE_CODE_SDK_READS_SESSION_STATE": "1", "GIT_TERMINAL_PROMPT": "0"})
	process, err := spawnJSONProcess(ctx, command)
	if err != nil {
		return nil, err
	}
	a := &claudeProcess{io: process, session: session, emit: emit, permissions: map[string]claudeApproval{}}
	go process.read(a.handle, onExit)
	ready := false
	defer func() {
		if !ready {
			process.close()
			<-process.done
		}
	}()
	if _, err := a.control(map[string]any{"subtype": "initialize", "hooks": nil}); err != nil {
		return nil, err
	}
	ready = true
	return a, nil
}

func (a *claudeProcess) control(request any) (json.RawMessage, error) {
	id := a.io.nextID()
	return a.io.call(id, map[string]any{"type": "control_request", "request_id": id, "request": request})
}
func (a *claudeProcess) Close() { a.io.close() }
func (a *claudeProcess) NativeCheckpoint() runnerproto.AgentCheckpoint {
	return runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: a.session.ID, NativeLogID: a.session.ID}
}
func (a *claudeProcess) StartTurn(id, input string) error {
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
	a.started = time.Now()
	a.cancelled = false
	a.mu.Unlock()
	started := runnerproto.Frame("turn.started")
	started.TurnID = id
	if err := a.emit(started); err != nil {
		return err
	}
	return a.io.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": input}, "parent_tool_use_id": nil, "session_id": a.session.ID})
}
func (a *claudeProcess) Cancel(id string) error {
	a.mu.Lock()
	active := a.turn == id
	if active {
		a.cancelled = true
	}
	a.mu.Unlock()
	if !active {
		return nil
	}
	_, err := a.control(map[string]any{"subtype": "interrupt"})
	return err
}
func (a *claudeProcess) Decide(id, decision string) error {
	a.mu.Lock()
	permission, ok := a.permissions[id]
	if ok {
		delete(a.permissions, id)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	var result any
	if decision == "approve" {
		result = map[string]any{"behavior": "allow", "updatedInput": permission.input}
	} else if decision == "deny" {
		result = map[string]any{"behavior": "deny", "message": "Denied by the session owner or approval timeout"}
	} else {
		return errors.New("invalid permission decision")
	}
	return a.io.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": permission.requestID, "response": result}})
}

func (a *claudeProcess) delta(kind, content string) error {
	if content == "" {
		return nil
	}
	a.mu.Lock()
	id := a.turn
	a.mu.Unlock()
	if id == "" {
		return errors.New("agent output outside a turn")
	}
	return emitChunks(a.emit, id, kind, content)
}

func (a *claudeProcess) handle(message map[string]json.RawMessage) error {
	switch rawString(message["type"]) {
	case "control_response":
		var response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(message["response"], &response); err != nil {
			return err
		}
		a.io.deliver(response.RequestID, response.Response, response.Subtype != "success")
	case "control_request":
		var request struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(message["request"], &request); err != nil {
			return err
		}
		if request.Subtype != "can_use_tool" {
			return errors.New("unsupported Claude control request")
		}
		requestID := rawString(message["request_id"])
		if requestID == "" {
			return errors.New("permission request without identifier")
		}
		a.mu.Lock()
		turn := a.turn
		aid := uuid.NewString()
		a.permissions[aid] = claudeApproval{requestID, request.Input}
		a.mu.Unlock()
		if turn == "" {
			return errors.New("permission request outside a turn")
		}
		if a.session.PermissionMode == "auto" {
			return a.Decide(aid, "approve")
		}
		command := request.ToolName + " " + string(request.Input)
		if len(command) > 32<<10 {
			return errors.New("permission description exceeds limit")
		}
		m := runnerproto.Frame("approval.request")
		m.TurnID = turn
		m.ApprovalID = aid
		m.Command = command
		m.Context = "Claude Code tool permission"
		return a.emit(m)
	case "stream_event":
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
			Block struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(message["event"], &event); err != nil {
			return err
		}
		if event.Type == "content_block_delta" {
			switch event.Delta.Type {
			case "text_delta":
				return a.delta("text", event.Delta.Text)
			case "thinking_delta":
				return a.delta("thinking", event.Delta.Thinking)
			}
		}
		if event.Type == "content_block_start" && event.Block.Type == "tool_use" {
			return a.delta("tool_call", event.Block.Name)
		}
	case "user":
		var content struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(message["message"], &content) != nil {
			return nil
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(content.Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "tool_result" {
					if err := a.delta("tool_result", string(block.Content)); err != nil {
						return err
					}
				}
			}
		}
	case "result":
		var result struct {
			IsError    bool  `json:"is_error"`
			DurationMS int64 `json:"duration_ms"`
			Usage      struct {
				Input       int64 `json:"input_tokens"`
				Output      int64 `json:"output_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		data, _ := json.Marshal(message)
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
		a.mu.Lock()
		turn := a.turn
		a.turn = ""
		cancelled := a.cancelled
		a.permissions = map[string]claudeApproval{}
		elapsed := time.Since(a.started).Milliseconds()
		a.mu.Unlock()
		if turn == "" {
			return errors.New("unexpected Claude result")
		}
		m := runnerproto.Frame("turn.finished")
		m.TurnID = turn
		m.DurationMS = elapsed
		m.Tokens = result.Usage.Input + result.Usage.Output + result.Usage.CacheRead + result.Usage.CacheCreate
		if result.IsError {
			m.ExitCode = 1
		}
		if cancelled {
			m.ExitCode = 130
		}
		return a.emit(m)
	case "control_cancel_request":
		requestID := rawString(message["request_id"])
		a.mu.Lock()
		cancelled := []string{}
		turn := a.turn
		for id, permission := range a.permissions {
			if permission.requestID == requestID {
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
	case "system", "assistant", "tool_progress", "tool_use_summary", "rate_limit_event":
		// Full assistant messages repeat partial deltas and are not emitted twice.
	default:
		return errors.New("unsupported Claude stream message")
	}
	return nil
}

func emitChunks(emit func(runnerproto.Message) error, turn, kind, content string) error {
	for len(content) > 0 {
		limit := len(content)
		if limit > 32<<10 {
			limit = 32 << 10
			for limit > 0 && (content[limit]&0xC0) == 0x80 {
				limit--
			}
		}
		m := runnerproto.Frame("turn.event")
		m.TurnID = turn
		m.Kind = kind
		m.Content = content[:limit]
		if err := emit(m); err != nil {
			return err
		}
		content = content[limit:]
	}
	return nil
}
