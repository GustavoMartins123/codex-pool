package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-pool-proxy/internal/wire"
	"github.com/google/uuid"
)

func (h *proxyHandler) tryOnce(ctx context.Context, in *http.Request, bodyBytes []byte, targetBase *url.URL, provider ProviderDriver, acc *Account, reqID string, translateDir wire.TranslateDirection, requestedModel, userID, originID, conversationID string) (*http.Response, *bytes.Buffer, bool, error) {
	if err := h.checkLivePolicy(userID, requestedModel, provider.Type()); err != nil {
		return nil, nil, false, err
	}
	state, _ := in.Context().Value(grantRequestContextKey{}).(*grantRequestState)
	ownedState := state == nil
	if ownedState {
		state = newGrantRequestState()
		in = in.WithContext(context.WithValue(in.Context(), grantRequestContextKey{}, state))
	}
	prepared, release, err := h.acquireGovernedAccount(in, userID, conversationPinKey(userID, conversationID), acc, requestedModel)
	if err != nil {
		if ownedState {
			state.Release()
		}
		return nil, nil, false, err
	}
	in = prepared
	ctx = context.WithValue(ctx, grantAdmissionContextKey{}, in.Context().Value(grantAdmissionContextKey{}))
	if ownedState {
		original := release
		release = func() {
			if original != nil {
				original()
			}
			state.Release()
		}
	}
	resp, sample, refreshed, err := h.tryOnceWithAccountSlot(ctx, in, bodyBytes, targetBase, provider, acc, reqID, translateDir, requestedModel, userID, originID, conversationID)
	if resp != nil && resp.Body != nil {
		resp.Body = &accountLeaseBody{ReadCloser: resp.Body, release: release}
	} else {
		release()
	}
	return resp, sample, refreshed, err
}

