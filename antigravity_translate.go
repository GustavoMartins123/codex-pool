package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"codex-pool-proxy/internal/jsonvalue"
	"codex-pool-proxy/internal/routing"
	"codex-pool-proxy/internal/wire"
)

// The Antigravity wire contract and translation behavior are adapted from
// router-for-me/CLIProxyAPI at c8803713c972af0076f55933fdeed4db81d72d24.
// CLIProxyAPI is MIT licensed; see THIRD_PARTY_NOTICES.md.

type antigravityPreparedRequest struct {
	Body                 []byte
	Format               wire.RequestFormat
	ClientStream         bool
	PublicModel          string
	UpstreamModel        string
	Operation            string
	Tools                wire.ResponsesToolContext
	EstimatedInputTokens int
	Provider             AccountType // upstream account type; empty means Antigravity
}

func (prepared antigravityPreparedRequest) provider() AccountType {
	if prepared.Provider == "" {
		return AccountTypeAntigravity
	}
	return prepared.Provider
}

func shouldRouteAntigravityModel(model string) bool {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, "antigravity/") {
		return true
	}
	for _, provider := range []AccountType{AccountTypeCodex, AccountTypeClaude, AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi, AccountTypeAdverserial} {
		if _, ok := modelForProvider(provider, model); ok {
			return false
		}
	}
	if isGrokModel(model) {
		return false
	}
	return isAntigravityModel(model)
}

func antigravityModelFromGeminiPath(path string) string {
	marker := "/v1beta/models/"
	index := strings.Index(path, marker)
	if index < 0 {
		return ""
	}
	model := path[index+len(marker):]
	if colon := strings.IndexByte(model, ':'); colon >= 0 {
		model = model[:colon]
	}
	return strings.TrimSpace(model)
}

func prepareAntigravityRequest(path string, body []byte, requestedModel, projectID, conversationID string) (antigravityPreparedRequest, error) {
	publicModel := strings.TrimSpace(requestedModel)
	if publicModel == "" {
		publicModel = antigravityModelFromGeminiPath(path)
	}
	if publicModel == "" {
		return antigravityPreparedRequest{}, errors.New("Antigravity model is required")
	}
	if strings.TrimSpace(projectID) == "" {
		return antigravityPreparedRequest{}, errors.New("Antigravity project ID is required")
	}
	upstreamModel := antigravityCanonicalModel(publicModel)
	return prepareGeminiNativeRequest(path, body, publicModel, upstreamModel, conversationID, func(gemini map[string]any) map[string]any {
		if !strings.Contains(strings.ToLower(upstreamModel), "claude") {
			if generationConfig := jsonvalue.Map(gemini["generationConfig"]); generationConfig != nil {
				delete(generationConfig, "maxOutputTokens")
			}
		}
		requestType := "agent"
		if strings.Contains(strings.ToLower(upstreamModel), "image") {
			requestType = "image_gen"
		}
		requestID := "agent-" + uuid.NewString()
		if requestType == "image_gen" {
			requestID = fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
		}
		return map[string]any{
			"model": upstreamModel, "userAgent": "antigravity", "requestType": requestType,
			"project": projectID, "requestId": requestID, "request": gemini,
		}
	})
}

// prepareGeminiNativeRequest translates a client request (Gemini, Chat
// Completions, Anthropic Messages or Responses) into a native Gemini
// generateContent request. envelope wraps it the way the upstream expects; the
// wrapped body keeps "model" and "request" so the signature replay cache can
// scope it.
func prepareGeminiNativeRequest(path string, body []byte, publicModel, upstreamModel, conversationID string, envelope func(map[string]any) map[string]any) (antigravityPreparedRequest, error) {
	format := wire.FormatGemini
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		format = wire.FormatClaude
	case isAntigravityResponsesPath(path):
		format = wire.FormatResponses
	case strings.HasPrefix(path, "/v1/chat/completions"):
		format = wire.FormatOpenAI
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return antigravityPreparedRequest{}, fmt.Errorf("invalid JSON request: %w", err)
	}
	clientStream, _ := root["stream"].(bool)
	operation := "streamGenerateContent"
	if strings.Contains(path, "countTokens") {
		operation = "countTokens"
		clientStream = false
	}
	if format == wire.FormatGemini {
		clientStream = strings.Contains(path, "streamGenerateContent") || strings.HasPrefix(path, "/v1internal:") || clientStream
	}
	gemini, tools, err := wire.ClientRequestToGemini(root, format, upstreamModel)
	if err != nil {
		return antigravityPreparedRequest{}, err
	}
	gemini["sessionId"] = antigravityFormatSessionID(conversationID, gemini)
	encoded, err := json.Marshal(envelope(gemini))
	return antigravityPreparedRequest{Body: encoded, Format: format, ClientStream: clientStream, PublicModel: publicModel, UpstreamModel: upstreamModel, Operation: operation, Tools: tools}, err
}

