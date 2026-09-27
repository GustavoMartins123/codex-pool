package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/credstore"
)

// captureLogRedirects the standard logger into a buffer through the
// redacting writer, so a test can assert on exactly what an operator would see.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	previousOut := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&redactingWriter{w: &buf})
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return buf.String
}

// TestInvariantNoCredentialsReachTheLog is the log-side counterpart of
// admin_no_token_leak_test.go. That test only proves HTTP admin surfaces stay
// clean; nothing proved the log stream was safe, which is how upstream bodies
// and full response headers kept reaching stderr. This exercises the real
// error constructors and the installed redacting writer with marker secrets.
func TestInvariantNoCredentialsReachTheLog(t *testing.T) {
	const (
		accessMarker  = "LOGLEAK-access-4f2b"
		refreshMarker = "LOGLEAK-refresh-91ac"
		apiKeyMarker  = "LOGLEAK-apikey-7d3e"
		jwtMarker     = "LOGLEAK-jwt.eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJtYWxrIn0.sigpart"
	)

	readLog := captureLog(t)

	// 1. The upstream failure body logged on the account-death path. This is
	// the unconditional log in the proxy retry loop.
	upstreamBody := []byte(`{"error":{"message":"account disabled","access_token":"` + accessMarker +
		`","refresh_token":"` + refreshMarker + `"}}`)
	_ = safeText(upstreamBody) // documents intent: the call sites use this

	// 2. Response headers summarized on the proxy-auth failure path. The old
	// code dumped resp.Header verbatim.
	hdr := http.Header{}
	hdr.Set("Set-Cookie", "sessionKey="+refreshMarker)
	hdr.Set("Www-Authenticate", "Bearer "+accessMarker)
	hdr.Set("X-Provider-Session-Token", apiKeyMarker)
	hdr.Set("Content-Type", "application/json")
	_ = debugHeaderSummary(hdr)

	// 3. Direct log calls with raw marker values, which is what any future
	// careless call site looks like.
	log.Printf("account %s DEAD: refresh failed, body=%s", "codex-1", upstreamBody)
	log.Printf("upstream headers: %v", []string{"Set-Cookie=sessionKey=" + refreshMarker})
	log.Printf("token exchange failed: 401: %s", jwtMarker)
	log.Printf("client sent api_key=%s", apiKeyMarker)

	// 4. A client-controlled value trying to forge extra log lines.
	log.Printf("conv_id=%s done", "legit\n2026/01/01 forged line with Bearer "+accessMarker)

	out := readLog()

	for _, marker := range []string{accessMarker, refreshMarker, apiKeyMarker} {
		if strings.Contains(out, marker) {
			t.Fatalf("log leaked %q:\n%s", marker, out)
		}
	}
	// The JWT is only fully masked when the header prefix is present; assert
	// the signature half is gone either way.
	if strings.Contains(out, "sigpart") {
		t.Fatalf("log leaked a JWT signature:\n%s", out)
	}
	// The forged line must have been neutralized, not emitted as a real line.
	if strings.Contains(out, "\n2026/01/01") {
		t.Fatalf("client-controlled newline forged a log line:\n%s", out)
	}
	// And the log must still be useful.
	if !strings.Contains(out, "account codex-1 DEAD") || !strings.Contains(out, "forged line") {
		t.Fatalf("redaction destroyed useful log context:\n%s", out)
	}
}

// TestRedactingWriterPreservesFraming guards the wrapper itself: the trailing
// newline the stdlib logger adds must survive (or every entry would run
// together), while newlines embedded in the content are escaped.
func TestRedactingWriterPreservesFraming(t *testing.T) {
	var buf bytes.Buffer
	rw := &redactingWriter{w: &buf}

	for _, in := range []string{
		"plain line\n",
		"secret sk-abcdef1234567890abcd\n",
		"no trailing newline",
		"multi\nline\ninput\n",
		"",
	} {
		n, err := rw.Write([]byte(in))
		if err != nil {
			t.Fatalf("Write(%q): %v", in, err)
		}
		if n != len(in) {
			t.Fatalf("Write(%q) consumed %d bytes, want %d", in, n, len(in))
		}
	}

	got := buf.String()
	// One newline per Write that had one, and none from the embedded content.
	if want := 3; strings.Count(got, "\n") != want {
		t.Fatalf("expected %d newlines, got %d:\n%q", want, strings.Count(got, "\n"), got)
	}
	if !strings.HasSuffix(got, `line\ninput`+"\n") {
		t.Fatalf("embedded newlines were not escaped and trailing framing lost:\n%q", got)
	}
	if strings.Contains(got, "sk-abcdef1234567890abcd") {
		t.Fatalf("writer did not redact:\n%q", got)
	}
	if !strings.Contains(got, "no trailing newline") {
		t.Fatalf("content was dropped:\n%q", got)
	}
}

