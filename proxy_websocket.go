package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/claudecode"
	"codex-pool-proxy/internal/clientip"
	"codex-pool-proxy/internal/errclass"
	"codex-pool-proxy/internal/hostedmcp"
	"codex-pool-proxy/internal/httpwire"
	"codex-pool-proxy/internal/routing"
	"github.com/coder/websocket"
)

func (h *proxyHandler) proxyRequestWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	reqID string,
	userID string,
	originID string,
	provider ProviderDriver,
	targetBase *url.URL,
	rt proxyRuntime,
) {
	start := rt.now()
	accountType := provider.Type()
	admission := policyAdmissionFromRequest(r)
	if !h.enforcePolicy(w, admission, func() error {
		if model := r.URL.Query().Get("model"); model != "" {
			if err := admission.CheckModel(model); err != nil {
				return err
			}
		}
		return admission.CheckProvider(accountType)
	}) {
		return
	}

	conversationID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if conversationID == "" {
		conversationID = extractConversationIDFromHeaders(r.Header)
	}
	if conversationID == "" && provider.WireFormat() == WireResponses {
		conversationID = codexRealtimeCallPinKey(codexRealtimeCallIDFromRequest(r.URL.Path, r.URL.Query()))
	}
	// Use Claude Code session ID as fallback for conversation stickiness
	if conversationID == "" {
		for _, key := range []string{"X-Claude-Code-Session-Id", "x-claude-code-session-id"} {
			if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
				conversationID = v
				break
			}
		}
	}

	requiredPlan := requiredPlanForRequest(accountType, r, "")
	clientIP := clientip.Get(r)
	routingProfile, ok := h.pool.resolveRoutingProfile(r.Header.Get("X-Pool-Routing"))
	if !ok {
		http.Error(w, fmt.Sprintf("unknown routing profile %q", r.Header.Get("X-Pool-Routing")), http.StatusBadRequest)
		return
	}
	selectionConversationID := conversationID
	if selectionConversationID == "" && provider.WireFormat() == WireResponses {
		fallbackID := userID
		if fallbackID == "" {
			fallbackID = originID
		}
		if fallbackID == "" {
			fallbackID = clientIP
		}
		selectionConversationID = "cyber-fallback:" + fallbackID
	}
	acc := h.pool.selectRoute(routing.Request{
		UserID: userID, ConversationID: selectionConversationID, Provider: string(accountType),
		RequiredPlan: requiredPlan, ClientIP: clientIP, Profile: string(routingProfile),
	}).Account
	if acc == nil {
		if err := h.checkAccountRoutingAccess(userID, accountType, ""); err != nil {
			respondPolicyError(w, err)
			return
		}
		http.Error(w, fmt.Sprintf("no live %s accounts", accountType), http.StatusServiceUnavailable)
		return
	}

	if err := h.checkWebSocketGrant(userID, acc); err != nil {
		respondPolicyError(w, err)
		return
	}
	releaseAccount, err := h.acquireAccountSlot(userID, conversationPinKey(userID, conversationID), acc)
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	defer func() { releaseAccount() }()
	atomic.AddInt64(&acc.Inflight, 1)
	atomic.AddInt64(&h.inflight, 1)
	// inflightAcc tracks the account that currently owns the inflight
	// credit. The codex websocket relay swaps it to a cyber-access
	// account on cyber_policy; we transfer the credit at that moment
	// so the deferred decrement still touches the right account.
	inflightAcc := acc
	defer func() {
		atomic.AddInt64(&inflightAcc.Inflight, -1)
		atomic.AddInt64(&h.inflight, -1)
	}()

	refreshFailed := false
	if !h.cfg.disableRefresh && h.needsRefresh(acc) {
		if err := h.refreshAccount(r.Context(), acc); err != nil {
			if errclass.IsRateLimit(err) {
				h.applyRateLimit(acc, nil)
			} else {
				refreshFailed = true
			}
			if h.cfg.debug.Load() {
				log.Printf("[%s] refresh %s failed before websocket request: %v", reqID, acc.ID, err)
			}
		}
	}

	acc.mu.Lock()
	access := acc.AccessToken
	acc.mu.Unlock()
	if access == "" {
		http.Error(w, fmt.Sprintf("account %s has empty access token", acc.ID), http.StatusServiceUnavailable)
		return
	}

	outURL := new(url.URL)
	*outURL = *r.URL
	outURL.Scheme = targetBase.Scheme
	outURL.Host = targetBase.Host
	outURL.Path = singleJoin(targetBase.Path, provider.NormalizePath(r.URL.Path))

	// For Claude OAuth tokens, add beta=true query param (required for OAuth to work)
	if provider.Type() == AccountTypeClaude && strings.HasPrefix(access, "sk-ant-oat") {
		q := outURL.Query()
		q.Set("beta", "true")
		outURL.RawQuery = q.Encode()
	}

	// Build upstream headers: clone client headers, replace auth.
	upstreamHeaders := httpwire.CloneHeader(r.Header)
	upstreamHeaders.Del("Authorization")
	upstreamHeaders.Del("ChatGPT-Account-ID")
	upstreamHeaders.Del("X-Api-Key")
	upstreamHeaders.Del("x-goog-api-key")
	removeConflictingProxyHeaders(upstreamHeaders)

	// Use a temporary request to set provider auth headers.
	tmpReq := &http.Request{Header: upstreamHeaders}
	authAccount := acc
	if provider.WireFormat() == WireResponses {
		authAccount = contextAuthSnapshot(acc)
	}
	provider.SetAuthHeaders(tmpReq, authAccount)
	upstreamHeaders = tmpReq.Header
	h.guardCodexTurnStateEcho(userID, conversationID, acc, upstreamHeaders)
	// The Codex Responses relay still uses its websocket beta header, but the
	// GA Realtime API explicitly rejects that legacy beta shape. Preserve the
	// client-provided Realtime headers (for example openai-alpha) instead.
	if provider.WireFormat() == WireResponses && isCodexRealtimePath(r.URL.Path) {
		upstreamHeaders.Del("OpenAI-Beta")
	}

	if h.cfg.debug.Load() {
		log.Printf("[%s] websocket tunnel -> %s (account=%s)", reqID, outURL.String(), acc.ID)
	}

	cyberPinned := false
	relayLabel := fmt.Sprintf("%s account=%s", reqID, acc.ID)
	readLimit := effectiveWebSocketReadLimit(accountType, h.cfg.websocketReadLimit)
	downstreamHeartbeatInterval := time.Duration(0)
	if provider.WireFormat() == WireResponses {
		downstreamHeartbeatInterval = h.cfg.websocketHeartbeatInterval
	}

	// Every Codex websocket goes through the cyber-aware relay so we can
	// universally suppress cyber_policy frames before they reach the
	// client. Non-cyber accounts additionally get a one-shot hot-swap to
	// a cyber_access account on the first cyber_policy hit.
	if provider.WireFormat() == WireResponses {
		swap := h.relayCodexWithCyberSwap(w, r, codexCyberSwapOptions{
			ReqID:                       reqID,
			Runtime:                     rt,
			RequestPath:                 r.URL.Path,
			Provider:                    provider,
			InitialAccount:              acc,
			InitialContextAccount:       authAccount,
			InitialOutURL:               outURL,
			InitialUpstreamHeaders:      upstreamHeaders,
			ConversationID:              conversationID,
			RoutingProfile:              routingProfile,
			RequiredPlan:                requiredPlan,
			ClientIP:                    h.traceClientIP(clientIP),
			UserID:                      userID,
			OriginID:                    originID,
			IdleTimeout:                 h.cfg.websocketIdleTimeout,
			DownstreamHeartbeatInterval: downstreamHeartbeatInterval,
			ReadLimit:                   readLimit,
			CompressionEnabled:          h.cfg.websocketCompression,
			LogLabel:                    relayLabel,
			PrepareAccount: func(next *Account) (func(bool), error) {
				return h.prepareWebSocketAccountChange(userID, conversationPinKey(userID, conversationID), &inflightAcc, &releaseAccount, next)
			},
		})
		finalAcc := acc
		if swap.finalAccount != nil {
			finalAcc = swap.finalAccount
		}
		termination := swap.termination
		if termination.Side == "" {
			termination = classifyWebSocketTermination(swap.err)
		}
		duration := rt.now().Sub(start)
		h.recordWebSocketTermination(reqID, finalAcc.ID, termination, duration)
		if swap.err != nil {
			h.recent.add(swap.err.Error())
			h.metrics.inc("error", finalAcc.ID)
			if h.cfg.debug.Load() {
				log.Printf("[%s] websocket tunnel error (account=%s): %v", reqID, finalAcc.ID, swap.err)
			}
			return
		}
		if swap.statusCode != 0 {
			h.metrics.inc(strconv.Itoa(swap.statusCode), finalAcc.ID)
		}
		// A handoff already pinned the conversation to the reconnect target.
		h.applyWebSocketStatusEffects(reqID, finalAcc, userID, conversationID, swap.swapped || swap.handedOff, refreshFailed, swap.statusCode)
		if h.cfg.debug.Load() {
			rt.logRequest(r.Context(), reqID, "websocket proxy completed",
				slog.Int("status", swap.statusCode), slog.String("account", finalAcc.ID), slog.String("user_id", userID),
				slog.String("origin_id", originID), slog.Int64("duration_ms", duration.Milliseconds()), slog.Bool("cyber_swapped", swap.swapped))
		}
		return
	}

	relay := relayWebSocket(w, r, outURL, upstreamHeaders, webSocketRelayOptions{
		OnClientMessage: func([]byte) error {
			if err := h.revalidatePoolCredential(r, userID); err != nil {
				return err
			}
			return h.checkAccountUse(userID, acc)
		},
		IdleTimeout:                 h.cfg.websocketIdleTimeout,
		DownstreamHeartbeatInterval: downstreamHeartbeatInterval,
		ReadLimit:                   readLimit,
		CompressionEnabled:          h.cfg.websocketCompression,
		LogLabel:                    relayLabel,
		Debug:                       h.cfg.debug.Load(),
		Registry:                    h.webSocketRegistry(),
	})
	statusCode, err := relay.statusCode, relay.err
	duration := rt.now().Sub(start)
	h.recordWebSocketTermination(reqID, acc.ID, relay.termination, duration)

	if err != nil {
		h.recent.add(err.Error())
		h.metrics.inc("error", acc.ID)
		if h.cfg.debug.Load() {
			log.Printf("[%s] websocket tunnel error (account=%s): %v", reqID, acc.ID, err)
		}
		return
	}

	if statusCode != 0 {
		h.metrics.inc(strconv.Itoa(statusCode), acc.ID)
	}

	h.applyWebSocketStatusEffects(reqID, acc, userID, conversationID, cyberPinned, refreshFailed, statusCode)

	if h.cfg.debug.Load() {
		rt.logRequest(r.Context(), reqID, "websocket proxy completed",
			slog.Int("status", statusCode), slog.String("account", acc.ID), slog.String("user_id", userID),
			slog.String("origin_id", originID), slog.Int64("duration_ms", duration.Milliseconds()))
	}
}

