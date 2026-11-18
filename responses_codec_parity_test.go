package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
)

type responsesParityFixture struct {
	Funcs   []responsesParityFunc   `json:"funcs"`
	Writers []responsesParityWriter `json:"writers"`
}

type responsesParityFunc struct {
	Codec         string `json:"codec"`
	Input         string `json:"input"`
	ResolvedModel string `json:"resolved_model,omitempty"`
	Output        string `json:"output"`
	Err           string `json:"err,omitempty"`
}

type responsesParityWrite struct {
	N   int    `json:"n"`
	Err string `json:"err,omitempty"`
}

type responsesParityWriter struct {
	Codec     string                 `json:"codec"`
	Model     string                 `json:"model,omitempty"`
	Chunks    []string               `json:"chunks"`
	Writes    []responsesParityWrite `json:"writes"`
	Output    string                 `json:"output"`
	Callbacks []string               `json:"callbacks,omitempty"`
	Result    string                 `json:"result,omitempty"`
}

func loadResponsesParityFixture(t *testing.T) responsesParityFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/responses_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture responsesParityFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func runResponsesParityFunc(t *testing.T, tc responsesParityFunc) ([]byte, error) {
	t.Helper()
	input := []byte(tc.Input)
	switch tc.Codec {
	case "chat-request":
		return translateChatCompletionsToResponses(input)
	case "completions-request":
		return translateCompletionsToResponses(input)
	case "responses-to-completions":
		return translateResponsesToCompletions(input)
	case "responses-to-chat":
		return translateResponsesToChatCompletions(input)
	case "responses-request-to-messages":
		return translateResponsesToClaudeRequest(input)
	case "messages-response-to-responses":
		return translateClaudeRespToResponses(input)
	case "messages-request-to-responses":
		return translateClaudeToResponsesRequest(input)
	case "responses-schema":
		return normalizeResponsesSchemaBody(input), nil
	case "codex-json-schema":
		var schema map[string]any
		if err := json.Unmarshal(input, &schema); err != nil {
			t.Fatal(err)
		}
		return json.Marshal(prepareCodexJSONSchema(schema))
	case "merge-messages", "messages-tool-pairing":
		var messages []map[string]any
		if err := json.Unmarshal(input, &messages); err != nil {
			t.Fatal(err)
		}
		if tc.Codec == "merge-messages" {
			return json.Marshal(mergeConsecutiveClaudeMessages(messages))
		}
		return json.Marshal(normalizeClaudeToolPairing(messages))
	}
	t.Fatalf("unknown codec %q", tc.Codec)
	return nil, nil
}

func runResponsesParityWriter(t *testing.T, tc responsesParityWriter) responsesParityWriter {
	t.Helper()
	var out bytes.Buffer
	got := responsesParityWriter{Codec: tc.Codec, Model: tc.Model, Chunks: tc.Chunks}
	callback := func(data []byte) { got.Callbacks = append(got.Callbacks, string(data)) }
	var writer io.Writer
	var result func() []byte
	switch tc.Codec {
	case "responses-stream-buffer":
		w := &responsesBufferingWriter{model: tc.Model}
		writer, result = w, w.Result
	case "responses-stream-to-completions":
		writer = &responsesToCompletionsWriter{w: &out, callback: callback}
	case "responses-stream-to-completions-buffer":
		w := &responsesToCompletionsBufferingWriter{callback: callback}
		writer, result = w, w.Result
	case "responses-stream-to-chat":
		writer = &responsesToChatCompletionsWriter{w: &out, callback: callback}
	case "responses-stream-to-chat-buffer":
		w := &responsesToChatCompletionsBufferingWriter{callback: callback}
		writer, result = w, w.Result
	case "responses-stream-to-messages-buffer":
		w := &responsesToClaudeBufferingWriter{callback: callback, model: tc.Model}
		writer, result = w, w.Result
	case "messages-stream-to-responses":
		writer = &claudeToResponsesWriter{w: &out, callback: callback}
	case "responses-stream-to-messages":
		writer = &responsesToClaudeWriter{w: &out, callback: callback}
	default:
		t.Fatalf("unknown codec %q", tc.Codec)
	}
	for _, chunk := range tc.Chunks {
		n, err := writer.Write([]byte(chunk))
		write := responsesParityWrite{N: n}
		if err != nil {
			write.Err = err.Error()
		}
		got.Writes = append(got.Writes, write)
	}
	got.Output = out.String()
	if result != nil {
		got.Result = string(result())
	}
	return got
}

func TestResponsesCodecParity(t *testing.T) {
	fixture := loadResponsesParityFixture(t)
	for i, tc := range fixture.Funcs {
		t.Run(fmt.Sprintf("%s/%d", tc.Codec, i), func(t *testing.T) {
			if tc.ResolvedModel != "" {
				var request struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal([]byte(tc.Input), &request); err != nil || claudeCanonicalModel(request.Model) != tc.ResolvedModel {
					t.Fatalf("resolved model for %q = %q, fixture has %q", request.Model, claudeCanonicalModel(request.Model), tc.ResolvedModel)
				}
			}
			got, err := runResponsesParityFunc(t, tc)
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if gotErr != tc.Err || string(got) != tc.Output {
				t.Fatalf("got  %s (err %q)\nwant %s (err %q)", got, gotErr, tc.Output, tc.Err)
			}
		})
	}
	for i, tc := range fixture.Writers {
		t.Run(fmt.Sprintf("%s/%d", tc.Codec, i), func(t *testing.T) {
			got, _ := json.Marshal(runResponsesParityWriter(t, tc))
			want, _ := json.Marshal(tc)
			if !bytes.Equal(got, want) {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
		})
	}
}