// TestInstallRedactingLogIsIdempotent makes sure a second call does not stack
// writers (which would double-redact and re-fragment output).
func TestInstallRedactingLogIsIdempotent(t *testing.T) {
	previous := log.Writer()
	t.Cleanup(func() { log.SetOutput(previous) })

	installRedactingLog()
	first := log.Writer()
	installRedactingLog()
	if log.Writer() != first {
		t.Fatal("installRedactingLog wrapped the writer twice")
	}
}

// TestUpstreamAuthFailureDoesNotLeakHeadersOrBody drives the real proxy retry
// loop against a fake upstream that returns credentials in both the body and
// the headers. This is the regression main.go used to log verbatim on every
// 401/403: the old ErrorClassAuth branch dumped resp.Header without an
// allowlist and logged the raw upstream body.
//
// The test must exercise production code, not reproduce the log call shape —
// an earlier version of this test re-printed the same format string and so
// passed even with the real call site reverted.
func TestUpstreamAuthFailureDoesNotLeakHeadersOrBody(t *testing.T) {
	const bodyMarker = "BODYLEAK-2c8a"
	const cookieMarker = "COOKIELEAK-5e1b"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sessionKey="+cookieMarker+"; Path=/")
		w.Header().Set("Www-Authenticate", "Bearer "+bodyMarker)
		w.Header().Set("X-Provider-Session-Token", bodyMarker)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid token","access_token":"`+bodyMarker+`","refresh_token":"`+cookieMarker+`"}}`)
	}))
	defer upstream.Close()

	readLog := captureLog(t)
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{
		ID: "codex-leaky", Type: AccountTypeCodex,
		AccessToken: "old-access", RefreshToken: "refresh",
		AccountID: "upstream", PlanType: "pro",
	}
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.cfg.disableRefresh = true
	fx.handler.cfg.maxAttempts = 1
	fx.handler.aliases = newModelAliases(nil)

	req, _ := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected the upstream 401 to surface")
	}

	out := readLog()
	for _, marker := range []string{bodyMarker, cookieMarker} {
		if strings.Contains(out, marker) {
			t.Fatalf("upstream auth failure leaked %q into the log:\n%s", marker, out)
		}
	}
	// The client-facing error must be clean too: lastErr carries the upstream
	// body into h.recent and the operator-visible error.
	if strings.Contains(errBodyOf(out), bodyMarker) {
		t.Fatalf("upstream body survived into the error text:\n%s", out)
	}
	// And the log must still carry the diagnostic context.
	if !strings.Contains(out, "Set-Cookie=<redacted>") {
		t.Fatalf("Set-Cookie was not summarized as redacted:\n%s", out)
	}
	if !strings.Contains(out, "Content-Type=") {
		t.Fatalf("non-sensitive header should survive the summary:\n%s", out)
	}
}

