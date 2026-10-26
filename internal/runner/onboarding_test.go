package runner

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

const (
	fakeRunnerID = "11111111-2222-3333-4444-555555555555"
	fakeSecret   = "dddddddddddddddddddddddddddddddddddddddddd0"
)

// fakePool speaks the pool's pairing endpoints with switches for the
// failures the runner must survive.
type fakePool struct {
	mu        sync.Mutex
	server    *httptest.Server
	delivery  string
	approved  bool
	denied    bool
	dropFirst bool
	dropped   bool
	issued    int
	acks      int
	begins    int
	token     string
}

func newFakePool(t *testing.T) *fakePool {
	p := &fakePool{delivery: runnerproto.PairingDeliveryRecoverable, token: "pool-runner-" + fakeRunnerID + "." + strings.Repeat("t", 43)}
	p.server = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *fakePool) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/runner/pairings")
	switch {
	case path == "":
		var q runnerproto.PairingRequest
		_ = json.NewDecoder(r.Body).Decode(&q)
		p.begins++
		_ = json.NewEncoder(w).Encode(runnerproto.PairingStart{ID: strings.Repeat("p", 43), DeviceSecret: fakeSecret, UserCode: "AbCd1234",
			ExpiresAt: time.Now().Add(5 * time.Minute), Interval: 3, Delivery: p.delivery})
	case strings.HasSuffix(path, "/ack"):
		if r.Header.Get("Authorization") != "Bearer "+p.token {
			w.WriteHeader(401)
			return
		}
		p.acks++
		w.WriteHeader(204)
	default:
		if r.Header.Get("Authorization") != "Bearer "+fakeSecret {
			w.WriteHeader(401)
			return
		}
		switch {
		case p.denied:
			w.WriteHeader(403)
		case !p.approved:
			w.WriteHeader(202)
		default:
			if p.issued == 0 {
				p.issued++
			}
			if p.dropFirst && !p.dropped {
				p.dropped = true
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(runnerproto.RunnerCredential{RunnerID: fakeRunnerID, Token: p.token})
		}
	}
}

func (p *fakePool) set(change func(*fakePool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p)
}

type onboardingRun struct {
	events chan OnboardingEvent
	done   chan struct{}
	cfg    Config
	err    error
	cancel context.CancelFunc
}

func startOnboarding(t *testing.T, pool *fakePool, state string, configure func(*OnboardingOptions)) *onboardingRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	run := &onboardingRun{events: make(chan OnboardingEvent, 32), done: make(chan struct{}), cancel: cancel}
	options := OnboardingOptions{StatePath: state, Name: "synthetic machine", HTTPClient: pool.server.Client(), Report: func(e OnboardingEvent) { run.events <- e },
		interfaces: lanInterfaces, listen: loopbackListen, receiptWait: 5 * time.Second}
	if configure != nil {
		configure(&options)
	}
	go func() {
		defer close(run.done)
		run.cfg, run.err = Onboard(ctx, options)
	}()
	t.Cleanup(func() { cancel(); <-run.done })
	return run
}

func (r *onboardingRun) next(t *testing.T, kind string) OnboardingEvent {
	t.Helper()
	for {
		select {
		case e := <-r.events:
			if e.Kind == kind {
				return e
			}
		case <-r.done:
			t.Fatalf("onboarding ended (%v) before %s", r.err, kind)
		case <-time.After(30 * time.Second):
			t.Fatalf("no %s event", kind)
		}
	}
}

func (p *phone) await(t *testing.T, phase string) runnerproto.BootstrapStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.status(); s.Phase == phase {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("runner never reached %s", phase)
	return runnerproto.BootstrapStatus{}
}

