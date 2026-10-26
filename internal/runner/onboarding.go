package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"codex-pool-proxy/internal/runnerproto"
)

// Onboarding event kinds, in the order a successful pairing reports them.
const (
	OnboardingRecovering = "recovering"
	OnboardingQR         = "qr"
	OnboardingPairing    = "pairing"
	OnboardingPaired     = "paired"
	// OnboardingRetrying follows a failed attempt; a new QR comes next.
	OnboardingRetrying = "retrying"
	// OnboardingAbandoned reports an earlier attempt the pool no longer
	// remembers: if the app approved it, an unused runner may need revoking.
	OnboardingAbandoned = "abandoned"
)

// OnboardingEvent tells the presentation (today the terminal) what the
// runner is doing, so a GUI can later show the same flow.
type OnboardingEvent struct {
	Kind      string
	Link      string
	Endpoints []string
	ExpiresAt time.Time
	PoolURL   string
	Name      string
	UserCode  string
	RunnerID  string
	Err       error
}

type OnboardingOptions struct {
	StatePath  string
	Name       string
	HTTPClient *http.Client
	Report     func(OnboardingEvent)

	// Interfaces and Listen default to the host's network; tests and future
	// transports replace them.
	Interfaces func() ([]BootstrapInterface, error)
	Listen     func(netip.Addr) (net.Listener, error)

	now         func() time.Time
	ttl         time.Duration
	receiptWait time.Duration
}

// DefaultMachineName is the host name, cut to what the pool accepts.
func DefaultMachineName() string {
	host, _ := os.Hostname()
	name := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, host))
	for len(name) > 80 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		name = "runner-" + runtime.GOOS
	}
	return name
}

