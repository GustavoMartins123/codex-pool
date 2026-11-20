package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bbolt "go.etcd.io/bbolt"
)

const accountVerifyWhamFixture = `{"rate_limit":{"primary_window":{"used_percent":12,"reset_at":4102444800,"limit_window_seconds":18000},"secondary_window":{"used_percent":30,"reset_at":4102444800,"limit_window_seconds":604800}}}`

func accountVerifyTestAccount(t *testing.T, accType AccountType, id string) *Account {
	t.Helper()
	file := filepath.Join(t.TempDir(), id+".json")
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Account{
		Type: accType, ID: id,
		vault:       plainTestVault(),
		File:        file,
		AccessToken: "token",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
}

func accountVerifyFixtureHandler(t *testing.T, accounts []*Account, transport http.RoundTripper) *proxyHandler {
	t.Helper()
	base := mustParse("https://verify.example.test")
	daily := mustParse("https://daily.example.test")
	production := mustParse("https://prod.example.test")
	return &proxyHandler{
		cfg: &config{
			whamBase: base, claudeBase: base, grokBase: base,
			kimiBase: base, zaiBase: base, minimaxBase: base,
		},
		pool: newPoolState(accounts, false),
		registry: NewProviderRegistry(
			NewCodexProvider(base, base, base),
			NewClaudeProvider(base),
			NewGeminiProvider(base, base),
			NewAntigravityProvider(daily, production),
			NewGrokProvider(base),
			NewKimiProvider(base),
			NewMinimaxProvider(base),
			NewZAIProvider(base),
		),
		transport:        transport,
		refreshTransport: transport,
	}
}

func TestVerifyAccountAccessCodexHealthy(t *testing.T) {
	account := accountVerifyTestAccount(t, AccountTypeCodex, "cx-ok")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/wham/usage") {
			t.Fatalf("codex probe must read WHAM usage, got %s %s", req.Method, req.URL)
		}
		return jsonResponse(accountVerifyWhamFixture), nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAccountAccess(context.Background(), account)

	if result.Status != "healthy" {
		t.Fatalf("status = %q, want healthy (detail %q)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "primary 12% used") {
		t.Fatalf("detail lost the usage summary: %q", result.Detail)
	}
	if account.Usage.RetrievedAt.IsZero() {
		t.Fatal("probe must refresh the usage snapshot")
	}
}

func TestVerifyAccountAccessCodexRateLimitedIsNotHealthy(t *testing.T) {
	account := accountVerifyTestAccount(t, AccountTypeCodex, "cx-limited")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: http.NoBody, Header: make(http.Header)}, nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAccountAccess(context.Background(), account)

	if result.Status != "rate_limited" {
		t.Fatalf("status = %q, want rate_limited: the usage fetcher swallows 429s", result.Status)
	}
	if !accountCoolingDownLocked(account, time.Now()) {
		t.Fatal("a 429 probe must leave the account cooling down")
	}
}

func TestVerifyAccountAccessKimiRevoked(t *testing.T) {
	account := accountVerifyTestAccount(t, AccountTypeKimi, "kimi-dead")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/v1/usages") {
			t.Fatalf("kimi probe hit %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: http.NoBody, Header: make(http.Header)}, nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAccountAccess(context.Background(), account)

	if result.Status != "revoked" {
		t.Fatalf("status = %q, want revoked (detail %q)", result.Status, result.Detail)
	}
	if !account.Dead {
		t.Fatal("a rejected Kimi key must stop routing")
	}
}

func TestVerifyAccountAccessMinimaxUsesModelListNotGeneration(t *testing.T) {
	account := accountVerifyTestAccount(t, AccountTypeMinimax, "mm-ok")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/v1/models") {
			t.Fatalf("minimax verify must never generate: got %s %s", req.Method, req.URL)
		}
		return jsonResponse(`{"data":[{"id":"MiniMax-M2"}]}`), nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAccountAccess(context.Background(), account)

	if result.Status != "healthy" {
		t.Fatalf("status = %q, want healthy (detail %q)", result.Status, result.Detail)
	}
}

func TestVerifyAccountAccessClaudeStaticKeyUsesModelList(t *testing.T) {
	account := accountVerifyTestAccount(t, AccountTypeClaude, "claude-key")
	account.AccessToken = "sk-ant-api03-static-key"
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/v1/models") {
			t.Fatalf("static Claude keys have no usage endpoint, got %s %s", req.Method, req.URL)
		}
		return jsonResponse(`{"data":[{"id":"claude-sonnet-4-6"}]}`), nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAccountAccess(context.Background(), account)

	if result.Status != "healthy" {
		t.Fatalf("status = %q, want healthy (detail %q)", result.Status, result.Detail)
	}
}

