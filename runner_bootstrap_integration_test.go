package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runner"
	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

// TestRunnerBootstrapEndToEnd drives a runner that knows no pool through the
// LAN QR, the real pool's approval and the first WebSocket connection, with
// a Go client standing in for the app.
func TestRunnerBootstrapEndToEnd(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "synthetic-bootstrap-secret")
	h, principal, _, _, _, _ := memberQuotaFixture(t)
	login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.startSessionBroker(ctx); err != nil {
		t.Fatal(err)
	}
	b, _ := h.sessionBroker()
	pool := httptest.NewTLSServer(h)
	defer pool.Close()

	lan := netip.MustParseAddr("192.168.77.10")
	state := filepath.Join(t.TempDir(), "runner.json")
	events := make(chan runner.OnboardingEvent, 32)
	type outcome struct {
		cfg runner.Config
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		cfg, err := runner.Onboard(ctx, runner.OnboardingOptions{StatePath: state, Name: "bootstrap machine", HTTPClient: pool.Client(),
			Report: func(e runner.OnboardingEvent) { events <- e },
			Interfaces: func() ([]runner.BootstrapInterface, error) {
				return []runner.BootstrapInterface{{Name: "wlan0", Flags: net.FlagUp | net.FlagRunning, Hardware: true, Addrs: []netip.Addr{lan}}}, nil
			},
			// The advertised LAN address is served from loopback.
			Listen: func(netip.Addr) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }})
		done <- outcome{cfg, err}
	}()
	var link string
	for link == "" {
		select {
		case e := <-events:
			if e.Kind == runner.OnboardingQR {
				link = e.Link
			}
		case <-ctx.Done():
			t.Fatal("runner showed no QR")
		}
	}
	if strings.Contains(link, strings.TrimPrefix(pool.URL, "https://")) {
		t.Fatal("bootstrap QR names the pool")
	}
	payload, err := runnerproto.ParseBootstrapLink(link, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// The app side: pinned TLS to the QR's certificate, the QR secret as the
	// only credential, and its pool login kept for the pool alone.
	local := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(address)
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
			digest := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if base64.RawURLEncoding.EncodeToString(digest[:]) != payload.TLSSPKISHA256 {
				return errors.New("pin mismatch")
			}
			return cs.PeerCertificates[0].VerifyHostname(lan.String())
		}},
	}}
	call := func(method, path string, body any, into any) int {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		request, _ := http.NewRequestWithContext(ctx, method, payload.Endpoints[0]+runnerproto.BootstrapPathPrefix+path, reader)
		request.Header.Set(runnerproto.BootstrapSecretHeader, payload.BootstrapSecret)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := local.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if into != nil {
			_ = json.NewDecoder(response.Body).Decode(into)
		}
		return response.StatusCode
	}
	operation := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	var ticket runnerproto.BootstrapTicket
	if status := call("POST", "/begin", runnerproto.BootstrapBegin{BootstrapID: payload.BootstrapID, OperationID: operation, PoolURL: pool.URL}, &ticket); status != 200 {
		t.Fatalf("begin = %d", status)
	}
	review := appAuthRequest(h, "GET", "/api/me/runner-pairings/"+ticket.PairingID, login.Token, "")
	var pairing runnerproto.PairingReview
	if review.Code != 200 || json.Unmarshal(review.Body.Bytes(), &pairing) != nil || pairing.Name != ticket.Name || pairing.OS != ticket.OS || pairing.Arch != ticket.Arch {
		t.Fatalf("review = %d %s, ticket %+v", review.Code, review.Body.String(), ticket)
	}
	decision, _ := json.Marshal(runnerproto.PairingDecision{Decision: "approve", UserCode: ticket.UserCode})
	if approved := appAuthRequest(h, "POST", "/api/me/runner-pairings/"+ticket.PairingID, login.Token, string(decision)); approved.Code != 204 {
		t.Fatalf("approve = %d %s", approved.Code, approved.Body.String())
	}
	var status runnerproto.BootstrapStatus
	for status.Phase != runnerproto.BootstrapPhasePaired {
		if call("GET", "/status", nil, &status) != 200 || status.Phase == runnerproto.BootstrapPhaseFailed {
			t.Fatalf("status %+v", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status.CommandKeyID != ticket.CommandKeyID || status.PoolURL != pool.URL {
		t.Fatalf("paired status %+v", status)
	}
	if code := call("POST", "/ack", runnerproto.BootstrapOperation{BootstrapID: payload.BootstrapID, OperationID: operation}, nil); code != 204 {
		t.Fatalf("local ack = %d", code)
	}
	result := <-done
	if result.err != nil || result.cfg.RunnerID != status.RunnerID || result.cfg.CommandKey != ticket.CommandKey || result.cfg.PoolURL != pool.URL {
		t.Fatalf("Onboard = %+v %v", result.cfg, result.err)
	}
	if _, err := local.Get(payload.Endpoints[0] + runnerproto.BootstrapPathPrefix + "/status"); err == nil {
		t.Fatal("LAN service still answers after pairing")
	}

	// The pool dropped its sealed copy and never saw the command key.
	_ = b.store.db.View(func(tx *bbolt.Tx) error {
		var p runnerPairing
		_ = json.Unmarshal(tx.Bucket([]byte(bucketRunnerPairings)).Get([]byte(ticket.PairingID)), &p)
		if p.Decision != "consumed" || len(p.Receipt) != 0 {
			t.Errorf("pairing after acknowledgement: %s receipt=%d", p.Decision, len(p.Receipt))
		}
		return tx.ForEach(func(name []byte, bucket *bbolt.Bucket) error {
			return bucket.ForEach(func(_, v []byte) error {
				if bytes.Contains(v, []byte(ticket.CommandKey)) || bytes.Contains(v, []byte(result.cfg.Token)) {
					t.Errorf("bucket %s stores a runner secret", name)
				}
				return nil
			})
		})
	})

	exe, _ := os.Executable()
	runnerCtx, stop := context.WithCancel(ctx)
	defer stop()
	stopped := make(chan error, 1)
	go func() {
		stopped <- runner.Run(runnerCtx, runner.Options{Config: result.cfg, HTTPClient: pool.Client(), WorkspaceRoot: t.TempDir(), Executables: map[string]string{"codex": exe}})
	}()
	for {
		b.mu.Lock()
		connected := b.runners[result.cfg.RunnerID]
		b.mu.Unlock()
		if connected != nil {
			if connected.hello.CommandKeyID != ticket.CommandKeyID {
				t.Fatalf("runner announced key %q", connected.hello.CommandKeyID)
			}
			break
		}
		select {
		case err := <-stopped:
			t.Fatalf("runner stopped: %v", err)
		case <-ctx.Done():
			t.Fatal("paired runner never connected")
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()
	<-stopped
}