func (h *proxyHandler) tryOnceWithAccountSlot(
	ctx context.Context,
	in *http.Request,
	bodyBytes []byte,
	targetBase *url.URL,
	provider ProviderDriver,
	acc *Account,
	reqID string,
	translateDir wire.TranslateDirection,
	requestedModel string,
	userID string,
	originID string,
	conversationID string,
) (*http.Response, *bytes.Buffer, bool, error) {
	if acc == nil {
		return nil, nil, false, errors.New("nil account")
	}
	refreshFailed := false // Track if refresh was attempted but failed
	var rawIncomingBody []byte
	if provider.Type() == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
		rawIncomingBody = append([]byte(nil), bodyBytes...)
	}
	preparation := requestPreparationFor(provider)
	if rewrite := preparation.prepareAttemptBody; rewrite != nil {
		bodyBytes = rewrite(bodyBytes, requestedModel)
	}

	if !h.cfg.disableRefresh && h.needsRefresh(acc) {
		if err := h.refreshAccount(ctx, acc); err != nil {
			if isRateLimitError(err) {
				h.applyRateLimit(acc, nil)
			}
			if h.cfg.debug.Load() {
				log.Printf("[%s] refresh %s failed: %v (continuing with existing token)", reqID, acc.ID, err)
			}
		}
	}

	outURL := new(url.URL)
	*outURL = *in.URL
	outURL.Scheme = targetBase.Scheme
	outURL.Host = targetBase.Host
	// Use provider's NormalizePath method for path handling
	outURL.Path = singleJoin(targetBase.Path, provider.NormalizePath(in.URL.Path))
	if provider.WireFormat() == WireResponses && isCodexLegacyLivePath(in.URL.Path) {
		q := outURL.Query()
		q.Set("intent", "quicksilver")
		q.Set("architecture", "avas")
		outURL.RawQuery = q.Encode()
	}

	// For Claude OAuth tokens, add beta=true query param (required for OAuth to work)
	if provider.Type() == AccountTypeClaude && strings.HasPrefix(acc.AccessToken, "sk-ant-oat") {
		q := outURL.Query()
		q.Set("beta", "true")
		outURL.RawQuery = q.Encode()
	}

	var claudeToolNameMapper map[string]string
	contextBody := bodyBytes
	contextObject, _ := in.Context().Value(contextFrameKey{}).(map[string]any)

	buildReq := func() (*http.Request, error) {
		if err := h.checkAccountUse(userID, acc); err != nil {
			return nil, err
		}
		authAccount := acc
		if provider.WireFormat() == WireResponses && isCodexResponsesPath(in.URL.Path) {
			authAccount = contextAuthSnapshot(acc)
			bodyBytes = contextBody
			if contextObject != nil {
				prepared, changed, err := h.prepareContextObject(userID, getClientIP(in), contextObject, authAccount)
				if err != nil {
					return nil, err
				}
				if changed {
					bodyBytes, err = json.Marshal(prepared)
					if err != nil {
						return nil, errContextInvalid
					}
				}
			} else {
				var err error
				bodyBytes, err = h.prepareContextFrame(userID, getClientIP(in), contextBody, authAccount)
				if err != nil {
					return nil, err
				}
			}
		}
		var body io.Reader
		if len(bodyBytes) > 0 {
			body = bytes.NewReader(bodyBytes)
		}
		outReq, err := http.NewRequestWithContext(ctx, in.Method, outURL.String(), body)
		if err != nil {
			return nil, err
		}

		outReq.Host = targetBase.Host
		outReq.Header = cloneHeader(in.Header)
		removeHopByHopHeaders(outReq.Header)
		removeConflictingProxyHeaders(outReq.Header)
		// A client echo minted by another account is a proxy-chain-only
		// contradiction upstream never sees from real Codex; strip it before
		// this attempt's account sees it.
		h.guardCodexTurnStateEcho(userID, conversationID, acc, outReq.Header)

		// Always overwrite client-provided auth; the proxy is the single source of truth.
		outReq.Header.Del("Authorization")
		outReq.Header.Del("ChatGPT-Account-ID")
		outReq.Header.Del("X-Api-Key") // Remove Claude API key from client (might be pool token)
		// Remove Gemini API key header (we use Bearer auth for pool accounts)
		outReq.Header.Del("x-goog-api-key")

		authAccount.mu.Lock()
		access := authAccount.AccessToken
		authAccount.mu.Unlock()

		if access == "" {
			return nil, fmt.Errorf("account %s has empty access token", acc.ID)
		}

		// Use provider's SetAuthHeaders method for provider-specific auth
		provider.SetAuthHeaders(outReq, authAccount)
		// Native Realtime and GPT Live reject the Codex Responses websocket beta
		// header. Keep it only on the Responses transport that requires it.
		if provider.WireFormat() == WireResponses && isCodexRealtimePath(in.URL.Path) {
			outReq.Header.Del("OpenAI-Beta")
		}
		if setAttemptHeaders := preparation.attemptHeaders; setAttemptHeaders != nil {
			setAttemptHeaders(outReq.Header, requestedModel, conversationID)
		}

		if provider.Type() == AccountTypeClaude && translateDir == wire.TranslateNone && strings.HasPrefix(access, "sk-ant-oat") {
			outReq.Header.Set("anthropic-beta", appendAnthropicBeta(outReq.Header.Get("anthropic-beta"), betaOAuth))
		}

		// When translating, body size changes — remove the client's Content-Length
		// so Go's HTTP client recalculates it from the actual body.
		if translateDir != wire.TranslateNone {
			outReq.Header.Del("Content-Length")
			outReq.ContentLength = int64(len(bodyBytes))
		}

		// Always request uncompressed responses. SSE is a line-oriented text
		// protocol — gzip/br compression breaks incremental frame parsing in
		// sseInterceptWriter and causes ZlibError in clients (e.g. Bun's
		// fetch) when compressed chunks split across TCP segments. Deleting
		// the header entirely would let Go's transport auto-add gzip, so we
		// must explicitly set identity.
		outReq.Header.Set("Accept-Encoding", "identity")

		// Force uncompressed response for model catalog so we can inject Claude models
		if strings.Contains(in.URL.Path, "codex/models") {
			outReq.Header.Set("Accept-Encoding", "identity")
		}

		// Adjust headers when doing format translation
		if translateDir == wire.TranslateClaudeToOAI || translateDir == wire.TranslateClaudeToResponses {
			// Client sent Claude headers but upstream is OpenAI/Codex — remove Claude-specific headers
			outReq.Header.Del("anthropic-version")
			outReq.Header.Del("anthropic-beta")
			outReq.Header.Del("anthropic-dangerous-direct-browser-access")
			outReq.Header.Del("Sec-Fetch-Mode")
			outReq.Header.Del("Accept-Language")
			outReq.Header.Del("X-App")
			// Remove x-stainless-* headers (Anthropic SDK internals)
			for key := range outReq.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-stainless-") {
					outReq.Header.Del(key)
				}
			}
			// For Codex Responses API, force SSE accept since Codex always streams
			if translateDir == wire.TranslateClaudeToResponses {
				outReq.Header.Set("Accept", "text/event-stream")
			}
		} else if translateDir == wire.TranslateChatToResponses || translateDir == wire.TranslateCompletionsToResponses || translateDir == wire.TranslateImagesToResponses {
			outReq.Header.Set("Accept", "text/event-stream")
			outReq.Header.Set("Content-Type", "application/json")
		} else if translateDir == wire.TranslateOAIToClaude || translateDir == wire.TranslateResponsesToClaude {
			// All Anthropic-compatible providers need the wire translation, but only
			// native Claude OAuth traffic should impersonate Claude Code.
			acceptHeader := "application/json"
			var bodyObj map[string]any
			if json.Unmarshal(bodyBytes, &bodyObj) == nil {
				if s, ok := bodyObj["stream"].(bool); ok && s {
					acceptHeader = "text/event-stream"
				}
			}

			if provider.Type() == AccountTypeClaude {
				isOAuth := strings.HasPrefix(access, "sk-ant-oat")
				is1M := strings.Contains(strings.ToLower(requestedModel), "[1m]")
				isFastMode := false
				if sp, ok := bodyObj["speed"].(string); ok && sp == "fast" {
					isFastMode = true
				}

				// Pace requests per session to avoid burst patterns.
				sessionID := h.claudeIdentity.sessionHeader(in, userID)
				h.pacer.wait(sessionID)

				acc.mu.Lock()
				accUUID := acc.claude.UUID
				acc.mu.Unlock()
				h.claudeIdentity.injectMetadata(bodyObj, accUUID, userID, sessionID)
				if !bodyHasClaudeSystemBlocks(bodyObj) {
					bodyBytes = ccInjectSystemBlocks(bodyObj, bodyBytes)
					_ = json.Unmarshal(bodyBytes, &bodyObj)
				}
				if reordered, err := wire.MarshalAnthropicRequest(bodyObj); err == nil {
					bodyBytes = reordered
				}

				bodyModel := ""
				if m, ok := bodyObj["model"].(string); ok {
					bodyModel = m
				}
				if bodyModel == "" {
					bodyModel = requestedModel
				}

				outReq.Header.Set("anthropic-version", ccAnthropicVersion)
				hasStructuredOutputs := ccRequestHasStructuredOutputs(bodyObj)
				hasTaskBudget := ccRequestHasTaskBudget(bodyObj)
				outReq.Header.Set("anthropic-beta", ccBetaHeader(bodyModel, isOAuth, is1M, isFastMode, hasStructuredOutputs, hasTaskBudget))
				outReq.Header.Set("anthropic-dangerous-direct-browser-access", "true")
				outReq.Header.Set("User-Agent", ccUserAgent())
				outReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
				outReq.Header.Set("X-App", "cli")
				outReq.Header.Set("x-client-request-id", uuid.NewString())
				outReq.Header.Set("Accept", acceptHeader)
				outReq.Header.Set("Accept-Language", "*")
				outReq.Header.Set("Content-Type", "application/json")
				outReq.Header.Set("Sec-Fetch-Mode", "cors")
				ccStainlessHeaders(outReq.Header.Set)
			} else {
				// ZAI and other Anthropic-compatible providers should receive a clean
				// Messages API request without Claude OAuth fingerprint headers.
				if outReq.Header.Get("anthropic-version") == "" {
					outReq.Header.Set("anthropic-version", ccAnthropicVersion)
				}
				outReq.Header.Set("Accept", acceptHeader)
				outReq.Header.Set("Content-Type", "application/json")
				outReq.Header.Del("anthropic-beta")
				outReq.Header.Del("anthropic-dangerous-direct-browser-access")
				outReq.Header.Del("X-Claude-Code-Session-Id")
				outReq.Header.Del("X-App")
				for key := range outReq.Header {
					if strings.HasPrefix(strings.ToLower(key), "x-stainless-") {
						outReq.Header.Del(key)
					}
				}
			}
			outReq.Header.Del("openai-beta")
			outReq.Header.Del("openai-organization")
		}

		if provider.Type() == AccountTypeClaude &&
			translateDir == wire.TranslateNone &&
			strings.HasPrefix(access, "sk-ant-oat") {
			var bodyObj map[string]any
			if json.Unmarshal(bodyBytes, &bodyObj) == nil && !ccIsGenuineClaudeCodeRequest(in, bodyObj) {
				sessionID := h.claudeIdentity.sessionHeader(in, userID)
				acc.mu.Lock()
				accUUID := acc.claude.UUID
				acc.mu.Unlock()
				h.claudeIdentity.injectMetadata(bodyObj, accUUID, userID, sessionID)
				if _, ok := bodyObj["tools"]; !ok {
					bodyObj["tools"] = []any{}
				}
				if _, ok := bodyObj["temperature"]; !ok {
					bodyObj["temperature"] = 1
				}
				if !bodyHasClaudeSystemBlocks(bodyObj) {
					bodyBytes = ccInjectSystemBlocks(bodyObj, bodyBytes)
					_ = json.Unmarshal(bodyBytes, &bodyObj)
				}
				if reordered, err := wire.MarshalAnthropicRequest(bodyObj); err == nil {
					bodyBytes = reordered
				}

				model, _ := bodyObj["model"].(string)
				outReq.Header.Set("anthropic-version", ccAnthropicVersion)
				outReq.Header.Set("anthropic-beta", ccBetaHeader(model, true, false, false, ccRequestHasStructuredOutputs(bodyObj), ccRequestHasTaskBudget(bodyObj)))
				outReq.Header.Set("anthropic-dangerous-direct-browser-access", "true")
				outReq.Header.Set("User-Agent", ccUserAgent())
				outReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
				outReq.Header.Set("X-App", "cli")
				outReq.Header.Set("x-client-request-id", uuid.NewString())
				outReq.Header.Set("Accept-Language", "*")
				outReq.Header.Set("Sec-Fetch-Mode", "cors")
				ccStainlessHeaders(outReq.Header.Set)
			}
		}

		if provider.Type() == AccountTypeClaude && strings.HasPrefix(access, "sk-ant-oat") {
			if normalized, hits, changed := normalizeAnthropicDateline(bodyBytes); changed {
				bodyBytes = normalized
				if h.cfg.debug.Load() {
					log.Printf("[%s] normalized %d Anthropic dateline fingerprint(s)", reqID, len(hits))
				}
			}
		}

		// Body fingerprinting and dateline normalization happen after the request
		// is constructed, so replace the reader with the final bytes.
		if len(bodyBytes) > 0 {
			outReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			outReq.ContentLength = int64(len(bodyBytes))
			outReq.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(bodyBytes)), nil
			}
			outReq.Header.Del("Content-Length")
		}

		if h.cfg.debug.Load() {
			log.Printf("[%s] outgoing headers (%s): %v", reqID, provider.Type(), debugHeaderSummary(outReq.Header))
		}

		// Keep the original User-Agent from the client - don't override it
		return outReq, nil
	}

	outReq, err := buildReq()
	if err != nil {
		if provider.Type() == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
			h.writeClaudeTrace(reqID, "pool", userID, originID, acc, in, rawIncomingBody, nil, bodyBytes, nil, translateDir, nil, err.Error())
		}
		return nil, nil, false, err
	}

	if h.cfg.debug.Load() {
		acc.mu.Lock()
		log.Printf("[%s] -> %s %s (account=%s account_id=%s)", reqID, outReq.Method, outReq.URL.String(), acc.ID, acc.codex.AccountID)
		acc.mu.Unlock()
	}

	resp, err := h.policyRoundTrip(outReq, provider.Type(), policyAdmissionFromRequest(in))
	captureCodexResponseState(h.pool.antigravity, acc, resp, reqID)
	if resp != nil && len(claudeToolNameMapper) > 0 {
		resp.Header.Del("Content-Length")
		resp.Body = newClaudeToolNameReadCloser(resp.Body, claudeToolNameMapper)
	}
	if err != nil {
		if provider.Type() == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
			h.writeClaudeTrace(reqID, "pool", userID, originID, acc, in, rawIncomingBody, outReq, bodyBytes, resp, translateDir, nil, err.Error())
		}
		acc.mu.Lock()
		acc.Penalty += 0.2
		acc.mu.Unlock()
		return nil, nil, false, err
	}

	// If we got a 401/403, try to refresh and retry on the *same* account once.
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !h.cfg.disableRefresh {
		if provider.WireFormat() == WireResponses && upstreamTokenRevoked(resp) {
			h.disableAccountPermanently(acc, reqID, "upstream token revoked")
			refreshFailed = true
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		// Log the error response body for debugging
		if h.cfg.debug.Load() {
			decompressed := bodyForInspection(nil, errBody)
			log.Printf("[%s] got %d from upstream, body: %s", reqID, resp.StatusCode, safeText(decompressed))
		}
		acc.mu.Lock()
		hasRefresh := acc.RefreshToken != ""
		acc.mu.Unlock()
		if hasRefresh {
			if err := h.refreshAccountAfterAuthFailure(ctx, acc); err == nil {
				outReq, err = buildReq()
				if err != nil {
					if provider.Type() == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
						h.writeClaudeTrace(reqID, "pool", userID, originID, acc, in, rawIncomingBody, nil, bodyBytes, nil, translateDir, nil, err.Error())
					}
					return nil, nil, false, err
				}
				if h.cfg.debug.Load() {
					acc.mu.Lock()
					log.Printf("[%s] retry after refresh -> %s %s (account=%s account_id=%s)", reqID, outReq.Method, outReq.URL.String(), acc.ID, acc.codex.AccountID)
					acc.mu.Unlock()
				}
				resp, err = h.policyRoundTrip(outReq, provider.Type(), policyAdmissionFromRequest(in))
				captureCodexResponseState(h.pool.antigravity, acc, resp, reqID)
				if resp != nil && len(claudeToolNameMapper) > 0 {
					resp.Header.Del("Content-Length")
					resp.Body = newClaudeToolNameReadCloser(resp.Body, claudeToolNameMapper)
				}
				if err != nil {
					if provider.Type() == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
						h.writeClaudeTrace(reqID, "pool", userID, originID, acc, in, rawIncomingBody, outReq, bodyBytes, resp, translateDir, nil, err.Error())
					}
					acc.mu.Lock()
					acc.Penalty += 0.2
					acc.mu.Unlock()
					return nil, nil, false, err
				}
				// Log response after retry
				if h.cfg.debug.Load() && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
					retryErrBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
					_ = resp.Body.Close()
					decompressed := bodyForInspection(nil, retryErrBody)
					log.Printf("[%s] after refresh retry got %d, body: %s", reqID, resp.StatusCode, safeText(decompressed))
					// Recreate body for downstream processing
					resp.Body = io.NopCloser(bytes.NewReader(retryErrBody))
				}
				// Refresh succeeded - if we still get 401/403 after refresh,
				// the account is truly dead (fresh token still rejected)
			} else {
				errStr := err.Error()
				if isRateLimitError(err) {
					h.applyRateLimit(acc, nil)
				} else if isPermanentRefreshTokenError(err) {
					if retireAfterRefreshFail(acc, err, time.Now()) {
						persistDeadAccount(h.pool.antigravity, acc, "refresh token revoked/invalid")
					} else {
						markAccountNeedsLogin(h.pool.antigravity, acc, errStr)
					}
					refreshFailed = true
				} else if !strings.Contains(errStr, "rate limited") {
					// Other non-rate-limited failures also count as refresh failed
					refreshFailed = true
				}
				if h.cfg.debug.Load() {
					log.Printf("[%s] refresh failed for %s: %v (refreshFailed=%v)", reqID, acc.ID, err, refreshFailed)
				}
			}
		} else {
			// No refresh token available - can't recover from 401/403
			refreshFailed = true
		}
	}

	if provider.WireFormat() == WireResponses && isCodexRealtimeCallCreatePath(in.URL.Path) && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if callID := codexRealtimeCallIDFromLocation(resp.Header.Get("Location")); callID != "" {
			h.pool.pin(codexRealtimeCallPinKey(callID), acc.ID)
			// OpenAI currently returns 201 with text/plain even though the body is
			// an SDP answer. Normalize the public contract for Codex clients that
			// require the documented signaling shape.
			resp.StatusCode = http.StatusOK
			resp.Status = "200 OK"
			resp.Header.Set("Content-Type", "application/sdp")
			if h.cfg.debug.Load() {
				log.Printf("[%s] pinned realtime call %s to account %s", reqID, callID, acc.ID)
			}
		}
	}

	// Tee only the bounded sample required for history, tracing or translation.
	// The tee forwards each chunk immediately; history parsing happens after EOF.
	sampleLimit := int64(16 * 1024)
	if conversationID != "" {
		sampleLimit = 256 * 1024
	}
	if h.cfg.logBodies && h.cfg.bodyLogLimit > 0 {
		sampleLimit = h.cfg.bodyLogLimit
	}
	sampleLimit = h.claudeTraceSampleLimit(sampleLimit)
	var buf *bytes.Buffer
	if provider.Type() == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
		buf = &bytes.Buffer{}
		h.attachClaudeTrace(reqID, "pool", userID, originID, acc, in, rawIncomingBody, outReq, bodyBytes, resp, translateDir, buf, sampleLimit)
	} else if shouldSampleResponseBodyForRequest(provider, acc, in.URL.Path, resp, translateDir, conversationID, h.cfg.logBodies) {
		buf = &bytes.Buffer{}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.TeeReader(resp.Body, &limitedWriter{w: buf, n: sampleLimit}),
			Closer: resp.Body,
		}
	}
	return resp, buf, refreshFailed, nil
}
