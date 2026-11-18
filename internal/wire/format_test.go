package wire

import "testing"

func TestRequestFormatNames(t *testing.T) {
	for format, want := range map[RequestFormat]string{
		FormatUnknown: "unknown", FormatOpenAI: "openai_chat", FormatClaude: "anthropic_messages",
		FormatResponses: "openai_responses", FormatGemini: "gemini", RequestFormat(99): "unknown",
	} {
		if got := format.String(); got != want {
			t.Fatalf("%d.String() = %q, want %q", format, got, want)
		}
	}
}