func (o *OnboardingOptions) defaults() {
	if o.Name == "" {
		o.Name = DefaultMachineName()
	}
	if o.Report == nil {
		o.Report = func(OnboardingEvent) {}
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.ttl == 0 {
		o.ttl = runnerproto.BootstrapTTLSeconds * time.Second
	}
	if o.receiptWait == 0 {
		o.receiptWait = runnerproto.BootstrapReceiptSeconds * time.Second
	}
	if o.Interfaces == nil {
		o.Interfaces = localBootstrapInterfaces
	}
	if o.Listen == nil {
		o.Listen = func(addr netip.Addr) (net.Listener, error) {
			return net.Listen("tcp", netip.AddrPortFrom(addr, 0).String())
		}
	}
}

// retryOnboarding marks attempts that end with a fresh QR rather than an
// error: expiry, cancellation, denial.
type retryOnboarding struct{ err error }

func (r *retryOnboarding) Error() string { return r.err.Error() }

var errJournalAbandoned = errors.New("the previous pairing attempt expired before this runner saved its credential; if the app lists an unused runner with this name, revoke it")

// Onboard pairs a runner that has no state: it shows a LAN QR, learns the
// pool from the app over the pinned channel, pairs with that pool and saves
// the credential. It first finishes an attempt a crash interrupted.
func Onboard(ctx context.Context, o OnboardingOptions) (Config, error) {
	o.defaults()
	if _, err := os.Stat(o.StatePath); !errors.Is(err, os.ErrNotExist) {
		return Config{}, errors.New("runner state already exists")
	}
	journal := JournalPath(o.StatePath)
	j, err := readJournal(journal)
	switch {
	case err == nil:
		cfg, err := o.resume(ctx, j)
		if !errors.Is(err, errJournalAbandoned) {
			return cfg, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return Config{}, fmt.Errorf("%w; remove %s to pair again", err, journal)
	}
	for {
		cfg, err := o.attempt(ctx)
		var retry *retryOnboarding
		if err == nil || ctx.Err() != nil || !errors.As(err, &retry) {
			if ctx.Err() != nil {
				return Config{}, ctx.Err()
			}
			return cfg, err
		}
		o.Report(OnboardingEvent{Kind: OnboardingRetrying, Err: retry.err})
	}
}

type pairingAttempt struct {
	journal pairingJournal
	start   runnerproto.PairingStart
}

func (o *OnboardingOptions) attempt(ctx context.Context) (Config, error) {
	started := make(chan pairingAttempt, 1)
	session, err := openBootstrap(ctx, bootstrapOptions{now: o.now, ttl: o.ttl, interfaces: o.Interfaces, listen: o.Listen,
		begin: func(ctx context.Context, s *bootstrapSession, pool, operation string) (runnerproto.BootstrapTicket, error) {
			return o.begin(ctx, s, pool, operation, started)
		}})
	if err != nil {
		return Config{}, err
	}
	defer session.Close()
	o.Report(OnboardingEvent{Kind: OnboardingQR, Link: session.link, Endpoints: session.payload.Endpoints, ExpiresAt: session.deadline, Name: o.Name})
	expired := time.NewTimer(session.deadline.Sub(o.now()))
	defer expired.Stop()
	var attempt pairingAttempt
	select {
	case attempt = <-started:
	case <-expired.C:
		// The deadline only stops new bindings; a bound operation may still
		// be waiting for the pool.
		select {
		case <-session.bound:
		default:
			return Config{}, &retryOnboarding{errors.New("pairing QR expired")}
		}
		select {
		case attempt = <-started:
		case <-time.After(50 * time.Second):
			return Config{}, &retryOnboarding{errors.New("the pool did not answer the pairing request")}
		case <-session.cancelled:
			return Config{}, &retryOnboarding{errors.New("pairing cancelled in the app")}
		case <-ctx.Done():
			return Config{}, ctx.Err()
		}
	case <-session.retired:
		return Config{}, &retryOnboarding{errors.New("pairing QR received too many wrong secrets")}
	case <-session.cancelled:
		return Config{}, &retryOnboarding{errors.New("pairing cancelled in the app")}
	case <-ctx.Done():
		return Config{}, ctx.Err()
	}
	o.Report(OnboardingEvent{Kind: OnboardingPairing, PoolURL: attempt.journal.PoolURL, Name: o.Name, UserCode: attempt.start.UserCode, ExpiresAt: attempt.start.ExpiresAt})
	poll, stopPolling := context.WithCancel(ctx)
	defer stopPolling()
	go func() {
		select {
		case <-session.cancelled:
			stopPolling()
		case <-poll.Done():
		}
	}()
	cfg, err := AwaitPairing(poll, o.HTTPClient, attempt.journal.PoolURL, attempt.start)
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down: the journal lets the next start finish this pairing.
			return Config{}, ctx.Err()
		}
		code := runnerproto.BootstrapErrorPoolRejected
		var rejected *PairingRejected
		switch {
		case poll.Err() != nil:
			err = errors.New("pairing cancelled in the app")
		case errors.As(err, &rejected) && rejected.Status == http.StatusForbidden:
			code = runnerproto.BootstrapErrorPairingDenied
		case errors.As(err, &rejected) && rejected.Status == http.StatusGone, strings.Contains(err.Error(), "expired"):
			code = runnerproto.BootstrapErrorPairingExpired
		}
		// Nothing was issued, or the pool already refuses to hand it out.
		_ = os.Remove(JournalPath(o.StatePath))
		session.update(func(s *runnerproto.BootstrapStatus) {
			if s.Phase != runnerproto.BootstrapPhaseCancelled {
				s.Phase, s.Error, s.Message = runnerproto.BootstrapPhaseFailed, code, err.Error()
			}
		})
		o.linger(ctx, session, 5*time.Second)
		return Config{}, &retryOnboarding{err}
	}
	return o.save(ctx, session, attempt.journal, cfg)
}