// applyWebSocketStatusEffects runs the post-relay account bookkeeping
// (rate-limit cooldown, auth-failure marking, success-path penalty
// decay, conversation pinning) shared between the Codex cyber-aware
// relay and the legacy passthrough/Claude/Gemini relay.
func (h *proxyHandler) applyWebSocketStatusEffects(reqID string, acc *Account, userID, conversationID string, cyberPinned, refreshFailed bool, statusCode int) {
	switch {
	case statusCode == http.StatusTooManyRequests:
		h.applyRateLimit(acc, nil)
		acc.mu.Lock()
		acc.Penalty += 1.0
		acc.mu.Unlock()
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		markedDead, _ := applyProxyAuthFailure(acc, refreshFailed, h.registry)
		if markedDead {
			if err := saveAccount(h.pool.antigravity, acc); err != nil {
				log.Printf("[%s] warning: failed to save dead account %s: %v", reqID, acc.ID, err)
			}
		}
	case statusCode == http.StatusSwitchingProtocols || (statusCode >= 200 && statusCode < 300):
		if conversationID != "" && !cyberPinned {
			h.pool.pin(conversationPinKey(userID, conversationID), acc.ID)
		}
		acc.mu.Lock()
		acc.LastUsed = time.Now()
		acc.BackoffLevel = 0
		if acc.Penalty > 0 {
			acc.Penalty *= 0.5
			if acc.Penalty < 0.01 {
				acc.Penalty = 0
			}
		}
		acc.mu.Unlock()
	}
}

