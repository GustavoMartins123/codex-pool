package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.etcd.io/bbolt"
)

func testGrant(id, recipient string, a *Account) accountGrant {
	return accountGrant{ID: id, Provider: a.Type, AccountID: a.ID, RecipientID: recipient, Models: []string{"gpt-5.5"}, Budget: PolicyLimits{DailyRequests: 3}, ExpiresAt: time.Now().UTC().Add(time.Hour), Reason: "team access"}
}
func accountRevision(t *testing.T, p *PassportStore, a *Account) uint64 {
	t.Helper()
	var revision uint64
	if err := p.db.View(func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), a.Type, a.ID)
		if err == nil {
			revision = r.Revision
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}
func grantUsage(t *testing.T, p *PassportStore, id string) policyUsageCounter {
	t.Helper()
	var value policyUsageCounter
	if err := p.db.View(func(tx *bbolt.Tx) error {
		_, day, _ := policyUsageKeys("grant:"+id, time.Now())
		var err error
		value, err = readPolicyCounter(tx.Bucket([]byte(bucketPassportPolicyUsage)), day)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAccountGrantsRequireConsentAndNeverTransferManagement(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	addTestPassportOperator(t, p, "charlie")
	grant := testGrant("delegated", "charlie", a)
	_, err := p.createAccountGrant("bob", 1, grant)
	requirePolicyCode(t, err, "account_delegation_denied")
	if err := p.setAccountDelegation("bob", a.Type, a.ID, 1, true); err == nil {
		t.Fatal("foreign operator consented")
	}
	if err := p.setAccountDelegation("alice", a.Type, a.ID, 1, true); err != nil {
		t.Fatal(err)
	}
	created, err := p.createAccountGrant("bob", 2, grant)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.createAccountGrant("bob", 2, grant)
	if err != nil || again.ID != created.ID || accountRevision(t, p, a) != 3 {
		t.Fatalf("idempotence: %v", err)
	}
	for _, action := range []string{"read", "use"} {
		if err := p.authorizeAccount("charlie", a, action); err != nil {
			t.Fatal(err)
		}
	}
	if p.authorizeAccount("charlie", a, "manage") == nil {
		t.Fatal("grant transferred management")
	}
	if _, err := p.accountGrantForUse("charlie", a, "gpt-6-astra"); err == nil {
		t.Fatal("grant widened models")
	}
	if err := p.setAccountDelegation("alice", a.Type, a.ID, 3, false); err != nil {
		t.Fatal(err)
	}
	if p.authorizeAccount("charlie", a, "use") == nil {
		t.Fatal("consent revocation retained access")
	}
	if err := p.setAccountDelegation("alice", a.Type, a.ID, 4, true); err != nil {
		t.Fatal(err)
	}
	if p.authorizeAccount("charlie", a, "use") == nil {
		t.Fatal("new consent revived revoked grant")
	}
	_, err = p.createAccountGrant("bob", 5, grant)
	requirePolicyCode(t, err, "grant_id_conflict")
}

func TestGrantAdmissionBudgetsRetriesControlsAndConcurrentRequests(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	grant := testGrant("limited", "bob", a)
	grant.Budget.ConcurrentRequests = 1
	grant.Budget.DailyRequests = 1
	if _, err := p.createAccountGrant("alice", 1, grant); err != nil {
		t.Fatal(err)
	}
	if err := p.updateAccountControls("alice", a.Type, a.ID, 2, accountControls{State: accountMaintenance}, "pause"); err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		return httptest.NewRequest("POST", "/v1/responses", nil).WithContext(context.WithValue(context.Background(), grantRequestContextKey{}, newGrantRequestState()))
	}
	r := request()
	if _, _, err := h.acquireGovernedAccount(r, "bob", "", a, "gpt-5.5"); err == nil {
		t.Fatal("maintenance admitted")
	}
	if grantUsage(t, p, grant.ID).Requests != 0 {
		t.Fatal("rejected capacity spent grant")
	}
	if err := p.updateAccountControls("alice", a.Type, a.ID, 3, accountControls{}, "resume"); err != nil {
		t.Fatal(err)
	}
	prepared, release, err := h.acquireGovernedAccount(r, "bob", "", a, "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, release, err = h.acquireGovernedAccount(prepared, "bob", "", a, "gpt-5.5")
	if err != nil {
		t.Fatal("retry spent request twice:", err)
	}
	release()
	_, _, err = h.acquireGovernedAccount(request(), "bob", "", a, "gpt-5.5")
	requirePolicyCode(t, err, "grant_concurrency_exceeded")
	r.Context().Value(grantRequestContextKey{}).(*grantRequestState).Release()
	_, _, err = h.acquireGovernedAccount(request(), "bob", "", a, "gpt-5.5")
	requirePolicyCode(t, err, "policy_daily_requests_exceeded")
	if grantUsage(t, p, grant.ID).Requests != 1 {
		t.Fatal("bad logical request count")
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	pool.accountAuthority = reloaded
	_, _, err = h.acquireGovernedAccount(request(), "bob", "", a, "gpt-5.5")
	requirePolicyCode(t, err, "policy_daily_requests_exceeded")
}

func TestGrantConcurrentAdmissionNeverOverspends(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	grant := testGrant("racing", "bob", a)
	grant.Budget.DailyRequests = 1
	created, err := p.createAccountGrant("alice", 1, grant)
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			admission, err := p.reserveGrantRequest(created, time.Now())
			if err == nil {
				count.Add(1)
				admission.Release()
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 || grantUsage(t, p, grant.ID).Requests != 1 {
		t.Fatal("concurrent grant budget overspent")
	}
}

func TestGrantCatalogAndRevocationPersistence(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	grant := testGrant("catalog", "bob", a)
	created, err := p.createAccountGrant("alice", 1, grant)
	if err != nil {
		t.Fatal(err)
	}
	models := poolModelDescriptors(h.catalogPool("bob"))
	found := false
	for _, m := range models {
		if m.Provider == "codex" {
			if m.ID != "gpt-5.5" {
				t.Fatalf("unauthorized catalog model: %s", m.ID)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("granted model absent")
	}
	if err := p.revokeAccountGrant("bob", grant.ID, 1); err == nil {
		t.Fatal("recipient revoked owner grant")
	}
	if err := p.revokeAccountGrant("alice", created.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.revokeAccountGrant("alice", created.ID, 1); err != nil {
		t.Fatal("revoke not idempotent:", err)
	}
	reloaded, err := newPassportStore(p.db)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.authorizeAccount("bob", a, "use") == nil {
		t.Fatal("restart revived grant")
	}
}

func TestMemberAndGuestRequireExplicitAccountGrants(t *testing.T) {
	for _, kind := range []PrincipalKind{PrincipalMember, PrincipalGuest} {
		t.Run(string(kind), func(t *testing.T) {
			p, pool := ownershipFixture(t)
			principal := *p.principal("bob")
			principal.Kind = kind
			if err := p.db.Update(func(tx *bbolt.Tx) error { return putJSON(tx.Bucket([]byte(bucketPrincipals)), principal.ID, principal) }); err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			p.principals[principal.ID] = &principal
			p.mu.Unlock()
			shared, private := pool.allAccounts()[0], pool.allAccounts()[1]
			if p.authorizeAccount("bob", shared, "use") == nil {
				t.Fatal("managed account implicitly shared")
			}
			grant := testGrant("explicit", "bob", private)
			if _, err := p.createAccountGrant("alice", 1, grant); err != nil {
				t.Fatal(err)
			}
			if err := p.authorizeAccount("bob", private, "use"); err != nil {
				t.Fatal(err)
			}
			if p.authorizeAccount("bob", private, "manage") == nil {
				t.Fatal("recipient gained account management")
			}
			policy := ClientPolicy{Models: PolicySelector{Deny: []string{"gpt-5.5"}}}
			if err := p.saveEditedPolicy("alice", "bob", policyEditorRequest{Target: "principal", Policy: &policy}); err != nil {
				t.Fatal(err)
			}
			h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
			if err := h.checkLivePolicy("bob", "gpt-5.5", private.Type); err == nil {
				t.Fatal("grant widened principal policy")
			}
		})
	}
}

func TestGrantAuthenticatedHTTPAndWebSocketTurns(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	var httpCalls, frames atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgradeRequest(r) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			for {
				_, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				if isCodexResponseCreate(data) {
					frames.Add(1)
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"response-test","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))
				}
			}
		}
		httpCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http-test\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	a := &Account{ID: "grant-account", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true, AccessToken: "upstream-private"}
	fx := newCodexProxyFixture(t, upstreamURL, []*Account{a})
	h := fx.handler
	h.cfg.maxAttempts = 1
	h.aliases = newModelAliases(nil)
	addTestPassportOperator(t, h.passport, "alice")
	addTestPassportOperator(t, h.passport, "bob")
	if err := h.passport.initializeAccountAuthority(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(a.Type, a.ID), accountResource{Version: 2, ID: a.ID, Provider: a.Type, OwnerID: "alice", AddedBy: "alice", Status: "active", Revision: 1})
	}); err != nil {
		t.Fatal(err)
	}
	h.pool.accountAuthority = h.passport
	grant := testGrant("network", "bob", a)
	grant.Budget.DailyRequests = 2
	if _, err := h.passport.createAccountGrant("alice", 1, grant); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("POST", fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "bob"))
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), "response.completed") {
		t.Fatalf("stream: %d %s", resp.StatusCode, data)
	}
	if err := h.passport.updateAccountControls("alice", a.Type, a.ID, 2, accountControls{MaxConcurrent: 1}, "one connection"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(fx.server.URL, "http")+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "bob")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	blockedRequest, _ := http.NewRequest("POST", fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
	blockedRequest.Header = r.Header.Clone()
	blockedResponse, err := http.DefaultClient.Do(blockedRequest)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, blockedResponse.Body)
	blockedResponse.Body.Close()
	if blockedResponse.StatusCode < 400 {
		t.Fatal("open websocket did not hold account concurrency")
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.revokeAccountGrant("alice", grant.ID, 1); err != nil {
		t.Fatal(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","input":[]}`))
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("revoked websocket turn returned a completion")
	}
	if frames.Load() != 1 || httpCalls.Load() != 1 || grantUsage(t, h.passport, grant.ID).Requests != 2 {
		t.Fatalf("network counts: HTTP=%d WS=%d usage=%+v", httpCalls.Load(), frames.Load(), grantUsage(t, h.passport, grant.ID))
	}
}

func TestNativeContextRejectsDelegationAndRevokedAccountsBeforeSending(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{}`) }))
	defer upstream.Close()
	p, _ := ownershipFixture(t)
	a := contextTestAccount("private-context", "native-user")
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(a.Type, a.ID), accountResource{Version: 2, ID: a.ID, Provider: a.Type, OwnerID: "alice", AddedBy: "alice", Status: "active", Revision: 1})
	}); err != nil {
		t.Fatal(err)
	}
	service := contextTestService(t, upstream.URL, a)
	service.pool.accountAuthority = p
	h := &proxyHandler{passport: p, pool: service.pool, nativeContext: service, cfg: &config{}}
	grant := testGrant("native-delegation", "bob", a)
	if _, err := p.createAccountGrant("alice", 1, grant); err != nil {
		t.Fatal(err)
	}
	if err := service.recordDispatch(contextScope("bob"), contextInference(contextProxySession), contextAuthSnapshot(a), ""); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"context":{"session_id":"` + contextProxySession + `","current_agent_name":"/root"}}`)
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/alpha/notes/v2/thread_hint", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		h.proxyNativeContext(w, r, "bob")
		return w
	}
	if w := call(); w.Code != 422 || !strings.Contains(w.Body.String(), "grant_context_unsupported") {
		t.Fatalf("delegated context: %d %s", w.Code, w.Body.String())
	}
	if err := p.revokeAccountGrant("alice", grant.ID, 1); err != nil {
		t.Fatal(err)
	}
	if w := call(); w.Code != 403 {
		t.Fatalf("revoked context: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("denied native context reached upstream")
	}
}

func TestGrantTokenConsumptionAndUnknownReservation(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[1]
	grant := testGrant("tokens", "bob", a)
	grant.Budget = PolicyLimits{DailyTokens: 3000000, TokenReservation: 100}
	created, err := p.createAccountGrant("alice", 1, grant)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := p.reserveGrantRequest(created, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := admission.reserveTokenAttempt(1200000)
	if err != nil {
		t.Fatal(err)
	}
	body := &policyCompletionBody{ReadCloser: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)), admission: admission, attempt: attempt, json: true, settleReported: true}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	body.Close()
	admission.Release()
	if usage := grantUsage(t, p, grant.ID); usage.Tokens != 3 || usage.ReservedTokens != 0 {
		t.Fatalf("settlement: %+v", usage)
	}
	admission, err = p.reserveGrantRequest(created, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.reserveTokenAttempt(1200000); err != nil {
		t.Fatal(err)
	}
	admission.Release()
	if usage := grantUsage(t, p, grant.ID); usage.ReservedTokens != 1200000 {
		t.Fatalf("unknown consumption released: %+v", usage)
	}
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	if err := h.checkWebSocketGrant("bob", a); err == nil {
		t.Fatal("unbounded websocket admitted token grant")
	}
}

func TestGrantTokensApplyToBufferedAndSpooledHTTP(t *testing.T) {
	for _, size := range []int{5, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Setenv("POOL_JWT_SECRET", "test-secret")
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"tokens\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			a := &Account{ID: "token-account", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true, AccessToken: "test-upstream"}
			fx := newCodexProxyFixture(t, base, []*Account{a})
			h := fx.handler
			h.cfg.maxAttempts = 1
			h.cfg.maxSpoolBodyBytes = 1 << 20
			h.aliases = newModelAliases(nil)
			addTestPassportOperator(t, h.passport, "alice")
			addTestPassportOperator(t, h.passport, "bob")
			if err := h.passport.initializeAccountAuthority(nil); err != nil {
				t.Fatal(err)
			}
			if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
				return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(a.Type, a.ID), accountResource{Version: 2, ID: a.ID, Provider: a.Type, OwnerID: "alice", AddedBy: "alice", Status: "active", Revision: 1})
			}); err != nil {
				t.Fatal(err)
			}
			h.pool.accountAuthority = h.passport
			grant := testGrant("http-tokens", "bob", a)
			grant.Budget = PolicyLimits{DailyTokens: 3000000, TokenReservation: 10}
			if _, err := h.passport.createAccountGrant("alice", 1, grant); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": true, "input": strings.Repeat("x", size)})
			r, _ := http.NewRequest("POST", fx.server.URL+"/v1/responses", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "bob"))
			r.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			result, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP tokens: %d %s", resp.StatusCode, result)
			}
			usage := grantUsage(t, h.passport, grant.ID)
			if calls.Load() != 1 || usage.Tokens != 3 || usage.ReservedTokens != 0 {
				t.Fatalf("token enforcement size=%d calls=%d usage=%+v", size, calls.Load(), usage)
			}
		})
	}
}

