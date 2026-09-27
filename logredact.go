package main

import (
	"regexp"
)

// Centralized secret redaction. Everything that leaves the process in
// textual form — log samples, trace files, error strings built from upstream
// bodies — funnels through safeText, which applies redactSecrets. New log or
// error paths must keep using safeText (or call redactSecrets directly) so
// credentials never reach logs, metrics, or admin responses.

var logSecretRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization header values.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9\-._~+/=]{12,}`), "$1 <redacted>"},
	// OpenAI-style API keys.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9\-_]{12,}`), "sk-<redacted>"},
	// JWTs (header.payload.signature).
	{regexp.MustCompile(`\beyJ[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{8,}\.[A-Za-z0-9\-_]{4,}`), "<redacted-jwt>"},
	// JSON secret assignments: "access_token": "...".
	{regexp.MustCompile(`(?i)("(?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|session[_-]?token|password|secret|authorization)"\s*:\s*")[^"]{8,}(")`), "${1}<redacted>${2}"},
	// Key/value secret assignments without JSON quotes.
	{regexp.MustCompile(`(?i)\b((?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|password|secret|authorization)["']?\s*[:=]\s*["']?)[A-Za-z0-9\-._~+/]{12,}`), "${1}<redacted>"},
}

// redactSecrets masks common credential shapes in a string. Benign text is
// left untouched; only the secret value is replaced.
func redactSecrets(s string) string {
	for _, rule := range logSecretRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	return s
}