func (h *proxyHandler) proxyPassthroughWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	reqID string,
	providerType AccountType,
	provider ProviderDriver,
	targetBase *url.URL,
	start time.Time,
	rt proxyRuntime,
) {
	outURL := new(url.URL)
	*outURL = *r.URL
	outURL.Scheme = targetBase.Scheme
	outURL.Host = targetBase.Host
	outURL.Path = singleJoin(targetBase.Path, provider.NormalizePath(r.URL.Path))

	// For Claude OAuth passthrough tokens, add beta=true query param.
	if providerType == AccountTypeClaude {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if strings.HasPrefix(token, "sk-ant-oat") {
				q := outURL.Query()
				q.Set("beta", "true")
				outURL.RawQuery = q.Encode()
			}
		}
	}

	upstreamHeaders := httpwire.CloneHeader(r.Header)
	removeConflictingProxyHeaders(upstreamHeaders)
	if providerType == AccountTypeClaude && upstreamHeaders.Get("anthropic-version") == "" {
		upstreamHeaders.Set("anthropic-version", claudecode.AnthropicVersion)
	}

	if h.cfg.debug.Load() {
		log.Printf("[%s] passthrough websocket tunnel -> %s", reqID, outURL.String())
	}

	readLimit := effectiveWebSocketReadLimit(providerType, h.cfg.websocketReadLimit)
	downstreamHeartbeatInterval := time.Duration(0)
	if providerType == AccountTypeCodex {
		downstreamHeartbeatInterval = h.cfg.websocketHeartbeatInterval
	}
	relayOpts := webSocketRelayOptions{
		IdleTimeout:                 h.cfg.websocketIdleTimeout,
		DownstreamHeartbeatInterval: downstreamHeartbeatInterval,
		ReadLimit:                   readLimit,
		CompressionEnabled:          h.cfg.websocketCompression,
		LogLabel:                    reqID + " passthrough",
		Debug:                       h.cfg.debug.Load(),
	}
	if providerType == AccountTypeCodex {
		relayOpts.TransformUpstreamMessage = func(data []byte) ([]byte, error) {
			filtered, _, drop, _ := hostedmcp.FilterResponseJSON(data)
			if drop {
				return []byte{}, nil
			}
			return filtered, nil
		}
		relayOpts.TransformClientMessage = func(data []byte) ([]byte, error) {
			if !isCodexResponseCreate(data) {
				return data, nil
			}
			filtered, _, err := hostedmcp.FilterRequestJSON(data)
			return filtered, err
		}
	}
	relayOpts.Registry = h.webSocketRegistry()
	relay := relayWebSocket(w, r, outURL, upstreamHeaders, relayOpts)
	statusCode, err := relay.statusCode, relay.err
	duration := rt.now().Sub(start)
	h.recordWebSocketTermination(reqID, "passthrough", relay.termination, duration)

	if err != nil {
		h.recent.add(err.Error())
		h.metrics.inc("error", "passthrough")
		if h.cfg.debug.Load() {
			log.Printf("[%s] passthrough websocket tunnel error: %v", reqID, err)
		}
		return
	}
	if statusCode != 0 {
		h.metrics.inc(strconv.Itoa(statusCode), "passthrough")
	}
	if h.cfg.debug.Load() {
		rt.logRequest(r.Context(), reqID, "websocket passthrough completed",
			slog.Int("status", statusCode), slog.Int64("duration_ms", duration.Milliseconds()))
	}
}

