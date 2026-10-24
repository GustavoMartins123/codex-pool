package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

type daemon struct {
	options  Options
	ctx      context.Context
	mu       sync.Mutex
	sessions map[string]*localSession
	claims   map[string]bool
	commands chan runnerproto.Message
	notify   chan struct{}
}

type localSession struct {
	daemon   *daemon
	session  runnerproto.Session
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	agent    agentProcess
	path     string
	control  string
	native   runnerproto.AgentCheckpoint
	claimed  bool
	sent     uint64
	sequence uint64
	pending  []runnerproto.Message
	bytes    int
	exited   bool
	closing  bool
	lastTurn string
	exitOnce sync.Once
	done     chan struct{}
	commands chan runnerproto.Message
}

func Run(ctx context.Context, options Options) error {
	if err := ValidateOrigin(options.Config.PoolURL); err != nil {
		return err
	}
	if options.Config.RunnerID == "" || options.Config.Token == "" {
		return errors.New("runner must be enrolled")
	}
	if !filepath.IsAbs(options.WorkspaceRoot) {
		return errors.New("workspace root must be absolute")
	}
	if err := os.MkdirAll(options.WorkspaceRoot, 0700); err != nil {
		return err
	}
	canonicalRoot, err := filepath.EvalSymlinks(options.WorkspaceRoot)
	if err != nil {
		return err
	}
	options.WorkspaceRoot = canonicalRoot
	if len(options.Executables) == 0 {
		return errors.New("configure at least one agent executable")
	}
	for agent, path := range options.Executables {
		if agent != "codex" && agent != "claude" {
			return errors.New("unsupported agent")
		}
		if !filepath.IsAbs(path) {
			return errors.New("agent executable path must be absolute")
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return errors.New("agent executable unavailable")
		}
		if runtime.GOOS == "windows" && strings.ToLower(filepath.Ext(path)) != ".exe" {
			return errors.New("Windows agents require a native .exe; shell shims are not supported")
		}
	}
	for i, path := range options.AllowedPaths {
		if !filepath.IsAbs(path) {
			return errors.New("allowed workspace paths must be absolute")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return errors.New("allowed workspace path unavailable")
		}
		options.AllowedPaths[i] = canonical
	}
	d := &daemon{options: options, ctx: ctx, sessions: map[string]*localSession{}, claims: map[string]bool{}, commands: make(chan runnerproto.Message, 128), notify: make(chan struct{}, 1)}
	defer func() {
		d.mu.Lock()
		sessions := []*localSession{}
		for _, s := range d.sessions {
			sessions = append(sessions, s)
		}
		d.mu.Unlock()
		for _, s := range sessions {
			s.close()
		}
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for _, s := range sessions {
			select {
			case <-s.done:
			case <-deadline.C:
				return
			}
		}
	}()
	delay := time.Second
	for {
		connected, err := d.connect()
		if ctx.Err() != nil {
			return nil
		}
		var rejected *credentialRejection
		if errors.As(err, &rejected) {
			return rejected
		}
		var protocol *protocolRejection
		if errors.As(err, &protocol) {
			return protocol
		}
		if connected {
			delay = time.Second
		}
		log.Print("runner connection interrupted; reconnecting")
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

type credentialRejection struct{}
type protocolRejection struct{}

func (*protocolRejection) Error() string { return "runner protocol rejected; execution stopped" }

func (*credentialRejection) Error() string {
	return "runner credential rejected; enroll or rotate explicitly"
}

func (d *daemon) connect() (bool, error) {
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	wsURL := "wss" + strings.TrimPrefix(d.options.Config.PoolURL, "https") + "/api/runner/ws"
	connectCtx, connectCancel := context.WithTimeout(ctx, 20*time.Second)
	conn, response, err := websocket.Dial(connectCtx, wsURL, &websocket.DialOptions{HTTPClient: httpClient(d.options.HTTPClient), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + d.options.Config.Token}}, CompressionMode: websocket.CompressionDisabled})
	connectCancel()
	if err != nil {
		if response != nil && (response.StatusCode == 401 || response.StatusCode == 403) {
			return false, &credentialRejection{}
		}
		return false, errors.New("runner connection failed")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(runnerproto.MaxMessageBytes)
	agents := []string{}
	for agent := range d.options.Executables {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	hello := runnerproto.Frame("hello")
	hello.Hello = &runnerproto.Hello{Agents: agents, OS: runtime.GOOS, Arch: runtime.GOARCH, Labels: d.options.Labels, AllowedPaths: d.options.AllowedPaths, PersistentSessions: true}
	write := func(m runnerproto.Message) error {
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		writeCtx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		return conn.Write(writeCtx, websocket.MessageText, data)
	}
	if err := write(hello); err != nil {
		return false, err
	}
	d.mu.Lock()
	d.claims = map[string]bool{}
	for _, s := range d.sessions {
		s.mu.Lock()
		s.claimed = false
		s.sent = 0
		s.mu.Unlock()
	}
	d.mu.Unlock()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer cancel()
		defer conn.CloseNow()
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case command := <-d.commands:
				if write(command) != nil {
					return
				}
			case <-d.notify:
				d.mu.Lock()
				sessions := []*localSession{}
				for _, s := range d.sessions {
					sessions = append(sessions, s)
				}
				d.mu.Unlock()
				for _, s := range sessions {
					s.mu.Lock()
					pending := []runnerproto.Message{}
					if s.claimed {
						for _, event := range s.pending {
							if event.Sequence > s.sent {
								pending = append(pending, event)
							}
						}
					}
					s.mu.Unlock()
					for _, event := range pending {
						if write(event) != nil {
							return
						}
						s.mu.Lock()
						if event.Sequence > s.sent {
							s.sent = event.Sequence
						}
						s.mu.Unlock()
					}
				}
			case <-heartbeat.C:
				d.mu.Lock()
				ids := []string{}
				for id, s := range d.sessions {
					s.mu.Lock()
					if s.claimed {
						ids = append(ids, id)
					}
					s.mu.Unlock()
				}
				d.mu.Unlock()
				m := runnerproto.Frame("heartbeat")
				m.Sessions = ids
				if write(m) != nil {
					return
				}
			}
		}
	}()
	defer func() { cancel(); conn.CloseNow(); <-writerDone }()
	connected := false
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
				return connected, &protocolRejection{}
			}
			return connected, err
		}
		if kind != websocket.MessageText {
			return connected, &protocolRejection{}
		}
		var m runnerproto.Message
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&m) != nil || m.Version != runnerproto.Version {
			return connected, &protocolRejection{}
		}
		if !connected && m.Type != "hello.ack" {
			return false, errors.New("server hello required")
		}
		switch m.Type {
		case "hello.ack":
			if connected {
				return true, errors.New("duplicate server hello")
			}
			connected = true
			log.Print("runner connected")
			d.mu.Lock()
			ids := []string{}
			for id := range d.sessions {
				ids = append(ids, id)
			}
			d.mu.Unlock()
			for _, id := range ids {
				d.claim(id)
			}
		case "session.pending", "claim.required":
			if m.SessionID == "" {
				return true, errors.New("missing session identifier")
			}
			d.claim(m.SessionID)
		case "claim.ack":
			if err := d.claimed(m); err != nil {
				return true, err
			}
		case "claim.denied":
			d.denied(m.SessionID)
		case "event.ack":
			if err := d.acknowledge(m); err != nil {
				return true, err
			}
		case "session.purge":
			go d.purge(m)
		case "purge.ack":
		case "turn.start", "turn.cancel", "approval.decision", "session.close":
			d.mu.Lock()
			s := d.sessions[m.SessionID]
			d.mu.Unlock()
			if s == nil {
				return true, errors.New("command for unknown session")
			}
			if m.Type == "session.close" {
				s.mu.Lock()
				exited := s.exited
				s.mu.Unlock()
				s.close()
				if exited {
					d.mu.Lock()
					delete(d.sessions, m.SessionID)
					d.mu.Unlock()
				}
				continue
			}
			select {
			case s.commands <- m:
			default:
				s.cancel()
				return true, errors.New("session command buffer exceeded")
			}
		case "heartbeat.ack":
		default:
			return true, errors.New("unknown server message")
		}
	}
}

