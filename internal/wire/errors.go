package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
	out, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    claudeType,
			"message": message,
		},
	})
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

// ClaudeErrorToOpenAI converts a Claude error response to OpenAI format.
// Claude: {"type":"error","error":{"type":"...","message":"..."}}
// OpenAI: {"error":{"message":"...","type":"...","code":null}}

func ClaudeErrorToOpenAI(parsed map[string]any, original []byte) []byte {
	if t, _ := parsed["type"].(string); t != "error" {
		return original
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		return original
	}
	message, _ := errObj["message"].(string)
	errType, _ := errObj["type"].(string)
	if errType == "" {
		errType = "api_error"
	}
	oaiType := mapClaudeErrorTypeToOAI(errType)
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    oaiType,
			"code":    nil,
		},
	})
	if err != nil {
		return original
	}
	return out
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
