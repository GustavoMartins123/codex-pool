package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type ProviderErrorClass string

const (
	ProviderErrorUnknown   ProviderErrorClass = "unknown"
	ProviderErrorQuota     ProviderErrorClass = "quota"
	ProviderErrorSession   ProviderErrorClass = "session"
	ProviderErrorContext   ProviderErrorClass = "context"
	ProviderErrorProtocol  ProviderErrorClass = "protocol"
	ProviderErrorCapacity  ProviderErrorClass = "capacity"
	ProviderErrorAuth      ProviderErrorClass = "auth"
	ProviderErrorPolicy    ProviderErrorClass = "policy"
	ProviderErrorTransient ProviderErrorClass = "transient"
)

type ProviderError struct {
	Class      ProviderErrorClass
	StatusCode int
	Retryable  bool
	ResetAt    time.Time
	Message    string
}

// collectProviderErrorText includes structured ErrorInfo reasons as well as
// human-readable messages. The HTTP status and RESOURCE_EXHAUSTED alone do
// not establish a quota failure.
func collectProviderErrorText(value any, parts *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "message", "reason", "status", "error", "code":
				if text, ok := child.(string); ok {
					*parts = append(*parts, strings.ToLower(text))
				}
			}
			collectProviderErrorText(child, parts)
		}
	case []any:
		for _, child := range typed {
			collectProviderErrorText(child, parts)
		}
	}
}

func classifyAntigravityError(status int, body []byte) ProviderError {
	result := ProviderError{Class: ProviderErrorUnknown, StatusCode: status, Message: strings.TrimSpace(string(body))}
	var decoded any
	var parts []string
	if json.Unmarshal(body, &decoded) == nil {
		collectProviderErrorText(decoded, &parts)
	} else {
		parts = append(parts, strings.ToLower(string(body)))
	}
	semantic := strings.Join(parts, " ")
	contains := func(needles ...string) bool {
		for _, needle := range needles {
			if strings.Contains(semantic, needle) {
				return true
			}
		}
		return false
	}
	switch {
	case status == http.StatusUnauthorized || contains("unauthenticated", "invalid credential", "invalid token"):
		result.Class = ProviderErrorAuth
	case contains("invalid session", "session mismatch", "session expired", "session not found", "invalid session identifier"):
		result.Class, result.Retryable = ProviderErrorSession, true
	case contains("context mismatch", "context window", "context length", "context too long", "context exceeded"):
		result.Class, result.Retryable = ProviderErrorContext, true
	case contains("thought signature", "thoughtsignature", "invalid signature", "malformed function call", "invalid argument"):
		result.Class = ProviderErrorProtocol
	case contains("no capacity", "capacity exhausted", "overloaded", "server busy"):
		result.Class, result.Retryable = ProviderErrorCapacity, true
	case contains("policy violation", "terms of service", "safety policy", "permission denied"):
		result.Class = ProviderErrorPolicy
	case contains("rate_limit_exceeded", "rate limit exceeded", "quota_exceeded", "quota exhausted", "quota limit", "resource_exhausted: quota", "rate limited", "too many requests"):
		result.Class, result.Retryable = ProviderErrorQuota, true
		result.ResetAt, _ = parseAntigravityRetry(body, time.Now())
	case status >= 500:
		result.Class, result.Retryable = ProviderErrorTransient, true
	}
	return result
}