func (d *daemon) command(m runnerproto.Message) {
	select {
	case d.commands <- m:
	case <-d.ctx.Done():
	}
}
func (d *daemon) wake() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}
func (d *daemon) claim(id string) {
	d.mu.Lock()
	if d.claims[id] {
		d.mu.Unlock()
		return
	}
	d.claims[id] = true
	if s := d.sessions[id]; s != nil {
		s.mu.Lock()
		s.claimed = false
		s.mu.Unlock()
	}
	d.mu.Unlock()
	m := runnerproto.Frame("claim")
	m.SessionID = id
	d.command(m)
}

func (d *daemon) claimed(m runnerproto.Message) error {
	if _, err := uuid.Parse(m.SessionID); err != nil {
		return errors.New("invalid session identifier")
	}
	if m.Session == nil || m.Session.ID != m.SessionID || m.Session.RunnerID != d.options.Config.RunnerID {
		return errors.New("invalid session claim")
	}
	d.mu.Lock()
	delete(d.claims, m.SessionID)
	s := d.sessions[m.SessionID]
	fresh := s == nil
	if fresh {
		ctx, cancel := context.WithCancel(d.ctx)
		s = &localSession{daemon: d, session: *m.Session, ctx: ctx, cancel: cancel, commands: make(chan runnerproto.Message, 32), sequence: m.Sequence, done: make(chan struct{})}
		d.sessions[m.SessionID] = s
	}
	d.mu.Unlock()
	s.mu.Lock()
	if m.Sequence > s.sequence {
		s.mu.Unlock()
		s.cancel()
		return errors.New("server event sequence exceeds local state")
	}
	s.claimed = true
	s.sent = m.Sequence
	s.mu.Unlock()
	if err := d.acknowledge(m); err != nil {
		return err
	}
	d.wake()
	if fresh {
		if m.Session.State != "claimed" {
			s.finish(1)
			return nil
		}
		if m.CredentialPath == "" {
			return errors.New("new claim omitted credential nonce")
		}
		go s.initialize(m.CredentialPath)
	}
	return nil
}

