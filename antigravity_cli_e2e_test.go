package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAntigravityCLISmokeChain simulates the Antigravity CLI (agy) in Gemini
// API key mode: requests go to GOOGLE_GEMINI_BASE_URL/v1beta/... with the
// GEMINI_API_KEY as x-goog-api-key, must authenticate through the passport
// client credential, and must reach the Antigravity upstream account.
func TestAntigravityCLISmokeChain(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "agy-e2e-secret")

	// The same account ID is used for the model snapshot and the pool account
	// so the scheduler resolves models through the real registry association
	// (Supports/DiscoveryAvailability), not the cold-start fallback.
	const accountID = "agy-e2e-account"
	antigravityModels.Reset()
	antigravityModels.ReplaceAccount(accountID, AntigravityAccountSnapshot{
		FetchedAt: time.Now(),
		Models: map[string]AntigravityModelInfo{
			"gemini-3.8-flash-high": {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
			"gemini-pro-agent":      {ID: "gemini-pro-agent", DisplayName: "Gemini 3.1 Pro (High)"},
		},
	})
	t.Cleanup(antigravityModels.Reset)

	var mu sync.Mutex
	upstreamCalls := make(map[string]int)
	upstreamAuth := ""
	upstreamModel := ""

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var envelope struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &envelope)
		mu.Lock()
		upstreamCalls[r.URL.Path]++
		upstreamAuth = r.Header.Get("Authorization")
		upstreamModel = envelope.Model
		mu.Unlock()
		if r.URL.Path == "/v1internal:countTokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"response":{"totalTokens":7}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"responseId":"anti-1","candidates":[{"content":{"role":"model","parts":[{"text":"pool-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}`+"\n\n")
	}))
	defer upstream.Close()

	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatalf("newPassportStore: %v", err)
	}
	principal, _, client, _, err := passport.createGuest("operator", "agy tester", "A", nil)
	if err != nil {
		t.Fatalf("createGuest: %v", err)
	}
	identity := principal.ID + "-c-" + client.ID
	apiKey := generateGeminiAPIKey(getPoolJWTSecret(), &PoolUser{ID: identity, Email: "agy@pool.local", PlanType: "pro", CreatedAt: time.Now()})

	upstreamBase, _ := url.Parse(upstream.URL)
	registry := NewProviderRegistry(
		NewCodexProvider(upstreamBase, upstreamBase, nil),
		NewClaudeProvider(upstreamBase),
		NewGeminiProvider(upstreamBase, upstreamBase),
		NewAntigravityProvider(upstreamBase, upstreamBase),
	)
	antiAcc := &Account{Type: AccountTypeAntigravity, ID: accountID, AccessToken: "anti-token", ProjectID: "proj-1", PlanType: "pro"}
	h := &proxyHandler{
		cfg: &config{
			requestTimeout:       5 * time.Second,
			streamTimeout:        5 * time.Second,
			maxInMemoryBodyBytes: 4 << 20,
			maxAttempts:          1,
			disableRefresh:       true,
		},
		transport: http.DefaultTransport,
		pool:      newPoolState([]*Account{antiAcc}, false),
		registry:  registry,
		metrics:   newMetrics(),
		recent:    newRecentErrors(5),
		passport:  passport,
	}
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()

	get := func(path string, key string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("x-goog-api-key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	post := func(path string, key string, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("x-goog-api-key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 1. Model discovery (agy startup / models listing).
	resp := get("/v1beta/models", apiKey)
	modelsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1beta/models status = %d body=%s", resp.StatusCode, modelsBody)
	}
	if !strings.Contains(string(modelsBody), "models/gemini-3.8-flash-high") {
		t.Fatalf("model catalog missing antigravity slug:\n%s", modelsBody)
	}

	// 2. Invalid keys are rejected before reaching the upstream.
	bad := get("/v1beta/models", "AIzaSy-pool-bogus.1.bogus")
	badBody, _ := io.ReadAll(bad.Body)
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus key status = %d body=%s", bad.StatusCode, badBody)
	}

	// 3. generateContent routes to the upstream account with its credentials.
	contents := `{"contents":[{"role":"user","parts":[{"text":"Reply with exactly: pool-ok"}]}]}`
	resp = post("/v1beta/models/gemini-3.1-pro-high:generateContent", apiKey, contents)
	genBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generateContent status = %d body=%s", resp.StatusCode, genBody)
	}
	if !strings.Contains(string(genBody), "pool-ok") {
		t.Fatalf("generateContent response missing upstream text:\n%s", genBody)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("generateContent Content-Type = %q", ct)
	}
	mu.Lock()
	if upstreamCalls["/v1internal:streamGenerateContent"] != 1 || upstreamAuth != "Bearer anti-token" {
		t.Fatalf("upstream call = %v auth = %q", upstreamCalls, upstreamAuth)
	}
	if upstreamModel != "gemini-pro-agent" {
		t.Fatalf("deprecated alias gemini-3.1-pro-high resolved upstream to %q, want gemini-pro-agent", upstreamModel)
	}
	mu.Unlock()

	// 4. streamGenerateContent streams the Gemini SSE passthrough.
	resp = post("/v1beta/models/gemini-3.8-flash-high:streamGenerateContent", apiKey, contents)
	streamBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streamGenerateContent status = %d body=%s", resp.StatusCode, streamBody)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("streamGenerateContent Content-Type = %q", ct)
	}
	if !strings.Contains(string(streamBody), "data: ") || !strings.Contains(string(streamBody), "pool-ok") {
		t.Fatalf("stream response missing SSE payload:\n%s", streamBody)
	}

	// 5. countTokens unwraps the upstream JSON response.
	resp = post("/v1beta/models/gemini-3.8-flash-high:countTokens", apiKey, contents)
	countBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("countTokens status = %d body=%s", resp.StatusCode, countBody)
	}
	if !strings.Contains(string(countBody), "totalTokens") {
		t.Fatalf("countTokens response missing token count:\n%s", countBody)
	}
	mu.Lock()
	if upstreamCalls["/v1internal:countTokens"] != 1 {
		t.Fatalf("upstream calls = %v", upstreamCalls)
	}
	mu.Unlock()
}
