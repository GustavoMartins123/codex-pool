package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGuestPassesFollowSponsorStatus(t *testing.T) {
	p, operator := testPassportWithOperator(t)
	member := insertTestPrincipal(t, p, "sponsor-member", PrincipalMember, "sponsor", "sponsor@example.com")
	expires := time.Now().Add(24 * time.Hour)
	guest, _, client, joinToken, err := p.createGuest(member.ID, "friend", "Friend", &expires)
	if err != nil {
		t.Fatal(err)
	}
	benched, _, _, _, err := p.createGuest(member.ID, "benched friend", "Benched", &expires)
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

func TestMemberGuestPassesAreBounded(t *testing.T) {
	passport, alpha, _ := newPassportWithTwoMembers(t)
	now := time.Now()
	soon, tooLate := now.Add(24*time.Hour), now.Add(maxGuestPassLifetime+time.Hour)
	if _, _, _, _, err := passport.createGuest(alpha.ID, "forever", "F", nil); err == nil {
		t.Fatal("member created a pass without expiry")
	}
	if _, _, _, _, err := passport.createGuest(alpha.ID, "too long", "L", &tooLate); err == nil {
		t.Fatal("member created a pass beyond the maximum lifetime")
	}
	var first *Principal
	for i := 0; i < maxActiveGuestPassesPerMember; i++ {
		guest, _, _, _, err := passport.createGuest(alpha.ID, "friend", "F", &soon)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if first == nil {
			first = guest
		}
	}
	if _, _, _, _, err := passport.createGuest(alpha.ID, "one too many", "M", &soon); err == nil {
		t.Fatal("member exceeded the active pass cap")
	}
	if _, err := passport.updateGuestPass(alpha, first.ID, "friend", "F", nil); err == nil {
		t.Fatal("member removed a pass expiry")
	}
	if _, err := passport.setGuestPassStatus(alpha, first.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := passport.createGuest(alpha.ID, "replacement", "R", &soon); err != nil {
		t.Fatalf("revoking a pass did not free a slot: %v", err)
	}
	if _, err := passport.setGuestPassStatus(alpha, first.ID, true); err == nil {
		t.Fatal("restoring a pass exceeded the active pass cap")
	}
	operator := insertTestPrincipal(t, passport, "pass-operator", PrincipalOperator, "passop", "passop@example.com")
	if _, _, _, _, err := passport.createGuest(operator.ID, "operator guest", "O", nil); err != nil {
		t.Fatalf("operator passes must stay unbounded: %v", err)
	}
}