// relayWebSocket accepts a client WS upgrade and opens a separate WS
// connection to the upstream server, then relays JSON messages between
// them at the application level. Unlike raw TCP tunneling, this works
// through Cloudflare (which negotiates h2 and can't tunnel raw WS
// frames) because each side is an independent WS connection.
//
// Flow:
//  1. Dial upstream WS first so we can mirror selected handshake state.
//  2. Accept the client WS upgrade with the negotiated subprotocol.
//  3. Relay messages bidirectionally until either side closes.
type webSocketRelayOptions struct {
	IdleTimeout                 time.Duration
	DownstreamHeartbeatInterval time.Duration
	ReadLimit                   int64
	CompressionEnabled          bool
	LogLabel                    string
	Debug                       bool
	OnUpstreamResponse          func(*http.Response)
	OnUpstreamMessage           func([]byte) error
	OnClientMessage             func([]byte) error
	TransformUpstreamMessage    func([]byte) ([]byte, error)
	TransformClientMessage      func([]byte) ([]byte, error)
	Registry                    *webSocketRegistry
}

type webSocketRelayResult struct {
	statusCode  int
	err         error
	termination webSocketTermination
}

const codexWebSocketReadLimit = 512 * 1024 * 1024

func effectiveWebSocketReadLimit(accountType AccountType, configured int64) int64 {
	if configured <= 0 {
		configured = 64 * 1024 * 1024
	}
	if accountType == AccountTypeCodex && configured < codexWebSocketReadLimit {
		return codexWebSocketReadLimit
	}
	return configured
}

