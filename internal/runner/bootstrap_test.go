package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

var lanAddress = netip.MustParseAddr("10.77.0.5")

func lanInterfaces() ([]bootstrapInterface, error) {
	return []bootstrapInterface{{Name: "eth0", Flags: net.FlagUp | net.FlagRunning, Hardware: true, Addrs: []netip.Addr{lanAddress}}}, nil
}

// The advertised LAN address is served from loopback; the phone's dialer
// maps it back, so Host checks, the IP SAN and the pin see the real values.
func loopbackListen(netip.Addr) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

func pinnedTLS(pin string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || spkiPin(state.PeerCertificates[0]) != pin {
			return errors.New("bootstrap certificate pin mismatch")
		}
		leaf := state.PeerCertificates[0]
		if now := time.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return errors.New("bootstrap certificate outside its validity")
		}
		return leaf.VerifyHostname(lanAddress.String())
	}}
}

// phone is the app side of the LAN channel, as the Android client does it:
// pinned TLS, the QR secret in its own header, no redirects, no cookies.
type phone struct {
	t       *testing.T
	payload runnerproto.BootstrapPayload
	secret  string
	client  *http.Client
}

func newPhone(t *testing.T, link, pin string) *phone {
	t.Helper()
	payload, err := runnerproto.ParseBootstrapLink(link, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pin == "" {
		pin = payload.TLSSPKISHA256
	}
	transport := &http.Transport{TLSClientConfig: pinnedTLS(pin), DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(address)
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &phone{t: t, payload: payload, secret: payload.BootstrapSecret, client: client}
}

func (p *phone) do(method, path, contentType string, body []byte, header map[string]string) (int, []byte, error) {
	request, err := http.NewRequest(method, p.payload.Endpoints[0]+runnerproto.BootstrapPathPrefix+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if p.secret != "" {
		request.Header.Set(runnerproto.BootstrapSecretHeader, p.secret)
	}
	for k, v := range header {
		if k == "Host" {
			request.Host = v
		} else {
			request.Header.Set(k, v)
		}
	}
	response, err := p.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, runnerproto.BootstrapMaxResponseBytes))
	return response.StatusCode, data, nil
}

func (p *phone) post(path string, value any) (int, []byte) {
	p.t.Helper()
	body, _ := json.Marshal(value)
	status, data, err := p.do(http.MethodPost, path, "application/json", body, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	return status, data
}

func (p *phone) status() runnerproto.BootstrapStatus {
	p.t.Helper()
	status, data, err := p.do(http.MethodGet, "/status", "", nil, nil)
	if err != nil || status != 200 {
		p.t.Fatalf("status = %d %s %v", status, data, err)
	}
	var s runnerproto.BootstrapStatus
	if err := json.Unmarshal(data, &s); err != nil {
		p.t.Fatal(err)
	}
	return s
}

func (p *phone) begin(operation, pool string) (int, runnerproto.BootstrapTicket) {
	p.t.Helper()
	status, data := p.post("/begin", runnerproto.BootstrapBegin{BootstrapID: p.payload.BootstrapID, OperationID: operation, PoolURL: pool})
	var ticket runnerproto.BootstrapTicket
	if status == 200 {
		if err := json.Unmarshal(data, &ticket); err != nil {
			p.t.Fatal(err)
		}
	}
	return status, ticket
}

func operationID(n byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{n + 1}, 32))
}

