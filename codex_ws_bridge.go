package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-pool-proxy/internal/sse"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Codex keeps one Responses websocket per session and sends every turn over
// it, whatever model the user picks. The websocket upstream is the ChatGPT
// backend, which only serves OpenAI models, so a turn for a model another
// provider owns (Qwen, AI Studio, Antigravity, Claude, Kimi, ...) is served by
// the pool's HTTP model routing instead, and its Responses events are written
// back to the client as websocket frames.

// webSocketModelUsesProviderBridge mirrors the HTTP router: OpenAI models stay
// on the Codex upstream and every other model goes to the provider that owns it.
func webSocketModelUsesProviderBridge(model string) bool {
	model = strings.TrimSpace(model)
	return model != "" && !isOpenAIModel(model)
}

// codexBridgeRequestBody turns a response.create frame into the HTTP Responses
// body: the request object without the websocket event type, always streamed.
func codexBridgeRequestBody(frame []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	object := contextRequestObject(root)
	delete(object, "type")
	object["stream"] = true
	return json.Marshal(object)
}

// codexBridgeWriter receives the HTTP proxy response for one bridged turn and
// forwards each Responses SSE event as one websocket text frame.
type codexBridgeWriter struct {
	header  http.Header
	status  int
	isSSE   bool
	framer  sse.Framer
	buf     []byte
	body    bytes.Buffer
	onEvent func([]byte) error
	err     error
}

func (w *codexBridgeWriter) Header() http.Header { return w.header }

func (w *codexBridgeWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.isSSE = status >= 200 && status < 300 && strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream")
}

func (w *codexBridgeWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return 0, w.err
	}
	if !w.isSSE {
		if w.body.Len() < 1<<20 {
			w.body.Write(p)
		}
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		event, advance, ok := w.framer.Next(w.buf)
		if !ok {
			break
		}
		w.buf = w.buf[advance:]
		_, data := sse.ParseEvent(event)
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if err := w.onEvent(data); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(p), nil
}

func (w *codexBridgeWriter) Flush() {}

