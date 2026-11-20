package logredact

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	cases := []struct{ name, in, wantSub string }{
		{"bearer header", `Authorization: Bearer eyJhbGciOi.eyJzdWIi.sigpart`, "Bearer <redacted>"},
		{"basic header", `proxy-authorization: Basic dXNlcjpwYXNzd29yZA==`, "Basic <redacted>"},
		{"openai key", `key=sk-proj-AbCdEf1234567890123456`, "sk-<redacted>"},
		{"jwt", `token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4`, "<redacted-jwt>"},
		{"json access token", `{"access_token":"ya29.a0AfH6SMBx1234567890"}`, `{"access_token":"<redacted>"}`},
		{"json refresh token", `{"refresh_token":"1//0abcDEF1234567890"}`, `{"refresh_token":"<redacted>"}`},
		{"kv password", `password=SuperSecretValue12345`, "password=<redacted>"},
		{"query api key", `url?api_key=AIzaSyABCDEF1234567890`, "api_key=<redacted>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("Redact(%q) = %q, want substring %q", tc.in, got, tc.wantSub)
			}
			if strings.Contains(got, "SuperSecretValue12345") || strings.Contains(got, "AbCdEf1234567890123456") {
				t.Fatalf("secret survived redaction: %q", got)
			}
		})
	}
}

func TestTextHandlerRedactsMessageAndAttributes(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewTextHandler(&output)).With("request_id", "test-request")
	logger.Info(`upstream body {"access_token":"example-access-token"}`,
		"api_key", "example-key-0123456789",
		"error", errors.New(`{"password":"example-password"}`))

	record := output.String()
	for _, secret := range []string{"example-access-token", "example-key-0123456789", "example-password"} {
		if strings.Contains(record, secret) {
			t.Errorf("credential survived: %s", record)
		}
	}
	if !strings.Contains(record, "request_id=test-request") || strings.Count(record, "<redacted>") != 3 {
		t.Errorf("unexpected log record: %s", record)
	}
}

func TestRedactSecretsKeepsBenignText(t *testing.T) {
	benign := []string{
		`{"error":{"code":"token_expired","message":"upstream token expired"}}`,
		`data: {"type":"response.completed","sequence_number":42}`,
		`claude usage fetch codex-1: 5hr=42.0% 7day=12.3%`,
		`route selected account=codex-2 score=1.37 reason=conversation_pin`,
	}
	for _, s := range benign {
		if got := Redact(s); got != s {
			t.Errorf("Redact changed benign text:\n in: %s\ngot: %s", s, got)
		}
	}
}

func TestRedactingWriterPreservesFraming(t *testing.T) {
	var buf bytes.Buffer
	rw := NewWriter(&buf)

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

	Install()
	first := log.Writer()
	Install()
	if log.Writer() != first {
		t.Fatal("Install wrapped the writer twice")
	}
}