func antigravitySessionID(request map[string]any) string {
	seed := ""
	if contents, ok := request["contents"].([]any); ok {
		for _, content := range contents {
			entry, _ := content.(map[string]any)
			if entry["role"] != "user" {
				continue
			}
			parts, _ := entry["parts"].([]any)
			for _, part := range parts {
				if text, _ := part.(map[string]any)["text"].(string); text != "" {
					seed = text
					break
				}
			}
			if seed != "" {
				break
			}
		}
	}
	if seed == "" {
		seed = uuid.NewString()
	}
	return antigravitySessionIDFromSeed(seed)
}

func isAntigravitySessionID(id string) bool {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "-") {
		return false
	}
	val, err := strconv.ParseInt(id, 10, 64)
	return err == nil && val < 0
}

func antigravitySessionIDFromSeed(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	value := int64(0)
	for _, b := range sum[:8] {
		value = value<<8 | int64(b)
	}
	if value > 0 {
		value = -value
	}
	if value == 0 {
		value = -1
	}
	return strconv.FormatInt(value, 10)
}

func antigravityFormatSessionID(conversationID string, request map[string]any) string {
	conversationID = strings.TrimSpace(conversationID)
	if isAntigravitySessionID(conversationID) {
		return conversationID
	}
	if conversationID != "" {
		return antigravitySessionIDFromSeed(conversationID)
	}
	return antigravitySessionID(request)
}

func cloneAnyMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func (h *proxyHandler) handleAntigravityProxy(w http.ResponseWriter, r *http.Request, body []byte, requestedModel, conversationID, userID, originID, clientIP, reqID string, routingProfile RoutingProfile) bool {
	if !shouldRouteAntigravityModel(requestedModel) {
		return false
	}
	canonical := antigravityCanonicalModel(requestedModel)
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity provider is not configured")
		return true
	}
	exclude := make(map[string]bool)
	var lastError error
	var lastRateLimitBody []byte
	var lastRateLimitUntil time.Time
	// The last account-specific failure (unrecoverable 401, 5xx) is relayed
	// when every account fails, instead of a generic capacity error.
	var lastFailureStatus int
	var lastFailureBody []byte
	serverFailures := 0
	attempts := h.cfg.maxAttempts
	if attempts < 1 {
		attempts = 1
	}
	if accountCount := h.pool.countByType(AccountTypeAntigravity); accountCount > attempts {
		attempts = accountCount
	}
	contextCompactionRetry := false
	for attempt := 0; attempt < attempts; attempt++ {
		selection := h.pool.selectRoute(routing.Request{
			Mode: routing.MatchModelFamily, Provider: string(AccountTypeAntigravity),
			UserID: userID, ConversationID: conversationID, Exclude: exclude,
			Model: canonical, ClientIP: clientIP, Profile: string(routingProfile), Trace: true,
		})
		account := selection.Account
		if account == nil {
			if err := h.checkAccountRoutingAccess(userID, AccountTypeAntigravity, canonical); err != nil {
				respondPolicyError(w, err)
				return true
			}
			break
		}
		primaryReason := "quota_headroom"
		if len(selection.Reasons) > 0 {
			primaryReason = selection.Reasons[len(selection.Reasons)-1]
		}
		trace := &RouteTrace{
			RequestID:      reqID,
			Transition:     h.transitionForTrace(userID, conversationID, AccountTypeAntigravity, w.Header()),
			Timestamp:      time.Now().UTC(),
			Policy:         selection.Policy,
			Selected:       RouteTarget{Provider: "antigravity", Model: canonical},
			Score:          selection.Score,
			Reasons:        selection.Reasons,
			Alternatives:   selection.Alternatives,
			AccountID:      account.ID,
			ScoreBreakdown: selection.Breakdown,
			ClientIP:       clientIP,
			UserID:         userID,
			Attempts:       attempt + 1,
		}
		h.getRouteTraces().Record(trace)
		setPoolRouteHeaders(w.Header(), "antigravity", canonical, selection.Policy, primaryReason, attempt+1, reqID)
		exclude[account.ID] = true
		if h.needsRefresh(account) {
			_ = h.refreshAccount(r.Context(), account)
		}
		account.mu.Lock()
		projectID := account.google.ProjectID
		account.mu.Unlock()
		nativeSeed := conversationID
		freshSession := false
		if conversationID != "" {
			nativeSeed, freshSession = h.getContextHandoff().NativeSessionSeed(conversationScopedKey(userID, conversationID), AccountTypeAntigravity)
		}
		prepared, err := prepareAntigravityRequest(r.URL.Path, body, requestedModel, projectID, nativeSeed)
		if err != nil {
			respondJSONError(w, http.StatusBadRequest, err.Error())
			return true
		}
		var replayScope antigravityReplayScope
		var transitionEpoch uint64
		if conversationID != "" {
			state, _ := h.getContextHandoff().State(conversationScopedKey(userID, conversationID))
			transitionEpoch = state.TransitionEpoch
		}
		prepared.Body, replayScope, _ = antigravityScopedReplay(prepared.Body, conversationID, transitionEpoch, freshSession)
		prepared.EstimatedInputTokens = estimateAntigravitySerializedTokens(prepared.Body)
		if conversationID != "" {
			var envelope map[string]any
			if json.Unmarshal(prepared.Body, &envelope) == nil {
				request, _ := envelope["request"].(map[string]any)
				h.getContextHandoff().BindNativeSession(conversationScopedKey(userID, conversationID), AccountTypeAntigravity, jsonvalue.String(request["sessionId"]))
			}
		}
		attemptStarted := time.Now()
		atomic.AddInt64(&account.Inflight, 1)
		resp, err := h.doAntigravityRequestWithTransientRetry(r.Context(), r.Header, account, provider, prepared)
		atomic.AddInt64(&account.Inflight, -1)
		if err != nil {
			h.pool.recordRoutingOutcome(account.ID, time.Since(attemptStarted), 0, http.StatusServiceUnavailable, time.Now())
			lastError = err
			continue
		}
		trace.DurationMs = float64(time.Since(attemptStarted).Milliseconds())
		trace.StatusCode = resp.StatusCode
		if trace.Transition != nil {
			h.getContextHandoff().MarkTransitionOutcome(conversationScopedKey(userID, conversationID), trace.Transition.Epoch, resp.StatusCode, "", false)
			trace.Transition.StatusCode = resp.StatusCode
		}
		h.getRouteTraces().Record(trace)
		h.pool.recordRoutingOutcome(account.ID, time.Since(attemptStarted), 0, resp.StatusCode, time.Now())
		if resp.StatusCode == http.StatusTooManyRequests && freshSession && conversationID != "" {
			errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.ContentLength = int64(len(errBody))
			providerErr := classifyAntigravityError(resp.StatusCode, errBody)
			if trace.Transition != nil {
				trace.Transition.ErrorClass = providerErr.Class
				h.getContextHandoff().MarkTransitionOutcome(conversationScopedKey(userID, conversationID), trace.Transition.Epoch, resp.StatusCode, providerErr.Class, false)
			}
			state, ok := h.getContextHandoff().State(conversationScopedKey(userID, conversationID))
			if ok && (providerErr.Class == ProviderErrorSession || providerErr.Class == ProviderErrorContext || providerErr.Class == ProviderErrorUnknown) {
				transition := &TransitionAttempt{
					Owner: userID, ConversationID: conversationID, Epoch: state.TransitionEpoch,
					From: state.LastTransitionFrom, To: AccountTypeAntigravity,
				}
				if h.getContextHandoff().RecoverNativeSession(transition) {
					if trace.Transition != nil {
						trace.Transition.Epoch = transition.Epoch
						trace.Transition.RecoveryRetry = true
					}
					_ = resp.Body.Close()
					nativeSeed, _ = h.getContextHandoff().NativeSessionSeed(conversationScopedKey(userID, conversationID), AccountTypeAntigravity)
					prepared, err = prepareAntigravityRequest(r.URL.Path, body, requestedModel, projectID, nativeSeed)
					if err != nil {
						antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), http.StatusBadRequest, []byte(err.Error()))
						return true
					}
					prepared.Body, replayScope, _ = antigravityScopedReplay(prepared.Body, conversationID, transition.Epoch, true)
					prepared.EstimatedInputTokens = estimateAntigravitySerializedTokens(prepared.Body)
					var envelope map[string]any
					if json.Unmarshal(prepared.Body, &envelope) == nil {
						request, _ := envelope["request"].(map[string]any)
						h.getContextHandoff().BindNativeSession(conversationScopedKey(userID, conversationID), AccountTypeAntigravity, jsonvalue.String(request["sessionId"]))
					}
					log.Printf("conversation=%s transition=%s->antigravity epoch=%d recovery_retry=true error_class=%s", conversationID, transition.From, transition.Epoch, providerErr.Class)
					atomic.AddInt64(&account.Inflight, 1)
					resp, err = h.doAntigravityRequestWithTransientRetry(r.Context(), r.Header, account, provider, prepared)
					atomic.AddInt64(&account.Inflight, -1)
					if err != nil {
						lastError = err
						continue
					}
					trace.StatusCode = resp.StatusCode
					if trace.Transition != nil {
						trace.Transition.StatusCode = resp.StatusCode
						h.getContextHandoff().MarkTransitionOutcome(conversationScopedKey(userID, conversationID), transition.Epoch, resp.StatusCode, "", true)
					}
					h.getRouteTraces().Record(trace)
					h.pool.recordRoutingOutcome(account.ID, time.Since(attemptStarted), 0, resp.StatusCode, time.Now())
				}
			}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if err := h.refreshAccountAfterAuthFailure(r.Context(), account); err != nil {
				// This credential cannot serve the request; another account may.
				lastFailureStatus, lastFailureBody = http.StatusUnauthorized, errBody
				lastError = fmt.Errorf("Antigravity account %s rejected its credentials: %v", account.ID, err)
				continue
			}
			atomic.AddInt64(&account.Inflight, 1)
			resp, err = h.doAntigravityRequestWithTransientRetry(r.Context(), r.Header, account, provider, prepared)
			atomic.AddInt64(&account.Inflight, -1)
			if err != nil {
				lastError = err
				continue
			}
			if resp.StatusCode == http.StatusUnauthorized {
				errBody, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				lastFailureStatus, lastFailureBody = http.StatusUnauthorized, errBody
				lastError = fmt.Errorf("Antigravity account %s rejected refreshed credentials", account.ID)
				continue
			}
		}
		if resp.StatusCode == http.StatusBadRequest && !contextCompactionRetry {
			errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			providerErr := classifyAntigravityError(resp.StatusCode, errBody)
			if providerErr.Class == ProviderErrorContext {
				compacted, didCompact, compactErr := compactAntigravityRequestBodyWithRecentTokens(r.URL.Path, body, antigravityEmergencyRecentTokens)
				if compactErr == nil && didCompact {
					antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
					logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, providerErr)
					body = compacted
					contextCompactionRetry = true
					log.Printf("[%s] provider=antigravity context_compaction_retry=true", reqID)
					delete(exclude, account.ID)
					if attempt == 0 {
						attempts++
					} else {
						attempt--
					}
					continue
				}
			}
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.ContentLength = int64(len(errBody))
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			providerErr := classifyAntigravityError(resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, providerErr)
			if providerErr.Class == ProviderErrorProtocol || providerErr.Class == ProviderErrorPolicy {
				antigravityWriteError(w, prepared.Format, resp.StatusCode, errBody)
				return true
			}
			if providerErr.Class == ProviderErrorQuota {
				until := providerErr.ResetAt
				if wait, ok := parseRetryAfter(resp.Header); ok && time.Now().Add(wait).After(until) {
					until = time.Now().Add(wait)
				}
				if until.IsZero() {
					until = time.Now().Add(backoffDuration(attempt))
				}
				setAntigravityQuotaCooldown(account, canonical, errBody, until)
				lastRateLimitBody = append(lastRateLimitBody[:0], errBody...)
				lastRateLimitUntil = until
				lastError = fmt.Errorf("Antigravity %s is rate limited until %s", canonical, until.Format(time.RFC3339))
				continue
			}
			lastRateLimitBody = append(lastRateLimitBody[:0], errBody...)
			lastError = fmt.Errorf("Antigravity %s returned %d (%s): %s", canonical, resp.StatusCode, providerErr.Class, safeText(errBody))
			continue
		}
		if resp.StatusCode == http.StatusForbidden {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, classifyAntigravityError(resp.StatusCode, errBody))
			recordAntigravityForbidden(account, errBody)
			lastError = fmt.Errorf("Antigravity account %s was rejected: %s", account.ID, safeText(errBody))
			continue
		}
		if resp.StatusCode >= 500 {
			// Upstream failures are account-scoped until proven otherwise:
			// rotate instead of failing a request other accounts can serve.
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, classifyAntigravityError(resp.StatusCode, errBody))
			lastFailureStatus, lastFailureBody = resp.StatusCode, errBody
			lastError = fmt.Errorf("Antigravity %s returned %d on account %s", canonical, resp.StatusCode, account.ID)
			// A failure the request itself causes would repeat on every
			// account; bound how many accounts it may consume.
			if serverFailures++; serverFailures >= antigravityMaxServerFailover {
				break
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, classifyAntigravityError(resp.StatusCode, errBody))
			antigravityWriteError(w, prepared.Format, resp.StatusCode, errBody)
			return true
		}
		clearAntigravityModelCooldown(account, canonical)
		clearAntigravityHealth(account)
		if prepared.Operation == "countTokens" {
			defer resp.Body.Close()
			responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			unwrapped, unwrapErr := wire.UnwrapAntigravityResponse(responseBody)
			if unwrapErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(unwrapErr.Error()))
				return true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(unwrapped)
			return true
		}
		if conversationID != "" {
			h.pool.pinForUser(userID, conversationID, account.ID)
		}
		h.writeAntigravityResponse(w, resp, prepared, replayScope, account, conversationID, userID, originID, reqID)
		return true
	}
	if len(lastRateLimitBody) > 0 {
		if !lastRateLimitUntil.IsZero() {
			seconds := int64(time.Until(lastRateLimitUntil).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), http.StatusTooManyRequests, lastRateLimitBody)
		return true
	}
	if lastFailureStatus != 0 {
		antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), lastFailureStatus, lastFailureBody)
		return true
	}
	if lastError == nil {
		lastError = fmt.Errorf("no Antigravity account currently supports %s", requestedModel)
	}
	antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), http.StatusServiceUnavailable, []byte(lastError.Error()))
	return true
}

