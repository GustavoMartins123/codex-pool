package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-pool-proxy/internal/pricing"
)

func TestCodexJSONResponsesRecordUsage(t *testing.T) {
	const secret = "codex-json-usage-secret"
	const model = "gpt-5.5"
	t.Setenv("POOL_JWT_SECRET", secret)

	const usage = `{"input_tokens":8,"output_tokens":4,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}`
	directBody := "{\n" + `"id":"resp-usage","model":"` + model + `","output":[{"type":"message","content":[{"type":"output_text","text":"` +
		strings.Repeat("x", 32<<10) + `"}]}],"usage":` + usage + "\n}"
	terminalEvent := `data: {"type":"response.completed","response":{"id":"resp-usage","model":"` + model + `","usage":` + usage + `}}` + "\n\n"

	for _, tc := range []struct {
		name, contentType, body string
		spool                   bool
	}{
		{name: "direct JSON", contentType: "application/json", body: directBody},
		{name: "direct JSON with spooled request", contentType: "application/json", body: directBody, spool: true},
		{name: "SSE buffered as JSON", contentType: "text/event-stream", body: terminalEvent + terminalEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := preflightHandler(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				defer r.Body.Close()
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {tc.contentType}},
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			}))
			h.cfg.maxSpoolBodyBytes = 1 << 20
			h.store = testUsageStore(t)
			h.duckAnalytics = testDuckCosts(t, h.store)
			prices, err := pricing.New()
			if err != nil {
				t.Fatal(err)
			}
			h.pricing = prices

			stream := "false"
			if tc.spool {
				stream = "true"
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hello","stream":`+stream+`}`))
			req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken(secret, "json-user"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-ID", "codex-json-usage")
			if tc.spool {
				req.ContentLength = -1
			}
			w := httptest.NewRecorder()
			testPoolServeHTTP(t, h, w, req)
			var reply struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.ID != "resp-usage" || reply.Model != model {
				t.Fatalf("response was not the expected JSON, status=%d id=%q model=%q", w.Code, reply.ID, reply.Model)
			}
			if tc.contentType == "application/json" && w.Body.String() != directBody {
				t.Fatal("proxy changed the direct JSON body")
			}

			drainTestCosts(t, h.duckAnalytics)
			daily, err := h.duckAnalytics.getModelDailyUsage(1)
			if err != nil || len(daily) != 1 {
				t.Fatalf("DuckDB model usage = %+v, err = %v", daily, err)
			}
			row := daily[0]
			if row.AccountType != "codex" || row.Model != model || row.RequestCount != 1 ||
				row.InputTokens != 8 || row.CachedTokens != 3 || row.CacheCreationTokens != 2 ||
				row.OutputTokens != 4 || row.ReasoningTokens != 1 || row.CostUSD <= 0 {
				t.Fatalf("incorrect Codex usage or cost: %+v", row)
			}
		})
	}
}