// begin runs once per bound operation: it opens the pairing at the pool the
// app named and journals it before the app can approve it.
func (o *OnboardingOptions) begin(ctx context.Context, s *bootstrapSession, pool, operation string, started chan<- pairingAttempt) (runnerproto.BootstrapTicket, error) {
	start, err := BeginPairing(ctx, o.HTTPClient, pool, runnerproto.PairingRequest{Name: o.Name, OS: runtime.GOOS, Arch: runtime.GOARCH, Delivery: runnerproto.PairingDeliveryRecoverable}, "")
	if err != nil {
		code := runnerproto.BootstrapErrorPoolRejected
		switch {
		case errors.Is(err, ErrPoolIncompatible):
			code = runnerproto.BootstrapErrorPoolIncompatible
		case errors.Is(err, errPairingTransport):
			code = runnerproto.BootstrapErrorPoolUnreachable
		}
		return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusBadGateway, Code: code, Message: err.Error()}
	}
	key, err := NewCommandKey()
	if err != nil {
		return runnerproto.BootstrapTicket{}, err
	}
	j := pairingJournal{PoolURL: pool, Name: o.Name, PairingID: start.ID, DeviceSecret: start.DeviceSecret, CommandKey: key, Interval: start.Interval, ExpiresAt: start.ExpiresAt}
	if err := writeJournal(JournalPath(o.StatePath), j); err != nil {
		return runnerproto.BootstrapTicket{}, &bootstrapError{Status: http.StatusInternalServerError, Code: runnerproto.BootstrapErrorSaveFailed, Message: "the runner could not save the pairing attempt"}
	}
	raw, _ := decodeCommandKey(key)
	s.update(func(status *runnerproto.BootstrapStatus) {
		status.Phase, status.PairingID = runnerproto.BootstrapPhaseAwaitingApproval, start.ID
	})
	started <- pairingAttempt{journal: j, start: *start}
	return runnerproto.BootstrapTicket{BootstrapID: s.payload.BootstrapID, OperationID: operation, PoolURL: pool, PairingID: start.ID, UserCode: start.UserCode,
		Name: o.Name, OS: runtime.GOOS, Arch: runtime.GOARCH, CommandKey: key, CommandKeyID: runnerproto.CommandKeyID(raw), ExpiresAt: start.ExpiresAt}, nil
}

// save persists the credential before anything reports success, then lets
// the pool drop its sealed copy and gives the app a moment to read the result.
func (o *OnboardingOptions) save(ctx context.Context, session *bootstrapSession, j pairingJournal, cfg Config) (Config, error) {
	cfg.CommandKey = j.CommandKey
	if err := WriteConfig(o.StatePath, cfg); err != nil {
		if session != nil {
			session.update(func(s *runnerproto.BootstrapStatus) {
				s.Phase, s.Error, s.Message = runnerproto.BootstrapPhaseFailed, runnerproto.BootstrapErrorSaveFailed, "the runner could not save its credential"
			})
		}
		return Config{}, fmt.Errorf("saving the runner credential failed (%v); restart the runner within a few minutes to recover it", err)
	}
	_ = os.Remove(JournalPath(o.StatePath))
	raw, _ := decodeCommandKey(cfg.CommandKey)
	if session != nil {
		session.update(func(s *runnerproto.BootstrapStatus) {
			s.Phase, s.RunnerID, s.CommandKeyID = runnerproto.BootstrapPhasePaired, cfg.RunnerID, runnerproto.CommandKeyID(raw)
		})
	}
	o.Report(OnboardingEvent{Kind: OnboardingPaired, PoolURL: cfg.PoolURL, RunnerID: cfg.RunnerID, Name: o.Name})
	for range 3 {
		ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := AcknowledgePairing(ackCtx, o.HTTPClient, cfg, j.PairingID)
		cancel()
		// Unacknowledged receipts expire on their own; this only shortens that.
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if session != nil {
		o.linger(ctx, session, o.receiptWait)
	}
	return cfg, nil
}

// linger keeps the LAN service up briefly so the app can read the final
// status; it never delays a pairing the runner already saved.
func (o *OnboardingOptions) linger(ctx context.Context, session *bootstrapSession, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-session.acked:
	case <-session.cancelled:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// resume finishes a journaled attempt without reopening the LAN service: the
// app approves through the pool, and the pool still holds the credential.
func (o *OnboardingOptions) resume(ctx context.Context, j pairingJournal) (Config, error) {
	journal := JournalPath(o.StatePath)
	abandon := func() (Config, error) {
		_ = os.Remove(journal)
		o.Report(OnboardingEvent{Kind: OnboardingAbandoned, PoolURL: j.PoolURL, Name: j.Name, Err: errJournalAbandoned})
		return Config{}, errJournalAbandoned
	}
	if !o.now().Before(j.recoverableUntil()) {
		return abandon()
	}
	o.Report(OnboardingEvent{Kind: OnboardingRecovering, PoolURL: j.PoolURL, Name: j.Name})
	start := j.start()
	start.ExpiresAt = j.recoverableUntil()
	cfg, err := AwaitPairing(ctx, o.HTTPClient, j.PoolURL, start)
	if err != nil {
		if ctx.Err() != nil {
			return Config{}, ctx.Err()
		}
		var rejected *PairingRejected
		if errors.As(err, &rejected) && rejected.Status < 500 || strings.Contains(err.Error(), "expired") {
			return abandon()
		}
		return Config{}, err
	}
	return o.save(ctx, nil, j, cfg)
}
