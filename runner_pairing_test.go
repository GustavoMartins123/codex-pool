package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func startTestPairing(t *testing.T, b *sessionBroker, rotation *runnerRecord) *runnerproto.PairingStart {
	t.Helper()
	p, err := b.beginPairing(runnerproto.PairingRequest{Name: "synthetic runner", OS: "test", Arch: "test"}, rotation, "synthetic-ip")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func pairedRunner(t *testing.T, b *sessionBroker, principal string) *runnerproto.RunnerCredential {
	t.Helper()
	p := startTestPairing(t, b, nil)
	if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err != nil {
		t.Fatal(err)
	}
	r, err := b.completePairing(p.ID, p.DeviceSecret)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunnerPairingRequiresMachineSecretAndSingleConsumption(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	p := startTestPairing(t, b, nil)
	if _, err := b.completePairing(p.ID, strings.Repeat("x", 43)); err == nil {
		t.Fatal("wrong machine secret accepted")
	}
	if _, err := b.completePairing(p.ID, p.DeviceSecret); err == nil {
		t.Fatal("unapproved request issued credential")
	}
	if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err != nil {
		t.Fatal(err)
	}
	clock := b.now().Add(4 * time.Second)
	b.now = func() time.Time { return clock }
	var won atomic.Int32
	var token *runnerproto.RunnerCredential
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.completePairing(p.ID, p.DeviceSecret)
			if err == nil {
				won.Add(1)
				mu.Lock()
				token = r
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("credentials issued=%d", won.Load())
	}
	_ = b.store.db.View(func(tx *bbolt.Tx) error {
		for _, bucket := range []string{bucketRunners, bucketRunnerPairings} {
			_ = tx.Bucket([]byte(bucket)).ForEach(func(_, v []byte) error {
				for _, secret := range []string{token.Token, p.DeviceSecret, p.UserCode} {
					if strings.Contains(string(v), secret) {
						t.Fatal("plaintext secret persisted")
					}
				}
				return nil
			})
		}
		return nil
	})
	r := httptest.NewRequest("GET", "/api/runner/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token.Token)
	if _, err := b.runnerCredential(r); err != nil {
		t.Fatal(err)
	}
	if err := b.revokeRunner(principal, token.RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runnerCredential(r); err == nil {
		t.Fatal("revoked token accepted")
	}
}

func TestRunnerPairingExpiryDenialCodeAttemptsAndPollRate(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	p := startTestPairing(t, b, nil)
	for range 5 {
		if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: "incorrect"}); err == nil {
			t.Fatal("wrong code accepted")
		}
	}
	if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err == nil {
		t.Fatal("locked code accepted")
	}
	if _, err := b.completePairing(p.ID, p.DeviceSecret); err == nil {
		t.Fatal("denied request issued token")
	}
	p = startTestPairing(t, b, nil)
	if _, err := b.completePairing(p.ID, p.DeviceSecret); err == nil {
		t.Fatal("pending request accepted")
	}
	if _, err := b.completePairing(p.ID, p.DeviceSecret); err == nil {
		t.Fatal("poll rate not enforced")
	} else if f, ok := err.(*sessionFault); !ok || f.status != 429 {
		t.Fatal(err)
	}
	clock := b.now().Add(6 * time.Minute)
	b.now = func() time.Time { return clock }
	if _, err := b.reviewPairing(principal, p.ID); err == nil {
		t.Fatal("expired request reviewed")
	}
	if _, err := b.completePairing(p.ID, p.DeviceSecret); err == nil {
		t.Fatal("expired request consumed")
	}
}

func TestRunnerPairingRotationPreservesOwnerAndRequiresIdleRunner(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	old := pairedRunner(t, b, principal)
	req := httptest.NewRequest("GET", "/api/runner/ws", nil)
	req.Header.Set("Authorization", "Bearer "+old.Token)
	record, err := b.runnerCredential(req)
	if err != nil {
		t.Fatal(err)
	}
	p := startTestPairing(t, b, record)
	if _, err := b.reviewPairing("other-owner", p.ID); err == nil {
		t.Fatal("cross principal rotation reviewed")
	}
	if err := b.decidePairing("other-owner", p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err == nil {
		t.Fatal("cross principal rotation approved")
	}
	if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err != nil {
		t.Fatal(err)
	}
	updated, err := b.completePairing(p.ID, p.DeviceSecret)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RunnerID != old.RunnerID || updated.Token == old.Token {
		t.Fatal("rotation identity invalid")
	}
	if _, err := b.runnerCredential(req); err == nil {
		t.Fatal("old credential accepted")
	}
	req.Header.Set("Authorization", "Bearer "+updated.Token)
	record, err = b.runnerCredential(req)
	if err != nil {
		t.Fatal(err)
	}
	p = startTestPairing(t, b, record)
	active := &connectedRunner{id: old.RunnerID, principal: principal, hello: runnerproto.Hello{Agents: []string{"claude"}, OS: "test", Arch: "test"}, out: make(chan runnerproto.Message, 128), stop: func() {}}
	if err := b.connectRunner(active); err != nil {
		t.Fatal(err)
	}
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(active, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err == nil {
		t.Fatal("rotation with live session approved")
	}
}

func TestRunnerPairingHTTPAuthAndLegacyEndpointsClosed(t *testing.T) {
	h, b, login, _ := brokerFixture(t)
	p := startTestPairing(t, b, nil)
	path := "/api/me/runner-pairings/" + p.ID
	if r := appAuthRequest(h, "GET", path, "", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
	r := appAuthRequest(h, "GET", path, login.Token, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	for _, secret := range []string{p.DeviceSecret, p.UserCode} {
		if strings.Contains(r.Body.String(), secret) {
			t.Fatal("private field leaked to phone")
		}
	}
	data, _ := json.Marshal(runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode})
	if r := appAuthRequest(h, "POST", path, login.Token, string(data)); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := appAuthRequest(h, "POST", "/api/me/runners", login.Token, ""); r.Code != 405 {
		t.Fatal("legacy mobile enrollment accepted", r.Code)
	}
	if r := appAuthRequest(h, "POST", "/api/runner/enroll", "", `{}`); r.Code < 400 {
		t.Fatal("legacy enrollment accepted")
	}
}