func TestGrantAPIRejectsCSRFUnknownFieldsAndLeaksNoSecrets(t *testing.T) {
	p, pool := ownershipFixture(t)
	cookie, csrf := addTestPassportOperator(t, p, "owner-api")
	shared := pool.allAccounts()[0]
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	call := func(token, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.handleAccountSharing(w, passportContributionRequest("POST", "/api/accounts/kimi/shared/grants", cookie, token, []byte(body)), shared.Type, shared.ID, "grants")
		return w
	}
	grant := testGrant("api", "bob", shared)
	grant.Models = []string{"kimi-k2.5"}
	payload, _ := json.Marshal(map[string]any{"revision": 1, "id": grant.ID, "recipient_id": grant.RecipientID, "models": grant.Models, "budget": grant.Budget, "expires_at": grant.ExpiresAt, "reason": grant.Reason})
	if w := call("", string(payload)); w.Code != 403 {
		t.Fatalf("CSRF=%d", w.Code)
	}
	if w := call(csrf, `{"revision":1,"secret":"unexpected"}`); w.Code != 400 {
		t.Fatalf("unknown=%d", w.Code)
	}
	w := call(csrf, string(payload))
	if w.Code != 200 {
		t.Fatalf("grant API: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SecretRef") || strings.Contains(w.Body.String(), "access_token") {
		t.Fatal("credential leaked")
	}
	grant.ID = "bad"
	grant.ExpiresAt = time.Now().Add(-time.Hour)
	_, err := p.createAccountGrant("owner-api", 2, grant)
	requirePolicyCode(t, err, "grant_invalid")
}
