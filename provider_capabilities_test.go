package main

import (
	"testing"

	"codex-pool-proxy/internal/wire"
)

func TestProviderCapabilitiesMatchWireAndHandoffContract(t *testing.T) {
	tests := []struct {
		provider ProviderDriver
		wire     WireFormat
		target   wire.RequestFormat
		full     bool
		replay   bool
	}{
		{&CodexProvider{}, WireResponses, wire.FormatOpenAI, false, false},
		{&AntigravityProvider{}, WireGemini, wire.FormatUnknown, false, true},
		{&ClaudeProvider{}, WireAnthropic, wire.FormatClaude, true, false},
		{&ZAIProvider{}, WireAnthropic, wire.FormatClaude, true, false},
		{&GeminiProvider{}, WireGemini, wire.FormatUnknown, false, false},
	}
	for _, test := range tests {
		t.Run(string(test.provider.Type()), func(t *testing.T) {
			caps := capabilitiesFor(test.provider.Type())
			if caps.WireFormat != test.wire || requestFormatForCodec(test.provider) != test.target || caps.SupportsFullHistoryHandoff != test.full || caps.SupportsNativeReplay != test.replay {
				t.Fatalf("capabilities=%+v target=%s", caps, requestFormatForCodec(test.provider))
			}
			if !caps.SupportsForeignHistory || !caps.SupportsToolHistory || !caps.SupportsParallelTools {
				t.Fatalf("missing known conversation capabilities: %+v", caps)
			}
		})
	}
	unknown := capabilitiesFor(AccountType("unregistered"))
	if unknown.WireFormat != WireUnknown || unknown.SupportsForeignHistory || unknown.SupportsFullHistoryHandoff {
		t.Fatalf("unknown provider was assumed compatible: %+v", unknown)
	}
}
