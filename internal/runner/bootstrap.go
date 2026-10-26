package runner

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

const (
	bootstrapMaxConnections = 8
	// A QR that collects this many wrong secrets is retired for a new one.
	bootstrapMaxFailures = 64
)

// bootstrapError is a failure the app can show. Pool failures leave the
// operation bound, so the app may repeat it.
type bootstrapError struct {
	Status  int
	Code    string
	Message string
}

func (e *bootstrapError) Error() string { return e.Message }

type bootstrapOptions struct {
	now        func() time.Time
	ttl        time.Duration
	interfaces func() ([]bootstrapInterface, error)
	listen     func(netip.Addr) (net.Listener, error)
	// begin contacts the pool for a newly bound operation. It runs at most
	// once at a time; a failure leaves the operation bound for a retry.
	begin func(ctx context.Context, session *bootstrapSession, pool, operation string) (runnerproto.BootstrapTicket, error)
}

// bootstrapSession is the temporary LAN service behind one QR. It accepts
// only the app holding the QR secret, binds to the first pool origin it
// receives, and exposes nothing but the onboarding of this runner.
type bootstrapSession struct {
	payload  runnerproto.BootstrapPayload
	link     string
	hosts    map[string]bool
	deadline time.Time
	options  bootstrapOptions
	ctx      context.Context
	stop     context.CancelFunc
	servers  []*http.Server

	mu        sync.Mutex
	operation string
	pool      string
	ticket    *runnerproto.BootstrapTicket
	inflight  chan struct{}
	status    runnerproto.BootstrapStatus
	failures  int
	bound     chan struct{}
	acked     chan struct{}
	cancelled chan struct{}
	retired   chan struct{}
}

func openBootstrap(ctx context.Context, options bootstrapOptions) (*bootstrapSession, error) {
	interfaces, err := options.interfaces()
	if err != nil {
		return nil, err
	}
	addrs := bootstrapCandidates(interfaces)
	if len(addrs) == 0 {
		return nil, errNoBootstrapAddress
	}
	now := options.now()
	deadline := now.Add(options.ttl)
	id, err := bootstrapToken()
	if err != nil {
		return nil, err
	}
	secret, err := bootstrapToken()
	if err != nil {
		return nil, err
	}
	// The certificate outlives the QR so a bound operation can finish.
	identity, err := newBootstrapIdentity(addrs, now, deadline.Add(runnerReceiptWindow()))
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(ctx)
	s := &bootstrapSession{deadline: deadline, options: options, ctx: ctx, stop: stop, hosts: map[string]bool{},
		bound: make(chan struct{}), acked: make(chan struct{}), cancelled: make(chan struct{}), retired: make(chan struct{})}
	s.payload = runnerproto.BootstrapPayload{Version: runnerproto.BootstrapVersion, BootstrapID: id, TLSSPKISHA256: identity.pin, BootstrapSecret: secret, ExpiresAt: deadline.UTC().Truncate(time.Millisecond)}
	s.status = runnerproto.BootstrapStatus{BootstrapID: id, Phase: runnerproto.BootstrapPhaseWaiting}
	config := identity.serverConfig()
	for _, addr := range addrs {
		listener, err := options.listen(addr)
		if err != nil {
			continue
		}
		port := listener.Addr().(*net.TCPAddr).Port
		endpoint := netip.AddrPortFrom(addr, uint16(port)).String()
		s.hosts[endpoint] = true
		s.payload.Endpoints = append(s.payload.Endpoints, "https://"+endpoint)
		server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second,
			IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
			ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
		s.servers = append(s.servers, server)
		go server.Serve(tls.NewListener(limitListener(listener, bootstrapMaxConnections), config))
	}
	if len(s.servers) == 0 {
		stop()
		return nil, errors.New("could not listen on any LAN address for pairing")
	}
	s.link, err = runnerproto.BootstrapLink(s.payload)
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func runnerReceiptWindow() time.Duration {
	// The pool keeps a pairing for 5 minutes and an issued credential for 5
	// more; the confirmation window follows.
	return 10*time.Minute + runnerproto.BootstrapReceiptSeconds*time.Second
}

// Close stops listening. Nothing else about the session stays reachable.
func (s *bootstrapSession) Close() {
	s.stop()
	for _, server := range s.servers {
		_ = server.Close()
	}
}

func (s *bootstrapSession) update(change func(*runnerproto.BootstrapStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.status)
}

func (s *bootstrapSession) phase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status.Phase
}

