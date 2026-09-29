package main

// Streaming relay invariant: once any byte of an upstream response has been
// delivered to the client, a mid-stream failure must never splice content
// from a different upstream account into the same HTTP response.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// TestAuditStreamUpstreamDeathDoesNotSpliceFallbackContent
// Invariant under attack: upstream account A streams part of an SSE
// response and then the connection dies. The proxy may retry the request on
// account B for a FRESH response, but B's bytes must never be appended to
// the same client response that already carries A's bytes — the client
// would see two interleaved/concatenated event streams from different
// conversations.
func TestAuditStreamUpstreamDeathDoesNotSpliceFallbackContent(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	base, _ := url.Parse("https://chatgpt.com/backend-api")
	ordinary := &Account{Type: AccountTypeCodex, ID: "ordinary", AccessToken: "ordinary-token", AccountID: "acct_ordinary", PlanType: "pro"}
	backup := &Account{Type: AccountTypeCodex, ID: "backup", AccessToken: "backup-token", AccountID: "acct_backup", PlanType: "pro", Usage: UsageSnapshot{SecondaryUsedPercent: 0.10}}

	ordinaryPrefix := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_ordinary","status":"in_progress"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"partial reply from ordinary"}`,
		``,
		``,
	}, "\n")

	backupFull := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_backup","status":"in_progress"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_backup","status":"completed"}}`,
		``,
		``,
	}, "\n")

	var dialed []string
	h := &proxyHandler{
		cfg: &config{maxAttempts: 3, maxInMemoryBodyBytes: 1 << 20, requestTimeout: 5 * time.Second, streamTimeout: 5 * time.Second},
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			accountID := req.Header.Get("ChatGPT-Account-ID")
			dialed = append(dialed, accountID)
			if accountID == "acct_backup" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(bytes.NewBufferString(backupFull)),
				}, nil
			}
			// Ordinary account: SSE starts, then the connection dies.
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(io.MultiReader(strings.NewReader(ordinaryPrefix), iotest.ErrReader(io.ErrUnexpectedEOF))),
			}, nil
		}),
		refreshTransport: http.DefaultTransport,
		pool:             newPoolState([]*Account{ordinary, backup}, false),
		registry:         NewProviderRegistry(NewCodexProvider(base, base, base), NewClaudeProvider(base), NewGeminiProvider(base, base)),
		metrics:          newMetrics(),
		recent:           newRecentErrors(5),
	}

	reqBody := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user-no-splice"))
	req.Header.Set("session_id", "no-splice-audit")
	rr := httptest.NewRecorder()
	testPoolServeHTTP(t, h, rr, req)

	body := rr.Body.String()
	hasOrdinary := strings.Contains(body, "partial reply from ordinary")
	hasBackup := strings.Contains(body, "resp_backup")
	if hasOrdinary && hasBackup {
		t.Fatalf("upstream splice: client response contains content from both upstreams (ordinary partial + backup retry): %.400s", body)
	}
	if !hasOrdinary && !hasBackup {
		t.Fatalf("client received neither upstream's stream: status=%d body=%.200s", rr.Code, body)
	}
}
