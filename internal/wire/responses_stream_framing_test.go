package wire

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestStreamingBufferingFraming(t *testing.T) {
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"FIRST\"}\r\n\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"SECOND\"}\r\r"
	for split := 0; split <= len(stream); split++ {
		response := NewResponsesBuffer("")
		completion := NewResponsesToOpenAICompletionsBuffer(StreamOptions{})
		chat := NewResponsesToOpenAIChatBuffer(StreamOptions{})
		claude := NewResponsesToAnthropicBuffer("", StreamOptions{})
		for _, w := range []io.Writer{response, completion, chat, claude} {
			_, err := io.WriteString(w, stream[:split])
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.WriteString(w, stream[split:])
			if err != nil {
				t.Fatal(err)
			}
		}
		if string(response.contentText) != "FIRSTSECOND" || string(completion.contentText) != "FIRSTSECOND" || string(chat.contentText) != "FIRSTSECOND" || string(claude.contentText) != "FIRSTSECOND" {
			t.Fatalf("buffered content missing at split %d", split)
		}
	}
}

func TestStreamingIncompleteSiblings(t *testing.T) {
	terminal := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\r\r"
	stream := terminal + terminal + "data: [DONE]\n\n"
	var completionOut, claudeOut bytes.Buffer
	completion := NewResponsesToOpenAICompletionsStream(&completionOut, StreamOptions{})
	claude := NewResponsesToAnthropicStream(&claudeOut, StreamOptions{})
	for _, w := range []io.Writer{completion, claude} {
		if _, err := io.WriteString(w, stream); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(completionOut.String(), "data: [DONE]") != 1 || !strings.Contains(completionOut.String(), `"finish_reason":"length"`) || !strings.Contains(completionOut.String(), `"prompt_tokens":7`) {
		t.Fatal(completionOut.String())
	}
	if strings.Count(claudeOut.String(), "event: message_stop") != 1 || !strings.Contains(claudeOut.String(), `"stop_reason":"max_tokens"`) || !strings.Contains(claudeOut.String(), `"input_tokens":7`) || !strings.Contains(claudeOut.String(), `"output_tokens":3`) {
		t.Fatal(claudeOut.String())
	}

	response := NewResponsesBuffer("")
	completionBuffer := NewResponsesToOpenAICompletionsBuffer(StreamOptions{})
	chatBuffer := NewResponsesToOpenAIChatBuffer(StreamOptions{})
	claudeBuffer := NewResponsesToAnthropicBuffer("", StreamOptions{})
	for _, w := range []io.Writer{response, completionBuffer, chatBuffer, claudeBuffer} {
		if _, err := io.WriteString(w, stream); err != nil {
			t.Fatal(err)
		}
	}
	if response.status != "incomplete" || completionBuffer.finishReason != "length" || chatBuffer.finishReason != "length" || claudeBuffer.stopReason != "max_tokens" {
		t.Fatal("incomplete terminal not mapped by buffering writer")
	}
	if response.inputTokens != 7 || response.outputTokens != 3 || completionBuffer.inputTokens != 7 || completionBuffer.outputTokens != 3 || chatBuffer.inputTokens != 7 || chatBuffer.outputTokens != 3 || claudeBuffer.inputTokens != 7 || claudeBuffer.outputTokens != 3 {
		t.Fatal("incomplete usage lost by buffering writer")
	}
}