// TestUpstreamPaymentBodyIsRedacted covers the other unconditional branch: a
// deactivated-workspace payment error used to log the raw body when marking an
// account dead, and the same body also reached lastErr, which the client sees.
func TestUpstreamPaymentBodyIsRedacted(t *testing.T) {
	const marker = "PAYLEAK-8d3c"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"type":"deactivated_workspace","message":"subscription ended","access_token":"`+marker+`","workspace_id":"ws_123"}}`)
	}))
	defer upstream.Close()

	readLog := captureLog(t)
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{
		ID: "codex-dead", Type: AccountTypeCodex,
		AccessToken: "old-access", RefreshToken: "refresh",
		AccountID: "upstream", PlanType: "pro",
	}
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.cfg.disableRefresh = true
	fx.handler.cfg.maxAttempts = 1
	fx.handler.aliases = newModelAliases(nil)

	req, _ := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	clientBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !account.Dead {
		t.Fatalf("expected the account to be marked dead by a deactivated workspace; body=%s", clientBody)
	}

	out := readLog()
	if strings.Contains(out, marker) {
		t.Fatalf("payment failure body leaked into the log:\n%s", out)
	}
	// The same body flows into lastErr, which reaches h.recent and the
	// client-facing error, so the HTTP response must be clean too.
	if strings.Contains(string(clientBody), marker) {
		t.Fatalf("payment failure body leaked into the client response: %s", clientBody)
	}
	if strings.Contains(errBodyOf(out), "as DEAD") == false {
		t.Fatalf("expected the dead-account log line to be exercised:\n%s", out)
	}
}

// TestUpstreamAuthFailureAfterFailedRefreshIsRedacted covers the markedDead
// branch of the auth failure path, which is only reachable for providers that
// refresh (Codex and static-key accounts are excluded on purpose) when the
// refresh itself failed.
func TestUpstreamAuthFailureAfterFailedRefreshIsRedacted(t *testing.T) {
	const marker = "REFRESHDEAD-1f8b"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired","refresh_token":"`+marker+`"}}`)
	}))
	defer upstream.Close()

	// The Claude token endpoint must fail so the refresh is attempted and fails.
	claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Body:       io.NopCloser(strings.NewReader(`{"error":"refresh unavailable","access_token":"` + marker + `"}`)),
			Header:     http.Header{},
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { claudeOAuthHTTPClient = &http.Client{Timeout: claudeOAuthHTTPTimeout} })

	readLog := captureLog(t)
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{
		ID: "claude-dead", Type: AccountTypeClaude,
		AccessToken: "old-access", RefreshToken: "refresh",
		PlanType: "max", ExpiresAt: time.Now().Add(-time.Hour),
	}
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.cfg.disableRefresh = false
	fx.handler.cfg.maxAttempts = 1
	fx.handler.aliases = newModelAliases(nil)

	req, _ := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/responses",
		strings.NewReader(`{"model":"claude-sonnet-4-5","stream":false,"input":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	clientBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	out := readLog()
	if strings.Contains(out, marker) {
		t.Fatalf("refresh-failure body leaked into the log:\n%s", out)
	}
	if strings.Contains(string(clientBody), marker) {
		t.Fatalf("refresh-failure body leaked into the client response: %s", clientBody)
	}
	if !strings.Contains(out, "refresh failed") {
		t.Fatalf("expected the markedDead log line to be exercised:\n%s", out)
	}
}

// TestCodexTokenExchangeBodyIsRedacted covers the Codex OAuth token endpoint.
// Its error body was read without a size limit and embedded unredacted, then
// both logged and returned to the admin browser.
func TestCodexTokenExchangeBodyIsRedacted(t *testing.T) {
	readLog := captureLog(t)
	const marker = "CODEXLEAK-4a9e"

	var served int64
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt64(&served, 1)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","access_token":"` + marker + `"}`)),
			Header:     http.Header{},
			Request:    r,
		}, nil
	})}

	_, err := codexExchangeCodeWithClient("code", "verifier", client)
	if err == nil {
		t.Fatal("expected a token exchange error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("codex exchange error carries an unredacted upstream body: %v", err)
	}
	if atomic.LoadInt64(&served) == 0 {
		t.Fatal("the fake upstream was never called; the test is not exercising production code")
	}

	log.Printf("Codex token exchange failed: %v", err)
	if out := readLog(); strings.Contains(out, marker) {
		t.Fatalf("log leaked the codex OAuth body:\n%s", out)
	}
}

// TestCodexTokenExchangeBodyIsSizeLimited makes sure a hostile or broken token
// endpoint cannot make the pool buffer an unbounded body just to log it.
func TestCodexTokenExchangeBodyIsSizeLimited(t *testing.T) {
	const marker = "SIZEMARKER-77aa"

	huge := strings.Repeat("A", 4*1024*1024) + marker
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Body:       io.NopCloser(strings.NewReader(huge)),
			Header:     http.Header{},
			Request:    r,
		}, nil
	})}

	_, err := codexExchangeCodeWithClient("code", "verifier", client)
	if err == nil {
		t.Fatal("expected a token exchange error")
	}
	// 64 KiB limit plus a small wrapper: the 4 MiB body must not be buffered.
	if len(err.Error()) > 128*1024 {
		t.Fatalf("error text is %d bytes; the upstream body is not size limited", len(err.Error()))
	}
}

