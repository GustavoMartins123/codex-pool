package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAntigravityCLISmokeChain simulates the Antigravity CLI (agy) in Gemini
// API key mode: requests go to GOOGLE_GEMINI_BASE_URL/v1beta/... with the
// GEMINI_API_KEY as x-goog-api-key, must authenticate through the passport
// client credential, and must reach the Antigravity upstream account.
func TestAntigravityCLISmokeChain(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	const jwtSecret = "agy-e2e-secret"

	// The same account ID is used for the model snapshot and the pool account
	// so the scheduler resolves models through the real registry association
	// (Supports/DiscoveryAvailability).
	const accountID = "agy-e2e-account"
	rt := newAntigravityRuntime()
	rt.models.ReplaceAccount(accountID, AntigravityAccountSnapshot{
		FetchedAt:  time.Now(),
		Deprecated: map[string]string{"gemini-3.1-pro-high": "gemini-pro-agent"},
		Models: map[string]AntigravityModelInfo{
			"gemini-3.8-flash-high": {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)"},
			"gemini-pro-agent":      {ID: "gemini-pro-agent", DisplayName: "Gemini 3.1 Pro (High)"},
			"gemini-4-neon-xhigh":   {ID: "gemini-4-neon-xhigh", DisplayName: "Gemini 4 Neon (XHigh)"},
		},
	})

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
	apiKey := generateGeminiAPIKey(jwtSecret, &PoolUser{ID: identity, Email: "agy@pool.local", PlanType: "pro", CreatedAt: time.Now()})

	upstreamBase, _ := url.Parse(upstream.URL)
	registry := NewProviderRegistry(
		NewCodexProvider(upstreamBase, upstreamBase, nil),
		NewClaudeProvider(upstreamBase),
		NewGeminiProvider(upstreamBase, upstreamBase),
		NewAntigravityProvider(upstreamBase, upstreamBase),
	)
	antiAcc := &Account{Type: AccountTypeAntigravity, ID: accountID, AccessToken: "anti-token", google: googleAccountState{ProjectID: "proj-1"}, PlanType: "pro"}
	h := &proxyHandler{
		passportConfig: passportConfig{jwtSecret: jwtSecret},
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
	h.pool.antigravity = rt
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

	// 6. agy sends the base model and carries the tier as a thinking level.
	tiered := `{"contents":[{"role":"user","parts":[{"text":"Reply with exactly: pool-ok"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"XHIGH"}}}`
	resp = post("/v1beta/models/gemini-4-neon:streamGenerateContent?alt=sse", apiKey, tiered)
	tieredBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("base model with thinking level status = %d body=%s", resp.StatusCode, tieredBody)
	}
	mu.Lock()
	if upstreamModel != "gemini-4-neon-xhigh" {
		t.Fatalf("gemini-4-neon with thinkingLevel XHIGH resolved upstream to %q, want gemini-4-neon-xhigh", upstreamModel)
	}
	mu.Unlock()
}