func TestOnboardPairsFromTheLANAndSavesBeforeReportingSuccess(t *testing.T) {
	t.Parallel()
	pool := newFakePool(t)
	pool.set(func(p *fakePool) { p.dropFirst = true })
	state := filepath.Join(t.TempDir(), "runner.json")
	run := startOnboarding(t, pool, state, nil)
	qr := run.next(t, OnboardingQR)
	phone := newPhone(t, qr.Link, "")
	status, ticket := phone.begin(operationID(1), pool.server.URL)
	if status != 200 || ticket.PoolURL != pool.server.URL || ticket.UserCode != "AbCd1234" || ticket.Name != "synthetic machine" {
		t.Fatalf("begin = %d %+v", status, ticket)
	}
	key, err := decodeCommandKey(ticket.CommandKey)
	if err != nil || ticket.CommandKeyID != runnerproto.CommandKeyID(key) {
		t.Fatalf("ticket command key: %v", err)
	}
	if _, err := os.Stat(JournalPath(state)); err != nil {
		t.Fatal("pairing was not journaled before the app could approve it")
	}
	phone.await(t, runnerproto.BootstrapPhaseAwaitingApproval)
	// The app approves through the pool; the first credential response is lost.
	pool.set(func(p *fakePool) { p.approved = true })
	paired := phone.await(t, runnerproto.BootstrapPhasePaired)
	if paired.RunnerID != fakeRunnerID || paired.CommandKeyID != ticket.CommandKeyID {
		t.Fatalf("paired status %+v", paired)
	}
	saved, err := ReadConfig(state)
	if err != nil || saved.Token != pool.token || saved.CommandKey != ticket.CommandKey || saved.PoolURL != pool.server.URL {
		t.Fatalf("saved state %+v %v", saved, err)
	}
	if _, err := os.Stat(JournalPath(state)); !os.IsNotExist(err) {
		t.Fatal("journal kept after the state was saved")
	}
	if status, _ := phone.post("/ack", runnerproto.BootstrapOperation{BootstrapID: ticket.BootstrapID, OperationID: operationID(1)}); status != 204 {
		t.Fatalf("local ack = %d", status)
	}
	select {
	case <-run.done:
	case <-time.After(3 * time.Second):
		t.Fatal("onboarding kept running after the app acknowledged")
	}
	if run.err != nil || run.cfg != saved {
		t.Fatalf("Onboard = %+v %v", run.cfg, run.err)
	}
	pool.set(func(p *fakePool) {
		if !p.dropped || p.issued != 1 || p.acks != 1 || p.begins != 1 {
			t.Errorf("pool saw dropped=%t issued=%d acks=%d begins=%d", p.dropped, p.issued, p.acks, p.begins)
		}
	})
	if _, err := net.DialTimeout("tcp", strings.TrimPrefix(strings.Replace(qr.Endpoints[0], lanAddress.String(), "127.0.0.1", 1), "https://"), time.Second); err == nil {
		t.Fatal("LAN listener still accepts connections")
	}
}

func TestOnboardOffersANewQRAfterDenialOrCancellation(t *testing.T) {
	t.Parallel()
	pool := newFakePool(t)
	pool.set(func(p *fakePool) { p.denied = true })
	state := filepath.Join(t.TempDir(), "runner.json")
	run := startOnboarding(t, pool, state, nil)
	first := run.next(t, OnboardingQR)
	phone := newPhone(t, first.Link, "")
	if status, _ := phone.begin(operationID(1), pool.server.URL); status != 200 {
		t.Fatal(status)
	}
	failed := phone.await(t, runnerproto.BootstrapPhaseFailed)
	if failed.Error != runnerproto.BootstrapErrorPairingDenied {
		t.Fatalf("denial status %+v", failed)
	}
	second := run.next(t, OnboardingQR)
	if second.Link == first.Link {
		t.Fatal("denied QR reused")
	}
	if _, err := os.Stat(JournalPath(state)); !os.IsNotExist(err) {
		t.Fatal("denied pairing left a journal")
	}
	again := newPhone(t, second.Link, "")
	if status, _ := again.post("/cancel", runnerproto.BootstrapOperation{BootstrapID: again.payload.BootstrapID, OperationID: operationID(2)}); status != 204 {
		t.Fatal(status)
	}
	if third := run.next(t, OnboardingQR); third.Link == second.Link {
		t.Fatal("cancelled QR reused")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("state written without a credential")
	}
}

