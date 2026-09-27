package main

import (
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
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
	// Google API keys (AIzaSy...).
	{regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{10,}`), "AIza<redacted>"},
	// Google OAuth access tokens (ya29.*).
	{regexp.MustCompile(`\bya29\.[0-9A-Za-z\-_]{8,}`), "ya29.<redacted>"},
	// Google OAuth refresh tokens (1//...).
	{regexp.MustCompile(`\b1//[0-9A-Za-z\-_]{12,}`), "1//<redacted>"},
	// Pool refresh tokens minted by this proxy (poolrt_<id>_<ts>_<nonce>_<sig>).
	{regexp.MustCompile(`\bpoolrt_[0-9A-Za-z_\-]{8,}`), "poolrt_<redacted>"},
	// Anthropic and pool-generated Claude bearer tokens.
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{8,}`), "sk-<redacted>"},
	// Cloudflare and session cookies that carry identity.
	{regexp.MustCompile(`(?i)\b((?:__cf_bm|cf_clearance|__Host-[A-Za-z0-9_\-]*session[A-Za-z0-9_\-]*|__Secure-[A-Za-z0-9_\-]*session[A-Za-z0-9_\-]*|sessionKey|session_token)["']?\s*[:=]\s*["']?)[A-Za-z0-9\-._~+/]{8,}`), "${1}<redacted>"},
	// Credentials embedded in a URL (https://user:pass@host).
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}<redacted>@"},
}

// redactSecrets masks common credential shapes in a string. Benign text is
// left untouched; only the secret value is replaced.
func redactSecrets(s string) string {
	for _, rule := range logSecretRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	return s
}

// redactingWriter is the last line of defense for credentials. The stdlib
// logger has no per-call hook, and the pool has hundreds of log.Printf call
// sites, so wrapping the output stream is the only way to guarantee that a
// future call site cannot print an upstream token. It also neutralizes
// newlines the caller embedded, which would otherwise let client-controlled
// text forge extra log lines.
type redactingWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (rw *redactingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	// The stdlib logger calls Write once per entry and appends a single
	// trailing newline. That trailing newline is framing and must survive;
	// any newline the caller embedded is content, so it is escaped. Without
	// this, a client-controlled value (conversation id, header, query) could
	// forge extra log lines.
	body := p
	trailing := len(body) > 0 && body[len(body)-1] == '\n'
	if trailing {
		body = body[:len(body)-1]
	}
	cleaned := strings.NewReplacer("\r\n", "\\n", "\n", "\\n", "\r", "\\r").Replace(string(body))
	cleaned = redactSecrets(cleaned)

	if _, err := io.WriteString(rw.w, cleaned); err != nil {
		return 0, err
	}
	if trailing {
		if _, err := io.WriteString(rw.w, "\n"); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// installRedactingLog routes the standard logger through redactSecrets,
// preserving whatever destination was configured before. Idempotent.
func installRedactingLog() {
	existing := log.Writer()
	if _, already := existing.(*redactingWriter); already {
		return
	}
	if existing == nil {
		existing = os.Stderr
	}
	log.SetOutput(&redactingWriter{w: existing})
}
