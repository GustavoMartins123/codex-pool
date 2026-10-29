package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const chainFirstRequest = `{"type":"response.create","model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}`

// chainToolOutputRequest is what Codex sends on a reused socket after the
// model calls apply_patch: the previous response ID plus only the new output.
const chainToolOutputRequest = `{"type":"response.create","model":"gpt-5.5","previous_response_id":"resp_chain_1","input":[{"type":"custom_tool_call_output","call_id":"call_patch","output":"Done"}]}`

var chainFirstResponse = []string{
	`{"type":"response.created","response":{"id":"resp_chain_1","status":"in_progress"}}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_chain","type":"reasoning","summary":[],"encrypted_content":"enc-a"}}`,
	`{"type":"response.output_item.done","output_index":1,"item":{"id":"ctc_chain","type":"custom_tool_call","status":"completed","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch"}}`,
	`{"type":"response.completed","response":{"id":"resp_chain_1","status":"completed"}}`,
}

// assertChainReplay checks that a swap target received the whole chain
// behind chainToolOutputRequest instead of the orphaned tool output.
func assertChainReplay(t *testing.T, frame []byte) {
	t.Helper()
	var replay map[string]any
	if err := json.Unmarshal(frame, &replay); err != nil {
		t.Fatalf("replay is not JSON: %v: %s", err, frame)
	}
	if _, ok := replay["previous_response_id"]; ok {
		t.Fatalf("replay kept previous_response_id: %s", frame)
	}
	if replay["model"] != "gpt-5.5" {
		t.Fatalf("replay lost model: %s", frame)
	}
	input, _ := replay["input"].([]any)
	wantTypes := []string{"message", "reasoning", "custom_tool_call", "custom_tool_call_output"}
	if len(input) != len(wantTypes) {
		t.Fatalf("replay input has %d items, want %d: %s", len(input), len(wantTypes), frame)
	}
	for i, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] != wantTypes[i] {
			t.Fatalf("replay input[%d] type = %v, want %s: %s", i, item["type"], wantTypes[i], frame)
		}
		if _, ok := item["id"]; ok {
			t.Fatalf("replay input[%d] kept a server item ID: %s", i, frame)
		}
	}
	call, _ := input[2].(map[string]any)
	output, _ := input[3].(map[string]any)
	if call["call_id"] != "call_patch" || output["call_id"] != "call_patch" {
		t.Fatalf("replay broke the tool pair: %s", frame)
	}
	reasoning, _ := input[1].(map[string]any)
	if reasoning["encrypted_content"] != "enc-a" {
		t.Fatalf("replay dropped encrypted reasoning: %s", frame)
	}
}

func writeFrames(ctx context.Context, conn *websocket.Conn, frames []string) {
	for _, frame := range frames {
		_ = conn.Write(ctx, websocket.MessageText, []byte(frame))
	}
}

func TestCodexResponseCreateInput(t *testing.T) {
	t.Parallel()
	previous, input, ok := codexResponseCreateInput([]byte(chainToolOutputRequest))
	if !ok || previous != "resp_chain_1" || len(input) != 1 {
		t.Fatalf("flat = %q %d %v", previous, len(input), ok)
	}
	nested := `{"type":"response.create","response":{"previous_response_id":"resp_n","input":[{"type":"message"},{"type":"message"}]}}`
	previous, input, ok = codexResponseCreateInput([]byte(nested))
	if !ok || previous != "resp_n" || len(input) != 2 {
		t.Fatalf("nested = %q %d %v", previous, len(input), ok)
	}
	if _, input, ok = codexResponseCreateInput([]byte(`{"type":"response.create"}`)); !ok || input != nil {
		t.Fatalf("absent input = %d %v", len(input), ok)
	}
	if _, _, ok = codexResponseCreateInput([]byte(`{"type":"response.create","input":"hi"}`)); ok {
		t.Fatal("string input reported as an item array")
	}
}