func TestOnboardRefusesPoolsThatCannotRecoverCredentials(t *testing.T) {
	t.Parallel()
	pool := newFakePool(t)
	pool.set(func(p *fakePool) { p.delivery = "" })
	run := startOnboarding(t, pool, filepath.Join(t.TempDir(), "runner.json"), nil)
	phone := newPhone(t, run.next(t, OnboardingQR).Link, "")
	status, body := phone.post("/begin", runnerproto.BootstrapBegin{BootstrapID: phone.payload.BootstrapID, OperationID: operationID(1), PoolURL: pool.server.URL})
	var failure runnerproto.BootstrapStatus
	_ = json.Unmarshal(body, &failure)
	if status != 502 || failure.Error != runnerproto.BootstrapErrorPoolIncompatible {
		t.Fatalf("legacy pool begin = %d %s", status, body)
	}
}

func TestOnboardFinishesAJournaledPairingWithoutTheLAN(t *testing.T) {
	t.Parallel()
	pool := newFakePool(t)
	pool.set(func(p *fakePool) { p.approved = true })
	state := filepath.Join(t.TempDir(), "runner.json")
	key, _ := NewCommandKey()
	if err := writeJournal(JournalPath(state), pairingJournal{PoolURL: pool.server.URL, Name: "synthetic machine", PairingID: strings.Repeat("p", 43),
		DeviceSecret: fakeSecret, CommandKey: key, Interval: 3, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	run := startOnboarding(t, pool, state, func(o *OnboardingOptions) {
		o.interfaces = func() ([]bootstrapInterface, error) {
			t.Error("recovery opened the LAN service")
			return nil, nil
		}
	})
	run.next(t, OnboardingRecovering)
	<-run.done
	if run.err != nil || run.cfg.CommandKey != key || run.cfg.Token != pool.token {
		t.Fatalf("recovery = %+v %v", run.cfg, run.err)
	}
	if _, err := os.Stat(JournalPath(state)); !os.IsNotExist(err) {
		t.Fatal("journal kept after recovery")
	}
	pool.set(func(p *fakePool) {
		if p.acks != 1 || p.begins != 0 {
			t.Errorf("recovery acks=%d begins=%d", p.acks, p.begins)
		}
	})
}

func TestOnboardAbandonsExpiredJournalsAndKeepsCorruptOnes(t *testing.T) {
	t.Parallel()
	pool := newFakePool(t)
	state := filepath.Join(t.TempDir(), "runner.json")
	key, _ := NewCommandKey()
	_ = writeJournal(JournalPath(state), pairingJournal{PoolURL: pool.server.URL, Name: "synthetic machine", PairingID: strings.Repeat("p", 43),
		DeviceSecret: fakeSecret, CommandKey: key, Interval: 3, ExpiresAt: time.Now().Add(-6 * time.Minute)})
	run := startOnboarding(t, pool, state, nil)
	if abandoned := run.next(t, OnboardingAbandoned); abandoned.Err == nil || abandoned.Name != "synthetic machine" {
		t.Fatalf("abandoned event %+v", abandoned)
	}
	run.next(t, OnboardingQR)
	run.cancel()
	<-run.done

	corrupt := filepath.Join(t.TempDir(), "runner.json")
	_ = os.WriteFile(JournalPath(corrupt), []byte("{not json"), 0600)
	_, err := Onboard(context.Background(), OnboardingOptions{StatePath: corrupt, interfaces: lanInterfaces, listen: loopbackListen})
	if err == nil || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("corrupt journal = %v", err)
	}
	if raw, _ := os.ReadFile(JournalPath(corrupt)); string(raw) != "{not json" {
		t.Fatal("corrupt journal overwritten")
	}
}

func TestDefaultMachineNameFitsThePool(t *testing.T) {
	name := DefaultMachineName()
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\x00\r\n") {
		t.Fatalf("machine name %q", name)
	}
}
