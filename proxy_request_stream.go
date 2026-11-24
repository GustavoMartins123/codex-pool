package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/claudecode"
	"codex-pool-proxy/internal/clientip"
	"codex-pool-proxy/internal/errclass"
	"codex-pool-proxy/internal/hostedmcp"
	"codex-pool-proxy/internal/httpwire"
	"codex-pool-proxy/internal/requestjson"
	"codex-pool-proxy/internal/requestlog"
	"codex-pool-proxy/internal/routing"
	"codex-pool-proxy/internal/wire"
)

func (h *proxyHandler) proxyRequestStreamed(w http.ResponseWriter, r *http.Request, reqID, userID, originID string, provider ProviderDriver, targetBase *url.URL, rt requestlog.Runtime) {
	if policyAdmissionFromRequest(r).hasTokenBudget() {
		respondPolicyError(w, unboundedPolicyRequest())
		return
	}
	start := rt.Now()
	accountType := provider.Type()
	admission := policyAdmissionFromRequest(r)
	if !h.enforcePolicy(w, admission, func() error { return admission.CheckProvider(accountType) }) {
		return
	}
	spooled, _ := r.Context().Value(contextSpoolKey{}).(*streamedResponsesRequest)
	contextSession := ""
	if spooled != nil {
		contextSession = contextSessionID(spooled.contextMetadata)
	}

	requiredPlan := requiredPlanForRequest(accountType, r, "")
	clientIP := clientip.Get(r)
	requestedModel := ""
	if spooled != nil {
		requestedModel = spooled.Model
	}
	routingProfile, ok := h.pool.resolveRoutingProfile(r.Header.Get("X-Pool-Routing"))
	if !ok {
		http.Error(w, fmt.Sprintf("unknown routing profile %q", r.Header.Get("X-Pool-Routing")), http.StatusBadRequest)
		return
	}
	acc := h.pool.selectRoute(routing.Request{
		UserID: userID, ConversationID: contextSession, Provider: string(accountType),
		RequiredPlan: requiredPlan, ClientIP: clientIP, Model: requestedModel, Profile: string(routingProfile),
	}).Account
	if acc == nil {
		if err := h.checkAccountRoutingAccess(userID, accountType, requestedModel); err != nil {
			respondPolicyError(w, err)
			return
		}
		http.Error(w, fmt.Sprintf("no live %s accounts", accountType), http.StatusServiceUnavailable)
		return
	}

	if err := h.checkLivePolicy(userID, requestedModel, provider.Type()); err != nil {
		respondPolicyError(w, err)
		return
	}
	prepared, releaseAccount, err := h.acquireGovernedAccount(r, userID, contextSession, acc, requestedModel)
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	r = prepared
	defer func() { releaseAccount() }()
	atomic.AddInt64(&acc.Inflight, 1)
	atomic.AddInt64(&h.inflight, 1)
	defer func() {
		atomic.AddInt64(&acc.Inflight, -1)
		atomic.AddInt64(&h.inflight, -1)
	}()

	// For streamed-body requests we can't inspect the body, so pass nil.
	// clientOrDefaultTimeout will still check X-Stainless-Timeout header.
	timeout := clientOrDefaultTimeout(r, h.cfg.requestTimeout, h.cfg.streamTimeout, nil)

	ctx := r.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// Refresh before building headers to ensure we use the latest token.
	refreshFailed := false
	if !h.cfg.disableRefresh && h.needsRefresh(acc) {
		if err := h.refreshAccount(ctx, acc); err != nil {
			if errclass.IsRateLimit(err) {
				h.applyRateLimit(acc, nil)
			} else {
				refreshFailed = true
			}
			if h.cfg.debug.Load() {
				log.Printf("[%s] refresh %s failed before streamed request: %v", reqID, acc.ID, err)
			}
		}
	}

	authAccount := acc
	if spooled != nil {
		if err := h.nativeContext.prepareSpool(contextScope(userID), clientIP, spooled); err != nil {
			writeContextError(w, err)
			return
		}
		r.Body = spooled.File
		// A spooled body is not re-readable, so replay needs a fresh reader
		// over the same section. Authentication recovery may issue the
		// request twice and must send identical bytes both times.
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(spooled.File, 0, spooled.Size)), nil
		}
		r.ContentLength = spooled.Size
		r.Header.Del("Content-Length")
		authAccount = contextAuthSnapshot(acc)
		if err := h.nativeContext.recordDispatch(contextScope(userID), spooled.contextMetadata, authAccount, clientIP); err != nil {
			writeContextError(w, err)
			return
		}
	}

	outURL := new(url.URL)
	*outURL = *r.URL
	outURL.Scheme = targetBase.Scheme
	outURL.Host = targetBase.Host
	outURL.Path = singleJoin(targetBase.Path, provider.NormalizePath(r.URL.Path))

	authAccount.mu.Lock()
	access := authAccount.AccessToken
	authAccount.mu.Unlock()
	if access == "" {
		http.Error(w, fmt.Sprintf("account %s has empty access token", acc.ID), http.StatusServiceUnavailable)
		return
	}

	// For Claude OAuth tokens, add beta=true query param (required for OAuth to work)
	if provider.Type() == AccountTypeClaude && strings.HasPrefix(access, "sk-ant-oat") {
		q := outURL.Query()
		q.Set("beta", "true")
		outURL.RawQuery = q.Encode()
	}

	var reqSample *bytes.Buffer
	var body io.Reader = r.Body
	if r.GetBody != nil {
		var err error
		body, err = r.GetBody()
		if err != nil {
			http.Error(w, "request replay unavailable", http.StatusInternalServerError)
			return
		}
	}
	if spooled == nil && h.cfg.logBodies && h.cfg.bodyLogLimit > 0 {
		reqSample = &bytes.Buffer{}
		body = io.TeeReader(body, &limitedWriter{w: reqSample, n: h.cfg.bodyLogLimit})
	}

	outReq, err := http.NewRequestWithContext(ctx, r.Method, outURL.String(), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outReq.Host = targetBase.Host
	outReq.Header = httpwire.CloneHeader(r.Header)
	httpwire.RemoveHopByHopHeaders(outReq.Header)
	removeConflictingProxyHeaders(outReq.Header)
	// Single-account path has no retry, but the next request may pin this
	// session elsewhere; never hand this account a known foreign echo.
	h.guardCodexTurnStateEcho(userID, extractConversationIDFromHeaders(r.Header), acc, outReq.Header)
	if r.ContentLength >= 0 {
		outReq.ContentLength = r.ContentLength
	}

	// Always overwrite client-provided auth; the proxy is the single source of truth.
	outReq.Header.Del("Authorization")
	outReq.Header.Del("X-Api-Key")
	outReq.Header.Del("x-goog-api-key")
	outReq.Header.Del("ChatGPT-Account-ID")

	// Remove Cloudflare/proxy headers that would cause issues with OpenAI's Cloudflare
	outReq.Header.Del("Cdn-Loop")
	outReq.Header.Del("Cf-Connecting-Ip")
	outReq.Header.Del("Cf-Ray")
	outReq.Header.Del("Cf-Visitor")
	outReq.Header.Del("Cf-Warp-Tag-Id")
	outReq.Header.Del("Cf-Ipcountry")
	outReq.Header.Del("X-Forwarded-For")
	outReq.Header.Del("X-Forwarded-Proto")
	outReq.Header.Del("X-Real-Ip")

	// Register and authenticate with the same credential snapshot.
	provider.SetAuthHeaders(outReq, authAccount)

	// Force uncompressed responses — SSE frame parsing and client decompression
	// break on gzip-compressed streams that split across TCP segments.
	outReq.Header.Set("Accept-Encoding", "identity")

	if h.cfg.debug.Load() {
		log.Printf("[%s] streamed -> %s %s (account=%s)", reqID, outReq.Method, outReq.URL.String(), acc.ID)
	}

	outReq.GetBody = r.GetBody
	resp, err := h.policyRoundTrip(outReq, provider.Type(), policyAdmissionFromRequest(r))
	captureCodexResponseState(h.pool.antigravity, acc, resp, reqID)
	if err != nil {
		var blocked *policyError
		if errors.As(err, &blocked) {
			respondPolicyError(w, err)
			return
		}
		acc.mu.Lock()
		acc.Penalty += 0.2
		acc.mu.Unlock()
		h.recent.add(err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// A 401 here means the account credential was rejected, not the client
	// credential. Recover once by refreshing and replaying the identical body,
	// so a mid-stream token expiry does not surface to the caller as an
	// upstream 401 that looks like their own login failed.
	if resp.StatusCode == http.StatusUnauthorized && r.GetBody != nil && h.retryPoolAuth(ctx, acc) {
		_ = resp.Body.Close()
		authAccount = contextAuthSnapshot(acc)
		if spooled != nil {
			if err := h.nativeContext.recordDispatch(contextScope(userID), spooled.contextMetadata, authAccount, clientIP); err != nil {
				writeContextError(w, err)
				return
			}
		}
		retry := outReq.Clone(ctx)
		retry.Body, err = r.GetBody()
		if err != nil {
			http.Error(w, "request replay unavailable", http.StatusInternalServerError)
			return
		}
		provider.SetAuthHeaders(retry, authAccount)
		resp, err = h.policyRoundTrip(retry, provider.Type(), policyAdmissionFromRequest(r))
		captureCodexResponseState(h.pool.antigravity, acc, resp, reqID)
		if err != nil {
			http.Error(w, "upstream request failed after authentication recovery", http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	// The replay either succeeded or the credential is genuinely unusable.
	// Report a retryable upstream condition and deliberately withhold the
	// upstream 401 headers, which would otherwise read to the client as a
	// rejection of its own pool token.
	if resp.StatusCode == http.StatusUnauthorized {
		if provider.WireFormat() == WireResponses && upstreamTokenRevoked(resp) {
			h.disableAccountPermanently(acc, reqID, "upstream token revoked")
		}
		markedDead, _ := applyProxyAuthFailure(acc, refreshFailed, h.registry)
		if markedDead {
			if err := saveAccount(h.pool.antigravity, acc); err != nil {
				log.Printf("[%s] warning: failed to save dead account %s: %v", reqID, acc.ID, err)
			}
		}
		log.Printf("[%s] upstream authentication unavailable: account=%s path=streamed", reqID, acc.ID)
		h.metrics.inc(strconv.Itoa(http.StatusServiceUnavailable), acc.ID)
		writeUpstreamAuthError(w)
		return
	}

	if h.cfg.logBodies && reqSample != nil && reqSample.Len() > 0 {
		log.Printf("[%s] request body sample (%d bytes): %s", reqID, reqSample.Len(), safeText(reqSample.Bytes()))
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		h.applyRateLimit(acc, resp.Header)
		acc.mu.Lock()
		acc.Penalty += 0.2
		acc.mu.Unlock()
	}
	if resp.StatusCode == http.StatusForbidden {
		// Log the error body for debugging
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		decompressed := bodyForInspection(nil, errBody) // nil request - will auto-detect gzip
		log.Printf("[%s] account %s got %d from %s, body=%s", reqID, acc.ID, resp.StatusCode, outReq.URL.Host, safeText(decompressed))
		// Replace body so client still gets the error
		resp.Body = io.NopCloser(bytes.NewReader(errBody))

		markedDead, _ := applyProxyAuthFailure(acc, refreshFailed, h.registry)
		if markedDead {
			if err := saveAccount(h.pool.antigravity, acc); err != nil {
				log.Printf("[%s] warning: failed to save dead account %s: %v", reqID, acc.ID, err)
			}
		}
	} else if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		acc.mu.Lock()
		acc.Penalty += 0.3
		acc.mu.Unlock()
	}

	if quota, ok := provider.(RateLimitHeaderParser); ok {
		quota.ParseUsageHeaders(acc, resp.Header)
	}
	h.logRateLimitResponseHeaders(reqID, acc.Type, resp.Header)

	// Snapshot rate limits from headers for use in SSE callback
	acc.mu.Lock()
	headerPrimaryPct := acc.Usage.PrimaryUsedPercent
	headerSecondaryPct := acc.Usage.SecondaryUsedPercent
	acc.mu.Unlock()

	// Keep hosted MCP output filtering identical for streamed-body requests even
	// when the upstream returns a non-SSE JSON response.
	respContentType := resp.Header.Get("Content-Type")
	isSSE := provider.DetectsSSE(r.URL.Path, respContentType)
	var codexJSON bool
	if !isSSE && provider.WireFormat() == WireResponses && isCodexResponsesPath(r.URL.Path) {
		response, err := hostedmcp.FilterNonStreamingResponse(resp)
		if err != nil {
			http.Error(w, "hosted MCP response filtering error", http.StatusBadGateway)
			return
		}
		codexJSON = accountType == AccountTypeCodex && resp.StatusCode >= 200 && resp.StatusCode < 300
		if codexJSON {
			h.recordCodexJSONUsage(acc, provider, response, reqID, userID, originID, admission)
		}
	}

	// Write response to client.
	httpwire.CopyHeader(w.Header(), resp.Header)
	httpwire.RemoveHopByHopHeaders(w.Header())
	h.replaceUsageHeaders(w.Header())
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Single-account path commits here; every exit below writes to the
		// client, so the blob (if any) is confirmedly downstream-held.
		h.noteCodexTurnState(userID, extractConversationIDFromHeaders(r.Header), acc, resp.Header.Get("x-codex-turn-state"))
	}
	flusher, _ := w.(http.Flusher)
	if isSSE {
		applyStreamingResponseHeaders(w.Header())
	}
	w.WriteHeader(resp.StatusCode)

	var writer io.Writer = w
	var fw *flushWriter
	var hw2 *heartbeatWriter
	if isSSE && flusher != nil {
		fw = &flushWriter{w: w, f: flusher, flushInterval: h.cfg.flushInterval}
		hw2 = newHeartbeatWriter(fw, flusher)
		writer = hw2
	}

	// Tee a bounded sample for usage extraction and conversation pinning.
	sampleLimit := int64(16 * 1024)
	if h.cfg.logBodies && h.cfg.bodyLogLimit > 0 {
		sampleLimit = h.cfg.bodyLogLimit
	}
	sampleBuf := &bytes.Buffer{}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.TeeReader(resp.Body, &limitedWriter{w: sampleBuf, n: sampleLimit}),
		Closer: resp.Body,
	}

	// Claude sends usage across two SSE events (message_start: input, message_delta: output).
	// Accumulate them into a single RequestUsage before recording.
	// Declared outside the if-block so it can be flushed after io.Copy completes.
	usageWriter := &streamUsageWriter{record: func(usage RequestUsage) { h.recordUsage(acc, usage) }}
	var claudeAccum2 *RequestUsage
	cyberPinned := false
	conversationID := extractConversationIDFromHeaders(r.Header)
	// Use Claude Code session ID as fallback for conversation stickiness
	if conversationID == "" {
		for _, key := range []string{"X-Claude-Code-Session-Id", "x-claude-code-session-id"} {
			if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
				conversationID = v
				break
			}
		}
	}

	if isSSE {
		interceptWriter := &sseInterceptWriter{
			w: writer,
			callback: func(data []byte) {
				if provider.WireFormat() == WireResponses && !acc.codex.CyberAccess && errclass.IsCyberPolicy(data) {
					if h.pinConversationToCyberAccess(conversationID, accountType, requiredPlan, clientIP, acc.ID, reqID) {
						cyberPinned = true
						cancel()
					}
					return
				}
				var obj map[string]any
				if err := json.Unmarshal(data, &obj); err != nil {
					var arr []map[string]any
					if err2 := json.Unmarshal(data, &arr); err2 != nil || len(arr) == 0 {
						return
					}
					obj = arr[0]
				}
				ru := provider.ParseUsage(obj)
				if ru == nil {
					return
				}

				// Anthropic-wire providers split input (message_start) and output
				// (message_delta); accumulate them into a single usage record.
				if requestFormatForCodec(provider) == wire.FormatClaude {
					if claudeAccum2 == nil {
						claudeAccum2 = ru
					} else {
						claudeAccum2.OutputTokens = ru.OutputTokens
						claudeAccum2.BillableTokens = clampNonNegative(
							claudeAccum2.InputTokens - claudeAccum2.CachedInputTokens + ru.OutputTokens)
						ru = claudeAccum2
						claudeAccum2 = nil
						ru.AccountID = acc.ID
						ru.UserID = userID
						ru.OriginID = originID
						ru.AccountType = acc.Type
						acc.mu.Lock()
						ru.PlanType = acc.PlanType
						acc.mu.Unlock()
						// Bridge rate limits from response headers
						if ru.PrimaryUsedPct == 0 && headerPrimaryPct > 0 {
							ru.PrimaryUsedPct = headerPrimaryPct
						}
						if ru.SecondaryUsedPct == 0 && headerSecondaryPct > 0 {
							ru.SecondaryUsedPct = headerSecondaryPct
						}
						usageWriter.add(*ru)
					}
					return
				}
				// Non-Claude usage commits after forwarding this chunk.
				ru.AccountID = acc.ID
				ru.UserID = userID
				ru.OriginID = originID
				ru.AccountType = acc.Type
				acc.mu.Lock()
				ru.PlanType = acc.PlanType
				acc.mu.Unlock()
				usageWriter.add(*ru)
			},
		}
		if provider.WireFormat() == WireResponses && !acc.codex.CyberAccess {
			suppressor := &cyberPolicyHTTPSuppressor{
				h:              h,
				reqID:          reqID,
				conversationID: conversationID,
				requiredPlan:   requiredPlan,
				clientIP:       clientIP,
				accountID:      acc.ID,
				pinned:         &cyberPinned,
			}
			writer = &sseInterceptWriter{w: writer, onEvent: suppressor.onEvent}
		}
		if provider.WireFormat() == WireResponses {
			writer = &hostedmcp.ResponseFilterWriter{W: writer}
		}
		interceptWriter.w = writer
		writer = interceptWriter
	}

	// Wrap response body with idle timeout to kill zombie SSE connections.
	var idleReader *idleTimeoutReader
	if isSSE && h.cfg.streamIdleTimeout > 0 {
		idleReader = newIdleTimeoutReader(resp.Body, h.cfg.streamIdleTimeout, cancel)
		resp.Body = idleReader
	}

	if isSSE {
		usageWriter.w = writer
		writer = usageWriter
	}
	_, copyErr := io.Copy(writer, resp.Body)
	if hw2 != nil {
		if err := hw2.Stop(); copyErr == nil {
			copyErr = err
		}
	}
	if fw != nil {
		if err := fw.stop(); copyErr == nil {
			copyErr = err
		}
	}

	// Flush any accumulated Claude usage that wasn't emitted (e.g., stream ended
	// without message_delta, or only got message_start before error/disconnect).
	if claudeAccum2 != nil {
		claudeAccum2.AccountID = acc.ID
		claudeAccum2.UserID = userID
		claudeAccum2.OriginID = originID
		claudeAccum2.AccountType = acc.Type
		acc.mu.Lock()
		claudeAccum2.PlanType = acc.PlanType
		acc.mu.Unlock()
		if claudeAccum2.PrimaryUsedPct == 0 && headerPrimaryPct > 0 {
			claudeAccum2.PrimaryUsedPct = headerPrimaryPct
		}
		if claudeAccum2.SecondaryUsedPct == 0 && headerSecondaryPct > 0 {
			claudeAccum2.SecondaryUsedPct = headerSecondaryPct
		}
		// Model should already be set from ParseUsage (extracted from message_start)
		h.recordUsage(acc, *claudeAccum2)
		claudeAccum2 = nil
	}

	if copyErr != nil {
		if ctx.Err() == nil {
			h.recent.add(copyErr.Error())
			h.metrics.inc("error", acc.ID)
		}
		if idleReader != nil {
			log.Printf("[%s] SSE stream error (account=%s): %v", reqID, acc.ID, copyErr)
		}
		return
	}

	respSample := sampleBuf.Bytes()
	if h.cfg.logBodies && len(respSample) > 0 {
		if provider.WireFormat() == WireResponses {
			respSample = hostedmcp.FilterResponseSample(respSample, isSSE)
		}
		log.Printf("[%s] response body sample (%d bytes): %s", reqID, len(respSample), safeText(respSample))
	}
	if !codexJSON && !isSSE && len(respSample) > 0 {
		h.updateUsageFromBody(acc, respSample, userID, originID)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if conversationID == "" && len(respSample) > 0 {
			conversationID = requestjson.ConversationIDFromSSE(respSample)
		}
		if conversationID != "" && !cyberPinned {
			h.pool.pin(conversationPinKey(userID, conversationID), acc.ID)
		}
		h.noteCodexTurnState(userID, conversationID, acc, resp.Header.Get(codexTurnStateHeader))
		acc.mu.Lock()
		acc.LastUsed = time.Now()
		if acc.Penalty > 0 {
			acc.Penalty *= 0.5
			if acc.Penalty < 0.01 {
				acc.Penalty = 0
			}
		}
		acc.mu.Unlock()
	}

	h.metrics.inc(strconv.Itoa(resp.StatusCode), acc.ID)

	if h.cfg.debug.Load() {
		rt.LogRequest(r.Context(), reqID, "streamed proxy request completed",
			slog.Int("status", resp.StatusCode), slog.String("account", acc.ID),
			slog.Int64("duration_ms", rt.Now().Sub(start).Milliseconds()))
	}
}

func (h *proxyHandler) proxyPassthroughStreamed(w http.ResponseWriter, r *http.Request, reqID string, providerType AccountType, provider ProviderDriver, targetBase *url.URL, start time.Time, rt requestlog.Runtime) {
	timeout := clientOrDefaultTimeout(r, h.cfg.requestTimeout, h.cfg.streamTimeout, nil)

	ctx := r.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// Build the outgoing request - preserving the original Authorization header
	outURL := new(url.URL)
	*outURL = *r.URL
	outURL.Scheme = targetBase.Scheme
	outURL.Host = targetBase.Host
	outURL.Path = singleJoin(targetBase.Path, provider.NormalizePath(r.URL.Path))

	var reqSample *bytes.Buffer
	var body io.Reader = r.Body
	if (h.cfg.logBodies && h.cfg.bodyLogLimit > 0) || (providerType == AccountTypeClaude && h.cfg.claudeTraceEnabled()) {
		sampleLimit := h.cfg.bodyLogLimit
		if sampleLimit <= 0 || (providerType == AccountTypeClaude && h.cfg.claudeTraceEnabled() && h.cfg.claudeTraceBodyLimit > sampleLimit) {
			sampleLimit = h.cfg.claudeTraceBodyLimit
		}
		reqSample = &bytes.Buffer{}
		body = io.TeeReader(r.Body, &limitedWriter{w: reqSample, n: sampleLimit})
	}

	outReq, err := http.NewRequestWithContext(ctx, r.Method, outURL.String(), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	outReq.Host = targetBase.Host
	outReq.Header = httpwire.CloneHeader(r.Header)
	httpwire.RemoveHopByHopHeaders(outReq.Header)
	removeConflictingProxyHeaders(outReq.Header)
	if r.ContentLength >= 0 {
		outReq.ContentLength = r.ContentLength
	}

	// Force uncompressed responses — SSE streams break with on-the-fly decompression.
	outReq.Header.Set("Accept-Encoding", "identity")

	// For Claude, ensure required headers are set
	if providerType == AccountTypeClaude {
		if outReq.Header.Get("anthropic-version") == "" {
			outReq.Header.Set("anthropic-version", claudecode.AnthropicVersion)
		}
	}

	if h.cfg.debug.Load() {
		log.Printf("[%s] passthrough streamed -> %s %s", reqID, outReq.Method, outReq.URL.String())
	}

	resp, err := h.transport.RoundTrip(outReq)
	if err != nil {
		if providerType == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
			var reqBody []byte
			if reqSample != nil {
				reqBody = reqSample.Bytes()
			}
			h.writeClaudeTrace(reqID, "passthrough_streamed", "", hashRequestOrigin(r, h.originHashSalt()), nil, r, reqBody, outReq, reqBody, nil, wire.TranslateNone, nil, err.Error())
		}
		h.recent.add(err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if providerType == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
		var reqBody []byte
		if reqSample != nil {
			reqBody = reqSample.Bytes()
		}
		sampleLimit := h.claudeTraceSampleLimit(16 * 1024)
		if h.cfg.logBodies && h.cfg.bodyLogLimit > 0 {
			sampleLimit = h.claudeTraceSampleLimit(h.cfg.bodyLogLimit)
		}
		h.attachClaudeTrace(reqID, "passthrough_streamed", "", hashRequestOrigin(r, h.originHashSalt()), nil, r, reqBody, outReq, reqBody, resp, wire.TranslateNone, &bytes.Buffer{}, sampleLimit)
	}

	if h.cfg.logBodies && reqSample != nil && reqSample.Len() > 0 {
		log.Printf("[%s] passthrough request body sample (%d bytes): %s", reqID, reqSample.Len(), safeText(reqSample.Bytes()))
	}

	// Write response to client
	httpwire.CopyHeader(w.Header(), resp.Header)
	httpwire.RemoveHopByHopHeaders(w.Header())
	h.logRateLimitResponseHeaders(reqID, providerType, resp.Header)
	h.replaceUsageHeaders(w.Header())
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	respContentType := resp.Header.Get("Content-Type")
	isSSE := provider.DetectsSSE(r.URL.Path, respContentType)

	var writer io.Writer = w
	var fw *flushWriter
	if isSSE && flusher != nil {
		fw = &flushWriter{w: w, f: flusher, flushInterval: h.cfg.flushInterval}
		writer = fw
		defer fw.stop()
	}
	if isSSE && providerType == AccountTypeCodex {
		writer = &hostedmcp.ResponseFilterWriter{W: writer}
	}

	// Wrap response body with idle timeout to kill zombie SSE connections.
	var idleReader *idleTimeoutReader
	if isSSE && h.cfg.streamIdleTimeout > 0 {
		idleReader = newIdleTimeoutReader(resp.Body, h.cfg.streamIdleTimeout, cancel)
		defer idleReader.Close()
	}

	source := io.Reader(resp.Body)
	if idleReader != nil {
		source = idleReader
	}
	_, copyErr := io.Copy(writer, source)
	if fw != nil {
		if err := fw.stop(); copyErr == nil {
			copyErr = err
		}
	}
	if copyErr != nil {
		h.recent.add(copyErr.Error())
		h.metrics.inc("error", "passthrough")
		if idleReader != nil {
			log.Printf("[%s] passthrough streamed SSE error: %v", reqID, copyErr)
		}
		return
	}

	h.metrics.inc(strconv.Itoa(resp.StatusCode), "passthrough")

	if h.cfg.debug.Load() {
		rt.LogRequest(r.Context(), reqID, "streamed proxy passthrough completed",
			slog.Int("status", resp.StatusCode), slog.Int64("duration_ms", rt.Now().Sub(start).Milliseconds()))
	}
}
