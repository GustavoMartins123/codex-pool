package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The setup screen must come back with a usable session, not just the
// principal body, otherwise the dashboard renders and then 401s.
func TestOperatorBootstrapSetsSession(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	p, err := newPassportStore(testUsageStore(t).db)
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{cfg: &config{adminToken: "configured-test-admin-token"}, passport: p, metrics: newMetrics()}

	body := `{"username":"root","email":"root@local","password":"correct-horse-battery","display_name":"Root"}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup/operator", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", "configured-test-admin-token")
	rr := httptest.NewRecorder()
	h.handleOperatorBootstrap(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if len(rr.Result().Cookies()) == 0 {
		t.Fatal("bootstrap set no session cookie")
	}
	var out struct {
		Principal struct {
			Kind PrincipalKind `json:"kind"`
		} `json:"principal"`
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Principal.Kind != PrincipalOperator {
		t.Fatalf("kind = %q, want operator", out.Principal.Kind)
	}
	if out.CSRF == "" {
		t.Fatal("bootstrap returned no csrf token")
	}
}

func TestOperatorBootstrapRequiresAdminToken(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	p, err := newPassportStore(testUsageStore(t).db)
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{cfg: &config{adminToken: "configured-test-admin-token"}, passport: p, metrics: newMetrics()}
	body := `{"username":"root","email":"root@local","password":"correct-horse-battery","display_name":"Root"}`

	for _, token := range []string{"", "incorrect-admin-token"} {
		req := httptest.NewRequest(http.MethodPost, "/api/setup/operator", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Admin-Token", token)
		}
		rr := httptest.NewRecorder()
		h.handleOperatorBootstrap(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("bootstrap with token %q = %d, want 401", token, rr.Code)
		}
		// The setup form surfaces JSON error fields; a plain-text 401 would
		// render as an opaque "Invalid JSON response" instead of a hint.
		var errBody struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("bootstrap 401 body is not JSON: %v (%s)", err, rr.Body.String())
		}
		if errBody.Error != "invalid admin token" {
			t.Fatalf("bootstrap 401 error = %q, want invalid admin token", errBody.Error)
		}
		if p.hasOperator() {
			t.Fatal("unauthorized bootstrap created an operator")
		}
	}
}
