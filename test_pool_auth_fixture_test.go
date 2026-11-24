package main

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-pool-proxy/internal/clientip"
	"codex-pool-proxy/internal/requestlog"
	"go.etcd.io/bbolt"
)

var testPoolCredentialMu sync.Mutex

func testProxyHTTPApp(h *proxyHandler) http.Handler {
	routes := &proxyRoutes{proxyHandler: h, runtime: requestlog.System()}
	return routes.newHTTPApp()
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	testProxyHTTPApp(h).ServeHTTP(w, r)
}

// testPoolAuthenticatedHandler gives proxy integration tests real Passport
// principals and clients for their signed request credentials.
func testPoolAuthenticatedHandler(t *testing.T, h *proxyHandler) http.Handler {
	t.Helper()
	if err := ensureTestPassport(t, h); err != nil {
		t.Fatal(err)
	}
	app := testProxyHTTPApp(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hijacked websocket handlers outlive the test, so never touch t here.
		if err := prepareTestPoolCredentialRequest(h, r); err != nil {
			http.Error(w, "pool credential fixture failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		app.ServeHTTP(w, r)
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
	r = clientip.With(r, testResolvedClientIP(t, h.ipAccess.Snapshot(), r))
	h.proxyRequest(w, r, requestID, requestlog.System())
}

func ensureTestPassport(t *testing.T, h *proxyHandler) error {
	t.Helper()
	testPoolCredentialMu.Lock()
	defer testPoolCredentialMu.Unlock()
	if h.contextHandoff == nil {
		h.contextHandoff = newConversationHandoffStore()
	}
	if h.requestQueue == nil {
		h.requestQueue = newFairQueue()
	}
	if h.routeTraces == nil {
		h.routeTraces = newRouteTraceStore(2048)
	}
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
	h.passportConfig.jwtSecret = getenv("POOL_JWT_SECRET", "")
	h.passport = p
	return nil
}

// testPoolIdentity returns the credential identity the fixture mints for
// subject, so tests can pre-seed owner-scoped state.
func testPoolIdentity(t *testing.T, h *proxyHandler, subject string) string {
	t.Helper()
	if err := ensureTestPassport(t, h); err != nil {
		t.Fatal(err)
	}
	testPoolCredentialMu.Lock()
	defer testPoolCredentialMu.Unlock()
	clientID, err := testPoolFixtureClientIDLocked(h, subject)
	if err != nil {
		t.Fatal(err)
	}
	return subject + "-c-" + clientID
}

func testPoolFixtureClientIDLocked(h *proxyHandler, identity string) (string, error) {
	var clientID string
	h.passport.mu.RLock()
	for id, client := range h.passport.clients {
		if client.PrincipalID == identity && client.Label == "integration fixture" {
			clientID = id
			break
		}
	}
	h.passport.mu.RUnlock()
	if clientID != "" {
		return clientID, nil
	}
	principal := h.passport.principal(identity)
	if principal == nil {
		principal = &Principal{ID: identity, Kind: PrincipalMember, Status: PrincipalActive, CreatedAt: time.Now().UTC()}
		if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
			return putJSON(tx.Bucket([]byte(bucketPrincipals)), identity, principal)
		}); err != nil {
			return "", err
		}
		h.passport.mu.Lock()
		h.passport.principals[identity] = principal
		h.passport.mu.Unlock()
	}
	client, err := h.passport.createClient(identity, "integration fixture", nil)
	if err != nil {
		return "", err
	}
	return client.ID, nil
}

func prepareTestPoolCredential(t *testing.T, h *proxyHandler, r *http.Request) error {
	t.Helper()
	if err := ensureTestPassport(t, h); err != nil {
		return err
	}
	return prepareTestPoolCredentialRequest(h, r)
}

func prepareTestPoolCredentialRequest(h *proxyHandler, r *http.Request) error {
	if h.passport == nil {
		return errors.New("test passport not initialized")
	}
	secret := h.passportConfig.jwtSecret
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
	clientID, err := testPoolFixtureClientIDLocked(h, identity)
	if err != nil {
		return err
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