func (d *daemon) denied(id string) {
	d.mu.Lock()
	delete(d.claims, id)
	s := d.sessions[id]
	delete(d.sessions, id)
	d.mu.Unlock()
	if s != nil {
		s.close()
	}
}
func (d *daemon) acknowledge(m runnerproto.Message) error {
	d.mu.Lock()
	s := d.sessions[m.SessionID]
	d.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Sequence > s.sequence {
		return errors.New("invalid event acknowledgement")
	}
	keep := s.pending[:0]
	for _, event := range s.pending {
		if event.Sequence > m.Sequence {
			keep = append(keep, event)
		} else {
			data, _ := json.Marshal(event)
			s.bytes -= len(data)
		}
	}
	s.pending = keep
	return nil
}

func (s *localSession) emit(m runnerproto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited && m.Type != "process.exit" {
		return errors.New("agent process already exited")
	}
	m.SessionID = s.session.ID
	m.Sequence = s.sequence + 1
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if m.Type != "process.exit" && (len(s.pending) >= 512 || s.bytes+len(data) > 8<<20) {
		s.cancel()
		return errors.New("session event buffer exceeded")
	}
	s.sequence++
	s.pending = append(s.pending, m)
	s.bytes += len(data)
	s.daemon.wake()
	return nil
}

