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
	"time"

	"codex-pool-proxy/internal/claudecode"
	"codex-pool-proxy/internal/hostedmcp"
	"codex-pool-proxy/internal/httpwire"
	"codex-pool-proxy/internal/wire"
)

// proxyPassthrough handles requests where the user provides their own credentials.
// The request is proxied directly to the upstream without using pool accounts.
func (h *proxyHandler) proxyPassthrough(w http.ResponseWriter, r *http.Request, reqID string, providerType AccountType, start time.Time, rt proxyRuntime) {
	h.metrics.incPassport("passthrough_requests", string(providerType))
	provider := h.registry.ForType(providerType)
	if provider == nil {
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}

	path := r.URL.Path
	originalPath := path
	if providerType == AccountTypeCodex {
		if shouldNoopCodexPath(path) {
			serveNoopCodexPath(w, r)
			return
		}
	}

	targetBase := provider.UpstreamURL(path)
	if httpwire.IsWebSocketUpgradeRequest(r) {
		h.proxyPassthroughWebSocket(w, r, reqID, providerType, provider, targetBase, start, rt)
		return
	}
	streamBody := httpwire.ShouldStreamBody(r, h.cfg.maxInMemoryBodyBytes)
	if providerType == AccountTypeCodex && (codexPassthroughNeedsBodyRewrite(path) || isCodexResponsesPath(path)) {
		streamBody = false
	}
	if streamBody {
		if h.cfg.debug.Load() {
			log.Printf("[%s] passthrough streaming body: method=%s path=%s provider=%s content-length=%d",
				reqID, r.Method, r.URL.Path, providerType, r.ContentLength)
		}
		h.proxyPassthroughStreamed(w, r, reqID, providerType, provider, targetBase, start, rt)
		return
	}

	releaseLargeBody, err := h.acquireLargeReplayBody(r.Context(), r.ContentLength)
	if err != nil {
		return
	}
	defer releaseLargeBody()
	bodyBytes, bodySample, err := httpwire.ReadBodyForReplay(r.Body, h.cfg.maxInMemoryBodyBytes, h.cfg.logBodies, h.cfg.bodyLogLimit)
	if err != nil {
		if errors.Is(err, httpwire.ErrReplayBodyTooLarge) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	passthroughTranslateDir := wire.TranslateNone
	compactRequest := providerType == AccountTypeCodex && r.Method == http.MethodPost && isCompactPath(path)
	if compactRequest {
		decoded, changed, err := decodeRequestBody(r.Header.Get("Content-Encoding"), bodyBytes, h.cfg.maxInMemoryBodyBytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if changed {
			r.Header.Del("Content-Encoding")
		}
		var obj map[string]any
		if err := json.Unmarshal(decoded, &obj); err != nil {
			http.Error(w, "compaction request must be a JSON object", http.StatusBadRequest)
			return
		}
		if err := prepareCompactRequest(r, obj); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodyBytes, err = json.Marshal(obj)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		path = r.URL.Path
	}
	if providerType == AccountTypeCodex {
		if strings.HasPrefix(originalPath, "/v1/completions") && !strings.HasPrefix(originalPath, "/v1/chat/completions") {
			passthroughTranslateDir = wire.TranslateCompletionsToResponses
		} else if strings.HasPrefix(originalPath, "/v1/chat/completions") {
			passthroughTranslateDir = wire.TranslateChatToResponses
		} else if strings.HasPrefix(originalPath, "/v1/messages") {
			passthroughTranslateDir = wire.TranslateClaudeToResponses
		}
		path, bodyBytes, err = codexPassthroughRewrite(path, bodyBytes)
		if err != nil {
			http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
			return
		}
		targetBase = provider.UpstreamURL(path)
		r = r.Clone(r.Context())
		r.URL.Path = path
		if isCodexResponsesPath(path) {
			bodyBytes, _, err = hostedmcp.FilterRequestJSON(bodyBytes)
			if err != nil {
				http.Error(w, "hosted MCP request filtering error: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
	}

	if h.cfg.debug.Load() {
		log.Printf("[%s] passthrough %s %s provider=%s content-type=%q body_bytes=%d",
			reqID, r.Method, r.URL.Path, providerType,
			r.Header.Get("Content-Type"), len(bodyBytes))
		// Debug: log all headers for Claude passthrough
		if providerType == AccountTypeClaude {
			var hdrs []string
			for k, v := range r.Header {
				if strings.HasPrefix(strings.ToLower(k), "anthropic") {
					hdrs = append(hdrs, fmt.Sprintf("%s=%s", k, v[0]))
				}
			}
			log.Printf("[%s] passthrough claude anthropic headers: %v", reqID, hdrs)
		}
	}
	if h.cfg.logBodies && len(bodySample) > 0 {
		logSample := bodySample
		if providerType == AccountTypeCodex && isCodexResponsesPath(r.URL.Path) {
			logSample = bodyBytes
			if h.cfg.bodyLogLimit > 0 && int64(len(logSample)) > h.cfg.bodyLogLimit {
				logSample = logSample[:h.cfg.bodyLogLimit]
			}
		}
		log.Printf("[%s] passthrough request body sample (%d bytes): %s", reqID, len(logSample), safeText(logSample))
	}

	timeout := clientOrDefaultTimeout(r, h.cfg.requestTimeout, h.cfg.streamTimeout, bodyBytes)

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

	var body io.Reader
	if len(bodyBytes) > 0 {
		body = bytes.NewReader(bodyBytes)
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

	// Force uncompressed responses — SSE streams break with on-the-fly decompression.
	outReq.Header.Set("Accept-Encoding", "identity")

	// For Claude, ensure required headers are set
	if providerType == AccountTypeClaude {
		if outReq.Header.Get("anthropic-version") == "" {
			outReq.Header.Set("anthropic-version", claudecode.AnthropicVersion)
		}
	}

	if h.cfg.debug.Load() {
		log.Printf("[%s] passthrough -> %s %s", reqID, outReq.Method, outReq.URL.String())
	}

	resp, err := h.transport.RoundTrip(outReq)
	if err != nil {
		if providerType == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
			h.writeClaudeTrace(reqID, "passthrough", "", hashRequestOrigin(r, h.originHashSalt()), nil, r, bodyBytes, outReq, bodyBytes, nil, wire.TranslateNone, nil, err.Error())
		}
		h.recent.add(err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if compactRequest {
		if err := compactHTTPResponse(resp, cancel); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	if providerType == AccountTypeClaude && h.cfg.claudeTraceEnabled() {
		sampleLimit := h.claudeTraceSampleLimit(16 * 1024)
		if h.cfg.logBodies && h.cfg.bodyLogLimit > 0 {
			sampleLimit = h.claudeTraceSampleLimit(h.cfg.bodyLogLimit)
		}
		h.attachClaudeTrace(reqID, "passthrough", "", hashRequestOrigin(r, h.originHashSalt()), nil, r, bodyBytes, outReq, bodyBytes, resp, wire.TranslateNone, &bytes.Buffer{}, sampleLimit)
	}

	respContentType := resp.Header.Get("Content-Type")
	isSSE := provider.DetectsSSE(r.URL.Path, respContentType)
	if isSSE && resp.StatusCode >= 400 && !strings.Contains(strings.ToLower(respContentType), "text/event-stream") {
		isSSE = false
	}
	if providerType == AccountTypeCodex && isCodexResponsesPath(r.URL.Path) && !isSSE {
		if _, err := hostedmcp.FilterNonStreamingResponse(resp); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	clientWantsNonStreaming := true
	if len(bodyBytes) > 0 {
		var obj map[string]any
		if json.Unmarshal(bodyBytes, &obj) == nil {
			if stream, ok := obj["stream"].(bool); ok && stream {
				clientWantsNonStreaming = false
			}
		}
	}
	if passthroughTranslateDir != wire.TranslateNone && resp.StatusCode < 400 && clientWantsNonStreaming && isSSE {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Content-Length")
		if passthroughTranslateDir == wire.TranslateCompletionsToResponses {
			bufWriter := wire.NewResponsesToOpenAICompletionsBuffer(wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: reqID})
			_, _ = io.Copy(&hostedmcp.ResponseFilterWriter{W: bufWriter}, resp.Body)
			w.WriteHeader(resp.StatusCode)
			w.Write(bufWriter.Result())
			return
		}
		if passthroughTranslateDir == wire.TranslateChatToResponses {
			bufWriter := wire.NewResponsesToOpenAIChatBuffer(wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: reqID})
			_, _ = io.Copy(&hostedmcp.ResponseFilterWriter{W: bufWriter}, resp.Body)
			w.WriteHeader(resp.StatusCode)
			w.Write(bufWriter.Result())
			return
		}
	}
	if passthroughTranslateDir != wire.TranslateNone && resp.StatusCode < 400 && !isSSE {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadGateway)
			return
		}
		translated := respBody
		if passthroughTranslateDir == wire.TranslateCompletionsToResponses {
			if b, err := wire.ResponsesResponseToOpenAICompletions(respBody); err == nil {
				translated = b
			}
		} else if passthroughTranslateDir == wire.TranslateChatToResponses {
			if b, err := wire.ResponsesResponseToOpenAIChat(respBody); err == nil {
				translated = b
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		w.Write(translated)
		return
	}

	// Write response to client
	httpwire.CopyHeader(w.Header(), resp.Header)
	httpwire.RemoveHopByHopHeaders(w.Header())
	h.logRateLimitResponseHeaders(reqID, providerType, resp.Header)
	h.replaceUsageHeaders(w.Header())
	if isSSE && passthroughTranslateDir != wire.TranslateNone {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)

	var writer io.Writer = w
	var fw *flushWriter
	if isSSE && flusher != nil {
		fw = &flushWriter{w: w, f: flusher, flushInterval: h.cfg.flushInterval}
		writer = fw
		defer fw.stop()
	}
	if isSSE && passthroughTranslateDir == wire.TranslateCompletionsToResponses {
		writer = wire.NewResponsesToOpenAICompletionsStream(writer, wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: reqID})
	} else if isSSE && passthroughTranslateDir == wire.TranslateChatToResponses {
		writer = wire.NewResponsesToOpenAIChatStream(writer, wire.StreamOptions{Debug: h.cfg.debug.Load(), RequestID: reqID})
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
		if r.Context().Err() == nil {
			h.recent.add(copyErr.Error())
			h.metrics.inc("error", "passthrough")
		}
		if idleReader != nil {
			log.Printf("[%s] passthrough SSE stream error: %v", reqID, copyErr)
		}
		return
	}

	h.metrics.inc(strconv.Itoa(resp.StatusCode), "passthrough")

	if h.cfg.debug.Load() {
		rt.logRequest(r.Context(), reqID, "proxy passthrough completed",
			slog.Int("status", resp.StatusCode), slog.Int64("duration_ms", rt.now().Sub(start).Milliseconds()))
	}
}