func TestCodexReplayFrameRebuildsInput(t *testing.T) {
	t.Parallel()
	items := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user"}`), json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"ok"}`)}

	flat, err := codexReplayFrame([]byte(`{"type":"response.create","model":"gpt-5.5","max_output_tokens":128000,"previous_response_id":"resp_a","input":[]}`), items)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(flat, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["previous_response_id"]; ok || got["type"] != "response.create" || got["model"] != "gpt-5.5" {
		t.Fatalf("flat replay = %s", flat)
	}
	if !strings.Contains(string(flat), `"max_output_tokens":128000`) {
		t.Fatalf("flat replay changed numbers: %s", flat)
	}
	if input, _ := got["input"].([]any); len(input) != 2 {
		t.Fatalf("flat replay input = %s", flat)
	}

	nested, err := codexReplayFrame([]byte(`{"type":"response.create","response":{"model":"gpt-5.5","previous_response_id":"resp_a","input":[]}}`), items)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := json.Unmarshal(nested, &got); err != nil {
		t.Fatal(err)
	}
	response, _ := got["response"].(map[string]any)
	if _, ok := response["previous_response_id"]; ok || response["model"] != "gpt-5.5" {
		t.Fatalf("nested replay = %s", nested)
	}
	if input, _ := response["input"].([]any); len(input) != 2 {
		t.Fatalf("nested replay input = %s", nested)
	}
}

func TestCodexRelayHistoryChainsCompletedResponses(t *testing.T) {
	t.Parallel()
	s := &codexRelayState{}
	first := &codexRelayTurn{request: []byte(chainFirstRequest)}
	s.bindTurnInput(first)
	s.turns = []*codexRelayTurn{first}
	first.responseID = "resp_chain_1"
	// Output items may complete out of order; the chain keeps output order.
	s.observeTurnEvent(first, []byte(chainFirstResponse[2]))
	s.observeTurnEvent(first, []byte(chainFirstResponse[1]))
	s.observeTurnEvent(first, []byte(chainFirstResponse[3]))
	s.turns = nil
	if s.history == nil || s.history.responseID != "resp_chain_1" || len(s.history.items) != 3 {
		t.Fatalf("history = %+v", s.history)
	}

	chained := &codexRelayTurn{request: []byte(chainToolOutputRequest)}
	s.bindTurnInput(chained)
	if !chained.replayable() {
		t.Fatal("turn chained from the retained response is not replayable")
	}
	frame, err := swapReplayFrame(chained)
	if err != nil {
		t.Fatal(err)
	}
	assertChainReplay(t, frame)

	unknown := &codexRelayTurn{request: []byte(strings.Replace(chainToolOutputRequest, "resp_chain_1", "resp_elsewhere", 1))}
	s.bindTurnInput(unknown)
	if unknown.replayable() {
		t.Fatal("turn chained from an unknown response reported replayable")
	}
	if _, err := swapReplayFrame(unknown); !errors.Is(err, errCodexReplayUnavailable) {
		t.Fatalf("unknown chain replay err = %v", err)
	}
}

func TestCodexRelayHistoryDropsChainWithUnattributedOutput(t *testing.T) {
	t.Parallel()
	s := &codexRelayState{}
	turns := []*codexRelayTurn{{request: []byte(chainFirstRequest)}, {request: []byte(chainFirstRequest)}}
	for _, turn := range turns {
		s.bindTurnInput(turn)
	}
	s.turns = turns
	s.observeTurnEvent(nil, []byte(chainFirstResponse[2]))
	turns[0].responseID = "resp_chain_1"
	s.observeTurnEvent(turns[0], []byte(chainFirstResponse[3]))
	if s.history != nil {
		t.Fatalf("history retained without the unattributed tool call: %+v", s.history)
	}
}

// The reported failure: the account hits its quota on the request that
// carries a tool output. The replacement must receive the whole chain, not
// a custom_tool_call_output whose call it never produced.
func TestQuotaErrorMidChainReplaysCompleteInput(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	upstream := newFakeCodexUpstream(t)
	upstream.on("acct_drain_chain", func(ctx context.Context, conn *websocket.Conn) {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		writeFrames(ctx, conn, chainFirstResponse)
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"usage_limit_reached","message":"You've hit your usage limit"}}`))
		_, _, _ = conn.Read(ctx)
	})
	var mu sync.Mutex
	var replay []byte
	upstream.on("acct_fresh_chain", func(ctx context.Context, conn *websocket.Conn) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		mu.Lock()
		replay = data
		mu.Unlock()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_fresh_chain","status":"completed"}}`))
	})

	upURL, _ := url.Parse(upstream.server.URL)
	drain := &Account{Type: AccountTypeCodex, ID: "drain", AccessToken: "drain-token", AccountID: "acct_drain_chain", PlanType: "pro"}
	fresh := &Account{Type: AccountTypeCodex, ID: "fresh", AccessToken: "fresh-token", AccountID: "acct_fresh_chain", PlanType: "pro"}
	fx := newCodexProxyFixture(t, upURL, []*Account{drain, fresh})
	fx.handler.pool.pin("conv-chain", "drain")

	conn := dialClientWS(t, fx, http.Header{
		"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "chain-user")},
		"session_id":    []string{"conv-chain"},
	})
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(chainFirstRequest)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if frames := mustReadUntil(t, conn, `"response.completed"`, 5*time.Second); len(frames) == 0 {
		t.Fatal("client did not receive the first response")
	}
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(chainToolOutputRequest)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	frames := mustReadUntil(t, conn, `"resp_fresh_chain"`, 5*time.Second)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "resp_fresh_chain") {
		t.Fatalf("client never got the replacement completion; frames: %s", joined)
	}
	if strings.Contains(joined, "usage_limit_reached") {
		t.Fatalf("quota error leaked to client: %s", joined)
	}
	mu.Lock()
	got := replay
	mu.Unlock()
	assertChainReplay(t, got)
	waitForZeroInflight(t, fx.handler, []*Account{drain, fresh})
}

