package main

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

var testPoolCredentialMu sync.Mutex

// testPoolAuthenticatedHandler gives proxy integration tests real Passport
// principals and clients for their signed request credentials.
func testPoolAuthenticatedHandler(t *testing.T, h *proxyHandler) http.Handler {
	t.Helper()
	if err := ensureTestPassport(t, h); err != nil {
		t.Fatal(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := prepareTestPoolCredential(t, h, r); err != nil {
			t.Errorf("pool credential fixture: %v", err)
			http.Error(w, "pool credential fixture failed", http.StatusInternalServerError)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func testPoolServeHTTP(t *testing.T, h *proxyHandler, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if err := prepareTestPoolCredential(t, h, r); err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)
}

func testPoolProxyRequest(t *testing.T, h *proxyHandler, w http.ResponseWriter, r *http.Request, requestID string) {
	t.Helper()
	if err := prepareTestPoolCredential(t, h, r); err != nil {
		t.Fatal(err)
	}
	h.proxyRequest(w, r, requestID)
}

func ensureTestPassport(t *testing.T, h *proxyHandler) error {
	t.Helper()
	testPoolCredentialMu.Lock()
	defer testPoolCredentialMu.Unlock()
	if h.passport != nil {
		return nil
	}
	store := h.store
	if store == nil {
		store = testUsageStore(t)
	}
	aead, err := passportAEADForSecret("integration-test-passport-key")
	if err != nil {
		return err
	}
	p, err := newPassportStoreWithAEAD(store.db, aead)
	if err != nil {
		return err
	}
	h.passport = p
	return nil
}

func prepareTestPoolCredential(t *testing.T, h *proxyHandler, r *http.Request) error {
	t.Helper()
	if err := ensureTestPassport(t, h); err != nil {
		return err
	}
	secret := getPoolJWTSecret()
	identity, _, kind, ok := parsePoolCredentialRequest(r, secret)
	passthrough := false
	if !ok {
		if provider, _ := looksLikeProviderCredential(r.Header.Get("Authorization")); !provider {
			return nil
		}
		identity = "passthrough-fixture"
		passthrough = true
	} else if kind != "claude" {
		return nil
	}
	testPoolCredentialMu.Lock()
	defer testPoolCredentialMu.Unlock()
	var clientID string
	h.passport.mu.RLock()
	for id, client := range h.passport.clients {
		if client.PrincipalID == identity && client.Label == "integration fixture" {
			clientID = id
			break
		}
	}
	h.passport.mu.RUnlock()
	if clientID == "" {
		principal := h.passport.principal(identity)
		if principal == nil {
			principal = &Principal{ID: identity, Kind: PrincipalMember, Status: PrincipalActive, CreatedAt: time.Now().UTC()}
			if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
				return putJSON(tx.Bucket([]byte(bucketPrincipals)), identity, principal)
			}); err != nil {
				return err
			}
			h.passport.mu.Lock()
			h.passport.principals[identity] = principal
			h.passport.mu.Unlock()
		}
		client, err := h.passport.createClient(identity, "integration fixture", nil)
		if err != nil {
			return err
		}
		clientID = client.ID
	}
	if passthrough {
		if secret == "" {
			session, _, err := h.passport.createSession(identity)
			if err != nil {
				return err
			}
			r.AddCookie(&http.Cookie{Name: "pool_session", Value: session})
		} else {
			r.Header.Set("X-Pool-Token", generateClaudePoolToken(secret, identity+"-c-"+clientID))
		}
		return nil
	}
	issued := generateClaudePoolToken(secret, identity+"-c-"+clientID)
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		r.Header.Set("Authorization", "Bearer "+issued)
	} else if r.Header.Get("X-Api-Key") != "" {
		r.Header.Set("X-Api-Key", issued)
	} else if r.Header.Get("X-Pool-Token") != "" {
		r.Header.Set("X-Pool-Token", issued)
	} else {
		return errors.New("unsupported test credential location")
	}
	return nil
}
