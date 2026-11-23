package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/errclass"
	"codex-pool-proxy/internal/httpwire"
	"codex-pool-proxy/internal/requestjson"
	"codex-pool-proxy/internal/wire"
)

type proxyHTTPResponse struct {
	account              *Account
	sample               *bytes.Buffer
	provider             ProviderDriver
	accountType          AccountType
	admission            *policyAdmission
	requestID            string
	userID               string
	originID             string
	originIP             string
	conversationID       string
	model                string
	requiredPlan         string
	translation          wire.TranslateDirection
	imagesResponseFormat string
	qwen                 *qwenResponsesRequest
	nonStreaming         bool
	compact              bool
	attempt              int
	attempts             int
	runtime              proxyRuntime
	startedAt            time.Time

	primaryUsedPct   float64
	secondaryUsedPct float64
	isSSE            bool
	codexJSON        bool
	cyberPinned      bool
}

// writeProxyHTTPResponse asks dispatch for another account only when a
// buffered response detects a retryable policy rejection before sending data.
func (h *proxyHandler) writeProxyHTTPResponse(
	ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, r *http.Request,
	resp *http.Response, opts proxyHTTPResponse,
) (retryWithCyberAccount bool) {
	acc, sampleBuf := opts.account, opts.sample
	if opts.compact {
		if err := compactHTTPResponse(resp, cancel); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return false
		}
	}

	// Success path — reset exponential backoff.
	acc.mu.Lock()
	acc.BackoffLevel = 0
	acc.mu.Unlock()

	if quota, ok := opts.provider.(RateLimitHeaderParser); ok {
		quota.ParseUsageHeaders(acc, resp.Header)
	}
	h.logRateLimitResponseHeaders(opts.requestID, acc.Type, resp.Header)

	// Snapshot rate limits from headers for use in SSE callback
	// (Claude SSE events carry 0% — real data comes from headers)
	acc.mu.Lock()
	opts.primaryUsedPct = acc.Usage.PrimaryUsedPercent
	opts.secondaryUsedPct = acc.Usage.SecondaryUsedPercent
	acc.mu.Unlock()

	// Prepare response headers.
	httpwire.CopyHeader(w.Header(), resp.Header)
	httpwire.RemoveHopByHopHeaders(w.Header())
	h.replaceUsageHeaders(w.Header())

	// Complete the catalog and set the order used by the Codex picker.
	if strings.Contains(r.URL.Path, "codex/models") && resp.StatusCode == 200 {
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding") // We'll return uncompressed JSON
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr == nil {
			modified, err := finalizeCodexModelCatalog(injectClaudeModels(h.pool.antigravity, respBody), codexCatalogDescriptors(h.pool))
			if err != nil {
				log.Printf("[%s] %v", opts.requestID, err)
				respondJSONError(w, http.StatusBadGateway, "invalid Codex model catalog")
				return false
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(modified)
		} else {
			w.WriteHeader(http.StatusBadGateway)
		}
		return false
	}

	flusher, _ := w.(http.Flusher)
	respContentType := resp.Header.Get("Content-Type")
	opts.isSSE = opts.provider.DetectsSSE(r.URL.Path, respContentType)
	// When translating to Responses API, the path-based SSE detection may
	// incorrectly flag non-SSE error responses (plain JSON 4xx/5xx) as SSE.
	// Check the actual content-type on error responses to avoid feeding
	// plain JSON through the SSE translator (which would silently drop it).
	if opts.isSSE && resp.StatusCode >= 400 {
		if !strings.Contains(strings.ToLower(respContentType), "text/event-stream") {
			opts.isSSE = false
		}
	}
	if opts.provider.WireFormat() == WireResponses && isCodexResponsesPath(r.URL.Path) && !opts.isSSE {
		response, err := filterHostedMCPNonStreamingResponse(resp)
		if err != nil {
			resp.Body.Close()
			http.Error(w, err.Error(), http.StatusBadGateway)
			return false
		}
		opts.codexJSON = opts.accountType == AccountTypeCodex && resp.StatusCode >= 200 && resp.StatusCode < 300
		if opts.codexJSON {
			h.recordCodexJSONUsage(acc, opts.provider, response, opts.requestID, opts.userID, opts.originID, opts.admission)
		}
		w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
	}
	if opts.isSSE {
		applyStreamingResponseHeaders(w.Header())
	}
	if h.cfg.debug.Load() {
		log.Printf("[%s] response: isSSE=%v content-type=%s translateDir=%d status=%d", opts.requestID, opts.isSSE, respContentType, opts.translation, resp.StatusCode)
	}

	// Client wanted non-streaming but Codex requires streaming:
	// buffer SSE events and assemble a non-streaming response.
	if opts.qwen != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		payload := func(data []byte) {
			if sampleBuf != nil {
				sampleBuf.Write(data)
			}
			var object map[string]any
			if json.Unmarshal(data, &object) == nil {
				if usage := opts.provider.ParseUsage(object); usage != nil {
					usage.AccountID, usage.AccountType, usage.UserID, usage.OriginID = acc.ID, acc.Type, opts.userID, opts.originID
					acc.mu.Lock()
					usage.PlanType = acc.PlanType
					acc.mu.Unlock()
					if usage.Model == "" {
						usage.Model = opts.model
					}
					h.recordUsage(acc, *usage, opts.admission)
				}
			}
		}
		if err := writeQwenResponsesResponse(w, resp, opts.qwen, h.cfg.streamIdleTimeout, cancel, payload); err != nil {
			if ctx.Err() == nil {
				h.recent.add(err.Error())
				h.metrics.inc("error", acc.ID)
			}
			return false
		}
	} else if opts.nonStreaming && opts.isSSE && opts.translation == wire.TranslateNone && opts.provider.WireFormat() == WireResponses && strings.HasPrefix(r.URL.Path, "/v1/responses") {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "application/json")

		bufWriter := wire.NewResponsesBuffer(opts.model)
		inspectWriter := h.wrapBufferedSSEWithCyberDetector(bufWriter, opts.accountType, acc, opts.conversationID, opts.requiredPlan, opts.originIP, opts.requestID, &opts.cyberPinned)
		inspectWriter = &hostedMCPResponseFilterWriter{w: inspectWriter}
		readErr := copyBufferedProxySSE(inspectWriter, resp.Body)

		if h.shouldRetryBufferedSSEForCyberPolicy(opts.cyberPinned, opts.attempt, opts.attempts, acc, opts.requestID, "responses") {
			return true
		}

		if opts.accountType == AccountTypeCodex && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			h.recordCodexJSONUsage(acc, opts.provider, bufWriter.FinalUsage(), opts.requestID, opts.userID, opts.originID, opts.admission)
		}
		if readErr != nil {
			h.writeProxyHTTPReadError(w, acc, readErr)
			return false
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(bufWriter.Result())
	} else if opts.nonStreaming && opts.isSSE && opts.translation == wire.TranslateClaudeToResponses {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "application/json")

		usageCallback := opts.translatedSSEUsage(h)

		bufWriter := wire.NewResponsesToAnthropicBuffer(opts.model, wire.StreamOptions{OnEvent: usageCallback, Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		var responseWriter io.Writer = bufWriter
		if opts.provider.WireFormat() == WireResponses {
			responseWriter = &hostedMCPResponseFilterWriter{w: responseWriter}
		}

		if err := copyBufferedProxySSE(responseWriter, resp.Body); err != nil {
			h.writeProxyHTTPReadError(w, acc, err)
			return false
		}

		if h.shouldRetryBufferedSSEForCyberPolicy(opts.cyberPinned, opts.attempt, opts.attempts, acc, opts.requestID, "claude") {
			return true
		}

		result := bufWriter.Result()
		w.WriteHeader(resp.StatusCode)
		w.Write(result)
	} else if opts.nonStreaming && opts.isSSE && opts.translation == wire.TranslateImagesToResponses {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "application/json")

		bufWriter := wire.NewResponsesBuffer(opts.model)
		inspectWriter := h.wrapBufferedSSEWithCyberDetector(bufWriter, opts.accountType, acc, opts.conversationID, opts.requiredPlan, opts.originIP, opts.requestID, &opts.cyberPinned)
		var rawImageStream bytes.Buffer
		if opts.provider.WireFormat() == WireResponses {
			inspectWriter = &hostedMCPResponseFilterWriter{w: io.MultiWriter(inspectWriter, &rawImageStream)}
		} else {
			inspectWriter = io.MultiWriter(inspectWriter, &rawImageStream)
		}
		if err := copyBufferedProxySSE(inspectWriter, resp.Body); err != nil {
			h.writeProxyHTTPReadError(w, acc, err)
			return false
		}
		if h.shouldRetryBufferedSSEForCyberPolicy(opts.cyberPinned, opts.attempt, opts.attempts, acc, opts.requestID, "images") {
			return true
		}
		result := bufWriter.Result()
		translated, err := wire.ResponsesResponseToImagesGeneration(result, opts.imagesResponseFormat)
		if err != nil {
			recordImageGenerationResult(acc, false)
			h.writeImageGenerationTrace(opts.requestID, acc, rawImageStream.Bytes(), result, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return false
		}
		recordImageGenerationResult(acc, true)
		w.WriteHeader(resp.StatusCode)
		w.Write(translated)
	} else if opts.nonStreaming && opts.isSSE && opts.translation == wire.TranslateCompletionsToResponses {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "application/json")

		usageCallback := opts.translatedSSEUsage(h)

		bufWriter := wire.NewResponsesToOpenAICompletionsBuffer(wire.StreamOptions{OnEvent: usageCallback, Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		var responseWriter io.Writer = bufWriter
		if opts.provider.WireFormat() == WireResponses {
			responseWriter = &hostedMCPResponseFilterWriter{w: responseWriter}
		}

		if err := copyBufferedProxySSE(responseWriter, resp.Body); err != nil {
			h.writeProxyHTTPReadError(w, acc, err)
			return false
		}

		if h.shouldRetryBufferedSSEForCyberPolicy(opts.cyberPinned, opts.attempt, opts.attempts, acc, opts.requestID, "completions") {
			return true
		}

		result := bufWriter.Result()
		w.WriteHeader(resp.StatusCode)
		w.Write(result)
	} else if opts.nonStreaming && opts.isSSE && opts.translation == wire.TranslateChatToResponses {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "application/json")

		usageCallback := opts.translatedSSEUsage(h)

		bufWriter := wire.NewResponsesToOpenAIChatBuffer(wire.StreamOptions{OnEvent: usageCallback, Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		var responseWriter io.Writer = bufWriter
		if opts.provider.WireFormat() == WireResponses {
			responseWriter = &hostedMCPResponseFilterWriter{w: responseWriter}
		}

		if err := copyBufferedProxySSE(responseWriter, resp.Body); err != nil {
			h.writeProxyHTTPReadError(w, acc, err)
			return false
		}

		if h.shouldRetryBufferedSSEForCyberPolicy(opts.cyberPinned, opts.attempt, opts.attempts, acc, opts.requestID, "chat") {
			return true
		}

		result := bufWriter.Result()
		w.WriteHeader(resp.StatusCode)
		w.Write(result)
	} else if !opts.isSSE && opts.translation != wire.TranslateNone {
		if !h.writeTranslatedProxyHTTPJSON(w, resp, &opts) {
			return false
		}
	} else if !h.writeProxyHTTPBody(ctx, cancel, w, resp, flusher, &opts) {
		return false
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if opts.conversationID != "" && sampleBuf != nil && sampleBuf.Len() > 0 {
			if text := extractAssistantTextFromResponseSample(sampleBuf.Bytes()); text != "" {
				h.contextHandoff.RecordAssistantText(conversationScopedKey(opts.userID, opts.conversationID), opts.accountType, text)
			}
		}
		if opts.conversationID == "" {
			if sampleBuf != nil && sampleBuf.Len() > 0 {
				opts.conversationID = requestjson.ConversationIDFromSSE(sampleBuf.Bytes())
			}
		}
		if opts.conversationID != "" && !opts.cyberPinned {
			h.pool.pin(conversationPinKey(opts.userID, opts.conversationID), acc.ID)
		}
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
		opts.runtime.logRequest(r.Context(), opts.requestID, "proxy request completed",
			slog.Int("status", resp.StatusCode), slog.String("account", acc.ID),
			slog.Int64("duration_ms", opts.runtime.now().Sub(opts.startedAt).Milliseconds()))
	}
	return false
}

func copyBufferedProxySSE(dst io.Writer, body io.ReadCloser) error {
	defer body.Close()
	_, err := io.Copy(dst, body)
	return err
}

func (h *proxyHandler) writeProxyHTTPReadError(w http.ResponseWriter, acc *Account, err error) {
	h.recent.add(err.Error())
	h.metrics.inc("error", acc.ID)
	w.Header().Del("Content-Encoding")
	respondJSONError(w, http.StatusBadGateway, "could not read upstream response")
}

func (opts *proxyHTTPResponse) translatedSSEUsage(h *proxyHandler) func([]byte) {
	acc, sampleBuf := opts.account, opts.sample
	return func(data []byte) {
		if sampleBuf != nil {
			sampleBuf.Write(data)
		}
		if opts.provider.WireFormat() == WireResponses && !acc.codex.CyberAccess && errclass.IsCyberPolicy(data) {
			if h.pinConversationToCyberAccess(opts.conversationID, opts.accountType, opts.requiredPlan, opts.originIP, acc.ID, opts.requestID) {
				opts.cyberPinned = true
			}
			return
		}
		var obj map[string]any
		if json.Unmarshal(data, &obj) == nil {
			if ru := opts.provider.ParseUsage(obj); ru != nil {
				if ru.PrimaryUsedPct == 0 && opts.primaryUsedPct > 0 {
					ru.PrimaryUsedPct = opts.primaryUsedPct
				}
				if ru.SecondaryUsedPct == 0 && opts.secondaryUsedPct > 0 {
					ru.SecondaryUsedPct = opts.secondaryUsedPct
				}
				ru.UserID = opts.userID
				ru.OriginID = opts.originID
				ru.AccountType = acc.Type
				h.recordUsage(acc, *ru, opts.admission)
			}
		}
	}
}

func (h *proxyHandler) writeTranslatedProxyHTTPJSON(w http.ResponseWriter, resp *http.Response, opts *proxyHTTPResponse) bool {
	acc, sampleBuf := opts.account, opts.sample
	// Non-SSE format translation: buffer the whole body, translate, write.
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")

	responseDir := chatMessagesResponseDirection(opts.translation)
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		h.writeProxyHTTPReadError(w, acc, readErr)
		return false
	}
	// Let the existing usage/sample logic see the raw body
	if sampleBuf != nil {
		sampleBuf.Write(respBody)
	}
	if !opts.codexJSON {
		h.updateUsageFromBody(acc, respBody, opts.userID, opts.originID, opts.admission)
	}

	var translated []byte
	var translateErr error
	switch {
	case responseDir != wire.TranslateNone:
		switch {
		case resp.StatusCode >= 400:
			translated, translateErr = wire.TranslateChatMessagesError(respBody, responseDir)
		case responseDir == wire.TranslateOAIToClaude:
			translated, translateErr = wire.OpenAIChatResponseToAnthropic(respBody, opts.model)
		case responseDir == wire.TranslateClaudeToOAI:
			translated, translateErr = wire.AnthropicResponseToOpenAIChat(respBody)
		}
	case resp.StatusCode >= 400:
		// Error response - translate error format
		if opts.translation == wire.TranslateClaudeToResponses {
			// Translate Codex error to Claude error format
			translated = wire.ErrorToClaudeFormat(respBody, resp.StatusCode)
		} else {
			translated = respBody
		}
	case opts.translation == wire.TranslateResponsesToClaude:
		translated, translateErr = wire.AnthropicResponseToResponses(respBody)
	case opts.translation == wire.TranslateChatToResponses:
		translated, translateErr = wire.ResponsesResponseToOpenAIChat(respBody)
	case opts.translation == wire.TranslateCompletionsToResponses:
		translated, translateErr = wire.ResponsesResponseToOpenAICompletions(respBody)
	case opts.translation == wire.TranslateImagesToResponses:
		translated, translateErr = wire.ResponsesResponseToImagesGeneration(respBody, opts.imagesResponseFormat)
		recordImageGenerationResult(acc, translateErr == nil)
	case opts.translation == wire.TranslateClaudeToResponses:
		translated, translateErr = wire.OpenAIChatResponseToAnthropic(respBody, opts.model)
	default:
		translateErr = fmt.Errorf("unsupported response translation direction %d", opts.translation)
	}
	if translateErr != nil {
		if h.cfg.debug.Load() {
			log.Printf("[%s] response translation error (direction=%d): %v", opts.requestID, opts.translation, translateErr)
		}
		h.metrics.inc("error", acc.ID)
		w.Header().Del("Content-Encoding")
		respondJSONError(w, http.StatusBadGateway, "invalid upstream response for translation")
		return false
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(translated)
	return true
}

func (h *proxyHandler) writeProxyHTTPBody(
	ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, resp *http.Response,
	flusher http.Flusher, opts *proxyHTTPResponse,
) bool {
	acc, sampleBuf := opts.account, opts.sample
	// Normal response path (streaming or no translation needed)
	if opts.isSSE && opts.translation != wire.TranslateNone {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.StatusCode)

	var writer io.Writer = w
	var fw *flushWriter
	var hw *heartbeatWriter
	if opts.isSSE && flusher != nil {
		fw = &flushWriter{w: w, f: flusher, flushInterval: h.cfg.flushInterval}
		hw = newHeartbeatWriter(fw, flusher)
		writer = hw
	}

	usageWriter := &streamUsageWriter{record: func(usage RequestUsage) { h.recordUsage(acc, usage, opts.admission) }}
	var claudeAccum *RequestUsage

	usageCallback := func(data []byte) {
		if opts.provider.WireFormat() == WireResponses && !acc.codex.CyberAccess && errclass.IsCyberPolicy(data) {
			if h.pinConversationToCyberAccess(opts.conversationID, opts.accountType, opts.requiredPlan, opts.originIP, acc.ID, opts.requestID) {
				opts.cyberPinned = true
				cancel()
			}
			return
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			var arr []map[string]any
			if err2 := json.Unmarshal(data, &arr); err2 != nil || len(arr) == 0 {
				if h.cfg.debug.Load() {
					log.Printf("[%s] SSE callback: failed to parse JSON: %v", opts.requestID, err)
				}
				return
			}
			obj = arr[0]
		}
		ru := opts.provider.ParseUsage(obj)
		if ru == nil {
			return
		}

		if requestFormatForCodec(opts.provider) == wire.FormatClaude {
			if claudeAccum == nil {
				claudeAccum = ru
			} else {
				claudeAccum.OutputTokens = ru.OutputTokens
				claudeAccum.BillableTokens = clampNonNegative(
					claudeAccum.InputTokens - claudeAccum.CachedInputTokens - claudeAccum.CacheCreationTokens + ru.OutputTokens)
				ru = claudeAccum
				claudeAccum = nil
				ru.AccountID = acc.ID
				ru.UserID = opts.userID
				ru.OriginID = opts.originID
				ru.AccountType = acc.Type
				acc.mu.Lock()
				ru.PlanType = acc.PlanType
				acc.mu.Unlock()
				if ru.PrimaryUsedPct == 0 && opts.primaryUsedPct > 0 {
					ru.PrimaryUsedPct = opts.primaryUsedPct
				}
				if ru.SecondaryUsedPct == 0 && opts.secondaryUsedPct > 0 {
					ru.SecondaryUsedPct = opts.secondaryUsedPct
				}
				if ru.Model == "" {
					ru.Model = opts.model
				}
				usageWriter.add(*ru)
			}
			return
		}
		ru.AccountID = acc.ID
		ru.UserID = opts.userID
		ru.OriginID = opts.originID
		ru.AccountType = acc.Type
		acc.mu.Lock()
		ru.PlanType = acc.PlanType
		acc.mu.Unlock()
		if ru.Model == "" {
			ru.Model = opts.model
		}
		usageWriter.add(*ru)
	}

	if opts.isSSE {
		if opts.translation == wire.TranslateCompletionsToResponses {
			writer = wire.NewResponsesToOpenAICompletionsStream(writer, wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		} else if opts.translation == wire.TranslateChatToResponses {
			writer = wire.NewResponsesToOpenAIChatStream(writer, wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		} else if opts.translation == wire.TranslateResponsesToClaude {
			// Claude SSE response → Responses API SSE
			writer = wire.NewAnthropicToResponsesStream(writer, wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		} else if opts.translation == wire.TranslateClaudeToResponses {
			// Responses API SSE → Claude SSE
			writer = wire.NewResponsesToAnthropicStream(writer, wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: opts.requestID})
		} else if opts.translation == wire.TranslateOAIToClaude {
			writer = wire.NewAnthropicToChatStream(writer)
		} else if opts.translation == wire.TranslateClaudeToOAI {
			writer = wire.NewChatToAnthropicStream(writer)
		} else if opts.provider.WireFormat() == WireResponses && !acc.codex.CyberAccess {
			suppressor := &cyberPolicyHTTPSuppressor{
				h: h, reqID: opts.requestID, conversationID: opts.conversationID,
				requiredPlan: opts.requiredPlan, clientIP: opts.originIP, accountID: acc.ID, pinned: &opts.cyberPinned,
			}
			writer = &sseInterceptWriter{w: writer, onEvent: suppressor.onEvent}
		}
		if opts.provider.WireFormat() == WireResponses {
			writer = &hostedMCPResponseFilterWriter{w: writer}
		}
		// Inspect received bytes even if translation or filtering stops on a write error.
		writer = &sseInterceptWriter{w: writer, callback: usageCallback}
	}

	var idleReader *idleTimeoutReader
	if opts.isSSE && h.cfg.streamIdleTimeout > 0 {
		idleReader = newIdleTimeoutReader(resp.Body, h.cfg.streamIdleTimeout, cancel)
		resp.Body = idleReader
	}

	if opts.isSSE {
		usageWriter.w = writer
		writer = usageWriter
	}
	_, copyErr := io.Copy(writer, resp.Body)
	resp.Body.Close()
	if hw != nil {
		if err := hw.Stop(); copyErr == nil {
			copyErr = err
		}
	}
	if fw != nil {
		if err := fw.stop(); copyErr == nil {
			copyErr = err
		}
	}

	if claudeAccum != nil {
		claudeAccum.AccountID = acc.ID
		claudeAccum.UserID = opts.userID
		claudeAccum.OriginID = opts.originID
		claudeAccum.AccountType = acc.Type
		acc.mu.Lock()
		claudeAccum.PlanType = acc.PlanType
		acc.mu.Unlock()
		if claudeAccum.PrimaryUsedPct == 0 && opts.primaryUsedPct > 0 {
			claudeAccum.PrimaryUsedPct = opts.primaryUsedPct
		}
		if claudeAccum.SecondaryUsedPct == 0 && opts.secondaryUsedPct > 0 {
			claudeAccum.SecondaryUsedPct = opts.secondaryUsedPct
		}
		if claudeAccum.Model == "" {
			claudeAccum.Model = opts.model
		}
		h.recordUsage(acc, *claudeAccum, opts.admission)
	}

	if copyErr != nil {
		if ctx.Err() == nil {
			// Only record as error if client didn't disconnect.
			h.recent.add(copyErr.Error())
			h.metrics.inc("error", acc.ID)
		}
		if idleReader != nil {
			log.Printf("[%s] SSE stream error (account=%s): %v", opts.requestID, acc.ID, copyErr)
		}
		return false
	}

	respSample := []byte(nil)
	if sampleBuf != nil {
		respSample = sampleBuf.Bytes()
	}
	if h.cfg.logBodies && len(respSample) > 0 {
		if opts.provider.WireFormat() == WireResponses {
			respSample = filterHostedMCPResponseSample(respSample, opts.isSSE)
		}
		log.Printf("[%s] response body sample (%d bytes): %s", opts.requestID, len(respSample), safeText(respSample))
	}
	if !opts.codexJSON && !opts.isSSE && len(respSample) > 0 {
		h.updateUsageFromBody(acc, respSample, opts.userID, opts.originID, opts.admission)
	}
	return true
}
