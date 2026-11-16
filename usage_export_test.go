package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type usageExportFixture struct {
	handler       *proxyHandler
	passport      *PassportStore
	operatorToken string
	memberToken   string
}

func newUsageExportFixture(t *testing.T) *usageExportFixture {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	insertTestPrincipal(t, passport, "op", PrincipalOperator, "op", "op@example.com")
	insertTestPrincipal(t, passport, "member-1", PrincipalMember, "alice", "alice@example.com")
	insertTestPrincipal(t, passport, "member-2", PrincipalMember, "bob", "bob@example.com")
	operatorToken, _, err := passport.createSession("op")
	if err != nil {
		t.Fatal(err)
	}
	memberToken, _, err := passport.createSession("member-1")
	if err != nil {
		t.Fatal(err)
	}

	duck, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	usages := []RequestUsage{
		{Timestamp: now.Add(-time.Hour), AccountID: "acc-1", AccountType: AccountTypeCodex, UserID: "member-1", ClientCredentialID: "cred-1", OriginID: "origin-hash-1", ProxyRequestID: "req-1", Model: "gpt-test", BillableTokens: 15},
		{Timestamp: now.Add(-48 * time.Hour), AccountID: "acc-1", AccountType: AccountTypeCodex, UserID: "member-1", ClientCredentialID: "cred-1", ProxyRequestID: "req-2", Model: "gpt-test", BillableTokens: 99},
		{Timestamp: now.Add(-30 * time.Minute), AccountID: "acc-2", AccountType: AccountTypeClaude, UserID: "member-2", ClientCredentialID: "cred-2", OriginID: "origin-hash-2", ProxyRequestID: "req-3", Model: `=HYPERLINK("http://evil")`, BillableTokens: 7},
	}
	for _, usage := range usages {
		if err := store.recordWithCost(usage, 0.01); err != nil {
			t.Fatal(err)
		}
	}
	duck.Notify()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		count, _, err := duck.ExportUsage(context.Background(), UsageExportQuery{Since: time.Unix(0, 0), Limit: 10}, func(UsageExportRow) error { return nil })
		if err == nil && count == len(usages) {
			return &usageExportFixture{
				handler:       &proxyHandler{cfg: &config{}, passport: passport, store: store, duckAnalytics: duck},
				passport:      passport,
				operatorToken: operatorToken,
				memberToken:   memberToken,
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("facts not drained")
	return nil
}

func usageExportRequest(path, sessionToken string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if sessionToken != "" {
		request.AddCookie(&http.Cookie{Name: "pool_session", Value: sessionToken})
	}
	return request
}

func decodeUsageExport(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Rows      []UsageExportRow `json:"rows"`
	RowsCount int              `json:"rows_count"`
	Truncated bool             `json:"truncated"`
	Hours     int              `json:"hours"`
	Limit     int              `json:"limit"`
} {
	t.Helper()
	var payload struct {
		Rows      []UsageExportRow `json:"rows"`
		RowsCount int              `json:"rows_count"`
		Truncated bool             `json:"truncated"`
		Hours     int              `json:"hours"`
		Limit     int              `json:"limit"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body = %s: %v", recorder.Body.String(), err)
	}
	return payload
}

func TestMyUsageExportIgnoresForeignPrincipalParam(t *testing.T) {
	fixture := newUsageExportFixture(t)
	recorder := httptest.NewRecorder()
	fixture.handler.handleMyUsageExport(recorder, usageExportRequest("/api/me/usage-export?hours=24&format=json&principal=member-2", fixture.memberToken))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeUsageExport(t, recorder)
	if payload.RowsCount != 1 || len(payload.Rows) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Rows[0].PrincipalID != "member-1" || payload.Rows[0].OriginID != "origin-hash-1" {
		t.Fatalf("row = %+v", payload.Rows[0])
	}
	if strings.Contains(recorder.Body.String(), "member-2") {
		t.Fatal("member export leaked another principal")
	}

	recorder = httptest.NewRecorder()
	fixture.handler.handleMyUsageExport(recorder, usageExportRequest("/api/me/usage-export?hours=24&principal=member-2", fixture.memberToken))
	if strings.Contains(recorder.Body.String(), "member-2") {
		t.Fatal("member CSV leaked another principal")
	}

	entries, err := fixture.passport.recentAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Action == "usage.exported" && entry.ActorID == "member-1" && entry.SubjectID == "member-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("member export not audited: %+v", entries)
	}
}

func TestConsoleUsageExportCoversEveryoneAndFilters(t *testing.T) {
	fixture := newUsageExportFixture(t)
	recorder := httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export?hours=24&format=json", fixture.operatorToken))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	all := decodeUsageExport(t, recorder)
	if all.RowsCount != 2 || all.Rows[0].PrincipalID != "member-2" || all.Rows[1].PrincipalID != "member-1" {
		t.Fatalf("payload = %+v", all)
	}
	if all.Hours != 24 {
		t.Fatalf("hours = %d", all.Hours)
	}

	recorder = httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export?hours=24&format=json&principal=member-1", fixture.operatorToken))
	scoped := decodeUsageExport(t, recorder)
	if scoped.RowsCount != 1 || scoped.Rows[0].PrincipalID != "member-1" {
		t.Fatalf("payload = %+v", scoped)
	}

	entries, err := fixture.passport.recentAudit(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Action == "usage.exported" && entry.ActorID == "op" && entry.SubjectID == "member-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("operator export not audited: %+v", entries)
	}
}

func TestConsoleUsageExportCSVAttachmentEscapesFormulas(t *testing.T) {
	fixture := newUsageExportFixture(t)
	recorder := httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export?hours=24&principal=member-2", fixture.operatorToken))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment; filename=\"usage-export-member-2-") || !strings.HasSuffix(disposition, ".csv\"") {
		t.Fatalf("content disposition = %q", disposition)
	}
	if got := recorder.Header().Get("X-Usage-Export-Rows"); got != "1" {
		t.Fatalf("rows header = %q", got)
	}
	records, err := csv.NewReader(strings.NewReader(recorder.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
	for i, column := range usageExportColumns {
		if records[0][i] != column {
			t.Fatalf("header = %v", records[0])
		}
	}
	if records[1][7] != `'=HYPERLINK("http://evil")` {
		t.Fatalf("model cell = %q", records[1][7])
	}
	if records[1][3] != "origin-hash-2" {
		t.Fatalf("origin cell = %q", records[1][3])
	}
}

func TestUsageExportLimitFlagsTruncation(t *testing.T) {
	fixture := newUsageExportFixture(t)
	recorder := httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export?hours=24&format=json&limit=1", fixture.operatorToken))
	payload := decodeUsageExport(t, recorder)
	if !payload.Truncated || payload.RowsCount != 1 || payload.Limit != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if got := recorder.Header().Get("X-Usage-Export-Truncated"); got != "true" {
		t.Fatalf("truncated header = %q", got)
	}
}

func TestUsageExportGuards(t *testing.T) {
	fixture := newUsageExportFixture(t)

	recorder := httptest.NewRecorder()
	fixture.handler.handleMyUsageExport(recorder, usageExportRequest("/api/me/usage-export", ""))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export", fixture.memberToken))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("member on console status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export?principal=nobody", fixture.operatorToken))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown principal status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export?format=xml", fixture.operatorToken))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad format status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	fixture.handler.handleConsoleUsageExport(recorder, httptest.NewRequest(http.MethodPost, "/api/console/usage-export", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("anonymous post status = %d", recorder.Code)
	}

	withoutAnalytics := &proxyHandler{cfg: &config{}, passport: fixture.passport}
	recorder = httptest.NewRecorder()
	withoutAnalytics.handleConsoleUsageExport(recorder, usageExportRequest("/api/console/usage-export", fixture.operatorToken))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("no analytics status = %d", recorder.Code)
	}
}