func TestVerifyAccountAccessGeminiLiveTokenSkipsUpstream(t *testing.T) {
	account := accountVerifyTestAccount(t, AccountTypeGemini, "gemini-ok")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("a live Gemini token must not be refreshed: %s %s", req.Method, req.URL)
		return nil, nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAccountAccess(context.Background(), account)

	if result.Status != "healthy" {
		t.Fatalf("status = %q, want healthy (detail %q)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "no usage endpoint") {
		t.Fatalf("detail must admit the probe only checked the token: %q", result.Detail)
	}
}

func TestAccountVerifyHandlerRejectsUnsupportedAndUnconfigured(t *testing.T) {
	xiaomi := accountVerifyTestAccount(t, AccountTypeXiaomi, "mi")
	minimax := accountVerifyTestAccount(t, AccountTypeMinimax, "mm")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unverifiable accounts must never be probed: %s", req.URL)
		return nil, nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{xiaomi, minimax}, transport)
	base := mustParse("https://verify.example.test")
	h.registry = NewProviderRegistry(NewCodexProvider(base, base, base), NewClaudeProvider(base), NewGeminiProvider(base, base))

	for _, test := range []struct {
		id   string
		want int
	}{
		{"mi", http.StatusBadRequest},
		{"mm", http.StatusServiceUnavailable},
		{"missing", http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		h.handleAccountVerify(recorder, httptest.NewRequest(http.MethodPost, "/admin/accounts/"+test.id+"/verify", nil), test.id)
		if recorder.Code != test.want {
			t.Fatalf("verify %s: got %d, want %d (%s)", test.id, recorder.Code, test.want, recorder.Body.String())
		}
	}
}

func TestAccountVerifyBatchCoversEverySupportedProvider(t *testing.T) {
	antigravity := antigravityVerifyTestAccount(t, "ag")
	codex := accountVerifyTestAccount(t, AccountTypeCodex, "cx")
	xiaomi := accountVerifyTestAccount(t, AccountTypeXiaomi, "mi")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "verify.example.test" {
			return jsonResponse(accountVerifyWhamFixture), nil
		}
		return antigravityTestResponse(http.StatusOK, antigravityHealthyQuotaFixture), nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{antigravity, codex, xiaomi}, transport)

	recorder := httptest.NewRecorder()
	h.handleAccountVerifyBatch(recorder, httptest.NewRequest(http.MethodPost, "/admin/accounts/verify", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("batch verify: got %d %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Accounts []accountVerifyResult `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, entry := range payload.Accounts {
		statuses[entry.AccountID] = entry.Status
	}
	if len(statuses) != 2 || statuses["ag"] != "healthy" || statuses["cx"] != "healthy" {
		t.Fatalf("batch must cover the verifiable accounts only, got %v", statuses)
	}
}

func putAccountVerifyResource(t *testing.T, passport *PassportStore, account *Account, owner string, operatorManaged bool) {
	t.Helper()
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(account.Type, account.ID), &accountResource{
			Version: 2, ID: account.ID, Provider: account.Type, OwnerID: owner, AddedBy: owner,
			OperatorManaged: operatorManaged, Revision: 1, Status: "active",
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountVerifyBatchRouteSkipsAccountsTheOperatorCannotManage(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	operator := addAuthorityPrincipal(t, passport, "operator", PrincipalOperator)
	managed := accountVerifyTestAccount(t, AccountTypeCodex, "pool-codex")
	private := accountVerifyTestAccount(t, AccountTypeCodex, "carol-codex")
	putAccountVerifyResource(t, passport, managed, "operator-managed", true)
	putAccountVerifyResource(t, passport, private, "carol", false)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(accountVerifyWhamFixture), nil
	})
	h := accountVerifyFixtureHandler(t, []*Account{managed, private}, transport)
	h.passport = passport
	h.store = store

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, authorityRequest(http.MethodPost, "/admin/accounts/verify", operator, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("batch route: got %d %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Accounts []accountVerifyResult `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].AccountID != managed.ID {
		t.Fatalf("batch probed accounts outside the operator's authority: %+v", payload.Accounts)
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, authorityRequest(http.MethodPost, "/admin/accounts/"+managed.ID+"/verify", operator, ""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("single route: got %d %s", recorder.Code, recorder.Body.String())
	}
	var single accountVerifyResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if single.AccountID != managed.ID || single.Status != "healthy" {
		t.Fatalf("single verify = %+v, want a healthy answer for %s", single, managed.ID)
	}
}

func TestAdminAccountsProjectsVerifyCapability(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	addAuthorityPrincipal(t, passport, "operator", PrincipalOperator)
	addAuthorityPrincipal(t, passport, "carol", PrincipalMember)
	codex := accountVerifyTestAccount(t, AccountTypeCodex, "pool-codex")
	xiaomi := accountVerifyTestAccount(t, AccountTypeXiaomi, "pool-xiaomi")
	own := accountVerifyTestAccount(t, AccountTypeCodex, "carol-codex")
	putAccountVerifyResource(t, passport, codex, "operator-managed", true)
	putAccountVerifyResource(t, passport, xiaomi, "operator-managed", true)
	putAccountVerifyResource(t, passport, own, "carol", false)
	h := accountVerifyFixtureHandler(t, []*Account{codex, xiaomi, own}, nil)
	h.passport = passport
	h.store = store

	type projection struct {
		CanVerify bool
		CanManage bool
	}
	listing := func(actor string) map[string]projection {
		recorder := httptest.NewRecorder()
		h.serveAccounts(recorder, contributionRequestActor(httptest.NewRequest(http.MethodGet, "/admin/accounts", nil), actor))
		if recorder.Code != http.StatusOK {
			t.Fatalf("listing: got %d %s", recorder.Code, recorder.Body.String())
		}
		var rows []struct {
			ID        string `json:"id"`
			CanVerify bool   `json:"can_verify"`
			CanManage bool   `json:"can_manage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		out := map[string]projection{}
		for _, row := range rows {
			out[row.ID] = projection{CanVerify: row.CanVerify, CanManage: row.CanManage}
		}
		return out
	}

	operatorRows := listing("operator")
	if !operatorRows["pool-codex"].CanVerify || operatorRows["pool-xiaomi"].CanVerify {
		t.Fatalf("can_verify must follow the provider probe, got %+v", operatorRows)
	}
	memberRows := listing("carol")
	if len(memberRows) != 1 || !memberRows["carol-codex"].CanVerify {
		t.Fatalf("owners must see the capability on their own accounts only: %+v", memberRows)
	}
}
