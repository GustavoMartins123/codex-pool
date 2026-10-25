package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuestPassesFollowSponsorStatus(t *testing.T) {
	p, operator := testPassportWithOperator(t)
	member := insertTestPrincipal(t, p, "sponsor-member", PrincipalMember, "sponsor", "sponsor@example.com")
	guest, _, client, joinToken, err := p.createGuest(member.ID, "friend", "Friend", nil)
	if err != nil {
		t.Fatal(err)
	}
	benched, _, _, _, err := p.createGuest(member.ID, "benched friend", "Benched", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.setPrincipalStatus(operator.ID, benched.ID, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	identity := guest.ID + "-c-" + client.ID
	_, session, _, err := p.redeemJoin(joinToken)
	if err != nil {
		t.Fatal(err)
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	sessionRequest.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
	usable := func() (credential, browser, join bool) {
		_, _, credential = p.credentialState(identity)
		viewer, _ := p.authenticate(sessionRequest)
		_, _, _, joinErr := p.redeemJoin(joinToken)
		return credential, viewer != nil, joinErr == nil
	}

	if credential, browser, join := usable(); !credential || !browser || !join {
		t.Fatalf("active sponsor: credential=%v browser=%v join=%v, want all usable", credential, browser, join)
	}
	if _, err := p.setPrincipalStatus(operator.ID, member.ID, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	if credential, browser, join := usable(); credential || browser || join {
		t.Fatalf("suspended sponsor: credential=%v browser=%v join=%v, want none usable", credential, browser, join)
	}
	if _, err := p.setPrincipalStatus(operator.ID, member.ID, PrincipalActive); err != nil {
		t.Fatal(err)
	}
	if credential, browser, join := usable(); !credential || !browser || !join {
		t.Fatalf("reactivated sponsor: credential=%v browser=%v join=%v, want all usable", credential, browser, join)
	}
	if got := p.principal(benched.ID); got == nil || got.Status != PrincipalSuspended {
		t.Fatal("reactivating the sponsor revived an individually suspended guest pass")
	}
}
