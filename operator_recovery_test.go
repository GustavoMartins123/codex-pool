package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConsoleRecoveryOnlyTargetsMembers(t *testing.T) {
	passport, actor := testPassportWithOperator(t)
	sessionToken, csrf, err := passport.createSession(actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := authoritySession{token: sessionToken, csrf: csrf}
	insertTestPrincipal(t, passport, "member-target", PrincipalMember, "member-target", "member-target@example.com")
	insertTestPrincipal(t, passport, "operator-target", PrincipalOperator, "operator-target", "operator-target@example.com")
	h := &proxyHandler{cfg: &config{}, passport: passport}
	for _, test := range []struct {
		email string
		want  int
	}{
		{"member-target@example.com", http.StatusOK},
		{"operator-target@example.com", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, authorityRequest(http.MethodPost, "/api/console/members", session,
			`{"email":"`+test.email+`","purpose":"recover"}`))
		if response.Code != test.want {
			t.Fatalf("recovery for %s = %d, want %d: %s", test.email, response.Code, test.want, response.Body.String())
		}
	}
}
