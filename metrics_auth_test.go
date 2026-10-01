package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsCredentialOnlyAuthorizesMetricsAndFailsClosed(t *testing.T) {
	token := strings.Repeat("a", 64)
	h := &proxyHandler{cfg: &config{metricsToken: token, adminToken: strings.Repeat("b", 32)}, metrics: newMetrics(), pool: newPoolState(nil, false)}
	for _, test := range []struct {
		path, method, token string
		status              int
	}{
		{"/metrics", http.MethodGet, token, 200},
		{"/metrics", http.MethodGet, "wrong", 403},
		{"/metrics", http.MethodPost, token, 405},
		{"/admin/accounts", http.MethodGet, token, 401},
	} {
		r := httptest.NewRequest(test.method, test.path, nil)
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s: status=%d", test.method, test.path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	r.Header.Set("X-Admin-Token", h.cfg.adminToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("invalid metrics token tried administrative credential")
	}
	h.cfg.metricsToken = ""
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("disabled monitoring credential accepted")
	}
}