func (s *localSession) initialize(credentialPath string) {
	fail := func() { s.cancel(); s.finish(1) }
	m := runnerproto.Frame("workspace.status")
	m.Status = "preparing"
	if s.emit(m) != nil {
		fail()
		return
	}
	var path, control string
	var err error
	if s.session.ResumeRequested {
		checkpoint, loadErr := loadLocalCheckpoint(s.daemon.options, s.session)
		if loadErr != nil {
			fail()
			return
		}
		path, control = checkpoint.Path, checkpoint.Control
		s.mu.Lock()
		s.path = path
		s.control = control
		s.native = runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: checkpoint.NativeSessionID, NativeLogID: checkpoint.NativeLogID}
		s.mu.Unlock()
	}
	token, err := fetchCredential(s.ctx, s.daemon.options.HTTPClient, s.daemon.options.Config, s.session.Agent, credentialPath)
	if err != nil {
		fail()
		return
	}
	if !s.session.ResumeRequested {
		path, err = prepareWorkspace(s.ctx, s.daemon.options.WorkspaceRoot, s.daemon.options.AllowedPaths, s.session)
		if err != nil {
			fail()
			return
		}
		s.mu.Lock()
		s.path = path
		s.mu.Unlock()
		control, err = runtimePath(s.daemon.options.WorkspaceRoot, ".control-", s.session.ID)
		if err != nil {
			fail()
			return
		}
		if err := os.Mkdir(control, 0700); err != nil {
			fail()
			return
		}
	}
	s.mu.Lock()
	s.control = control
	s.mu.Unlock()
	exe := s.daemon.options.Executables[s.session.Agent]
	var agent agentProcess
	switch s.session.Agent {
	case "codex":
		agent, err = startCodex(s.ctx, exe, path, control, s.daemon.options.Config.PoolURL, token, s.session, s.emit, s.finish)
	case "claude":
		agent, err = startClaude(s.ctx, exe, path, control, s.daemon.options.Config.PoolURL, token, s.session, s.emit, s.finish)
	default:
		err = errors.New("unsupported claimed agent")
	}
	token = ""
	if err != nil {
		fail()
		return
	}
	s.mu.Lock()
	s.agent = agent
	s.native = agent.NativeCheckpoint()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		agent.Close()
		return
	}
	m = runnerproto.Frame("agent.checkpoint")
	native := agent.NativeCheckpoint()
	m.Checkpoint = &native
	if s.emit(m) != nil {
		fail()
		return
	}
	m = runnerproto.Frame("workspace.status")
	m.Status = "ready"
	if s.emit(m) != nil {
		fail()
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			agent.Close()
			return
		case command := <-s.commands:
			switch command.Type {
			case "turn.start":
				if s.lastTurn == command.TurnID {
					continue
				}
				s.lastTurn = command.TurnID
				err = agent.StartTurn(command.TurnID, command.Input)
			case "turn.cancel":
				err = agent.Cancel(command.TurnID)
			case "approval.decision":
				err = agent.Decide(command.ApprovalID, command.Decision)
			}
			if err != nil {
				agent.Close()
				return
			}
		}
	}
}

func (s *localSession) close() {
	s.mu.Lock()
	s.closing = true
	agent := s.agent
	s.mu.Unlock()
	s.cancel()
	if agent != nil {
		agent.Close()
	}
}
func (s *localSession) finish(exit int) {
	s.exitOnce.Do(func() {
		defer close(s.done)
		s.mu.Lock()
		s.exited = true
		path, control, closing, native := s.path, s.control, s.closing, s.native
		s.mu.Unlock()
		s.cancel()
		if closing {
			exit = 0
		}
		m := runnerproto.Frame("process.exit")
		// Closing execution retains the workspace and native transcript. Only
		// an explicit conversation deletion may remove these persisted resources.
		if native.NativeSessionID != "" {
			checkpoint, err := saveLocalCheckpoint(s.daemon.options, s.session, path, control, native)
			if err != nil {
				exit = 1
				m.Status = "checkpoint_failed"
			} else {
				m.Checkpoint = checkpoint
			}
		}
		m.ExitCode = exit
		_ = s.emit(m)
	})
}