// antigravityMaxServerFailover bounds how many accounts a request may try
// after upstream 5xx answers.
const antigravityMaxServerFailover = 3

func recordAntigravityForbidden(account *Account, body []byte) {
	applyAntigravityForbidden(account, body)
	_ = saveAccount(account)
}

func applyAntigravityForbidden(account *Account, body []byte) {
	needsVerification, banned, verificationURL := classifyAntigravityForbidden(body)
	account.mu.Lock()
	defer account.mu.Unlock()
	account.NeedsVerification = needsVerification
	account.VerificationURL = verificationURL
	account.HealthError = strings.TrimSpace(string(body))
	if banned {
		account.Dead = true
	}
	if !needsVerification && !banned {
		account.RateLimitUntil = time.Now().Add(30 * time.Minute)
	}
}

// clearAntigravityHealth drops verification and health markers once the
// daily host has served the account again.
func clearAntigravityHealth(account *Account) {
	account.mu.Lock()
	hadHealthError := account.NeedsVerification || account.VerificationURL != "" || account.HealthError != ""
	account.NeedsVerification, account.VerificationURL, account.HealthError = false, "", ""
	account.mu.Unlock()
	if hadHealthError {
		_ = saveAccount(account)
	}
}

var antigravityVerificationURLPattern = regexp.MustCompile(`https://[^\s"'<>]+`)

