package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func startRecoverablePairing(t *testing.T, b *sessionBroker, principal string) *runnerproto.PairingStart {
	t.Helper()
	p, err := b.beginPairing(runnerproto.PairingRequest{Name: "synthetic runner", OS: "test", Arch: "test", Delivery: runnerproto.PairingDeliveryRecoverable}, nil, "synthetic-ip")
	if err != nil {
		t.Fatal(err)
	}
	if p.Delivery != runnerproto.PairingDeliveryRecoverable {
		t.Fatalf("pool answered delivery %q", p.Delivery)
	}
	if err := b.decidePairing(principal, p.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: p.UserCode}); err != nil {
		t.Fatal(err)
	}
	return p
}

func advanceBroker(b *sessionBroker, by time.Duration) {
	now := b.now().Add(by)
	b.now = func() time.Time { return now }
}

func sessionStatus(err error) int {
	var fault *sessionFault
	if errors.As(err, &fault) {
		return fault.status
	}
	return 0
}

func runnerRecordFor(t *testing.T, b *sessionBroker, token string) *runnerRecord {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/runner/pairings/x/ack", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	record, err := b.runnerCredential(r)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRecoverablePairingReturnsTheSameCredentialUntilAcknowledged(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	p := startRecoverablePairing(t, b, principal)
	first, err := b.completePairing(p.ID, p.DeviceSecret)
	if err != nil {
		t.Fatal(err)
	}
	// The response is lost: the runner polls again with the same device secret.
	advanceBroker(b, 4*time.Second)
	again, err := b.completePairing(p.ID, p.DeviceSecret)
	if err != nil || *again != *first {
		t.Fatalf("recovery = %+v, %v; want the issued credential", again, err)
	}
	runners := 0
	_ = b.store.db.View(func(tx *bbolt.Tx) error {
		_ = tx.Bucket([]byte(bucketRunners)).ForEach(func(_, v []byte) error {
			var r runnerRecord
			if json.Unmarshal(v, &r) == nil && r.PrincipalID == principal {
				runners++
			}
			return nil
		})
		raw := tx.Bucket([]byte(bucketRunnerPairings)).Get([]byte(p.ID))
		if strings.Contains(string(raw), first.Token) || strings.Contains(string(raw), strings.TrimPrefix(first.Token, "pool-runner-"+first.RunnerID+".")) {
			t.Error("receipt stores the credential in plaintext")
		}
		return nil
	})
	if runners != 1 {
		t.Fatalf("recovery created %d runners", runners)
	}
	if _, err := b.completePairing(p.ID, strings.Repeat("x", 43)); sessionStatus(err) != 401 {
		t.Fatalf("recovery without the device secret: %v", err)
	}

	other := pairedRunner(t, b, principal)
	if err := b.acknowledgeReceipt(p.ID, runnerRecordFor(t, b, other.Token)); sessionStatus(err) != 404 {
		t.Fatalf("another runner acknowledged the receipt: %v", err)
	}
	record := runnerRecordFor(t, b, first.Token)
	for range 2 {
		if err := b.acknowledgeReceipt(p.ID, record); err != nil {
			t.Fatalf("acknowledgement: %v", err)
		}
	}
	advanceBroker(b, 4*time.Second)
	if _, err := b.completePairing(p.ID, p.DeviceSecret); sessionStatus(err) != 409 {
		t.Fatalf("acknowledged receipt still recoverable: %v", err)
	}
	_ = b.store.db.View(func(tx *bbolt.Tx) error {
		var stored runnerPairing
		_ = json.Unmarshal(tx.Bucket([]byte(bucketRunnerPairings)).Get([]byte(p.ID)), &stored)
		if len(stored.Receipt) != 0 {
			t.Error("acknowledged receipt kept its sealed credential")
		}
		return nil
	})
}

func TestRecoverablePairingStopsForRevokedRunnersAndExpiredReceipts(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	revoked := startRecoverablePairing(t, b, principal)
	issued, err := b.completePairing(revoked.ID, revoked.DeviceSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.revokeRunner(principal, issued.RunnerID); err != nil {
		t.Fatal(err)
	}
	advanceBroker(b, 4*time.Second)
	if _, err := b.completePairing(revoked.ID, revoked.DeviceSecret); sessionStatus(err) != 403 {
		t.Fatalf("revoked runner recovered its credential: %v", err)
	}

	expiring := startRecoverablePairing(t, b, principal)
	if _, err := b.completePairing(expiring.ID, expiring.DeviceSecret); err != nil {
		t.Fatal(err)
	}
	advanceBroker(b, runnerReceiptTTL+time.Second)
	if _, err := b.completePairing(expiring.ID, expiring.DeviceSecret); sessionStatus(err) != 410 {
		t.Fatalf("expired receipt recovered: %v", err)
	}
}

func TestRecoverablePairingKeepsRotationAndLegacyDeliverySemantics(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	if _, err := b.beginPairing(runnerproto.PairingRequest{Name: "synthetic runner", OS: "test", Arch: "test", Delivery: "replay/9"}, nil, "synthetic-ip"); sessionStatus(err) != 400 {
		t.Fatalf("unknown delivery accepted: %v", err)
	}
	existing := pairedRunner(t, b, principal)
	record := runnerRecordFor(t, b, existing.Token)
	if _, err := b.beginPairing(runnerproto.PairingRequest{Name: "synthetic runner", OS: "test", Arch: "test", Delivery: runnerproto.PairingDeliveryRecoverable}, record, "synthetic-ip"); sessionStatus(err) != 400 {
		t.Fatalf("recoverable rotation accepted: %v", err)
	}
	legacy := startTestPairing(t, b, nil)
	if legacy.Delivery != "" {
		t.Fatalf("legacy pairing answered delivery %q", legacy.Delivery)
	}
	if err := b.decidePairing(principal, legacy.ID, runnerproto.PairingDecision{Decision: "approve", UserCode: legacy.UserCode}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.completePairing(legacy.ID, legacy.DeviceSecret); err != nil {
		t.Fatal(err)
	}
	advanceBroker(b, 4*time.Second)
	if _, err := b.completePairing(legacy.ID, legacy.DeviceSecret); sessionStatus(err) != 409 {
		t.Fatalf("legacy pairing issued twice: %v", err)
	}
}

func TestRecoverablePairingAcknowledgementOverHTTP(t *testing.T) {
	h, b, _, principal := brokerFixture(t)
	p := startRecoverablePairing(t, b, principal)
	issued, err := b.completePairing(p.ID, p.DeviceSecret)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runner/pairings/" + p.ID + "/ack"
	for _, c := range []struct {
		token string
		want  int
	}{{"", 401}, {issued.Token, 204}, {issued.Token, 204}} {
		r := httptest.NewRequest("POST", path, nil)
		if c.token != "" {
			r.Header.Set("Authorization", "Bearer "+c.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Fatalf("ack with token %t = %d %s, want %d", c.token != "", w.Code, w.Body.String(), c.want)
		}
	}
}