func relayWebSocket(
	w http.ResponseWriter,
	clientReq *http.Request,
	upstreamURL *url.URL,
	upstreamHeaders http.Header,
	opts webSocketRelayOptions,
) webSocketRelayResult {
	ctx := clientReq.Context()

	wsURL := *upstreamURL
	upstreamConn, upstreamResp, _, err := dialUpstreamWebSocket(ctx, &wsURL, upstreamHeaders, clientReq.Header, opts.ReadLimit, opts.CompressionEnabled)
	if err != nil {
		if upstreamResp != nil {
			status := writeWebSocketRejection(w, upstreamResp)
			return webSocketRelayResult{statusCode: status}
		}
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return webSocketRelayResult{err: err, termination: classifyWebSocketTermination(err)}
	}
	upstreamClosing := false
	defer func() {
		if !upstreamClosing {
			upstreamConn.CloseNow()
		}
	}()
	if opts.OnUpstreamResponse != nil {
		opts.OnUpstreamResponse(upstreamResp)
	}

	if turnState := upstreamResp.Header.Get("x-codex-turn-state"); turnState != "" {
		w.Header().Set("x-codex-turn-state", turnState)
	}

	acceptOpts := &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	}
	if opts.CompressionEnabled {
		acceptOpts.CompressionMode = websocket.CompressionNoContextTakeover
	}
	if subprotocol := upstreamConn.Subprotocol(); subprotocol != "" {
		acceptOpts.Subprotocols = []string{subprotocol}
	}

	clientConn, err := websocket.Accept(w, clientReq, acceptOpts)
	if err != nil {
		upstreamConn.Close(websocket.StatusInternalError, "client accept failed")
		err = fmt.Errorf("accept client WS: %w", err)
		return webSocketRelayResult{err: err, termination: classifyWebSocketTermination(err)}
	}
	clientClosing := false
	defer func() {
		if !clientClosing {
			clientConn.CloseNow()
		}
	}()
	clientConn.SetReadLimit(opts.ReadLimit)
	var session *trackedWebSocketSession
	if opts.Registry != nil {
		session = opts.Registry.register(clientConn)
		defer opts.Registry.unregister(session)
	}

	log.Printf("[ws-relay %s] connected to %s, relaying messages", opts.LogLabel, wsURL.Host)

	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	clientWriter := &webSocketWriter{conn: clientConn}
	upstreamWriter := &webSocketWriter{conn: upstreamConn}

	errCh := make(chan error, 2)
	clientHeartbeatErr, stopClientHeartbeat := startWebSocketHeartbeat(relayCtx, clientWriter, opts.DownstreamHeartbeatInterval, "client")
	defer stopClientHeartbeat()
	upstreamHeartbeatErr, stopUpstreamHeartbeat := startWebSocketHeartbeat(relayCtx, upstreamWriter, opts.DownstreamHeartbeatInterval, "upstream")
	defer stopUpstreamHeartbeat()
	markClientForwarded := func(data []byte) {
		if session != nil && isCodexResponseCreate(data) {
			session.setActive(true)
		}
	}
	markUpstreamForwarded := func(data []byte) {
		if session != nil && isTerminalCodexWebSocketEvent(data) {
			session.setActive(false)
		}
	}

	go func() {
		errCh <- relayMessages(relayCtx, upstreamConn, clientWriter, opts.LogLabel, "upstream->client", opts.IdleTimeout, opts.Debug, opts.OnUpstreamMessage, opts.TransformUpstreamMessage, markUpstreamForwarded)
	}()
	go func() {
		errCh <- relayMessages(relayCtx, clientConn, upstreamWriter, opts.LogLabel, "client->upstream", opts.IdleTimeout, opts.Debug, opts.OnClientMessage, opts.TransformClientMessage, markClientForwarded)
	}()

	var relayErr error
	select {
	case relayErr = <-errCh:
	case relayErr = <-clientHeartbeatErr:
	case relayErr = <-upstreamHeartbeatErr:
	}
	relayCancel()
	termination := normalizeCompletedWebSocketTermination(classifyWebSocketTermination(relayErr), session)
	closeCode, closeReason := termination.wireCloseCode(), termination.wireReason()
	switch termination.Side {
	case "upstream":
		upstreamConn.CloseNow()
		upstreamClosing = true
		clientClosing = true
		beginWebSocketClose(clientConn, closeCode, closeReason)
	case "client":
		clientConn.CloseNow()
		clientClosing = true
		upstreamClosing = true
		beginWebSocketClose(upstreamConn, closeCode, closeReason)
	default:
		clientClosing = true
		upstreamClosing = true
		beginWebSocketClose(clientConn, closeCode, closeReason)
		beginWebSocketClose(upstreamConn, closeCode, closeReason)
	}
	if relayErr != nil && !termination.accountFailure() {
		relayErr = nil
	}
	return webSocketRelayResult{statusCode: http.StatusSwitchingProtocols, err: relayErr, termination: termination}
}

type webSocketWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *webSocketWriter) Write(ctx context.Context, msgType websocket.MessageType, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Write(ctx, msgType, data)
}

func (w *webSocketWriter) Ping(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Ping(ctx)
}

func (w *webSocketWriter) CopyFrom(ctx context.Context, msgType websocket.MessageType, src io.Reader) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	writer, err := w.conn.Writer(ctx, msgType)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(writer, src, make([]byte, 32*1024))
	closeErr := writer.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// relayMessages reads messages from src and writes them to dst until
// the context is cancelled or an error occurs. If idleTimeout > 0 the
// connection is force-closed when no frame arrives within that window.
//
// We use a time.AfterFunc watchdog instead of context.WithTimeout because
// coder/websocket closes the connection when the read context is cancelled,
// which would tear the relay down on every successful frame.
func relayMessages(ctx context.Context, src *websocket.Conn, dst *webSocketWriter, logLabel, label string, idleTimeout time.Duration, debug bool, onMessage func([]byte) error, transform func([]byte) ([]byte, error), afterForward func([]byte)) error {
	if !debug && onMessage == nil && transform == nil {
		return relayMessagesStreaming(ctx, src, dst, label, idleTimeout)
	}
	var idleTimer *time.Timer
	if idleTimeout > 0 {
		idleTimer = time.AfterFunc(idleTimeout, func() {
			src.Close(websocket.StatusPolicyViolation, fmt.Sprintf("idle for %v", idleTimeout))
		})
		defer idleTimer.Stop()
	}
	for {
		if idleTimer != nil {
			idleTimer.Reset(idleTimeout)
		}
		msgType, data, err := src.Read(ctx)
		if err != nil {
			return fmt.Errorf("%s read: %w", label, err)
		}
		if onMessage != nil {
			if err := onMessage(data); err != nil {
				return err
			}
		}
		if transform != nil {
			data, err = transform(data)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				continue
			}
		}
		if debug {
			logRelayFrame(logLabel, label, msgType, data)
		}
		if err := dst.Write(ctx, msgType, data); err != nil {
			return fmt.Errorf("%s write: %w", label, err)
		}
		if afterForward != nil {
			afterForward(data)
		}
	}
}

func relayMessagesStreaming(ctx context.Context, src *websocket.Conn, dst *webSocketWriter, label string, idleTimeout time.Duration) error {
	var idleTimer *time.Timer
	if idleTimeout > 0 {
		idleTimer = time.AfterFunc(idleTimeout, func() {
			src.Close(websocket.StatusPolicyViolation, fmt.Sprintf("idle for %v", idleTimeout))
		})
		defer idleTimer.Stop()
	}
	for {
		if idleTimer != nil {
			idleTimer.Reset(idleTimeout)
		}
		msgType, reader, err := src.Reader(ctx)
		if err != nil {
			return fmt.Errorf("%s read: %w", label, err)
		}
		if err := dst.CopyFrom(ctx, msgType, reader); err != nil {
			return fmt.Errorf("%s write: %w", label, err)
		}
	}
}

func logRelayFrame(logLabel, label string, msgType websocket.MessageType, data []byte) {
	if mayHaveContext(data) {
		log.Printf("[ws-relay %s] %s: type=%v len=%d", logLabel, label, msgType, len(data))
		return
	}
	summary := data
	suffix := ""
	if len(summary) > 200 {
		summary = summary[:200]
		suffix = "..."
	}
	log.Printf("[ws-relay %s] %s: type=%v len=%d %s%s", logLabel, label, msgType, len(data), string(summary), suffix)
}