// codexBridgeErrorFrame is the websocket error event Codex expects when a
// response.create is rejected before a response starts.
func codexBridgeErrorFrame(status int, body []byte) []byte {
	message := strings.TrimSpace(string(body))
	errorType := "invalid_request_error"
	code := ""
	var parsed struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && len(parsed.Error) > 0 {
		var detail struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		if json.Unmarshal(parsed.Error, &detail) == nil && detail.Message != "" {
			message, code = detail.Message, detail.Code
			if detail.Type != "" {
				errorType = detail.Type
			}
		} else {
			var text string
			if json.Unmarshal(parsed.Error, &text) == nil && text != "" {
				message = text
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if status >= 500 {
		errorType = "server_error"
	}
	detail := map[string]any{"type": errorType, "message": message}
	if code != "" {
		detail["code"] = code
	}
	payload, _ := json.Marshal(map[string]any{"type": "error", "status": status, "error": detail})
	return payload
}

// startProviderBridge serves turn through the HTTP provider routing. The
// caller holds turnMu; the bridge runs on its own goroutine so the relay keeps
// pumping while the provider streams.
func (s *codexRelayState) startProviderBridge(turn *codexRelayTurn) {
	frame := turn.request
	if turn.continues != "" {
		// The provider never saw the referenced response, so it gets the
		// complete input the chain resolves to.
		if !turn.inputKnown {
			s.finishTurn(turn)
			payload, _ := json.Marshal(map[string]any{"type": "error", "status": 400, "error": map[string]any{
				"type": "invalid_request_error", "code": "previous_response_not_found",
				"message": fmt.Sprintf("Previous response with id '%s' not found.", turn.continues),
			}})
			go s.writeBridgeFrame(payload)
			return
		}
		replay, err := codexReplayFrame(turn.request, turn.input)
		if err != nil {
			s.finishTurn(turn)
			go s.writeBridgeFrame(codexBridgeErrorFrame(http.StatusBadRequest, []byte(err.Error())))
			return
		}
		frame = replay
	}
	body, err := codexBridgeRequestBody(frame)
	if err != nil {
		s.finishTurn(turn)
		go s.writeBridgeFrame(codexBridgeErrorFrame(http.StatusBadRequest, []byte(err.Error())))
		return
	}
	log.Printf("[%s] websocket model %q routed through the provider bridge", s.opts.ReqID, turn.model)
	if s.h.cfg != nil && s.h.cfg.debug.Load() {
		log.Printf("[%s] provider bridge request: %s", s.opts.ReqID, clipBridgeLog(body))
	}
	go s.runProviderBridge(turn, body)
}

func (s *codexRelayState) writeBridgeFrame(payload []byte) {
	if s.clientWriter != nil {
		if err := s.clientWriter.Write(s.ctx, websocket.MessageText, payload); err != nil {
			log.Printf("[%s] websocket provider bridge write failed: %v", s.opts.ReqID, err)
		}
	}
	s.turnMu.Lock()
	activity := s.activity
	s.turnMu.Unlock()
	if activity != nil {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
}

func (s *codexRelayState) runProviderBridge(turn *codexRelayTurn, body []byte) {
	finished := false
	handle := func(data []byte) error {
		s.turnMu.Lock()
		if !finished {
			if turn.responseID == "" {
				var event struct {
					Response struct {
						ID string `json:"id"`
					} `json:"response"`
				}
				if json.Unmarshal(data, &event) == nil && event.Response.ID != "" {
					turn.responseID = event.Response.ID
				}
			}
			s.observeTurnEvent(turn, data)
			if isTerminalCodexWebSocketEvent(data) || isCodexWebSocketErrorEvent(data) {
				finished = true
				s.finishBridgeTurnLocked(turn)
			}
		}
		s.turnMu.Unlock()
		s.writeBridgeFrame(data)
		return nil
	}
	if events, ok := codexBridgePrewarmEvents(body); ok {
		// Codex warms its websocket with generate=false; no provider call is
		// needed, only an empty response the next turn can chain from.
		for _, event := range events {
			_ = handle(event)
		}
		return
	}
	req, err := s.providerBridgeRequest(s.ctx, body)
	if err != nil {
		s.endProviderBridgeTurn(turn, codexBridgeErrorFrame(http.StatusInternalServerError, []byte(err.Error())))
		return
	}
	writer := &codexBridgeWriter{header: make(http.Header), onEvent: handle}
	s.h.proxyRequest(writer, req, s.opts.ReqID+"-bridge")
	if writer.status == 0 {
		writer.WriteHeader(http.StatusBadGateway)
	}
	s.turnMu.Lock()
	done := finished
	s.turnMu.Unlock()
	if done {
		return
	}
	var tail []byte
	if !writer.isSSE {
		tail = codexBridgeErrorFrame(writer.status, writer.body.Bytes())
	} else {
		tail = codexBridgeErrorFrame(http.StatusBadGateway, []byte("provider stream ended before the response completed"))
	}
	s.endProviderBridgeTurn(turn, tail)
}

// codexBridgePrewarmEvents answers a generate=false response.create the way
// the Responses websocket does: a response that completes without output.
func codexBridgePrewarmEvents(body []byte) ([][]byte, bool) {
	var request struct {
		Generate *bool  `json:"generate"`
		Model    string `json:"model"`
	}
	if json.Unmarshal(body, &request) != nil || request.Generate == nil || *request.Generate {
		return nil, false
	}
	response := map[string]any{
		"id": "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "object": "response",
		"created_at": time.Now().Unix(), "model": request.Model, "status": "in_progress", "output": []any{},
	}
	created, _ := json.Marshal(map[string]any{"type": "response.created", "response": response})
	response["status"] = "completed"
	response["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	return [][]byte{created, completed}, true
}

func (s *codexRelayState) endProviderBridgeTurn(turn *codexRelayTurn, frame []byte) {
	s.turnMu.Lock()
	s.finishBridgeTurnLocked(turn)
	s.turnMu.Unlock()
	s.writeBridgeFrame(frame)
}

// finishBridgeTurnLocked retires a bridged turn and remembers its response ID,
// which only the pool can resolve. The caller holds turnMu.
func (s *codexRelayState) finishBridgeTurnLocked(turn *codexRelayTurn) {
	if turn.responseID != "" {
		if s.bridgedResponses == nil {
			s.bridgedResponses = make(map[string]struct{})
		}
		s.bridgedResponses[turn.responseID] = struct{}{}
	}
	s.finishTurn(turn)
	if s.session != nil {
		s.session.setActive(len(s.turns) != 0)
	}
}

// providerBridgeRequest builds the HTTP request a bridged turn runs as. It
// carries the websocket client's pool credential and identity headers so the
// proxy authenticates, admits and routes it like any HTTP Responses request.
func (s *codexRelayState) providerBridgeRequest(ctx context.Context, body []byte) (*http.Request, error) {
	source := s.credentialRequest
	target := *source.URL
	target.Path = s.opts.RequestPath
	target.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = source.Header.Clone()
	for _, name := range []string{"Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Content-Encoding", "Content-Length"} {
		req.Header.Del(name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.ContentLength = int64(len(body))
	req.RemoteAddr = source.RemoteAddr
	req.Host = source.Host
	req.TLS = source.TLS
	return req, nil
}

func clipBridgeLog(body []byte) string {
	const limit = 400
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + fmt.Sprintf("... (%d bytes)", len(body))
}
