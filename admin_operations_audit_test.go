package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func adminOperationsAuditFixture(t *testing.T) (*proxyHandler, authoritySession, authoritySession) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	operator := addAuthorityPrincipal(t, passport, "operator", PrincipalOperator)
	member := addAuthorityPrincipal(t, passport, "member", PrincipalMember)
	limited := &Account{ID: "codex", Type: AccountTypeCodex, RateLimitUntil: time.Now().Add(time.Hour)}
	h := &proxyHandler{
		cfg:      &config{adminToken: "break-glass-admin-token", poolDir: t.TempDir()},
		passport: passport,
		store:    store,
		pool:     newPoolState([]*Account{limited}, false),
		metrics:  newMetrics(),
		recent:   newRecentErrors(5),
	}
	return h, operator, member
}

func auditActions(t *testing.T, h *proxyHandler) []AuditEntry {
	t.Helper()
	entries, err := h.passport.recentAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestDestructiveAdminOperationsRecordAudit(t *testing.T) {
	tests := []struct {
		path   string
		action string
		detail string
	}{
		{"/admin/reload", "pool.accounts_reloaded", "0 accounts"},
		{"/admin/clear-rate-limits", "pool.rate_limits_cleared", "cleared 1 accounts"},
		{"/admin/purge-anonymous", "pool.anonymous_usage_purged", "deleted 0 entries, kept 2 users"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			h, operator, _ := adminOperationsAuditFixture(t)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, authorityRequest(http.MethodPost, test.path, operator, ""))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			entries := auditActions(t, h)
			if len(entries) != 1 {
				t.Fatalf("audit entries = %d, want 1: %+v", len(entries), entries)
			}
			entry := entries[0]
			if entry.Action != test.action || entry.ActorID != "operator" || entry.Detail != test.detail {
				t.Fatalf("audit = %+v, want action %q actor \"operator\" detail %q", entry, test.action, test.detail)
			}
		})
	}
}

func TestDestructiveAdminOperationsAuditBreakGlassActor(t *testing.T) {
	for _, path := range []string{"/admin/reload", "/admin/clear-rate-limits", "/admin/purge-anonymous"} {
		t.Run(path, func(t *testing.T) {
			h, _, _ := adminOperationsAuditFixture(t)
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("X-Admin-Token", "break-glass-admin-token")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			entries := auditActions(t, h)
			if len(entries) != 1 || entries[0].ActorID != "break-glass" {
				t.Fatalf("audit = %+v, want a single break-glass entry", entries)
			}
		})
	}
}

func TestDestructiveAdminOperationsRejectMembersWithoutAudit(t *testing.T) {
	for _, path := range []string{"/admin/reload", "/admin/clear-rate-limits", "/admin/purge-anonymous"} {
		t.Run(path, func(t *testing.T) {
			h, _, member := adminOperationsAuditFixture(t)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, authorityRequest(http.MethodPost, path, member, ""))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.Code)
			}
			if entries := auditActions(t, h); len(entries) != 0 {
				t.Fatalf("refused mutation recorded audit: %+v", entries)
			}
		})
	}
}
