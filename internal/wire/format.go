package wire

// RequestFormat represents the API format of a request.
type RequestFormat int

const (
	FormatUnknown RequestFormat = iota
	FormatOpenAI                // OpenAI Chat Completions API
	FormatClaude                // Anthropic Messages API
)

func (f RequestFormat) String() string {
	switch f {
	case FormatOpenAI:
		return "openai"
	case FormatClaude:
		return "claude"
	default:
		return "unknown"
	}
}

// TranslateDirection indicates which way we're translating.
type TranslateDirection int

const (
	TranslateNone                   TranslateDirection = iota
	TranslateClaudeToOAI                               // Client sent Claude format, upstream expects OpenAI Chat Completions
	TranslateOAIToClaude                               // Client sent OpenAI format, upstream expects Claude
	TranslateChatToResponses                           // Client sent Chat Completions, upstream expects Responses API
	TranslateCompletionsToResponses                    // Client sent legacy Completions, upstream expects Responses API
	TranslateResponsesToClaude                         // Client sent Responses API, upstream expects Claude Messages
	TranslateClaudeToResponses                         // Client sent Claude format, upstream expects Responses API
	TranslateImagesToResponses                         // Client sent OpenAI Images API, upstream expects Responses image_generation tool
)
