package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutingKeepsOriginalURLAndDecodedSegments(t *testing.T) {
	cases := []struct {
		target string
		id     string
		local  bool
	}{
		{target: "/api/plain/details?view=all", id: "plain", local: true},
		{target: "/api%2fid/details?view=all", id: "id", local: true},
		{target: "/api/./details", id: ".", local: true},
		{target: "/api/%2e%2e/details", id: "..", local: true},
		{target: "/api//details", local: true},
		{target: "/api/a%2fb/details"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tc.target, nil)
			originalURL := request.URL
			assertRoute := func(w http.ResponseWriter, r *http.Request, local bool) {
				t.Helper()
				if tc.local != local {
					t.Errorf("matched local route = %t, want %t", local, tc.local)
				}
				if r.URL != originalURL || r.RequestURI != tc.target {
					t.Errorf("request URL or target changed: %s, %q", r.URL, r.RequestURI)
				}
				if id := PathSegment(r, "id"); id != tc.id {
					t.Errorf("id = %q, want %q", id, tc.id)
				}
				w.WriteHeader(http.StatusNoContent)
			}
			app := New(
				func(http.ResponseWriter, *http.Request) {},
				func(_ http.ResponseWriter, r *http.Request) *http.Request { return r },
				func() string { return "test-request" },
				func(w http.ResponseWriter, r *http.Request) { assertRoute(w, r, false) },
			)
			app.HandleFunc("/api/{id}/details", func(w http.ResponseWriter, r *http.Request) {
				assertRoute(w, r, true)
			})
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent {
				t.Errorf("status = %d, want 204", recorder.Code)
			}
		})
	}
}