func classifyAntigravityForbidden(body []byte) (needsVerification, banned bool, verificationURL string) {
	lower := strings.ToLower(string(body))
	needsVerification = strings.Contains(lower, "validation_required") || strings.Contains(lower, "verify your account") || strings.Contains(lower, "validation_url")
	banned = strings.Contains(lower, "terms of service") || strings.Contains(lower, "terms violation") || strings.Contains(lower, "policy violation") || strings.Contains(lower, "disabled in this account for violation")
	var root any
	if json.Unmarshal(body, &root) == nil {
		var walk func(any) string
		walk = func(value any) string {
			switch node := value.(type) {
			case map[string]any:
				for _, key := range []string{"validation_url", "appeal_url"} {
					if text, _ := node[key].(string); text != "" {
						return text
					}
				}
				for _, child := range node {
					if found := walk(child); found != "" {
						return found
					}
				}
			case []any:
				for _, child := range node {
					if found := walk(child); found != "" {
						return found
					}
				}
			}
			return ""
		}
		verificationURL = walk(root)
		var tosViolation func(any) bool
		tosViolation = func(value any) bool {
			switch node := value.(type) {
			case map[string]any:
				if reason, _ := node["reason"].(string); strings.EqualFold(reason, "TOS_VIOLATION") {
					return true
				}
				for _, child := range node {
					if tosViolation(child) {
						return true
					}
				}
			case []any:
				for _, child := range node {
					if tosViolation(child) {
						return true
					}
				}
			}
			return false
		}
		if tosViolation(root) {
			banned = true
		}
	}
	if verificationURL == "" {
		verificationURL = antigravityVerificationURLPattern.FindString(string(body))
	}
	return needsVerification, banned, verificationURL
}

