package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFreshSetupCredentialsRequireAccountGrant(t *testing.T) {
	for _, tc := range []struct {
		name, config, path, model, body string
		provider                        AccountType
	}{
		{"codex", "codex", "/responses", "gpt-5.5", `{"model":"gpt-5.5","input":"hello"}`, AccountTypeCodex},
		{"claude", "claude", "/v1/messages", "claude-sonnet-5", `{"model":"claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, AccountTypeClaude},
		{"claude-with-gpt", "claude", "/v1/messages", "gpt-5.5", `{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, AccountTypeCodex},
		{"codex-with-claude", "codex", "/responses", "claude-sonnet-5", `{"model":"claude-sonnet-5","input":"hello"}`, AccountTypeClaude},
		{"codex-with-claude-chunked", "codex", "/responses", "claude-sonnet-5", `{"model":"claude-sonnet-5","input":"hello"}`, AccountTypeClaude},
		{"codex-sonnet-alias", "codex", "/responses", "claude-sonnet-5-5", `{"model":"sonnet","input":"hello"}`, AccountTypeClaude},
		{"claude-opus-alias", "claude", "/v1/messages", "claude-opus-5-5", `{"model":"opus","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, AccountTypeClaude},
		{"claude-opus-pro", "claude", "/v1/messages", "claude-opus-5-5", `{"model":"claude-opus-5-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, AccountTypeClaude},
		{"codex-opus-pro", "codex", "/responses", "claude-opus-5-5", `{"model":"claude-opus-5-5","input":"hello"}`, AccountTypeClaude},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POOL_JWT_SECRET", "fresh-setup-secret")
			p, client := newNonceTestPassport(t)
			addTestPassportOperator(t, p, "owner")
			a := &Account{ID: "managed-account", Type: tc.provider, AccessToken: "upstream-secret", AccountID: "upstream-account", PlanType: "pro"}
			if err := p.initializeAccountAuthority([]*Account{a}); err != nil {
				t.Fatal(err)
			}
			pool := newPoolState([]*Account{a}, false)
			pool.accountAuthority = p
			base, _ := url.Parse("https://upstream.example")
			calls := 0
			h := &proxyHandler{cfg: &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, disableRefresh: true}, passport: p, pool: pool, aliases: newModelAliases(nil),
				registry: NewProviderRegistry(NewCodexProvider(base, base, base), NewClaudeProvider(base), nil), metrics: newMetrics(), recent: newRecentErrors(5),
				transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if !strings.Contains(r.Header.Get("Authorization"), a.AccessToken) && r.Header.Get("X-Api-Key") != a.AccessToken {
						t.Fatal("setup credential leaked to upstream")
					}
					body, contentType := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "application/json"
					if tc.provider == AccountTypeCodex {
						body, contentType = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", "text/event-stream"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
			nonce, _, err := p.mintConfigDownloadNonce(client)
			if err != nil {
				t.Fatal(err)
			}
			configResponse := httptest.NewRecorder()
			h.serveConfigDownload(configResponse, httptest.NewRequest("GET", "/config/"+tc.config+"/"+nonce, nil))
			var auth struct {
				AccessToken string `json:"access_token"`
				Tokens      struct {
					AccessToken string `json:"access_token"`
				} `json:"tokens"`
			}
			if configResponse.Code != 200 || json.Unmarshal(configResponse.Body.Bytes(), &auth) != nil {
				t.Fatal("setup config failed")
			}
			token := auth.AccessToken
			if tc.config == "codex" {
				token = auth.Tokens.AccessToken
			}
			if token == "" {
				t.Fatal("setup token missing")
			}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
				if strings.HasSuffix(tc.name, "-chunked") {
					r.ContentLength = -1
				}
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			denied := request()
			if denied.Code != 403 || !strings.Contains(denied.Body.String(), "account_access_denied") || calls != 0 {
				t.Fatalf("unshared setup request: %d %s, upstream calls=%d", denied.Code, denied.Body.String(), calls)
			}
			grant := testGrant("setup-grant", client.PrincipalID, a)
			grant.Models = []string{tc.model}
			if _, err := p.createAccountGrant("owner", accountRevision(t, p, a), grant); err != nil {
				t.Fatal(err)
			}
			allowed := request()
			if allowed.Code != 200 || calls != 1 {
				t.Fatalf("granted setup request: %d %s, upstream calls=%d", allowed.Code, allowed.Body.String(), calls)
			}
			// A valid grant with temporarily unavailable capacity must remain a 503.
			a.mu.Lock()
			a.Disabled = true
			a.mu.Unlock()
			unavailable := request()
			if unavailable.Code != 503 || calls != 1 {
				t.Fatalf("unavailable shared account: %d %s", unavailable.Code, unavailable.Body.String())
			}
			if err := p.revokeAccountGrant("owner", grant.ID, 1); err != nil {
				t.Fatal(err)
			}
			if revoked := request(); revoked.Code != 403 || calls != 1 {
				t.Fatalf("revoked grant: %d %s", revoked.Code, revoked.Body.String())
			}
		})
	}
}

func TestRoutingAccessDenialDoesNotExposeForeignAccounts(t *testing.T) {
	p, pool := ownershipFixture(t)
	h := &proxyHandler{passport: p, pool: pool}
	for _, provider := range []AccountType{AccountTypeCodex, AccountTypeClaude} {
		err := h.checkAccountRoutingAccess("bob", provider, "gpt-5.5")
		requirePolicyCode(t, err, "account_access_denied")
	}
	grant := testGrant("one-model", "bob", pool.allAccounts()[1])
	if _, err := p.createAccountGrant("alice", 1, grant); err != nil {
		t.Fatal(err)
	}
	if err := h.checkAccountRoutingAccess("bob", AccountTypeCodex, "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	requirePolicyCode(t, h.checkAccountRoutingAccess("bob", AccountTypeCodex, "gpt-6-astra"), "account_access_denied")
	if err := p.updateAccountControls("alice", AccountTypeCodex, "private", 2, accountControls{State: accountMaintenance}, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := h.checkAccountRoutingAccess("bob", AccountTypeCodex, "gpt-5.5"); err != nil {
		t.Fatal("maintenance confused with missing permission:", err)
	}
	// Revocation must stop authorizing immediately.
	if err := p.revokeAccountGrant("alice", grant.ID, 1); err != nil {
		t.Fatal(err)
	}
	requirePolicyCode(t, h.checkAccountRoutingAccess("bob", AccountTypeCodex, "gpt-5.5"), "account_access_denied")
}
