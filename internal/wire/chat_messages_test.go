package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestChatMessagesCodecParity(t *testing.T) {
	fixtures, err := os.ReadFile("testdata/chat_messages_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Codec string
		Input, Want json.RawMessage
	}
	if err := json.Unmarshal(fixtures, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var got []byte
			var err error
			switch tc.Codec {
			case "messages-request":
				got, err = AnthropicRequestToOpenAIChat(tc.Input)
			case "chat-request":
				got, err = OpenAIChatRequestToAnthropic(tc.Input, func(model string) string {
					if model != "sonnet" {
						t.Fatalf("unexpected model passed to resolver: %q", model)
					}
					return "claude-sonnet-5-5"
				})
				if err == nil && !bytes.HasPrefix(got, []byte(`{"model":"claude-sonnet-5-5","messages":`)) {
					t.Fatalf("Messages key order: %s", got)
				}
			case "chat-response":
				got, err = OpenAIChatResponseToAnthropic(tc.Input, "")
			case "messages-response":
				got, err = AnthropicResponseToOpenAIChat(tc.Input)
			case "chat-error":
				got, err = TranslateChatMessagesError(tc.Input, TranslateOAIToClaude)
			case "messages-error":
				got, err = TranslateChatMessagesError(tc.Input, TranslateClaudeToOAI)
			default:
				t.Fatalf("unknown codec %q", tc.Codec)
			}
			if err != nil {
				t.Fatal(err)
			}
			var gotObject, wantObject any
			if err := json.Unmarshal(got, &gotObject); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(tc.Want, &wantObject); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotObject, wantObject) {
				t.Fatalf("got  %s\nwant %s", got, tc.Want)
			}
		})
	}
}

func TestChatMessagesCodecRejectsInvalidInput(t *testing.T) {
	identity := func(model string) string { return model }
	cases := []struct {
		name string
		run  func() ([]byte, error)
	}{
		{"null request", func() ([]byte, error) { return AnthropicRequestToOpenAIChat([]byte(`null`)) }},
		{"unknown tool choice", func() ([]byte, error) {
			return AnthropicRequestToOpenAIChat([]byte(`{"tool_choice":{"type":"unknown"}}`))
		}},
		{"malformed request tool input", func() ([]byte, error) {
			return OpenAIChatRequestToAnthropic([]byte(`{"model":"sonnet","messages":[{"role":"assistant","tool_calls":[{"id":"tool-1","function":{"name":"read","arguments":"{"}}]}]}`), identity)
		}},
		{"malformed response tool input", func() ([]byte, error) {
			return OpenAIChatResponseToAnthropic([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"tool-1","function":{"name":"read","arguments":"{"}}]}}]}`), "sonnet")
		}},
		{"response from another protocol", func() ([]byte, error) {
			return OpenAIChatResponseToAnthropic([]byte(`{"object":"response","output":[]}`), "sonnet")
		}},
		{"non-JSON upstream error", func() ([]byte, error) {
			return TranslateChatMessagesError([]byte("<html>blocked</html>"), TranslateClaudeToOAI)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.run(); err == nil || len(got) != 0 {
				t.Fatalf("invalid input translated to %s, err=%v", got, err)
			}
		})
	}
}

func TestChatMessagesStreamStopsOnInvalidEvent(t *testing.T) {
	for _, tc := range []struct {
		name, errorEvent string
		new              func(io.Writer) io.Writer
	}{
		{"chat-to-messages", `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"invalid upstream stream event"}}` + "\n\n", NewChatToAnthropicStream},
		{"messages-to-chat", `data: {"error":{"message":"invalid upstream stream event","type":"server_error","code":null}}` + "\n\n", NewAnthropicToChatStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writer := tc.new(&out)
			_, err := fmt.Fprint(writer, "data: not-json\n\ndata: [DONE]\n\n")
			if err == nil || out.String() != tc.errorEvent {
				t.Fatalf("stream did not stop with one native error: %q, %v", out.String(), err)
			}
			n, secondErr := io.WriteString(writer, ": heartbeat\n\n")
			if n != 0 || secondErr != err || out.String() != tc.errorEvent {
				t.Fatalf("stream resumed after failure: n=%d err=%v body=%q", n, secondErr, out.String())
			}
		})
	}
}

func TestChatMessagesStreamTranslatesUpstreamFailure(t *testing.T) {
	var out bytes.Buffer
	writer := NewAnthropicToChatStream(&out)
	_, err := io.WriteString(writer, "event: error\ndata: "+`{"type":"error","error":{"type":"overloaded_error","message":"Busy"}}`+
		"\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if err == nil || !strings.Contains(out.String(), `"message":"Busy","type":"server_error"`) ||
		strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("unexpected terminal error: %q, %v", out.String(), err)
	}
}
