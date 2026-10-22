package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

const resetCreditConsumePath = "/admin/accounts/pool-codex/reset-credit/consume"

type resetCreditEndpointFixture struct {
	h        *proxyHandler
	upstream *resetCreditUpstream
	operator authoritySession
	member   authoritySession
	spent    int
}

// newResetCreditEndpointFixture serves an operator-managed Codex account with
// one banked reset, an operator-managed Kimi account and a Codex account
// privately owned by a member.
func newResetCreditEndpointFixture(t *testing.T, credits string) *resetCreditEndpointFixture {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	passport, err := newPassportStore(testUsageStore(t).db)
	if err != nil {
		t.Fatal(err)
	}
	f := &resetCreditEndpointFixture{
		operator: addAuthorityPrincipal(t, passport, "operator", PrincipalOperator),
		member:   addAuthorityPrincipal(t, passport, "member", PrincipalMember),
	}
	managed := codexAccountWithResets()
	pooled := &Account{ID: "pool-kimi", Type: AccountTypeKimi}
	private := &Account{ID: "member-codex", Type: AccountTypeCodex, AccessToken: "member-token"}
	if err := passport.initializeAccountAuthority([]*Account{managed, pooled}); err != nil {
		t.Fatal(err)
	}
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(AccountTypeCodex, private.ID), &accountResource{Version: 2, ID: private.ID, Provider: AccountTypeCodex, OwnerID: "member", AddedBy: "member", Revision: 1, Status: "active"})
	}); err != nil {
		t.Fatal(err)
	}
	pool := newPoolState([]*Account{managed, pooled, private}, false)
	pool.accountAuthority = passport
	f.upstream = &resetCreditUpstream{t: t, credits: credits, consume: func(map[string]string) *http.Response {
		f.spent++
		return jsonResponse(`{"code":"reset","windows_reset":1}`)
	}}
	f.h = resetCreditHandler(f.upstream)
	f.h.cfg.adminToken = "break-glass-admin-token"
	f.h.passport, f.h.pool = passport, pool
	return f
}

func oneResetCredit() string {
	expires := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339Nano)
	return `{"available_count":1,"credits":[{"id":"credit-1","status":"available","expires_at":"` + expires + `"}]}`
}

func (f *resetCreditEndpointFixture) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	f.h.ServeHTTP(response, request)
	return response
}

func (f *resetCreditEndpointFixture) auditActions(t *testing.T, subject string) map[string]string {
	t.Helper()
	entries, err := f.h.passport.recentAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	actors := map[string]string{}
	for _, entry := range entries {
		if entry.SubjectID == subject {
			actors[entry.Action] = entry.ActorID
		}
	}
	return actors
}

