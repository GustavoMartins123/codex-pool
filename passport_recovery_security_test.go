package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func newRecoveryTestPassport(t *testing.T) *PassportStore {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	return passport
}

func onboardMemberLink(t *testing.T, passport *PassportStore, email string) memberLinkResult {
	t.Helper()
	result, err := passport.createMemberLink("operator", email, "Member", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	return *result
}

// expireMemberLinksFor rewinds the stored expiry of every pending link of the
// principal without going through the store API.
func expireMemberLinksFor(t *testing.T, passport *PassportStore, principalID string) {
	t.Helper()
	err := passport.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketMemberRecoveryLinks))
		type pending struct {
			key  []byte
			link memberRecoveryLink
		}
		var updates []pending
		if err := bucket.ForEach(func(key, value []byte) error {
			var link memberRecoveryLink
			if json.Unmarshal(value, &link) == nil && link.PrincipalID == principalID {
				link.ExpiresAt = time.Now().UTC().Add(-time.Minute)
				updates = append(updates, pending{key: append([]byte(nil), key...), link: link})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, update := range updates {
			if err := putJSON(bucket, string(update.key), &update.link); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemberRecoveryRejectsExpiredLink(t *testing.T) {
	passport := newRecoveryTestPassport(t)
	link := onboardMemberLink(t, passport, "expired@example.com")

	expireMemberLinksFor(t, passport, link.Principal.ID)

	if _, _, _, err := passport.redeemMemberLink(link.Token, "replacement password 1"); err == nil {
		t.Fatal("expired link was redeemed")
	}
	principal := passport.principal(link.Principal.ID)
	if principal.PasswordHash != "" || !principal.PasswordChangedAt.IsZero() {
		t.Fatalf("failed redeem must not touch the password: hash=%q changed=%v", principal.PasswordHash, principal.PasswordChangedAt)
	}
}

func TestMemberRecoveryTokenCannotBeReused(t *testing.T) {
	passport := newRecoveryTestPassport(t)
	link := onboardMemberLink(t, passport, "reuse@example.com")

	if _, _, _, err := passport.redeemMemberLink(link.Token, "first password 123"); err != nil {
		t.Fatalf("first redeem failed: %v", err)
	}
	if _, _, _, err := passport.redeemMemberLink(link.Token, "second password 456"); err == nil {
		t.Fatal("token was redeemed twice")
	}

	if _, _, _, err := passport.login("reuse@example.com", "first password 123"); err != nil {
		t.Fatalf("login with the redeemed password failed: %v", err)
	}
	if _, _, _, err := passport.login("reuse@example.com", "second password 456"); err == nil {
		t.Fatal("login with the second, never-applied password succeeded")
	}
}

func TestMemberRecoveryConcurrentRedeemIsSingleUse(t *testing.T) {
	passport := newRecoveryTestPassport(t)
	link := onboardMemberLink(t, passport, "race@example.com")

	const attempts = 2
	results := make(chan error, attempts)
	var start sync.WaitGroup
	start.Add(1)
	for i := range attempts {
		go func(i int) {
			start.Wait()
			_, _, _, err := passport.redeemMemberLink(link.Token, "racing password "+string(rune('A'+i)))
			results <- err
		}(i)
	}
	start.Done()

	successes, failures := 0, 0
	for range attempts {
		if err := <-results; err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent redeem = %d successes, %d failures; want exactly 1 and 1", successes, failures)
	}
}

func TestPasswordChangeInvalidatesOutstandingRecovery(t *testing.T) {
	passport := newRecoveryTestPassport(t)
	first := onboardMemberLink(t, passport, "rotate@example.com")
	if _, _, _, err := passport.redeemMemberLink(first.Token, "original password 1"); err != nil {
		t.Fatalf("initial redeem failed: %v", err)
	}

	issued, err := passport.createMemberLink("operator", "rotate@example.com", "Member", "recover")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := passport.ChangePrincipalPassword("operator", issued.Principal.ID, "rotated password 2"); err != nil {
		t.Fatalf("centralized password change failed: %v", err)
	}
	if _, _, _, err := passport.redeemMemberLink(issued.Token, "stale recovery 3"); err == nil {
		t.Fatal("recovery issued before the password change stayed redeemable")
	}
	if _, _, _, err := passport.login("rotate@example.com", "rotated password 2"); err != nil {
		t.Fatalf("login with the centralized password failed: %v", err)
	}
}

func TestMemberRecoveryStatusMatrix(t *testing.T) {
	passport := newRecoveryTestPassport(t)
	h := &proxyHandler{cfg: &config{}, passport: passport}

	status := func(token string) (int, string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/auth/recover/status", strings.NewReader(`{"token":"`+token+`"}`))
		recorder := httptest.NewRecorder()
		h.handleMemberRecoveryStatus(recorder, request)
		return recorder.Code, recorder.Body.String()
	}

	// Fresh link: valid with server-controlled expiry metadata only.
	link := onboardMemberLink(t, passport, "status@example.com")
	code, body := status(link.Token)
	if code != http.StatusOK {
		t.Fatalf("valid status = %d: %s", code, body)
	}
	var valid struct {
		Valid     bool      `json:"valid"`
		ExpiresAt time.Time `json:"expires_at"`
		ExpiresIn int64     `json:"expires_in_seconds"`
	}
	if err := json.Unmarshal([]byte(body), &valid); err != nil || !valid.Valid || valid.ExpiresAt.IsZero() || valid.ExpiresIn <= 0 {
		t.Fatalf("valid status payload: %s", body)
	}
	for _, leak := range []string{"status@example.com", "Member", link.Principal.ID, "principal"} {
		if strings.Contains(body, leak) {
			t.Fatalf("valid status leaks %q: %s", leak, body)
		}
	}
	noStoreRecorder := httptest.NewRecorder()
	h.handleMemberRecoveryStatus(noStoreRecorder, httptest.NewRequest(http.MethodPost, "/api/auth/recover/status", strings.NewReader(`{"token":"`+link.Token+`"}`)))
	if noStoreRecorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("status response is not no-store")
	}

	// Unknown token.
	if code, body := status("totally-random-token"); code != http.StatusOK || strings.TrimSpace(body) != `{"valid":false}` {
		t.Fatalf("unknown token status = %d %s", code, body)
	}
	// Malformed body collapses into the same shape.
	recorder := httptest.NewRecorder()
	h.handleMemberRecoveryStatus(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/recover/status", strings.NewReader("not json")))
	if strings.TrimSpace(recorder.Body.String()) != `{"valid":false}` {
		t.Fatalf("malformed status = %s", recorder.Body.String())
	}

	// Expired link (and the lazy cleanup keeps it invalid afterwards).
	expireMemberLinksFor(t, passport, link.Principal.ID)
	for range 2 {
		if code, body := status(link.Token); code != http.StatusOK || strings.TrimSpace(body) != `{"valid":false}` {
			t.Fatalf("expired status = %d %s", code, body)
		}
	}

	// Consumed link.
	consumed := onboardMemberLink(t, passport, "consumed@example.com")
	if _, _, _, err := passport.redeemMemberLink(consumed.Token, "consumed password 1"); err != nil {
		t.Fatal(err)
	}
	if code, body := status(consumed.Token); code != http.StatusOK || strings.TrimSpace(body) != `{"valid":false}` {
		t.Fatalf("consumed status = %d %s", code, body)
	}

	// Replaced link: a fresh recovery retires the pending one.
	insertTestPrincipal(t, passport, "replaced-member", PrincipalMember, "replaced-member", "replaced@example.com")
	first, err := passport.createMemberLink("operator", "replaced@example.com", "Member", "recover")
	if err != nil {
		t.Fatal(err)
	}
	second, err := passport.createMemberLink("operator", "replaced@example.com", "Member", "recover")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := status(first.Token); code != http.StatusOK || strings.TrimSpace(body) != `{"valid":false}` {
		t.Fatalf("replaced status = %d %s", code, body)
	}
	if code, body := status(second.Token); code != http.StatusOK || !strings.Contains(body, `"valid":true`) {
		t.Fatalf("replacement status = %d %s", code, body)
	}

	// Suspended and expired principals.
	for name, mutate := range map[string]func(*Principal){
		"suspended member": func(principal *Principal) { principal.Status = PrincipalSuspended },
		"expired member":   func(principal *Principal) { past := time.Now().Add(-time.Minute); principal.ExpiresAt = &past },
	} {
		email := strings.ReplaceAll(name, " ", "-") + "@example.com"
		local := insertTestPrincipal(t, passport, strings.ReplaceAll(name, " ", "-"), PrincipalMember, name, email)
		issued, err := passport.createMemberLink("operator", email, "Member", "recover")
		if err != nil {
			t.Fatal(err)
		}
		mutate(local)
		if err := passport.db.Update(func(tx *bbolt.Tx) error {
			return putJSON(tx.Bucket([]byte(bucketPrincipals)), local.ID, local)
		}); err != nil {
			t.Fatal(err)
		}
		passport.mu.Lock()
		passport.principals[local.ID] = local
		passport.mu.Unlock()
		if code, body := status(issued.Token); code != http.StatusOK || strings.TrimSpace(body) != `{"valid":false}` {
			t.Fatalf("%s status = %d %s", name, code, body)
		}
	}
}
