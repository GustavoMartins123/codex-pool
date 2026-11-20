package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/jsonvalue"
	"codex-pool-proxy/internal/routing"
)

// aistudioMaxServerFailover bounds how many keys a request may try after
// upstream 5xx answers, since a request-caused failure repeats on every key.
const aistudioMaxServerFailover = 3

// prepareAIStudioRequest translates a client request into a native Gemini
// generateContent request for an AI Studio model. The body keeps the
// {model, request} envelope so the thought-signature replay cache can scope it;
// aistudioWireBody removes it before sending.
func prepareAIStudioRequest(path string, body []byte, requestedModel, conversationID string) (antigravityPreparedRequest, error) {
	upstream := aistudioUpstreamModel(requestedModel)
	if upstream == "" {
		return antigravityPreparedRequest{}, errors.New("AI Studio model is required")
	}
	effort := aistudioRequestedEffort(body)
	prepared, err := prepareGeminiNativeRequest(path, body, strings.TrimSpace(requestedModel), upstream, conversationID, func(gemini map[string]any) map[string]any {
		aistudioApplyThinking(gemini, upstream, effort)
		return map[string]any{"model": upstream, "request": gemini}
	})
	prepared.Provider = AccountTypeAIStudio
	return prepared, err
}

// aistudioWireBody returns the public Gemini API request body. The public API
// rejects the Code Assist sessionId field.
func aistudioWireBody(prepared antigravityPreparedRequest) ([]byte, error) {
	var envelope struct {
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal(prepared.Body, &envelope); err != nil || envelope.Request == nil {
		return nil, fmt.Errorf("invalid prepared AI Studio request")
	}
	delete(envelope.Request, "sessionId")
	return json.Marshal(envelope.Request)
}

func (h *proxyHandler) doAIStudioRequest(ctx context.Context, account *Account, provider *AIStudioProvider, prepared antigravityPreparedRequest) (*http.Response, error) {
	if admission, ok := ctx.Value(policyAdmissionContextKey{}).(*policyAdmission); ok && admission != nil {
		if err := h.checkAccountUse(admission.principalID, account); err != nil {
			return nil, err
		}
	}
	body, err := aistudioWireBody(prepared)
	if err != nil {
		return nil, err
	}
	operation := prepared.Operation
	if operation == "" {
		operation = "streamGenerateContent"
	}
	target := *provider.base
	target.Path = singleJoin(target.Path, "/v1beta/models/"+prepared.UpstreamModel+":"+operation)
	target.RawQuery = ""
	if operation == "streamGenerateContent" {
		target.RawQuery = "alt=sse"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	provider.SetAuthHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")
	if operation == "streamGenerateContent" {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return h.transport.RoundTrip(req)
}

// aistudioModelCoolingDown reports whether the key hit this model's quota.
// Gemini API quotas are per model, so one exhausted model must not block the
// key's other models.
func aistudioModelCoolingDown(account *Account, model string, now time.Time) bool {
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.google.ModelRateLimits[model].After(now)
}

func setAIStudioModelCooldown(account *Account, model string, until time.Time) {
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.google.ModelRateLimits == nil {
		account.google.ModelRateLimits = make(map[string]time.Time)
	}
	if until.After(account.google.ModelRateLimits[model]) {
		account.google.ModelRateLimits[model] = until
	}
}

// aistudioRejectedKey reports Gemini API answers that condemn the key itself,
// so the request moves to another key instead of failing.
func aistudioRejectedKey(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	text := string(body)
	return status == http.StatusBadRequest && (strings.Contains(text, "API_KEY_INVALID") || strings.Contains(text, "API key not valid") || strings.Contains(text, "API key expired"))
}

func (h *proxyHandler) handleAIStudioProxy(w http.ResponseWriter, r *http.Request, body []byte, requestedModel, conversationID, userID, originID, clientIP, reqID string) bool {
	if !isAIStudioModel(requestedModel) {
		return false
	}
	format := antigravityFormatForPath(r.URL.Path)
	provider, _ := h.registry.ForType(AccountTypeAIStudio).(*AIStudioProvider)
	if provider == nil {
		antigravityWriteError(w, format, http.StatusServiceUnavailable, []byte("AI Studio provider is not configured"))
		return true
	}
	model := strings.TrimSpace(requestedModel)
	upstream := aistudioUpstreamModel(model)
	exclude := make(map[string]bool)
	attempts := h.cfg.maxAttempts
	if count := h.pool.countByType(AccountTypeAIStudio); count > attempts {
		attempts = count
	}
	if attempts < 1 {
		attempts = 1
	}
	var lastError error
	var lastStatus int
	var lastBody []byte
	var cooldownUntil time.Time
	serverFailures := 0
	for attempt := 0; attempt < attempts; attempt++ {
		now := time.Now()
		for _, account := range h.pool.allAccounts() {
			if account.Type == AccountTypeAIStudio && aistudioModelCoolingDown(account, upstream, now) {
				exclude[account.ID] = true
			}
		}
		account := h.pool.selectRoute(routing.Request{
			UserID: userID, ConversationID: conversationID, Exclude: exclude, Provider: string(AccountTypeAIStudio),
			ClientIP: clientIP, Model: model, Profile: string(RoutingLegacy),
		}).Account
		if account == nil {
			if err := h.checkAccountRoutingAccess(userID, AccountTypeAIStudio, model); err != nil {
				respondPolicyError(w, err)
				return true
			}
			break
		}
		exclude[account.ID] = true
		setPoolRouteHeaders(w.Header(), "aistudio", model, "", "quota_headroom", attempt+1, reqID)
		prepared, err := prepareAIStudioRequest(r.URL.Path, body, model, conversationID)
		if err != nil {
			antigravityWriteError(w, format, http.StatusBadRequest, []byte(err.Error()))
			return true
		}
		var replayScope antigravityReplayScope
		prepared.Body, replayScope, _ = antigravityScopedReplay(prepared.Body, conversationID, 0, false)
		prepared.EstimatedInputTokens = estimateAntigravitySerializedTokens(prepared.Body)

		started := time.Now()
		atomic.AddInt64(&account.Inflight, 1)
		resp, err := h.doAIStudioRequest(r.Context(), account, provider, prepared)
		atomic.AddInt64(&account.Inflight, -1)
		if err != nil {
			h.pool.recordRoutingOutcome(account.ID, time.Since(started), 0, http.StatusServiceUnavailable, time.Now())
			lastError = err
			continue
		}
		h.pool.recordRoutingOutcome(account.ID, time.Since(started), 0, resp.StatusCode, time.Now())
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				until, ok := parseGeminiRateLimitReset(errBody, time.Now())
				if wait, hasRetry := parseRetryAfter(resp.Header); hasRetry && time.Now().Add(wait).After(until) {
					until, ok = time.Now().Add(wait), true
				}
				if !ok {
					until = time.Now().Add(backoffDuration(attempt))
				}
				setAIStudioModelCooldown(account, upstream, until)
				if cooldownUntil.IsZero() || until.Before(cooldownUntil) {
					cooldownUntil = until
				}
				lastStatus, lastBody = resp.StatusCode, errBody
				lastError = fmt.Errorf("AI Studio %s is rate limited on key %s until %s", upstream, account.ID, until.Format(time.RFC3339))
				continue
			case aistudioRejectedKey(resp.StatusCode, errBody):
				account.mu.Lock()
				account.HealthError = "AI Studio rejected this API key: " + safeText(errBody)
				account.mu.Unlock()
				lastStatus, lastBody = resp.StatusCode, errBody
				lastError = fmt.Errorf("AI Studio key %s was rejected", account.ID)
				continue
			case resp.StatusCode >= 500:
				lastStatus, lastBody = resp.StatusCode, errBody
				lastError = fmt.Errorf("AI Studio %s returned %d on key %s", upstream, resp.StatusCode, account.ID)
				if serverFailures++; serverFailures >= aistudioMaxServerFailover {
					attempt = attempts
				}
				continue
			default:
				// The request itself was rejected; another key would reject it too.
				antigravityWriteError(w, prepared.Format, resp.StatusCode, errBody)
				return true
			}
		}
		if prepared.Operation == "countTokens" {
			defer resp.Body.Close()
			responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(responseBody)
			return true
		}
		if conversationID != "" {
			h.pool.pinForUser(userID, conversationID, account.ID)
		}
		h.writeAntigravityResponse(w, resp, prepared, replayScope, account, conversationID, userID, originID, reqID)
		return true
	}
	if !cooldownUntil.IsZero() {
		seconds := int64(time.Until(cooldownUntil).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	if lastStatus != 0 {
		antigravityWriteError(w, format, lastStatus, lastBody)
		return true
	}
	if lastError == nil {
		lastError = fmt.Errorf("no AI Studio key currently supports %s", model)
	}
	antigravityWriteError(w, format, http.StatusServiceUnavailable, []byte(lastError.Error()))
	return true
}

// aistudioRequestedEffort reads the reasoning effort a client asked for:
// Responses reasoning.effort, Chat Completions reasoning_effort, or Anthropic
// output_config.effort.
func aistudioRequestedEffort(body []byte) string {
	var request struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		ReasoningEffort string `json:"reasoning_effort"`
		OutputConfig    struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	for _, effort := range []string{request.Reasoning.Effort, request.ReasoningEffort, request.OutputConfig.Effort} {
		if effort = strings.ToLower(strings.TrimSpace(effort)); effort != "" {
			return effort
		}
	}
	return ""
}

// aistudioApplyThinking replaces the translated thinking settings with what
// the Gemini API documents for model: a thinkingLevel for Gemini 3 and 4, a
// thinkingBudget for Gemini 2.5. An effort the model does not document, or a
// model outside the table, keeps the model's default thinking, because the
// API rejects unknown levels and Gemini 4 rejects a level and budget together.
func aistudioApplyThinking(gemini map[string]any, model, effort string) {
	config := jsonvalue.Map(gemini["generationConfig"])
	thinking, known := aistudioThinkingFor(model)
	if !known {
		if config != nil {
			delete(config, "thinkingConfig")
		}
		return
	}
	if config == nil {
		config = map[string]any{}
		gemini["generationConfig"] = config
	}
	settings := map[string]any{"includeThoughts": effort != "none"}
	if thinking.budgets != nil && effort == "minimal" {
		effort = "low"
	}
	if containsString(thinking.levels, effort) {
		switch {
		case thinking.budgets == nil:
			settings["thinkingLevel"] = effort
		case effort == "none":
			settings["thinkingBudget"] = 0
		default:
			settings["thinkingBudget"] = thinking.budgets[effort]
		}
	}
	config["thinkingConfig"] = settings
}