func openTestBootstrap(t *testing.T, begin func(context.Context, *bootstrapSession, string, string) (runnerproto.BootstrapTicket, error)) *bootstrapSession {
	t.Helper()
	s, err := openBootstrap(context.Background(), bootstrapOptions{now: time.Now, ttl: time.Minute, interfaces: lanInterfaces, listen: loopbackListen, begin: begin})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestBootstrapCandidatesPreferPhysicalPrivateIPv4(t *testing.T) {
	up := net.FlagUp | net.FlagRunning
	addrs := func(values ...string) []netip.Addr {
		var out []netip.Addr
		for _, v := range values {
			out = append(out, netip.MustParseAddr(v))
		}
		return out
	}
	got := bootstrapCandidates([]bootstrapInterface{
		{Name: "lo", Flags: up | net.FlagLoopback, Addrs: addrs("127.0.0.1")},
		{Name: "docker0", Flags: up, Addrs: addrs("172.17.0.1")},
		{Name: "tun0", Flags: up | net.FlagPointToPoint, Addrs: addrs("10.8.0.2")},
		{Name: "wlan1", Flags: net.FlagUp, Hardware: true, Addrs: addrs("192.168.9.9")},
		{Name: "bridge-ish", Flags: up, Addrs: addrs("192.168.50.1")},
		{Name: "wlan0", Flags: up, Hardware: true, Addrs: addrs("fe80::1", "fd00::2", "8.8.8.8", "100.64.0.1", "169.254.0.9", "192.168.1.20")},
		{Name: "eth1", Flags: up, Hardware: true, Addrs: addrs("10.0.0.3", "10.0.0.4", "10.0.0.5", "::ffff:10.0.0.6")},
	})
	want := addrs("192.168.1.20", "10.0.0.3", "10.0.0.4", "10.0.0.5")
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if len(bootstrapCandidates([]bootstrapInterface{{Name: "wlan0", Flags: up, Addrs: addrs("fd00::2")}})) != 0 {
		t.Fatal("IPv6-only network advertised")
	}
}

func TestBootstrapRefusesToStartWithoutALANAddress(t *testing.T) {
	_, err := openBootstrap(context.Background(), bootstrapOptions{now: time.Now, ttl: time.Minute, listen: loopbackListen,
		interfaces: func() ([]bootstrapInterface, error) { return nil, nil }})
	if !errors.Is(err, errNoBootstrapAddress) {
		t.Fatalf("open without LAN = %v", err)
	}
}

func TestBootstrapIdentityPinsOnlyItsCertificate(t *testing.T) {
	now := time.Now()
	a, err := newBootstrapIdentity([]netip.Addr{lanAddress}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newBootstrapIdentity([]netip.Addr{lanAddress}, now, now.Add(time.Hour))
	if a.pin == b.pin || len(a.pin) != 43 {
		t.Fatalf("pins %q %q", a.pin, b.pin)
	}
	leaf := a.certificate.Leaf
	if leaf.VerifyHostname("10.77.0.5") != nil || leaf.VerifyHostname("10.77.0.6") == nil || leaf.IsCA || !leaf.NotAfter.Equal(now.Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("certificate SAN/validity: %v %v %v", leaf.IPAddresses, leaf.IsCA, leaf.NotAfter)
	}
}

func TestBootstrapAuthenticatesBeforeAnything(t *testing.T) {
	var begins atomic.Int32
	s := openTestBootstrap(t, func(context.Context, *bootstrapSession, string, string) (runnerproto.BootstrapTicket, error) {
		begins.Add(1)
		return runnerproto.BootstrapTicket{}, nil
	})
	good := newPhone(t, s.link, "")
	if got := good.status(); got.Phase != runnerproto.BootstrapPhaseWaiting || got.BootstrapID != s.payload.BootstrapID || got.PoolURL != "" {
		t.Fatalf("initial status %+v", got)
	}

	// A different certificate fails the handshake before any header is sent.
	wrongPin := newPhone(t, s.link, strings.Repeat("A", 43))
	if _, _, err := wrongPin.do(http.MethodGet, "/status", "", nil, nil); err == nil || !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("wrong pin: %v", err)
	}
	s.mu.Lock()
	failures := s.failures
	s.mu.Unlock()
	if failures != 0 {
		t.Fatal("the secret reached a server whose pin did not match")
	}

	beginBody, _ := json.Marshal(runnerproto.BootstrapBegin{BootstrapID: s.payload.BootstrapID, OperationID: operationID(0), PoolURL: "https://pool.example"})
	cases := []struct {
		name, method, path, contentType, secret string
		body                                    []byte
		header                                  map[string]string
		want                                    int
	}{
		{"missing secret", http.MethodGet, "/status", "", "-", nil, nil, 401},
		{"wrong secret", http.MethodGet, "/status", "", strings.Repeat("x", 43), nil, nil, 401},
		{"foreign host", http.MethodGet, "/status", "", "", nil, map[string]string{"Host": "evil.example"}, 421},
		{"query string", http.MethodGet, "/status?x=1", "", "", nil, nil, 404},
		{"unknown path", http.MethodGet, "/files", "", "", nil, nil, 404},
		{"wrong method", http.MethodGet, "/begin", "", "", nil, nil, 405},
		{"not JSON", http.MethodPost, "/begin", "text/plain", "", beginBody, nil, 415},
		{"too large", http.MethodPost, "/begin", "application/json", "", bytes.Repeat([]byte(" "), runnerproto.BootstrapMaxRequestBytes+1), nil, 413},
		{"unknown field", http.MethodPost, "/begin", "application/json", "", []byte(`{"bootstrap_id":"x","shell":"id"}`), nil, 400},
		{"two values", http.MethodPost, "/begin", "application/json", "", append(beginBody, beginBody...), nil, 400},
	}
	for _, c := range cases {
		p := *good
		switch c.secret {
		case "-":
			p.secret = ""
		case "":
		default:
			p.secret = c.secret
		}
		status, body, err := p.do(c.method, c.path, c.contentType, c.body, c.header)
		if err != nil || status != c.want {
			t.Errorf("%s = %d %s %v, want %d", c.name, status, body, err, c.want)
		}
	}
	if begins.Load() != 0 {
		t.Fatal("a rejected request reached the pool")
	}
	if got := good.status(); got.Phase != runnerproto.BootstrapPhaseWaiting || got.OperationID != "" {
		t.Fatalf("rejected requests bound the QR: %+v", got)
	}
}

func TestBootstrapBeginBindsOneOperationAndOnePool(t *testing.T) {
	var begins atomic.Int32
	release := make(chan struct{})
	s := openTestBootstrap(t, func(_ context.Context, s *bootstrapSession, pool, operation string) (runnerproto.BootstrapTicket, error) {
		begins.Add(1)
		<-release
		return runnerproto.BootstrapTicket{BootstrapID: s.payload.BootstrapID, OperationID: operation, PoolURL: pool, PairingID: "pairing"}, nil
	})
	p := newPhone(t, s.link, "")
	var wg sync.WaitGroup
	tickets := make(chan runnerproto.BootstrapTicket, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status, ticket := p.begin(operationID(1), "https://POOL.example:443"); status == 200 {
				tickets <- ticket
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(tickets)
	count := 0
	for ticket := range tickets {
		count++
		if ticket.PoolURL != "https://pool.example" || ticket.OperationID != operationID(1) {
			t.Fatalf("ticket %+v", ticket)
		}
	}
	if count != 6 || begins.Load() != 1 {
		t.Fatalf("%d tickets from %d pool contacts, want 6 from 1", count, begins.Load())
	}
	for name, attempt := range map[string][2]string{
		"other operation": {operationID(2), "https://pool.example"},
		"other pool":      {operationID(1), "https://other.example"},
	} {
		if status, _ := p.begin(attempt[0], attempt[1]); status != 409 {
			t.Errorf("%s = %d, want 409", name, status)
		}
	}
	if status, _ := p.begin(operationID(1), "http://pool.example"); status != 400 {
		t.Errorf("plain HTTP pool = %d", status)
	}
	if got := p.status(); got.OperationID != operationID(1) || got.PoolURL != "https://pool.example" {
		t.Fatalf("status %+v", got)
	}
}

func TestBootstrapPoolFailureKeepsTheOperationForARetry(t *testing.T) {
	var calls atomic.Int32
	s := openTestBootstrap(t, func(_ context.Context, s *bootstrapSession, pool, operation string) (runnerproto.BootstrapTicket, error) {
		if calls.Add(1) == 1 {
			return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusBadGateway, Code: runnerproto.BootstrapErrorPoolUnreachable, Message: "pool unreachable"}
		}
		return runnerproto.BootstrapTicket{BootstrapID: s.payload.BootstrapID, OperationID: operation, PoolURL: pool}, nil
	})
	p := newPhone(t, s.link, "")
	status, body := p.post("/begin", runnerproto.BootstrapBegin{BootstrapID: s.payload.BootstrapID, OperationID: operationID(1), PoolURL: "https://pool.example"})
	var failure runnerproto.BootstrapStatus
	_ = json.Unmarshal(body, &failure)
	if status != 502 || failure.Error != runnerproto.BootstrapErrorPoolUnreachable {
		t.Fatalf("failed begin = %d %s", status, body)
	}
	if status, _ := p.begin(operationID(2), "https://pool.example"); status != 409 {
		t.Fatalf("a failed operation released the QR to another: %d", status)
	}
	if status, ticket := p.begin(operationID(1), "https://pool.example"); status != 200 || ticket.OperationID != operationID(1) {
		t.Fatalf("retry = %d", status)
	}
}

func TestBootstrapExpiryCancelAndAcknowledgement(t *testing.T) {
	clock := time.Now()
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	s, err := openBootstrap(context.Background(), bootstrapOptions{now: now, ttl: time.Minute, interfaces: lanInterfaces, listen: loopbackListen,
		begin: func(context.Context, *bootstrapSession, string, string) (runnerproto.BootstrapTicket, error) {
			return runnerproto.BootstrapTicket{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := newPhone(t, s.link, "")
	operation := runnerproto.BootstrapOperation{BootstrapID: s.payload.BootstrapID, OperationID: operationID(1)}
	if status, _ := p.post("/ack", operation); status != 409 {
		t.Fatalf("ack before pairing = %d", status)
	}
	mu.Lock()
	clock = clock.Add(time.Minute)
	mu.Unlock()
	if status, _ := p.begin(operationID(1), "https://pool.example"); status != 410 {
		t.Fatalf("expired QR bound an operation: %d", status)
	}
	if status, _ := p.post("/cancel", operation); status != 204 {
		t.Fatalf("cancel = %d", status)
	}
	select {
	case <-s.cancelled:
	default:
		t.Fatal("cancel did not reach the coordinator")
	}
	if got := p.status(); got.Phase != runnerproto.BootstrapPhaseCancelled {
		t.Fatalf("status after cancel %+v", got)
	}
}

func TestBootstrapRetiresAQRAfterRepeatedWrongSecrets(t *testing.T) {
	s := openTestBootstrap(t, nil)
	p := newPhone(t, s.link, "")
	p.secret = strings.Repeat("x", 43)
	for range bootstrapMaxFailures {
		if status, _, err := p.do(http.MethodGet, "/status", "", nil, nil); err != nil || status != 401 {
			t.Fatal(status, err)
		}
	}
	select {
	case <-s.retired:
	default:
		t.Fatal("QR not retired")
	}
}
