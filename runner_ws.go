package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/coder/websocket"
	"go.etcd.io/bbolt"
)

func (h *proxyHandler) handleRunnerWS(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	stored, err := b.runnerCredential(r)
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	if h.webSocketRegistry().isDraining() {
		respondJSONError(w, 503, "server draining")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(runnerproto.MaxMessageBytes)
	tracked := h.webSocketRegistry().register(conn)
	defer h.webSocketRegistry().unregister(tracked)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	helloCtx, helloCancel := context.WithTimeout(ctx, 10*time.Second)
	m, err := readRunnerMessage(helloCtx, conn)
	helloCancel()
	if err != nil || m.Type != "hello" || validHello(m.Hello) != nil {
		beginWebSocketClose(conn, websocket.StatusPolicyViolation, "valid hello required")
		return
	}
	var stopOnce sync.Once
	runner := &connectedRunner{id: stored.Runner.ID, principal: stored.PrincipalID, hello: *m.Hello, lastSeen: b.now(), out: make(chan runnerproto.Message, 128)}
	runner.stop = func() { stopOnce.Do(func() { cancel(); conn.CloseNow() }) }
	if err := b.connectRunner(runner); err != nil {
		beginWebSocketClose(conn, websocket.StatusPolicyViolation, "runner unavailable")
		return
	}
	defer b.disconnectRunner(runner)
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		defer runner.stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.stopped:
				return
			case message := <-runner.out:
				data, err := json.Marshal(message)
				if err != nil {
					return
				}
				writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err = conn.Write(writeCtx, websocket.MessageText, data)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
	defer func() { runner.stop(); <-writer }()
	for {
		m, err := readRunnerMessage(ctx, conn)
		if err != nil {
			return
		}
		if _, err := b.runnerCredential(r); err != nil {
			beginWebSocketClose(conn, websocket.StatusPolicyViolation, "runner credential revoked")
			return
		}
		switch m.Type {
		case "heartbeat":
			err = b.heartbeat(runner, m.Sessions)
		case "claim":
			err = b.claim(runner, m.SessionID)
			if err != nil {
				denied := runnerproto.Frame("claim.denied")
				denied.SessionID = m.SessionID
				var fault *sessionFault
				if errors.As(err, &fault) {
					denied.Reason = fault.message
				} else {
					denied.Reason = "claim unavailable"
				}
				b.mu.Lock()
				b.sendLocked(runner, denied)
				b.mu.Unlock()
				err = nil
			}
		case "claim.release":
			err = b.releaseClaim(runner, m.SessionID)
		default:
			err = b.runnerEvent(runner, m)
		}
		if err != nil {
			b.mu.Lock()
			for _, session := range b.sessions {
				if session.RunnerID == runner.id && session.State != "closed" {
					_ = b.closeLocked(session, "protocol_error")
				}
			}
			b.mu.Unlock()
			beginWebSocketClose(conn, websocket.StatusPolicyViolation, "invalid runner message or session state")
			return
		}
	}
}

func readRunnerMessage(ctx context.Context, conn *websocket.Conn) (runnerproto.Message, error) {
	var message runnerproto.Message
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return message, err
	}
	if kind != websocket.MessageText {
		return message, errors.New("text JSON required")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return message, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return message, errors.New("one JSON message required")
	}
	if message.Version != runnerproto.Version || message.Type == "" {
		return message, errors.New("unsupported runner protocol")
	}
	return message, nil
}

func (b *sessionBroker) connectRunner(r *connectedRunner) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.stopped:
		return errors.New("broker stopped")
	default:
	}
	if b.runners[r.id] != nil {
		return errors.New("runner already connected")
	}
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketRunners))
		var record runnerRecord
		if err := json.Unmarshal(bucket.Get([]byte(r.id)), &record); err != nil {
			return err
		}
		if record.Runner.Revoked || record.PrincipalID != r.principal {
			return errors.New("runner revoked")
		}
		record.Runner.Agents = append([]string{}, r.hello.Agents...)
		record.Runner.AllowedPaths = append([]string{}, r.hello.AllowedPaths...)
		record.Runner.Labels = append([]string{}, r.hello.Labels...)
		record.Runner.OS = r.hello.OS
		record.Runner.Arch = r.hello.Arch
		if err := putJSON(bucket, r.id, record); err != nil {
			return err
		}
		return b.store.audit(tx, r.principal, "runner.connected", r.id, "")
	}); err != nil {
		return err
	}
	b.runners[r.id] = r
	m := runnerproto.Frame("hello.ack")
	b.sendLocked(r, m)
	for _, s := range b.sessions {
		if s.PrincipalID == r.principal && (s.State == "pending_runner" || (s.RunnerID == r.id && s.State != "closed")) {
			b.offerLocked(s)
		}
	}
	return nil
}

func (b *sessionBroker) disconnectRunner(r *connectedRunner) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return
	}
	delete(b.runners, r.id)
	for _, s := range b.sessions {
		if s.RunnerID == r.id && s.State != "closed" {
			b.publishLocked(s.ID, "runner.status", map[string]any{"status": "offline"})
		}
	}
}