func antigravityFormatForPath(path string) wire.RequestFormat {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return wire.FormatClaude
	case isAntigravityResponsesPath(path):
		return wire.FormatResponses
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return wire.FormatOpenAI
	default:
		return wire.FormatGemini
	}
}

func antigravityErrorSummary(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		text := strings.TrimSpace(string(body))
		if len(text) > 256 {
			text = text[:256]
		}
		return safeText([]byte(text))
	}
	errObj := jsonvalue.Map(obj["error"])
	if errObj == nil {
		return "unstructured_error"
	}
	parts := make([]string, 0, 5)
	for _, key := range []string{"code", "status", "type", "message"} {
		if value := wire.GeminiErrorField(errObj[key]); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	if len(parts) == 0 {
		return "structured_error"
	}
	summary := strings.Join(parts, " ")
	if len(summary) > 512 {
		summary = summary[:512]
	}
	return safeText([]byte(summary))
}

func logAntigravityUpstreamError(reqID, path string, format wire.RequestFormat, model string, status int, body []byte, providerErr ProviderError) {
	log.Printf("[%s] provider=antigravity path=%s format=%s model=%s status=%d error_class=%s retryable=%t quota_affected=%t summary=%s", reqID, path, format, model, status, providerErr.Class, providerErr.Retryable, providerErr.Class == ProviderErrorQuota, antigravityErrorSummary(body))
}

func isAntigravityResponsesPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	for _, prefix := range []string{"/responses", "/v1/responses", "/api/codex/responses", "/backend-api/codex/responses"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func (h *proxyHandler) doAntigravityRequest(ctx context.Context, incoming http.Header, account *Account, provider *AntigravityProvider, prepared antigravityPreparedRequest) (*http.Response, error) {
	if admission, ok := ctx.Value(policyAdmissionContextKey{}).(*policyAdmission); ok && admission != nil {
		if err := h.checkAccountUse(admission.principalID, account); err != nil {
			return nil, err
		}
	}
	tryBase := func(base *url.URL) (*http.Response, error) {
		u := *base
		operation := prepared.Operation
		if operation == "" {
			operation = "streamGenerateContent"
		}
		u.Path = singleJoin(u.Path, "/v1internal:"+operation)
		if operation == "streamGenerateContent" {
			query := u.Query()
			query.Set("alt", "sse")
			u.RawQuery = query.Encode()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(prepared.Body))
		provider.SetAuthHeaders(req, account)
		if operation == "streamGenerateContent" {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if requestID := incoming.Get("X-Request-ID"); requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		transport := h.antigravityTransport
		if transport == nil {
			transport = h.transport
		}
		return transport.RoundTrip(req)
	}
	resp, err := tryBase(provider.dailyBase)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		if classifyAntigravityError(resp.StatusCode, body).Class != ProviderErrorQuota {
			return resp, nil
		}
	}
	if err == nil && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return resp, nil
	}
	var daily *http.Response
	if resp != nil {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr == nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			daily = resp
		}
	}
	fallback, fallbackErr := tryBase(provider.prodBase)
	if daily == nil || fallbackErr != nil || fallback.StatusCode != http.StatusForbidden {
		return fallback, fallbackErr
	}
	// Production can demand account verification while the daily host keeps
	// serving the same account. The daily answer is the one that describes
	// this account's state, so a production 403 must not replace it.
	_ = fallback.Body.Close()
	return daily, nil
}