func TestOperatorConsumesResetOnPoolManagedCodexAccount(t *testing.T) {
	f := newResetCreditEndpointFixture(t, oneResetCredit())
	response := f.serve(authorityRequest(http.MethodPost, resetCreditConsumePath, f.operator, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var result resetCreditConsumption
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CreditID != "credit-1" || result.Code != "reset" || result.WindowsReset != 1 {
		t.Fatalf("result = %+v", result)
	}
	if f.spent != 1 {
		t.Fatalf("resets spent = %d", f.spent)
	}
	actors := f.auditActions(t, "pool-codex")
	if actors["account.reset_credit_consume_requested"] != "operator" || actors["account.reset_credit_consumed"] != "operator" {
		t.Fatalf("audit = %+v", actors)
	}
}

func TestBreakGlassTokenConsumesReset(t *testing.T) {
	f := newResetCreditEndpointFixture(t, oneResetCredit())
	request := httptest.NewRequest(http.MethodPost, resetCreditConsumePath, nil)
	request.Header.Set("X-Admin-Token", "break-glass-admin-token")
	if response := f.serve(request); response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if actors := f.auditActions(t, "pool-codex"); actors["account.reset_credit_consumed"] != "break-glass" {
		t.Fatalf("audit = %+v", actors)
	}
}

func TestResetConsumeIsRefusedWithoutSpendingForNonOperators(t *testing.T) {
	f := newResetCreditEndpointFixture(t, oneResetCredit())
	for _, test := range []struct {
		name    string
		request *http.Request
		subject string
		want    int
	}{
		{"member session on a pool account", authorityRequest(http.MethodPost, resetCreditConsumePath, f.member, ""), "pool-codex", http.StatusForbidden},
		{"member session on own private account", authorityRequest(http.MethodPost, "/admin/accounts/member-codex/reset-credit/consume", f.member, ""), "member-codex", http.StatusForbidden},
		{"operator on a member's private account", authorityRequest(http.MethodPost, "/admin/accounts/member-codex/reset-credit/consume", f.operator, ""), "member-codex", http.StatusForbidden},
		{"operator on a non-Codex pool account", authorityRequest(http.MethodPost, "/admin/accounts/pool-kimi/reset-credit/consume", f.operator, ""), "pool-kimi", http.StatusForbidden},
		{"operator on an unknown account", authorityRequest(http.MethodPost, "/admin/accounts/missing/reset-credit/consume", f.operator, ""), "missing", http.StatusNotFound},
		{"no credentials", httptest.NewRequest(http.MethodPost, resetCreditConsumePath, nil), "pool-codex", http.StatusUnauthorized},
		{"operator with GET", authorityRequest(http.MethodGet, resetCreditConsumePath, f.operator, ""), "pool-codex", http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if response := f.serve(test.request); response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
			if f.spent != 0 || len(f.upstream.requests) != 0 {
				t.Fatalf("upstream was reached: spent=%d requests=%v", f.spent, f.upstream.requests)
			}
			if actors := f.auditActions(t, test.subject); actors["account.reset_credit_consumed"] != "" {
				t.Fatalf("audit recorded a consume: %+v", actors)
			}
		})
	}
}

func TestOperatorResetConsumeRequiresCSRF(t *testing.T) {
	f := newResetCreditEndpointFixture(t, oneResetCredit())
	request := authorityRequest(http.MethodPost, resetCreditConsumePath, f.operator, "")
	request.Header.Del("X-CSRF-Token")
	if response := f.serve(request); response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if f.spent != 0 {
		t.Fatal("a reset was spent without a CSRF token")
	}
}

func TestResetConsumeReportsConflictWhenNoResetIsBanked(t *testing.T) {
	f := newResetCreditEndpointFixture(t, `{"available_count":0,"credits":[]}`)
	response := f.serve(authorityRequest(http.MethodPost, resetCreditConsumePath, f.operator, ""))
	if response.Code != http.StatusConflict || f.spent != 0 {
		t.Fatalf("status = %d spent=%d body=%s", response.Code, f.spent, response.Body.String())
	}
	if actors := f.auditActions(t, "pool-codex"); actors["account.reset_credit_consumed"] != "" {
		t.Fatalf("audit recorded a consume that never happened: %+v", actors)
	}
}

func TestResetConsumeFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	f := newResetCreditEndpointFixture(t, oneResetCredit())
	if err := f.h.passport.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte(bucketPassportAudit)) }); err != nil {
		t.Fatal(err)
	}
	response := f.serve(authorityRequest(http.MethodPost, resetCreditConsumePath, f.operator, ""))
	if response.Code != http.StatusServiceUnavailable || f.spent != 0 || len(f.upstream.requests) != 0 {
		t.Fatalf("status = %d spent=%d requests=%v", response.Code, f.spent, f.upstream.requests)
	}
}

func TestAdminAccountsOffersResetConsumptionOnlyWhereAllowed(t *testing.T) {
	f := newResetCreditEndpointFixture(t, oneResetCredit())
	response := f.serve(authorityRequest(http.MethodGet, "/admin/accounts", f.operator, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var rows []struct {
		ID              string `json:"id"`
		CanConsumeReset bool   `json:"can_consume_reset"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = row.CanConsumeReset
	}
	if !got["pool-codex"] || got["pool-kimi"] {
		t.Fatalf("can_consume_reset by account = %v", got)
	}
	if strings.Contains(response.Body.String(), "member-codex") {
		t.Fatal("operator listing exposed a member's private account")
	}
}
