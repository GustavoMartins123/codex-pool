package runner

import (
	"bufio"
	"codex-pool-proxy/internal/runnerproto"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type agentProcess interface {
	StartTurn(string, string) error
	Cancel(string) error
	Decide(string, string) error
	Close()
	NativeCheckpoint() runnerproto.AgentCheckpoint
}

type rpcReply struct {
	result json.RawMessage
	failed bool
}
type jsonProcess struct {
	ctx      context.Context
	command  *exec.Cmd
	input    io.WriteCloser
	output   io.ReadCloser
	writeMu  sync.Mutex
	mu       sync.Mutex
	waiters  map[string]chan rpcReply
	sequence atomic.Uint64
	done     chan struct{}
	kill     func() error
	stopOnce sync.Once
}

func spawnJSONProcess(ctx context.Context, command *exec.Cmd) (*jsonProcess, error) {
	input, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	command.Stderr = io.Discard
	kill, err := startManaged(command)
	if err != nil {
		input.Close()
		output.Close()
		return nil, errors.New("agent process failed to start")
	}
	p := &jsonProcess{ctx: ctx, command: command, input: input, output: output, waiters: map[string]chan rpcReply{}, done: make(chan struct{}), kill: kill}
	go func() {
		select {
		case <-ctx.Done():
			p.close()
		case <-p.done:
		}
	}()
	return p, nil
}

func (p *jsonProcess) read(handler func(map[string]json.RawMessage) error, onExit func(int)) {
	scanner := bufio.NewScanner(p.output)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	failed := false
	for scanner.Scan() {
		var message map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil || handler(message) != nil {
			failed = true
			p.close()
			break
		}
	}
	if scanner.Err() != nil {
		failed = true
		p.close()
	}
	err := p.command.Wait()
	p.close()
	p.input.Close()
	close(p.done)
	exit := 0
	if err != nil {
		exit = 1
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			exit = failure.ExitCode()
		}
	}
	if failed {
		exit = 1
	}
	onExit(exit)
}

func (p *jsonProcess) close()         { p.stopOnce.Do(func() { _ = p.kill() }) }
func (p *jsonProcess) nextID() string { return "runner-" + strconv.FormatUint(p.sequence.Add(1), 10) }
func (p *jsonProcess) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.input.Write(append(data, '\n'))
	if err != nil {
		return errors.New("agent input channel failed")
	}
	return nil
}
func (p *jsonProcess) deliver(id string, result json.RawMessage, failed bool) {
	p.mu.Lock()
	waiter := p.waiters[id]
	p.mu.Unlock()
	if waiter != nil {
		select {
		case waiter <- rpcReply{result, failed}:
		default:
		}
	}
}
func (p *jsonProcess) call(id string, frame any) (json.RawMessage, error) {
	waiter := make(chan rpcReply, 1)
	p.mu.Lock()
	p.waiters[id] = waiter
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.waiters, id); p.mu.Unlock() }()
	if err := p.write(frame); err != nil {
		return nil, err
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	case <-p.done:
		return nil, errors.New("agent exited during request")
	case <-timer.C:
		return nil, errors.New("agent control request timed out")
	case reply := <-waiter:
		if reply.failed {
			return nil, errors.New("agent rejected control request")
		}
		return reply.result, nil
	}
}

func rawString(value json.RawMessage) string {
	var result string
	_ = json.Unmarshal(value, &result)
	return result
}