func TestAntigravityTieredModelResolvesThinkingLevel(t *testing.T) {
	rt := newAntigravityRuntime()
	rt.models.ReplaceAccount("acct", AntigravityAccountSnapshot{
		FetchedAt: time.Now(),
		Models: map[string]AntigravityModelInfo{
			"gemini-4-neon-high":     {ID: "gemini-4-neon-high"},
			"gemini-4-neon-xhigh":    {ID: "gemini-4-neon-xhigh"},
			"gemini-3.1-flash-lite":  {ID: "gemini-3.1-flash-lite"},
			"gemini-3.8-flash-low":   {ID: "gemini-3.8-flash-low"},
			"gemini-3.8-flash-high":  {ID: "gemini-3.8-flash-high"},
			"gemini-3.9-pro-preview": {ID: "gemini-3.9-pro-preview"},
		},
	})
	withLevel := func(level string) []byte {
		return []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}}}`)
	}
	cases := []struct {
		name, model string
		body        []byte
		want        string
	}{
		{"base plus level", "gemini-4-neon", withLevel("XHIGH"), "gemini-4-neon-xhigh"},
		{"code assist envelope", "gemini-4-neon", []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}}`), "gemini-4-neon-high"},
		{"prefixed base", "antigravity/gemini-4-neon", withLevel("HIGH"), "antigravity/gemini-4-neon-high"},
		{"already tiered", "gemini-4-neon-high", withLevel("XHIGH"), "gemini-4-neon-high"},
		{"unknown tier", "gemini-4-neon", withLevel("LOW"), "gemini-4-neon"},
		{"no thinking level", "gemini-4-neon", []byte(`{"contents":[]}`), "gemini-4-neon"},
		{"other provider model", "gpt-6.2-astra", withLevel("HIGH"), "gpt-6.2-astra"},
		{"preview build", "gemini-3.1-flash-lite-preview", []byte(`{"contents":[]}`), "gemini-3.1-flash-lite"},
		{"preview build with unknown tier", "gemini-3.1-flash-lite-preview", withLevel("MINIMAL"), "gemini-3.1-flash-lite"},
		{"preview build keeps known tier", "gemini-3.8-flash-preview", withLevel("LOW"), "gemini-3.8-flash-low"},
		{"catalog preview id unchanged", "gemini-3.9-pro-preview", withLevel("HIGH"), "gemini-3.9-pro-preview"},
		{"unknown preview unchanged", "gemini-9-preview", withLevel("HIGH"), "gemini-9-preview"},
	}
	for _, c := range cases {
		if got := antigravityTieredModel(rt, c.model, c.body); got != c.want {
			t.Errorf("%s: antigravityTieredModel(%q) = %q, want %q", c.name, c.model, got, c.want)
		}
	}
	if got := antigravityTieredModel(nil, "gemini-4-neon", withLevel("XHIGH")); got != "gemini-4-neon" {
		t.Errorf("nil runtime changed the model to %q", got)
	}
}

func TestDerivedGeminiConversationIDFollowsTheFirstUserTurn(t *testing.T) {
	turn := func(texts ...string) map[string]any {
		contents := make([]any, 0, len(texts))
		for i, text := range texts {
			role := "user"
			if i%2 == 1 {
				role = "model"
			}
			contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]any{"text": text}}})
		}
		return map[string]any{"contents": contents}
	}
	const path = "/v1beta/models/gemini-4-neon:streamGenerateContent"
	first := derivedGeminiConversationID(path, "user-a", turn("fix the build"))
	if first == "" {
		t.Fatal("no conversation id derived for a native Gemini request")
	}
	if later := derivedGeminiConversationID(path, "user-a", turn("fix the build", "done", "now run the tests")); later != first {
		t.Fatalf("later turn of the same conversation derived %q, want %q", later, first)
	}
	envelope := map[string]any{"model": "gemini-4-neon", "request": turn("fix the build")}
	if got := derivedGeminiConversationID("/v1internal:streamGenerateContent", "user-a", envelope); got != first {
		t.Fatalf("Code Assist envelope derived %q, want %q", got, first)
	}
	if other := derivedGeminiConversationID(path, "user-b", turn("fix the build")); other == first {
		t.Fatal("two callers with the same first turn share a conversation id")
	}
	if other := derivedGeminiConversationID(path, "user-a", turn("write the docs")); other == first {
		t.Fatal("two conversations with different first turns share a conversation id")
	}
	if got := derivedGeminiConversationID("/v1/chat/completions", "user-a", turn("fix the build")); got != "" {
		t.Fatalf("non-Gemini path derived %q", got)
	}
	if got := derivedGeminiConversationID(path, "user-a", map[string]any{"contents": []any{}}); got != "" {
		t.Fatalf("request without a user turn derived %q", got)
	}
}

