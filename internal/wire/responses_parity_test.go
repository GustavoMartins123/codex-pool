package wire

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
		return OpenAIChatRequestToResponses(input)
	case "completions-request":
		return OpenAICompletionsRequestToResponses(input)
	case "responses-to-completions":
		return ResponsesResponseToOpenAICompletions(input)
	case "responses-to-chat":
		return ResponsesResponseToOpenAIChat(input)
	case "responses-request-to-messages":
		return ResponsesRequestToAnthropic(input, func(model string) string {
			if tc.ResolvedModel == "" {
				t.Fatalf("model %q has no resolved name in the fixture", model)
			}
			return tc.ResolvedModel
		})
	case "messages-response-to-responses":
		return AnthropicResponseToResponses(input)
	case "messages-request-to-responses":
		return AnthropicRequestToResponses(input)
	case "responses-schema":
		return NormalizeResponsesSchemaBody(input), nil
	case "codex-json-schema":
		var schema map[string]any
		if err := json.Unmarshal(input, &schema); err != nil {
			t.Fatal(err)
		}
		return json.Marshal(PrepareCodexJSONSchema(schema))
	case "merge-messages", "messages-tool-pairing":
		var messages []map[string]any
		if err := json.Unmarshal(input, &messages); err != nil {
			t.Fatal(err)
		}
		if tc.Codec == "merge-messages" {
			return json.Marshal(MergeConsecutiveAnthropicMessages(messages))
		}
		return json.Marshal(NormalizeAnthropicToolPairing(messages))
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
		w := NewResponsesBuffer(tc.Model)
		writer, result = w, w.Result
	case "responses-stream-to-completions":
		writer = NewResponsesToOpenAICompletionsStream(&out, StreamOptions{OnEvent: callback})
	case "responses-stream-to-completions-buffer":
		w := NewResponsesToOpenAICompletionsBuffer(StreamOptions{OnEvent: callback})
		writer, result = w, w.Result
	case "responses-stream-to-chat":
		writer = NewResponsesToOpenAIChatStream(&out, StreamOptions{OnEvent: callback})
	case "responses-stream-to-chat-buffer":
		w := NewResponsesToOpenAIChatBuffer(StreamOptions{OnEvent: callback})
		writer, result = w, w.Result
	case "responses-stream-to-messages-buffer":
		w := NewResponsesToAnthropicBuffer(tc.Model, StreamOptions{OnEvent: callback})
		writer, result = w, w.Result
	case "messages-stream-to-responses":
		writer = NewAnthropicToResponsesStream(&out, StreamOptions{OnEvent: callback})
	case "responses-stream-to-messages":
		writer = NewResponsesToAnthropicStream(&out, StreamOptions{OnEvent: callback})
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
