package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Codex reuses its Responses websocket: after the first response on a
// connection, each response.create carries previous_response_id plus only
// the items appended since that response. ChatGPT accounts run with
// store=false, so the referenced response exists only inside that upstream
// connection. A swap to another account must therefore replay the complete
// input; replaying the delta alone hands the replacement tool outputs whose
// calls it never saw ("No tool call found for ... call_id").
//
// The relay keeps, per socket, what previous_response_id resolves to
// upstream: the complete input of the latest completed response followed by
// that response's output items.

// codexReplayHistoryLimit bounds the input retained per socket. A longer
// chain is not replayed; the client resends it on a fresh connection.
const codexReplayHistoryLimit = maxContextBytes

var errCodexReplayUnavailable = errors.New("websocket turn continues a response chain the pool cannot replay")

// codexInputHistory is the complete input a request chained from
// responseID resolves to, before its own delta.
type codexInputHistory struct {
	responseID string
	items      []json.RawMessage
	bytes      int
}

type codexOutputItem struct {
	index int
	item  json.RawMessage
}

// codexResponseCreateInput reads previous_response_id and the input items of
// a flat or nested response.create frame. ok is false when the input is not
// an item array.
func codexResponseCreateInput(data []byte) (previousResponseID string, input []json.RawMessage, ok bool) {
	type request struct {
		PreviousResponseID string          `json:"previous_response_id"`
		Input              json.RawMessage `json:"input"`
	}
	var frame struct {
		request
		Response *request `json:"response"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return "", nil, false
	}
	fields := frame.request
	if frame.Response != nil {
		fields = *frame.Response
	}
	raw := bytes.TrimSpace(fields.Input)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fields.PreviousResponseID, nil, true
	}
	if raw[0] != '[' || json.Unmarshal(raw, &input) != nil {
		return fields.PreviousResponseID, nil, false
	}
	return fields.PreviousResponseID, input, true
}

func rawItemsSize(items []json.RawMessage) int {
	size := 0
	for _, item := range items {
		size += len(item)
	}
	return size
}

// codexInputItemFromOutput turns a response output item into the input item
// Codex itself sends back: the same object without the server item ID, which
// a store=false response never persists.
func codexInputItemFromOutput(item json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil || fields == nil {
		return nil
	}
	if _, ok := fields["id"]; !ok {
		return item
	}
	delete(fields, "id")
	out, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return out
}

// bindTurnInput resolves the complete input of a new turn from the socket's
// retained chain. The caller holds turnMu.
func (s *codexRelayState) bindTurnInput(turn *codexRelayTurn) {
	previous, delta, ok := codexResponseCreateInput(turn.request)
	turn.continues = previous
	if !ok {
		return
	}
	size := rawItemsSize(delta)
	if previous == "" {
		if size <= codexReplayHistoryLimit {
			turn.input, turn.inputBytes, turn.inputKnown = delta, size, true
		}
		return
	}
	history := s.history
	if history == nil || history.responseID != previous || history.bytes+size > codexReplayHistoryLimit {
		return
	}
	input := make([]json.RawMessage, 0, len(history.items)+len(delta))
	input = append(append(input, history.items...), delta...)
	turn.input, turn.inputBytes, turn.inputKnown = input, history.bytes+size, true
}

// observeTurnEvent records a forwarded upstream event: output items extend
// the turn's chain, and a completed response becomes the socket's chain
// head. The caller holds turnMu.
func (s *codexRelayState) observeTurnEvent(turn *codexRelayTurn, data []byte) {
	var event struct {
		Type        string          `json:"type"`
		OutputIndex int             `json:"output_index"`
		Item        json.RawMessage `json:"item"`
	}
	if json.Unmarshal(data, &event) != nil || !strings.HasPrefix(event.Type, "response.") {
		return
	}
	if turn == nil {
		// Output the relay cannot attribute leaves every pending chain
		// incomplete; replaying one would drop those items.
		if event.Type == "response.output_item.done" {
			for _, pending := range s.turns {
				pending.outputUnknown = true
			}
		}
		return
	}
	switch event.Type {
	case "response.output_item.done":
		if turn.outputUnknown {
			return
		}
		item := codexInputItemFromOutput(event.Item)
		if item == nil || turn.inputBytes+turn.outputBytes+len(item) > codexReplayHistoryLimit {
			turn.output, turn.outputBytes, turn.outputUnknown = nil, 0, true
			return
		}
		turn.output = append(turn.output, codexOutputItem{index: event.OutputIndex, item: item})
		turn.outputBytes += len(item)
	case "response.completed":
		s.history = nil
		if turn.responseID == "" || !turn.inputKnown || turn.outputUnknown {
			return
		}
		sort.SliceStable(turn.output, func(i, j int) bool { return turn.output[i].index < turn.output[j].index })
		items := make([]json.RawMessage, 0, len(turn.input)+len(turn.output))
		items = append(items, turn.input...)
		for _, output := range turn.output {
			items = append(items, output.item)
		}
		s.history = &codexInputHistory{responseID: turn.responseID, items: items, bytes: turn.inputBytes + turn.outputBytes}
	}
}

// replayable reports whether a swap target can be handed this turn.
func (t *codexRelayTurn) replayable() bool {
	return t.continues == "" || t.inputKnown
}

// swapReplayFrame returns the response.create a swap target receives for
// turn. A chained request is rebuilt with its complete input and without
// previous_response_id, which the target never minted.
func swapReplayFrame(turn *codexRelayTurn) ([]byte, error) {
	if turn.continues == "" {
		return turn.request, nil
	}
	if !turn.inputKnown {
		return nil, errCodexReplayUnavailable
	}
	return codexReplayFrame(turn.request, turn.input)
}

func codexReplayFrame(request []byte, input []json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(request))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if input == nil {
		input = []json.RawMessage{}
	}
	object := contextRequestObject(root)
	object["input"] = input
	delete(object, "previous_response_id")
	delete(root, "previous_response_id")
	return json.Marshal(root)
}
