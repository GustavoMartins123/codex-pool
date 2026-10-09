package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountGrantUpdateAPI(t *testing.T) {
	p, pool := ownershipFixture(t)
	a := pool.allAccounts()[0]
	cookie, csrf := addTestPassportOperator(t, p, "editor-api")
	created, err := p.createAccountGrant("editor-api", 1, testGrant("api-edit", "bob", a))
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	call := func(token string, value any) *httptest.ResponseRecorder {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.handleAccountSharing(w, passportContributionRequest(http.MethodPut, "/api/accounts/kimi/shared/grants", cookie, token, body), a.Type, a.ID, "grants")
		return w
	}
	q := map[string]any{"revision": 2, "grant_revision": 1, "id": created.ID, "recipient_id": "bob", "models": []string{"kimi-k2.5", "kimi-k2"}, "budget": PolicyLimits{DailyRequests: 8}, "expires_at": created.ExpiresAt, "reason": "Add shared models"}
	if w := call("", q); w.Code != 403 {
		t.Fatalf("CSRF: %d", w.Code)
	}
	unknown := map[string]any{"secret": "unexpected"}
	if w := call(csrf, unknown); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
	q["grant_revision"] = 0
	if w := call(csrf, q); w.Code != 409 {
		t.Fatalf("missing grant revision: %d %s", w.Code, w.Body.String())
	}
	q["grant_revision"] = 1
	w := call(csrf, q)
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	var view accountSharingView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != 3 || len(view.Grants) != 1 || view.Grants[0].Revision != 2 || view.Grants[0].Budget.DailyRequests != 8 || len(view.Grants[0].Models) != 2 {
		t.Fatalf("persisted response: %+v", view)
	}
	if w := call(csrf, q); w.Code != 409 {
		t.Fatalf("stale update: %d %s", w.Code, w.Body.String())
	}
	if accountRevision(t, p, a) != 3 {
		t.Fatal("rejected update changed revision")
	}
}