func (h *proxyHandler) doAntigravityRequestWithTransientRetry(ctx context.Context, incoming http.Header, account *Account, provider *AntigravityProvider, prepared antigravityPreparedRequest) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := h.doAntigravityRequest(ctx, incoming, account, provider, prepared)
		if err != nil || resp == nil {
			return resp, err
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
			return resp, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		delay, retry := antigravityInstantRetryDelay(body, time.Now())
		if antigravityShouldRetryNoCapacity(resp.StatusCode, body) {
			delay, retry = 250*time.Millisecond, true
		}
		if !retry || attempt+1 >= 2 {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return resp, nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("Antigravity transient retry exhausted")
}

func antigravityInstantRetryDelay(body []byte, now time.Time) (time.Duration, bool) {
	if classifyAntigravityError(http.StatusTooManyRequests, body).Class != ProviderErrorQuota || !strings.Contains(strings.ToLower(string(body)), "rate_limit_exceeded") {
		return 0, false
	}
	until, ok := parseAntigravityRetry(body, now)
	if !ok {
		return 0, false
	}
	delay := until.Sub(now)
	if delay < 0 || delay >= 3*time.Second {
		return 0, false
	}
	if delay > 0 {
		delay += 800 * time.Millisecond
	}
	return delay, true
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	return statusCode == http.StatusServiceUnavailable && strings.Contains(strings.ToLower(string(body)), "no capacity available")
}

func parseAntigravityRetry(body []byte, now time.Time) (time.Time, bool) {
	if reset, ok := parseGeminiRateLimitReset(body, now); ok {
		return reset, true
	}
	var root any
	if json.Unmarshal(body, &root) != nil {
		return time.Time{}, false
	}
	var walk func(any) string
	walk = func(value any) string {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if key == "retryDelay" {
					if text, ok := child.(string); ok {
						return text
					}
				}
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range node {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	if duration, err := time.ParseDuration(walk(root)); err == nil && duration > 0 {
		return now.Add(duration), true
	}
	return time.Time{}, false
}

func (h *proxyHandler) writeAntigravityResponse(w http.ResponseWriter, resp *http.Response, prepared antigravityPreparedRequest, replayScope antigravityReplayScope, account *Account, conversationID, userID, originID, reqID string) {
	defer resp.Body.Close()
	w.Header().Set("X-Accel-Buffering", "no")
	if !prepared.ClientStream {
		result, usage, err := collectAntigravitySSE(resp.Body)
		if err != nil {
			antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(err.Error()))
			return
		}
		wrapped, marshalErr := json.Marshal(map[string]any{"response": result})
		if marshalErr != nil {
			antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(marshalErr.Error()))
			return
		}
		antigravityCaptureNativeReplay(replayScope, prepared.Body, wrapped)
		body, err := wire.GeminiResponseToClient(wrapped, prepared.Format, prepared.PublicModel, prepared.Tools)
		if err != nil {
			antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		if conversationID != "" {
			h.getContextHandoff().MarkNativeSessionEstablished(conversationScopedKey(userID, conversationID), prepared.provider())
			if text := responseTextFromObject(result); text != "" {
				h.getContextHandoff().RecordAssistantText(conversationScopedKey(userID, conversationID), prepared.provider(), text)
			}
		}
		h.recordAntigravityTokenDrift(reqID, prepared, usage)
		h.recordAntigravityUsage(account, usage, prepared.PublicModel, userID, originID, reqID, prepared.provider())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	translator := wire.NewGeminiToClientStream(w, prepared.Format, prepared.PublicModel, prepared.Tools)
	var usage *RequestUsage
	var assistantText strings.Builder
	validResponseSeen := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var wrapper map[string]any
		if json.Unmarshal([]byte(data), &wrapper) != nil {
			continue
		}
		response, _ := wrapper["response"].(map[string]any)
		if response == nil {
			response = wrapper
		}
		if _, ok := response["candidates"]; ok {
			validResponseSeen = true
		}
		antigravityCaptureNativeReplay(replayScope, prepared.Body, []byte(data))
		_, _ = translator.Write([]byte("data: " + data + "\n\n"))
		if value := antigravityUsage(response); value != nil {
			usage = value
		}
		assistantText.WriteString(responseTextFromObject(response))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	if conversationID != "" && validResponseSeen && scanner.Err() == nil {
		h.getContextHandoff().MarkNativeSessionEstablished(conversationScopedKey(userID, conversationID), prepared.provider())
	}
	if conversationID != "" {
		h.getContextHandoff().RecordAssistantText(conversationScopedKey(userID, conversationID), prepared.provider(), assistantText.String())
	}
	h.recordAntigravityTokenDrift(reqID, prepared, usage)
	h.recordAntigravityUsage(account, usage, prepared.PublicModel, userID, originID, reqID, prepared.provider())
}

func (h *proxyHandler) recordAntigravityTokenDrift(reqID string, prepared antigravityPreparedRequest, usage *RequestUsage) {
	if usage == nil || usage.InputTokens <= 0 || prepared.EstimatedInputTokens <= 0 {
		return
	}
	residual := usage.InputTokens - int64(prepared.EstimatedInputTokens)
	if residual > 0 {
		h.metrics.recordAntigravityInputResidual(residual)
	}
	log.Printf("[%s] provider=%s estimated_input_tokens=%d upstream_input_tokens=%d residual=%d", reqID, prepared.provider(), prepared.EstimatedInputTokens, usage.InputTokens, residual)
}

func (h *proxyHandler) recordAntigravityUsage(account *Account, usage *RequestUsage, model, userID, originID, reqID string, provider AccountType) {
	if usage == nil {
		return
	}
	if account != nil {
		usage.AccountID = account.ID
		usage.PlanType = account.PlanType
	}
	usage.Model, usage.UserID, usage.OriginID, usage.RequestID, usage.AccountType = model, userID, originID, reqID, provider
	h.recordUsage(account, *usage)
}

func collectAntigravitySSE(reader io.Reader) (map[string]any, *RequestUsage, error) {
	merged := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{}}}}}
	parts := []any{}
	var usage *RequestUsage
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var wrapper map[string]any
		if json.Unmarshal([]byte(data), &wrapper) != nil {
			continue
		}
		response, _ := wrapper["response"].(map[string]any)
		if response == nil {
			response = wrapper
		}
		parts = append(parts, antigravityResponseParts(response)...)
		if value := antigravityUsage(response); value != nil {
			usage = value
			merged["usageMetadata"] = response["usageMetadata"]
		}
		if candidates, _ := response["candidates"].([]any); len(candidates) > 0 {
			candidate, _ := candidates[0].(map[string]any)
			if reason := candidate["finishReason"]; reason != nil {
				mergedCandidates, _ := merged["candidates"].([]any)
				mergedCandidate, _ := mergedCandidates[0].(map[string]any)
				mergedCandidate["finishReason"] = reason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, usage, err
	}
	candidates, _ := merged["candidates"].([]any)
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	content["parts"] = parts
	return merged, usage, nil
}

func antigravityResponseParts(response map[string]any) []any {
	candidates, _ := response["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	return parts
}

func antigravityUsage(response map[string]any) *RequestUsage {
	provider := &AntigravityProvider{}
	return provider.ParseUsage(response)
}

func antigravityWriteError(w http.ResponseWriter, format wire.RequestFormat, status int, body []byte) {
	payload := wire.GeminiErrorToClient(body, format, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
