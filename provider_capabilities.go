package main

type WireFormat string

const (
	WireResponses WireFormat = "responses"
	WireAnthropic WireFormat = "anthropic"
	WireGemini    WireFormat = "gemini"
	WireOpenAI    WireFormat = "openai"
	WireUnknown   WireFormat = "unknown"
)

type ProviderCapabilities struct {
	WireFormat                 WireFormat
	RequiresNativeSession      bool
	SupportsForeignHistory     bool
	SupportsToolHistory        bool
	SupportsParallelTools      bool
	SupportsImages             bool
	SupportsReasoning          bool
	SupportsOpaqueReasoning    bool
	SupportsNativeReplay       bool
	SupportsFullHistoryHandoff bool
}

// capabilitiesFor describes protocol behavior used by context handoff and
// routing. Conservative defaults keep unknown providers on a fresh handoff.
func capabilitiesFor(provider AccountType) ProviderCapabilities {
	switch provider {
	case AccountTypeCodex:
		return ProviderCapabilities{
			WireFormat: WireResponses, RequiresNativeSession: true,
			SupportsForeignHistory: true, SupportsToolHistory: true,
			SupportsParallelTools: true, SupportsImages: true,
			SupportsReasoning: true, SupportsOpaqueReasoning: true,
		}
	case AccountTypeAntigravity:
		return ProviderCapabilities{
			WireFormat: WireGemini, RequiresNativeSession: true,
			SupportsForeignHistory: true, SupportsToolHistory: true,
			SupportsParallelTools: true, SupportsImages: true,
			SupportsReasoning: true, SupportsOpaqueReasoning: true,
			SupportsNativeReplay: true,
		}
	case AccountTypeClaude:
		return ProviderCapabilities{
			WireFormat: WireAnthropic, SupportsForeignHistory: true,
			SupportsToolHistory: true, SupportsParallelTools: true,
			SupportsImages: true, SupportsReasoning: true,
			SupportsOpaqueReasoning: true, SupportsFullHistoryHandoff: true,
		}
	case AccountTypeZAI:
		return ProviderCapabilities{
			WireFormat: WireAnthropic, SupportsForeignHistory: true,
			SupportsToolHistory: true, SupportsParallelTools: true,
			SupportsReasoning: true, SupportsFullHistoryHandoff: true,
		}
	case AccountTypeGemini:
		return ProviderCapabilities{
			WireFormat: WireGemini, SupportsForeignHistory: true,
			SupportsToolHistory: true, SupportsParallelTools: true,
			SupportsImages: true, SupportsReasoning: true,
		}
	default:
		if isGenericProviderType(provider) {
			return ProviderCapabilities{WireFormat: WireOpenAI, SupportsForeignHistory: true, SupportsToolHistory: true}
		}
		return ProviderCapabilities{WireFormat: WireUnknown}
	}
}