func (s *bootstrapSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.hosts[r.Host] {
		bootstrapFail(w, http.StatusMisdirectedRequest, "unexpected host")
		return
	}
	if !s.authenticate(r) {
		bootstrapFail(w, http.StatusUnauthorized, "bootstrap authentication required")
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, runnerproto.BootstrapPathPrefix+"/")
	if !ok || r.URL.RawQuery != "" {
		bootstrapFail(w, http.StatusNotFound, "not found")
		return
	}
	method := map[string]string{"begin": http.MethodPost, "status": http.MethodGet, "ack": http.MethodPost, "cancel": http.MethodPost}[path]
	if method == "" {
		bootstrapFail(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		bootstrapFail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	switch path {
	case "status":
		s.mu.Lock()
		status := s.status
		s.mu.Unlock()
		bootstrapRespond(w, http.StatusOK, status)
	case "begin":
		var q runnerproto.BootstrapBegin
		if !bootstrapDecode(w, r, &q) {
			return
		}
		ticket, err := s.begin(r.Context(), q)
		if err != nil {
			var failure *bootstrapError
			if !errors.As(err, &failure) {
				failure = &bootstrapError{Status: http.StatusInternalServerError, Message: "pairing could not start"}
			}
			bootstrapRespond(w, failure.Status, runnerproto.BootstrapStatus{BootstrapID: s.payload.BootstrapID, Phase: s.phase(), Error: failure.Code, Message: failure.Message})
			return
		}
		bootstrapRespond(w, http.StatusOK, ticket)
	case "ack", "cancel":
		var q runnerproto.BootstrapOperation
		if !bootstrapDecode(w, r, &q) {
			return
		}
		if status, message := s.finish(path, q); status != http.StatusNoContent {
			bootstrapFail(w, status, message)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *bootstrapSession) authenticate(r *http.Request) bool {
	values := r.Header.Values(runnerproto.BootstrapSecretHeader)
	ok := len(values) == 1 && subtle.ConstantTimeCompare([]byte(values[0]), []byte(s.payload.BootstrapSecret)) == 1
	if ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	if s.failures == bootstrapMaxFailures {
		close(s.retired)
	}
	return false
}

func (s *bootstrapSession) begin(ctx context.Context, q runnerproto.BootstrapBegin) (runnerproto.BootstrapTicket, error) {
	if q.BootstrapID != s.payload.BootstrapID || !validBootstrapToken(q.OperationID) {
		return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusBadRequest, Message: "invalid bootstrap operation"}
	}
	pool, err := normalizeOrigin(q.PoolURL)
	if err != nil {
		return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	for {
		s.mu.Lock()
		if s.status.Phase == runnerproto.BootstrapPhaseCancelled {
			s.mu.Unlock()
			return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusConflict, Message: "pairing was cancelled"}
		}
		switch {
		case s.operation == "" && !s.options.now().Before(s.deadline):
			s.mu.Unlock()
			return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusGone, Code: runnerproto.BootstrapErrorPairingExpired, Message: "this QR expired; scan the new one"}
		case s.operation == "":
			s.operation, s.pool = q.OperationID, pool
			s.status.OperationID, s.status.PoolURL = q.OperationID, pool
			close(s.bound)
		case s.operation != q.OperationID || s.pool != pool:
			s.mu.Unlock()
			return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusConflict, Message: "this QR is already pairing with another request"}
		}
		if s.ticket != nil {
			ticket := *s.ticket
			s.mu.Unlock()
			return ticket, nil
		}
		if wait := s.inflight; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return runnerproto.BootstrapTicket{}, ctx.Err()
			}
		}
		done := make(chan struct{})
		s.inflight = done
		s.mu.Unlock()
		// A phone that disconnects mid-request must not abandon a pairing
		// the pool already created, so the pool call outlives the request.
		beginCtx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
		ticket, err := s.options.begin(beginCtx, s, pool, q.OperationID)
		cancel()
		s.mu.Lock()
		s.inflight = nil
		close(done)
		if err == nil {
			s.ticket = &ticket
		}
		s.mu.Unlock()
		return ticket, err
	}
}

func (s *bootstrapSession) finish(kind string, q runnerproto.BootstrapOperation) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q.BootstrapID != s.payload.BootstrapID || !validBootstrapToken(q.OperationID) || s.operation != "" && q.OperationID != s.operation {
		return http.StatusConflict, "operation does not match this QR"
	}
	switch kind {
	case "ack":
		if s.status.Phase != runnerproto.BootstrapPhasePaired {
			return http.StatusConflict, "runner is not paired yet"
		}
		select {
		case <-s.acked:
		default:
			close(s.acked)
		}
	case "cancel":
		if s.status.Phase == runnerproto.BootstrapPhasePaired {
			return http.StatusConflict, "runner is already paired"
		}
		if s.status.Phase != runnerproto.BootstrapPhaseCancelled {
			s.status.Phase = runnerproto.BootstrapPhaseCancelled
			close(s.cancelled)
		}
	}
	return http.StatusNoContent, ""
}

func bootstrapDecode(w http.ResponseWriter, r *http.Request, value any) bool {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		bootstrapFail(w, http.StatusUnsupportedMediaType, "JSON body required")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, runnerproto.BootstrapMaxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			bootstrapFail(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			bootstrapFail(w, http.StatusBadRequest, "invalid request JSON")
		}
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		bootstrapFail(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func bootstrapRespond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func bootstrapFail(w http.ResponseWriter, status int, message string) {
	bootstrapRespond(w, status, map[string]string{"message": message})
}

func bootstrapToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validBootstrapToken(value string) bool {
	if len(value) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil
}

// normalizeOrigin accepts the pool origin the app sends and returns the
// spelling the runner stores: lowercase host, no default port.
func normalizeOrigin(raw string) (string, error) {
	if err := ValidateOrigin(raw); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != "443" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid pool port")
		}
		host += ":" + port
	}
	return "https://" + host, nil
}

// limitListener bounds concurrent connections, TLS handshakes included.
func limitListener(l net.Listener, n int) net.Listener {
	return &limitedListener{Listener: l, slots: make(chan struct{}, n)}
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
