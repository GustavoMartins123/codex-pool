package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/logredact"
)

func TestProxyRuntimeDrivesRequestLogsAndLatency(t *testing.T) {
	const requestID = "clock-test"
	const secret = "runtime-test-secret"
	t.Setenv("POOL_JWT_SECRET", secret)
	start := time.Date(2037, 1, 2, 3, 4, 5, 0, time.UTC)
	now := start
	handler := preflightHandler(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		now = start.Add(250 * time.Millisecond)
		return preflightReply(), nil
	}))
	handler.cfg.debug.Store(true)
	var output bytes.Buffer
	routes := &proxyRoutes{
		proxyHandler: handler,
		runtime: proxyRuntime{
			now:    func() time.Time { return now },
			logger: slog.New(logredact.NewTextHandler(&output)),
		},
	}
	app := routes.newHTTPApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
	r.Header.Set("X-Request-ID", requestID)
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken(secret, "runtime-user"))
	r.Header.Set("Content-Type", "application/json")
	if err := prepareTestPoolCredential(t, handler, r); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	trace, ok := handler.routeTraces.Get(requestID)
	if !ok || !trace.Timestamp.Equal(start) || trace.DurationMs != 250 {
		t.Fatalf("route trace = %+v, want timestamp %s and 250 ms", trace, start)
	}
	logs := output.String()
	for _, want := range []string{"time=2037-01-02T03:04:05", "request_id=clock-test",
		"proxy request completed", "duration_ms=250"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log missing %q: %s", want, logs)
		}
	}
}