// The 95% threshold swap runs before a chained request leaves the pool; it
// must hand the replacement the same complete input.
func TestQuotaThresholdMidChainReplaysCompleteInput(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	upstream := newFakeCodexUpstream(t)
	upstream.on("acct_drain_chain_pre", func(ctx context.Context, conn *websocket.Conn) {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		writeFrames(ctx, conn, chainFirstResponse)
		if _, data, err := conn.Read(ctx); err == nil && strings.Contains(string(data), "response.create") {
			t.Errorf("account past the threshold received the chained turn: %s", data)
		}
	})
	var mu sync.Mutex
	var replay []byte
	upstream.on("acct_fresh_chain_pre", func(ctx context.Context, conn *websocket.Conn) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		mu.Lock()
		replay = data
		mu.Unlock()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_fresh_chain_pre","status":"completed"}}`))
	})

	upURL, _ := url.Parse(upstream.server.URL)
	drain := &Account{Type: AccountTypeCodex, ID: "drain", AccessToken: "drain-token", AccountID: "acct_drain_chain_pre", PlanType: "pro"}
	fresh := &Account{Type: AccountTypeCodex, ID: "fresh", AccessToken: "fresh-token", AccountID: "acct_fresh_chain_pre", PlanType: "pro"}
	fx := newCodexProxyFixture(t, upURL, []*Account{drain, fresh})
	fx.handler.pool.pin("conv-chain-pre", "drain")

	conn := dialClientWS(t, fx, http.Header{
		"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "chain-pre-user")},
		"session_id":    []string{"conv-chain-pre"},
	})
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(chainFirstRequest)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if frames := mustReadUntil(t, conn, `"response.completed"`, 5*time.Second); len(frames) == 0 {
		t.Fatal("client did not receive the first response")
	}
	drain.mu.Lock()
	drain.Usage.PrimaryUsedPercent = 0.96
	drain.Usage.PrimaryResetAt = time.Now().Add(time.Hour)
	drain.mu.Unlock()
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(chainToolOutputRequest)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	frames := mustReadUntil(t, conn, `"resp_fresh_chain_pre"`, 5*time.Second)
	if !strings.Contains(strings.Join(frames, "\n"), "resp_fresh_chain_pre") {
		t.Fatalf("client never got the replacement completion; frames: %v", frames)
	}
	mu.Lock()
	got := replay
	mu.Unlock()
	assertChainReplay(t, got)
	waitForZeroInflight(t, fx.handler, []*Account{drain, fresh})
}

// Below a hard rejection the current account can still serve the turn, so a
// chain the pool cannot replay stays where it is instead of losing context.
func TestQuotaThresholdKeepsUnreplayableChainOnAccount(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	upstream := newFakeCodexUpstream(t)
	var mu sync.Mutex
	var served []byte
	upstream.on("acct_drain_unknown", func(ctx context.Context, conn *websocket.Conn) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		mu.Lock()
		served = data
		mu.Unlock()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_drain_unknown","status":"completed"}}`))
	})
	upstream.on("acct_fresh_unknown", func(ctx context.Context, conn *websocket.Conn) {
		if _, data, err := conn.Read(ctx); err == nil {
			t.Errorf("replacement received an unreplayable chain: %s", data)
		}
	})

	upURL, _ := url.Parse(upstream.server.URL)
	drain := &Account{Type: AccountTypeCodex, ID: "drain", AccessToken: "drain-token", AccountID: "acct_drain_unknown", PlanType: "pro"}
	fresh := &Account{Type: AccountTypeCodex, ID: "fresh", AccessToken: "fresh-token", AccountID: "acct_fresh_unknown", PlanType: "pro"}
	fx := newCodexProxyFixture(t, upURL, []*Account{drain, fresh})
	fx.handler.pool.pin("conv-unknown", "drain")

	conn := dialClientWS(t, fx, http.Header{
		"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "unknown-user")},
		"session_id":    []string{"conv-unknown"},
	})
	drain.mu.Lock()
	drain.Usage.PrimaryUsedPercent = 0.96
	drain.Usage.PrimaryResetAt = time.Now().Add(time.Hour)
	drain.mu.Unlock()
	request := strings.Replace(chainToolOutputRequest, "resp_chain_1", "resp_elsewhere", 1)
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(request)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if frames := mustReadUntil(t, conn, `"resp_drain_unknown"`, 5*time.Second); !strings.Contains(strings.Join(frames, "\n"), "resp_drain_unknown") {
		t.Fatalf("current account did not serve the chain; frames: %v", frames)
	}
	mu.Lock()
	got := string(served)
	mu.Unlock()
	if !strings.Contains(got, `"previous_response_id":"resp_elsewhere"`) {
		t.Fatalf("current account received a rewritten chain: %s", got)
	}
	if hits := upstream.hitCount("acct_fresh_unknown"); hits != 0 {
		t.Fatalf("replacement dialed %d times", hits)
	}
	waitForZeroInflight(t, fx.handler, []*Account{drain, fresh})
}
