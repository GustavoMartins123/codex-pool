package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TranslateChatMessagesError converts a native error once, from its upstream
// protocol to the client's protocol. Unrecognized envelopes are rejected.
func TranslateChatMessagesError(body []byte, direction TranslateDirection) ([]byte, error) {
	source, err := decodeJSONObject(body, "Anthropic/OpenAI Chat error")
	if err != nil {
		return nil, err
	}
	details, _ := source["error"].(map[string]any)
	message, ok := details["message"].(string)
	if !ok {
		return nil, fmt.Errorf("upstream error envelope has no message")
	}
	kind, _ := details["type"].(string)
	switch direction {
	case TranslateOAIToClaude:
		return json.Marshal(anthropicErrorEnvelope(mapOAIErrorTypeToClaude(kind), message))
	case TranslateClaudeToOAI:
		if source["type"] != "error" {
			return nil, fmt.Errorf("invalid Anthropic error envelope")
		}
		return json.Marshal(map[string]any{"error": map[string]any{
			"type": mapClaudeErrorTypeToOAI(kind), "message": message, "code": nil,
		}})
	default:
		return nil, fmt.Errorf("unsupported Anthropic/OpenAI Chat error translation direction: %d", direction)
	}
}

func anthropicErrorEnvelope(kind, message string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}}
}

// OpenAIErrorToClaude converts an OpenAI error response to Claude format.
// OpenAI: {"error":{"message":"...","type":"...","code":"..."}}
// Claude: {"type":"error","error":{"type":"...","message":"..."}}
func OpenAIErrorToClaude(parsed map[string]any, original []byte) []byte {
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		return original
	}
	message, _ := errObj["message"].(string)
	errType, _ := errObj["type"].(string)
	if errType == "" {
		errType = "api_error"
	}
	// Map OpenAI error types to Claude error types
	claudeType := mapOAIErrorTypeToClaude(errType)
	out, err := json.Marshal(anthropicErrorEnvelope(claudeType, message))
	if err != nil {
		return original
	}
	return out
}

// ErrorToClaudeFormat converts a Codex/OpenAI error response to Claude
// Messages API error format. Handles both OpenAI-style errors
// ({"error":{"message":"..."}}) and Codex-style errors ({"detail":"..."}).

func ErrorToClaudeFormat(body []byte, statusCode int) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON - wrap the raw text as a Claude error
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = fmt.Sprintf("upstream error: HTTP %d", statusCode)
		}
		out, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": msg,
			},
		})
		return out
	}

	// Try OpenAI error format: {"error":{"message":"...","type":"..."}}
	if _, ok := parsed["error"].(map[string]any); ok {
		return OpenAIErrorToClaude(parsed, body)
	}

	// Try Codex detail format: {"detail":"..."}
	if detail, ok := parsed["detail"].(string); ok && detail != "" {
		errType := "api_error"
		if statusCode == 400 {
			errType = "invalid_request_error"
		} else if statusCode == 429 {
			errType = "rate_limit_error"
		}
		out, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errType,
				"message": detail,
			},
		})
		return out
	}

	// Unknown format, return as-is
	return body
}

func mapOAIErrorTypeToClaude(oaiType string) string {
	switch oaiType {
	case "invalid_request_error":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	case "insufficient_quota", "billing_hard_limit_reached":
		return "overloaded_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "server_error", "service_unavailable_error":
		return "api_error"
	default:
		return "api_error"
	}
}

func mapClaudeErrorTypeToOAI(claudeType string) string {
	switch claudeType {
	case "invalid_request_error":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	case "rate_limit_error":
		return "rate_limit_error"
	case "overloaded_error":
		return "server_error"
	case "not_found_error":
		return "invalid_request_error"
	default:
		return "server_error"
	}
}
