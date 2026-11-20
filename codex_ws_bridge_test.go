package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"codex-pool-proxy/internal/jsonvalue"
)

// codexBridgeFixture is a Codex websocket proxy with a ChatGPT upstream that
// serves gpt models and a Qwen Chat Completions upstream for the bridge.
type codexBridgeFixture struct {
	proxy      *codexProxyFixture
	mu         sync.Mutex
	qwenBodies []map[string]any
	gptFrames  []map[string]any
}

func newCodexBridgeFixture(t *testing.T) *codexBridgeFixture {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	fx := &codexBridgeFixture{}
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fx.mu.Lock()
		fx.qwenBodies = append(fx.qwenBodies, body)
		turn := len(fx.qwenBodies)
		fx.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("chat-%d", turn), "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": fmt.Sprintf("QWEN_REPLY_%d", turn)}, "finish_reason": "stop"}}})
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", chunk)
	}))
	t.Cleanup(qwen.Close)

	upstream := newFakeCodexUpstream(t)
	upstream.on("acct_bridge", func(ctx context.Context, conn *websocket.Conn) {
		for turn := 1; ; turn++ {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var frame map[string]any
			_ = json.Unmarshal(data, &frame)
			fx.mu.Lock()
			fx.gptFrames = append(fx.gptFrames, frame)
			fx.mu.Unlock()
			id := fmt.Sprintf("resp_gpt_%d", turn)
			item := fmt.Sprintf(`{"id":"msg_gpt_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":"GPT_REPLY_%d"}]}`, turn, turn)
			for _, event := range []string{
				`{"type":"response.created","response":{"id":"` + id + `"}}`,
				`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
				`{"type":"response.completed","response":{"id":"` + id + `","usage":{"input_tokens":5,"output_tokens":1}}}`,
			} {
				if conn.Write(ctx, websocket.MessageText, []byte(event)) != nil {
					return
				}
			}
		}
	})
	upURL, _ := url.Parse(upstream.server.URL)
	codex := &Account{Type: AccountTypeCodex, ID: "bridge", AccessToken: "bridge-token", codex: codexAccountState{AccountID: "acct_bridge"}, PlanType: "pro"}
	qwenAccount := &Account{ID: "qwen-ws", Type: AccountTypeQwen, AccessToken: "sk-sp-ws", Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai"}}}
	fx.proxy = newCodexProxyFixture(t, upURL, []*Account{codex, qwenAccount})
	h := fx.proxy.handler
	qwenBase := mustParse(qwen.URL + "/compatible-mode/v1")
	h.cfg.qwenBase, h.cfg.maxAttempts, h.cfg.maxInMemoryBodyBytes = qwenBase, 1, 1<<20
	h.aliases = newModelAliases(nil)
	h.registry = NewProviderRegistry(NewCodexProvider(upURL, upURL, upURL), NewClaudeProvider(upURL), NewGeminiProvider(upURL, upURL), NewQwenProvider(qwenBase), NewAIStudioProvider(upURL))
	return fx
}

// readTurn reads frames until the turn's terminal event and returns them.
func readTurn(t *testing.T, conn *websocket.Conn) []map[string]any {
	t.Helper()
	var events []map[string]any
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read after %d events: %v", len(events), err)
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatalf("invalid frame %s", data)
		}
		events = append(events, event)
		switch event["type"] {
		case "response.completed", "response.failed", "response.incomplete", "error":
			return events
		}
	}
}

func completedResponseID(t *testing.T, events []map[string]any) string {
	t.Helper()
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatalf("turn did not complete: %v", last)
	}
	id := jsonvalue.String(jsonvalue.Map(last["response"])["id"])
	if id == "" {
		t.Fatalf("completed response has no id: %v", last)
	}
	return id
}

func eventsText(events []map[string]any) string {
	encoded, _ := json.Marshal(events)
	return string(encoded)
}

func TestCodexWebSocketRoutesEachModelToItsProvider(t *testing.T) {
	fx := newCodexBridgeFixture(t)
	conn := dialClientWS(t, fx.proxy, http.Header{
		"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "ws-bridge-user")},
		"session_id":    []string{"conv-ws-bridge"},
	})
	send := func(frame map[string]any) {
		t.Helper()
		frame["type"] = "response.create"
		data, _ := json.Marshal(frame)
		if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	user := func(text string) map[string]any {
		return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
	}

	// Turn 1: Qwen over the websocket goes to the Qwen provider.
	send(map[string]any{"model": "qwen3.8-max", "input": []any{user("first")}})
	first := readTurn(t, conn)
	if !strings.Contains(eventsText(first), "QWEN_REPLY_1") {
		t.Fatalf("Qwen turn was not served by Qwen: %s", eventsText(first))
	}
	qwenID := completedResponseID(t, first)

	// Turn 2: a gpt model chained from the Qwen response reaches the Codex
	// upstream with the complete input instead of an ID it never minted.
	send(map[string]any{"model": "gpt-5.6-sol", "previous_response_id": qwenID, "input": []any{user("second")}})
	second := readTurn(t, conn)
	if !strings.Contains(eventsText(second), "GPT_REPLY_1") {
		t.Fatalf("gpt turn was not served by the Codex upstream: %s", eventsText(second))
	}
	gptID := completedResponseID(t, second)

	// Turn 3: back to Qwen, chained from the gpt response.
	send(map[string]any{"model": "qwen3.8-max", "previous_response_id": gptID, "input": []any{user("third")}})
	third := readTurn(t, conn)
	if !strings.Contains(eventsText(third), "QWEN_REPLY_2") {
		t.Fatalf("second Qwen turn failed: %s", eventsText(third))
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.gptFrames) != 1 || len(fx.qwenBodies) != 2 {
		t.Fatalf("upstream calls: gpt=%d qwen=%d", len(fx.gptFrames), len(fx.qwenBodies))
	}
	gptFrame := fx.gptFrames[0]
	if _, chained := gptFrame["previous_response_id"]; chained {
		t.Fatalf("Codex upstream received a bridged response id: %v", gptFrame)
	}
	if input := fmt.Sprint(gptFrame["input"]); !strings.Contains(input, "first") || !strings.Contains(input, "QWEN_REPLY_1") || !strings.Contains(input, "second") {
		t.Fatalf("Codex upstream did not receive the complete history: %s", input)
	}
	var history []string
	for _, raw := range jsonvalue.Slice(fx.qwenBodies[1]["messages"]) {
		history = append(history, fmt.Sprint(jsonvalue.Map(raw)["content"]))
	}
	joined := strings.Join(history, " | ")
	for _, want := range []string{"first", "QWEN_REPLY_1", "second", "GPT_REPLY_1", "third"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Qwen did not receive the complete history (%s missing): %s", want, joined)
		}
	}
}

func TestCodexWebSocketBridgeReportsProviderErrors(t *testing.T) {
	fx := newCodexBridgeFixture(t)
	conn := dialClientWS(t, fx.proxy, http.Header{
		"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "ws-bridge-user")},
		"session_id":    []string{"conv-ws-bridge-error"},
	})
	// No AI Studio key is in the pool, so the provider rejects the turn.
	data := []byte(`{"type":"response.create","model":"aistudio/gemini-3.8-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	events := readTurn(t, conn)
	last := events[len(events)-1]
	if last["type"] != "error" || !strings.Contains(fmt.Sprint(jsonvalue.Map(last["error"])["message"]), "AI Studio") {
		t.Fatalf("provider failure was not reported as a websocket error: %s", eventsText(events))
	}
	if strings.Contains(eventsText(events), "ChatGPT account") {
		t.Fatalf("non-OpenAI model reached the ChatGPT upstream: %s", eventsText(events))
	}
	// The socket keeps serving later turns.
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"qwen3.8-max","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	if next := readTurn(t, conn); !strings.Contains(eventsText(next), "QWEN_REPLY_1") {
		t.Fatalf("socket did not recover after a provider error: %s", eventsText(next))
	}
}

func TestCodexWebSocketBridgeAnswersPrewarmWithoutProviderCall(t *testing.T) {
	fx := newCodexBridgeFixture(t)
	conn := dialClientWS(t, fx.proxy, http.Header{
		"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "ws-bridge-user")},
		"session_id":    []string{"conv-ws-prewarm"},
	})
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"qwen3.8-max","generate":false,"input":[]}`)); err != nil {
		t.Fatal(err)
	}
	prewarm := readTurn(t, conn)
	prewarmID := completedResponseID(t, prewarm)
	fx.mu.Lock()
	calls := len(fx.qwenBodies)
	fx.mu.Unlock()
	if calls != 0 {
		t.Fatalf("prewarm called the provider %d times", calls)
	}
	// Codex chains its first real turn from the warmed response.
	frame, _ := json.Marshal(map[string]any{"type": "response.create", "model": "qwen3.8-max", "previous_response_id": prewarmID,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "after warmup"}}}}})
	if err := conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	if turn := readTurn(t, conn); !strings.Contains(eventsText(turn), "QWEN_REPLY_1") {
		t.Fatalf("turn chained from the prewarm failed: %s", eventsText(turn))
	}
}
