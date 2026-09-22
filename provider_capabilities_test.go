package main

import "testing"

func TestProviderCapabilitiesMatchWireAndHandoffContract(t *testing.T) {
	tests := []struct {
		provider AccountType
		wire     WireFormat
		target   RequestFormat
		full     bool
		replay   bool
	}{
		{AccountTypeCodex, WireResponses, FormatOpenAI, false, false},
		{AccountTypeAntigravity, WireGemini, FormatUnknown, false, true},
		{AccountTypeClaude, WireAnthropic, FormatClaude, true, false},
		{AccountTypeZAI, WireAnthropic, FormatClaude, true, false},
		{AccountTypeGemini, WireGemini, FormatUnknown, false, false},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			caps := capabilitiesFor(test.provider)
			if caps.WireFormat != test.wire || providerTargetFormat(test.provider) != test.target || caps.SupportsFullHistoryHandoff != test.full || caps.SupportsNativeReplay != test.replay {
				t.Fatalf("capabilities=%+v target=%s", caps, providerTargetFormat(test.provider))
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