// TestAntigravityCLIConversationStaysOnItsAccount reproduces the Antigravity
// CLI, which sends no conversation id: a later turn must stay on the account
// that served the first one even when another account now scores higher.
func TestAntigravityCLIConversationStaysOnItsAccount(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	const jwtSecret = "agy-pin-secret"
	rt := newAntigravityRuntime()
	for _, id := range []string{"agy-pin-a", "agy-pin-b"} {
		rt.models.ReplaceAccount(id, AntigravityAccountSnapshot{
			FetchedAt: time.Now(),
			Models:    map[string]AntigravityModelInfo{"gemini-4-neon-xhigh": {ID: "gemini-4-neon-xhigh"}},
		})
	}

	var mu sync.Mutex
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		served = append(served, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"responseId":"anti-1","candidates":[{"content":{"role":"model","parts":[{"text":"pool-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}`+"\n\n")
	}))
	defer upstream.Close()

	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatalf("newPassportStore: %v", err)
	}
	principal, _, client, _, err := passport.createGuest("operator", "agy pin tester", "A", nil)
	if err != nil {
		t.Fatalf("createGuest: %v", err)
	}
	apiKey := generateGeminiAPIKey(jwtSecret, &PoolUser{ID: principal.ID + "-c-" + client.ID, Email: "agy@pool.local", PlanType: "pro", CreatedAt: time.Now()})

	upstreamBase, _ := url.Parse(upstream.URL)
	accountA := &Account{Type: AccountTypeAntigravity, ID: "agy-pin-a", AccessToken: "token-a", google: googleAccountState{ProjectID: "proj-a"}, PlanType: "pro"}
	accountB := &Account{Type: AccountTypeAntigravity, ID: "agy-pin-b", AccessToken: "token-b", google: googleAccountState{ProjectID: "proj-b"}, PlanType: "pro"}
	h := &proxyHandler{
		passportConfig: passportConfig{jwtSecret: jwtSecret},
		cfg:            &config{requestTimeout: 5 * time.Second, streamTimeout: 5 * time.Second, maxInMemoryBodyBytes: 4 << 20, maxAttempts: 1, disableRefresh: true},
		transport:      http.DefaultTransport,
		pool:           newPoolState([]*Account{accountA, accountB}, false),
		registry:       NewProviderRegistry(NewCodexProvider(upstreamBase, upstreamBase, nil), NewClaudeProvider(upstreamBase), NewGeminiProvider(upstreamBase, upstreamBase), NewAntigravityProvider(upstreamBase, upstreamBase)),
		metrics:        newMetrics(),
		recent:         newRecentErrors(5),
		passport:       passport,
	}
	h.pool.antigravity = rt
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()

	send := func(body string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1beta/models/gemini-4-neon:streamGenerateContent?alt=sse", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, respBody)
		}
		mu.Lock()
		defer mu.Unlock()
		return served[len(served)-1]
	}
	const level = `"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"XHIGH"}}`
	first := send(`{"contents":[{"role":"user","parts":[{"text":"refactor the parser"}]}],` + level + `}`)

	// Make the serving account the worst candidate by score. Without a pin the
	// next turn would move to the other account.
	servedAccount, otherAccount := accountA, accountB
	if first == "Bearer token-b" {
		servedAccount, otherAccount = accountB, accountA
	}
	atomic.StoreInt64(&servedAccount.Inflight, 1000)
	defer atomic.StoreInt64(&servedAccount.Inflight, 0)

	second := send(`{"contents":[{"role":"user","parts":[{"text":"refactor the parser"}]},{"role":"model","parts":[{"text":"done"}]},{"role":"user","parts":[{"text":"now add tests"}]}],` + level + `}`)
	if second != first {
		t.Fatalf("second turn moved from %q to %q (other account %s)", first, second, otherAccount.ID)
	}

	// A different conversation is free to use the better-scored account.
	third := send(`{"contents":[{"role":"user","parts":[{"text":"write the release notes"}]}],` + level + `}`)
	if third != "Bearer "+otherAccount.AccessToken {
		t.Fatalf("new conversation used %q, want the higher-scored %s", third, otherAccount.ID)
	}
}