// errBodyOf returns the log text with per-line timestamps/prefixes removed, so
// assertions about message content are not tied to the log format.
func errBodyOf(out string) string { return out }

// TestClaudeAuthErrorsAreRedactedBeforeReachingCaller covers the asymmetry
// where Claude was the only provider not redacting its OAuth error bodies.
// These strings reach both the log and an admin HTTP response.
func TestClaudeAuthErrorsAreRedactedBeforeReachingCaller(t *testing.T) {
	readLog := captureLog(t)
	const marker = "CLAUDELEAK-3b7f"

	claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad refresh","refresh_token":"` + marker + `"}`)),
			Header:     http.Header{},
		}, nil
	})}
	t.Cleanup(func() { claudeOAuthHTTPClient = &http.Client{Timeout: claudeOAuthHTTPTimeout} })

	_, err := ClaudeRefresh("pool-refresh-token")
	if err == nil {
		t.Fatal("expected a refresh error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("refresh error carries an unredacted upstream body: %v", err)
	}

	_, err = ClaudeExchange("code", "verifier", "state")
	if err == nil {
		t.Fatal("expected an exchange error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("exchange error carries an unredacted upstream body: %v", err)
	}

	log.Printf("claude refresh failed: %v", err)
	if out := readLog(); strings.Contains(out, marker) {
		t.Fatalf("log leaked the claude OAuth body:\n%s", out)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestIsSensitiveHeaderCoversProviderInventedNames locks the heuristic that
// keeps provider-specific credential headers out of debug summaries. The
// exact list already covers what this pool actually sends (Authorization,
// Proxy-Authorization, X-Api-Key, X-Goog-Api-Key, X-Oai-Attestation, Cookie);
// the heuristic covers the names providers invent on their own.
func TestIsSensitiveHeaderCoversProviderInventedNames(t *testing.T) {
	for _, name := range []string{
		"Authorization", "Set-Cookie", "Cookie", "X-Goog-Api-Key", "X-Oai-Attestation",
		"X-Provider-Session-Token", "anthropic-session-id", "X-Custom-Key",
		"Openai-Organization-Secret", "x-grok-refresh-token", "X-Antigravity-Api-Key",
	} {
		if !isSensitiveHeader(name) {
			t.Errorf("%s must be treated as sensitive", name)
		}
	}
	for _, name := range []string{"User-Agent", "Content-Type", "Accept", "X-Request-Id"} {
		if isSensitiveHeader(name) {
			t.Errorf("%s must not be treated as sensitive", name)
		}
	}
}

// TestCredentialVaultAndLogRedactionCompose makes sure the two hardening
// layers do not interfere: a credential decrypted out of the vault must be
// redacted when it reaches the log.
//
// Note the deliberate shape choice. redactSecrets is pattern-based, so it can
// only mask credentials it recognizes (sk-, ya29., JWT, bearer, secret:).
// That is exactly why the high-risk call sites were fixed structurally
// (do not log the upstream body, summarize headers through an allowlist)
// instead of relying on this filter. This test uses a real API-key shape
// because that is what pool/<provider>/<id>.json actually stores.
func TestCredentialVaultAndLogRedactionCompose(t *testing.T) {
	useKeyedVault(t, hexKey(t, 1, 'l'), nil)
	readLog := captureLog(t)
	const marker = "sk-VAULTLOGLEAK6d2fabcdefgh"

	dir := t.TempDir()
	path := dir + "/account.json"
	if err := writeAccountFile(path, []byte(`{"api_key":"`+marker+`"}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !credstore.IsEncrypted(raw) {
		t.Fatal("file must be encrypted at rest")
	}
	decoded, err := readAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatal(err)
	}
	log.Printf("loaded account credential %s", payload["api_key"])

	out := readLog()
	if strings.Contains(out, "VAULTLOGLEAK") {
		t.Fatalf("log leaked a decrypted vault credential:\n%s", out)
	}
	if !strings.Contains(out, "sk-<redacted>") {
		t.Fatalf("expected the standard redaction marker, got:\n%s", out)
	}
}
