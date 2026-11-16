package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecentErrorsEndpointReturnsNewestFirst(t *testing.T) {
	h, operator, _ := adminOperationsAuditFixture(t)
	h.recent = newRecentErrors(2)
	h.recent.add("first failure")
	h.recent.add("second failure")
	h.recent.add("third failure")

	response := httptest.NewRecorder()
	h.ServeHTTP(response, authorityRequest(http.MethodGet, "/api/pool/recent-errors", operator, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Errors []recentError `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Errors) != 2 {
		t.Fatalf("errors = %+v, want the 2 newest entries", payload.Errors)
	}
	if payload.Errors[0].Message != "third failure" || payload.Errors[1].Message != "second failure" {
		t.Fatalf("errors out of order: %+v", payload.Errors)
	}
	for _, entry := range payload.Errors {
		if entry.At.IsZero() {
			t.Fatalf("entry without timestamp: %+v", entry)
		}
	}
}

func TestRecentErrorsEndpointReturnsEmptyArray(t *testing.T) {
	h, operator, _ := adminOperationsAuditFixture(t)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, authorityRequest(http.MethodGet, "/api/pool/recent-errors", operator, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if body := response.Body.String(); body != "{\"errors\":[]}\n" && body != "{\"errors\":[]}" {
		t.Fatalf("body = %q, want an empty errors array", body)
	}
}

func TestRecentErrorsEndpointRequiresOperator(t *testing.T) {
	h, _, member := adminOperationsAuditFixture(t)
	h.recent.add("provider failure")

	memberResponse := httptest.NewRecorder()
	h.ServeHTTP(memberResponse, authorityRequest(http.MethodGet, "/api/pool/recent-errors", member, ""))
	if memberResponse.Code != http.StatusForbidden {
		t.Fatalf("member status = %d, want 403", memberResponse.Code)
	}

	anonymous := httptest.NewRecorder()
	h.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/pool/recent-errors", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anonymous.Code)
	}

	breakGlass := httptest.NewRequest(http.MethodGet, "/api/pool/recent-errors", nil)
	breakGlass.Header.Set("X-Admin-Token", "break-glass-admin-token")
	operatorResponse := httptest.NewRecorder()
	h.ServeHTTP(operatorResponse, breakGlass)
	if operatorResponse.Code != http.StatusOK {
		t.Fatalf("break-glass status = %d, want 200", operatorResponse.Code)
	}
}

func TestRecentErrorsEndpointRejectsWrites(t *testing.T) {
	h, operator, _ := adminOperationsAuditFixture(t)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, authorityRequest(http.MethodPost, "/api/pool/recent-errors", operator, ""))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}
