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

	"codex-pool-proxy/internal/routing"
	"codex-pool-proxy/internal/wire"
)

type proxyHTTPDispatch struct {
	body                       []byte
	baseURL                    *url.URL
	sourceFormat, targetFormat wire.RequestFormat
	routingProfile             string
	imageGeneration            bool
	internalShadow             bool
	shadowAccounts             []string
	experiment                 *experimentAssignment
	autoDecision               *AutoDecision
	response                   proxyHTTPResponse
}

func (h *proxyHandler) dispatchProxyHTTP(
	ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, r *http.Request,
	inputs proxyHTTPDispatch,
) {
	response := inputs.response
	provider, accountType := response.provider, response.accountType
	reqID, userID, originID, originIP := response.requestID, response.userID, response.originID, response.originIP
	conversationID, requestedModel, translateDir := response.conversationID, response.model, response.translation
	rt, start := response.runtime, response.startedAt
	imageGenerationRequest := inputs.imageGeneration

	attempts := h.cfg.maxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	// Try at least all accounts of this type, up to configured max
	if n := h.pool.countByType(accountType); n > attempts {
		attempts = n
	}
	// But don't exceed total pool size
	if n := h.pool.count(); n > 0 && attempts > n {
		attempts = n
	}

	// hardExclude holds restrictions that must survive every retry/fallback
	// reset below (traffic-experiment account allowlist; policy and ownership
	// restrictions will live here too). exclude is per-attempt state only.
	hardExclude := map[string]bool{}
	if inputs.internalShadow {
		h.markShadowAccountExclusions(hardExclude, inputs.shadowAccounts)
	}
	exclude := map[string]bool{}
	exhaustionRetries := 0
	var lastErr error
	var lastStatus int
	cyberAccessRetry := false
	requiredPlan := requiredPlanForRequest(accountType, r, requestedModel)

	const maxCooldownWait = 10 * time.Second // max time to wait for a rate-limited account
	imageFanoutChild := h.isValidImageFanout(r)
	imageFanoutIndex, _ := strconv.Atoi(r.Header.Get("X-Codex-Pool-Image-Fanout-Index"))
	accountRouting := proxyHTTPAccountRouting{
		request: routing.Request{
			UserID: userID, ConversationID: conversationID, Model: requestedModel,
			Provider: string(accountType), RequiredPlan: requiredPlan, ClientIP: originIP,
			Profile: inputs.routingProfile, Trace: true,
		},
		requestID: reqID, imageGeneration: imageGenerationRequest,
		imageFanout: imageFanoutChild, imageFanoutIndex: imageFanoutIndex,
	}
	if imageGenerationRequest {
		accountRouting.request.ConversationID = ""
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		for id, denied := range h.pool.accountExclusions(userID, nil) {
			hardExclude[id] = denied
		}
		candidateExclude := exclude
		if imageGenerationRequest || len(hardExclude) > 0 {
			candidateExclude = make(map[string]bool, len(exclude)+len(hardExclude)+h.pool.countByType(accountType))
			for id, excluded := range hardExclude {
				candidateExclude[id] = excluded
			}
			for id, excluded := range exclude {
				candidateExclude[id] = excluded
			}
			h.pool.excludeImageIncapable(candidateExclude)
			if imageFanoutChild && attempt == 1 {
				h.pool.excludeInflightWhenIdleAvailable(accountType, candidateExclude)
			}
		}
		accountRouting.request.Exclude = candidateExclude
		selection := h.selectProxyHTTPAccount(accountRouting, attempt, cyberAccessRetry)
		acc := selection.Account
		if acc == nil {
			if err := h.checkAccountRoutingAccess(userID, accountType, requestedModel); err != nil {
				respondPolicyError(w, err)
				return
			}
			waitExclude := h.pool.capacityWaitExclusions(userID, conversationID, hardExclude, accountType, requiredPlan, originIP, requestedModel)
			if reset, known := h.pool.nearestUsageReset(accountType, waitExclude); known && reset > h.capacityWaitBudget(ctx) {
				respondCapacityError(w, "provider_quota_exhausted", reset)
				return
			}
			// All accounts excluded or rate-limited. If there are rate-limited
			// accounts, wait for the shortest cooldown instead of 503 immediately.
			if cooldown := h.pool.nearestCooldown(accountType, waitExclude); cooldown > 0 {
				wait := cooldown
				if wait > maxCooldownWait {
					respondCapacityError(w, "provider_rate_limited", cooldown)
					return
				}
				if h.cfg.debug.Load() {
					log.Printf("[%s] all %s accounts exhausted, waiting %s for cooldown", reqID, accountType, wait)
				}
				select {
				case <-time.After(wait):
					// Retry with fresh exclude set.
					exclude = map[string]bool{}
					continue
				case <-ctx.Done():
				}
			}

			// All accounts usage-exhausted (primary/secondary windows at the
			// hard-exclude thresholds). Hold the request until a window resets
			// instead of failing fast, bounded by exhaustion_wait and the
			// request deadline.
			if h.cfg.exhaustionWait > 0 && exhaustionRetries < 3 && h.waitForUsageReset(ctx, accountType, reqID, waitExclude) {
				exhaustionRetries++
				exclude = map[string]bool{}
				if attempt == attempts {
					attempts++
				}
				continue
			}

			if lastErr != nil {
				http.Error(w, lastErr.Error(), http.StatusServiceUnavailable)
			} else {
				if requiredPlan != "" {
					http.Error(w, fmt.Sprintf("no live %s %s accounts for model %s", accountType, requiredPlan, requestedModel), http.StatusServiceUnavailable)
				} else {
					http.Error(w, fmt.Sprintf("no live %s accounts", accountType), http.StatusServiceUnavailable)
				}
			}
			return
		}
		exclude[acc.ID] = true

		primaryReason := "balanced"
		if len(selection.Reasons) > 0 {
			primaryReason = selection.Reasons[len(selection.Reasons)-1]
		}

		if inputs.autoDecision != nil {
			selection.Policy = "pool_auto:" + inputs.autoDecision.Profile
			selection.Reasons = append(inputs.autoDecision.Explanation, selection.Reasons...)
			selection.Score = inputs.autoDecision.Score
		}
		circuitState := string(h.pool.circuitBreakers.State(routing.ProviderKey(string(accountType))))
		trace := &RouteTrace{
			RequestID:      reqID,
			Transition:     h.transitionForTrace(userID, conversationID, accountType, w.Header()),
			Timestamp:      rt.now().UTC(),
			Policy:         selection.Policy,
			Selected:       RouteTarget{Provider: string(accountType), Model: requestedModel},
			Score:          selection.Score,
			Reasons:        selection.Reasons,
			Alternatives:   selection.Alternatives,
			CircuitState:   circuitState,
			AccountID:      acc.ID,
			ScoreBreakdown: selection.Breakdown,
			ClientIP:       h.traceClientIP(originIP),
			UserID:         userID,
			Attempts:       attempt,
		}
		if inputs.experiment != nil {
			trace.Experiment = inputs.experiment.Name
			trace.Variant = inputs.experiment.Variant
		}
		h.routeTraces.Record(trace)
		setPoolRouteHeaders(w.Header(), string(accountType), requestedModel, selection.Policy, primaryReason, attempt, reqID)
		w.Header().Set("X-Pool-Circuit-State", circuitState)

		if err := h.checkAccountUse(userID, acc); err != nil {
			respondPolicyError(w, err)
			return
		}
		atomic.AddInt64(&acc.Inflight, 1)
		atomic.AddInt64(&h.inflight, 1)

		resp, sampleBuf, refreshFailed, err := h.tryOnce(ctx, r, inputs.body, inputs.baseURL, provider, acc, reqID, translateDir, requestedModel, userID, originID, conversationID)

		atomic.AddInt64(&acc.Inflight, -1)
		atomic.AddInt64(&h.inflight, -1)

		var policyBlocked *policyError
		if errors.As(err, &policyBlocked) {
			h.auditPolicyDecision(response.admission, "policy.request_blocked", policyBlocked)
			respondPolicyError(w, policyBlocked)
			return
		}
		attemptEnded := rt.now()
		reqDuration := attemptEnded.Sub(start)
		trace.DurationMs = float64(reqDuration.Milliseconds())
		if resp != nil {
			trace.StatusCode = resp.StatusCode
			if trace.Transition != nil {
				h.contextHandoff.MarkTransitionOutcome(conversationScopedKey(userID, conversationID), trace.Transition.Epoch, resp.StatusCode, "", false)
				trace.Transition.StatusCode = resp.StatusCode
			}
			h.pool.recordRoutingOutcome(acc.ID, reqDuration, 0, resp.StatusCode, attemptEnded)
		} else if err != nil {
			h.pool.recordRoutingOutcome(acc.ID, reqDuration, 0, http.StatusServiceUnavailable, attemptEnded)
		}
		if err != nil {
			trace.Error = err.Error()
		}
		h.routeTraces.Record(trace)
		if resp != nil {
			h.metrics.recordPerformance(string(accountType), requestedModel, float64(reqDuration.Milliseconds()), float64(reqDuration.Milliseconds())*0.4, float64(reqDuration.Milliseconds())*0.2, 0, resp.StatusCode, attempt-1, false)
		}

		reqCaps := extractRequestCapabilities(r.URL.Path, inputs.body, r.Header)
		if err != nil {
			errClass := ErrorClassTransient
			if ctx.Err() != nil || isContextError(err) {
				errClass = ErrorClassNone
			}
			h.pool.circuitBreakers.RecordFailure(string(accountType), acc.ID, requestedModel, reqCaps.Modalities, errClass.Retryable())
			if isContextError(err) {
				writeContextError(w, err)
				return
			}
			lastErr = err
			// Don't retry if client already disconnected — context is dead,
			// every subsequent tryOnce will also fail instantly.
			if ctx.Err() != nil {
				rt.logRequest(ctx, reqID, "proxy request interrupted",
					slog.String("account", acc.ID), slog.Int64("duration_ms", rt.now().Sub(start).Milliseconds()),
					slog.String("error", ctx.Err().Error()))
				break
			}
			h.recent.add(err.Error())
			if h.cfg.debug.Load() {
				log.Printf("[%s] attempt %d/%d account=%s failed: %v", reqID, attempt, attempts, acc.ID, err)
			}
			continue
		}
		lastStatus = resp.StatusCode

		// --- Error classification & handling ---
		errClass := classifyStatus(resp.StatusCode)
		recordClass := errClass
		if errClass == ErrorClassNone {
			h.pool.circuitBreakers.RecordSuccess(string(accountType), acc.ID, requestedModel, reqCaps.Modalities)
		}
		if errClass != ErrorClassNone {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			errBody = bodyForInspection(nil, errBody)
			errBodyStr := string(errBody)
			if provider.WireFormat() == WireResponses && resp.StatusCode == http.StatusNotFound && isCodexItemReferenceError(errBody) {
				errClass = ErrorClassInvalid
				recordClass = ErrorClassNone
				if h.cfg.debug.Load() {
					log.Printf("[%s] reclassified codex 404 as item-reference error (invalid input) for account %s", reqID, acc.ID)
				}
			}
			if recordClass != ErrorClassNone {
				h.pool.circuitBreakers.RecordFailure(string(accountType), acc.ID, requestedModel, reqCaps.Modalities, recordClass.Retryable())
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				providerErr := classifyAntigravityError(resp.StatusCode, errBody)
				if providerErr.Class != ProviderErrorQuota {
					// A bare 429 is not proof of exhausted quota. Preserve the
					// original status without cooling or rerouting the account.
					errClass = ErrorClassFatal
				}
			}

			if provider.WireFormat() == WireResponses && isCyberPolicyError(errBody) && !acc.codex.CyberAccess {
				cyberAccessRetry = true
				lastErr = fmt.Errorf("upstream %s from non-cyber-access account %s: %s", resp.Status, acc.ID, safeText(errBody))
				h.recent.add(lastErr.Error())
				if h.metrics != nil {
					h.metrics.incCyberPolicy(acc.ID, "retry_4xx")
				}
				if h.cfg.debug.Load() {
					log.Printf("[%s] retrying cyber_policy response from account %s on a cyber_access account", reqID, acc.ID)
				}
				continue
			}

			// Refine classification with body content.
			if provider.WireFormat() == WireResponses && errClass == ErrorClassInvalid && isCodexModelUnavailableError(errBody) {
				errClass = ErrorClassNotFound
				if h.cfg.debug.Load() {
					log.Printf("[%s] reclassified codex 400 as model/account mismatch for account %s", reqID, acc.ID)
				}
			}
			if acc.Type == AccountTypeClaude && isClaudeOrganizationDisabled(errBody) {
				h.disableAccountPermanently(acc, reqID, safeText(errBody))
				lastErr = fmt.Errorf("claude organization disabled for account %s", acc.ID)
				h.recent.add(lastErr.Error())
				continue
			}

			if provider.WireFormat() == WireResponses && isCodexVoiceAccessDenied(errBody) {
				// AVAS voice entitlement is account-independent for this pool. Retrying
				// every account only delays the same terminal response and leaves the
				// caller holding a Location header without a valid SDP answer.
				errClass = ErrorClassInvalid
			}

			// Cloudflare bot challenges return 403 with HTML — not an auth failure.
			// Reclassify as transient so accounts don't accumulate auth penalties.
			if errClass == ErrorClassAuth && isCloudflareChallenge(errBody, resp.Header) {
				errClass = ErrorClassTransient
				if h.cfg.debug.Load() {
					log.Printf("[%s] reclassified 403 as cloudflare challenge for account %s", reqID, acc.ID)
				}
			}

			if errClass == ErrorClassPayment && isDeactivatedWorkspace(errBody) {
				acc.mu.Lock()
				acc.Dead = true
				acc.Penalty += 100.0
				acc.mu.Unlock()
				log.Printf("[%s] marking account %s as DEAD: %s", reqID, acc.ID, safeText(errBody))
				if err := saveAccount(h.pool.antigravity, acc); err != nil {
					log.Printf("[%s] warning: failed to save dead account %s: %v", reqID, acc.ID, err)
				}
				lastErr = fmt.Errorf("account deactivated: %s", safeText(errBody))
				h.recent.add(lastErr.Error())
				continue
			}

			switch errClass {
			case ErrorClassRateLimit:
				h.applyRateLimitResponse(acc, resp.Header, errBody)
				acc.mu.Lock()
				acc.Penalty += 0.2
				acc.mu.Unlock()

			case ErrorClassAuth:
				markedDead, penaltyNow := applyProxyAuthFailure(acc, refreshFailed)
				if markedDead {
					log.Printf("[%s] account %s DEAD: %d refresh failed, body=%s", reqID, acc.ID, resp.StatusCode, safeText(errBody))
					if err := saveAccount(h.pool.antigravity, acc); err != nil {
						log.Printf("[%s] warning: failed to save dead account %s: %v", reqID, acc.ID, err)
					}
				} else {
					// Upstream auth failures run on the hot path and can happen
					// several times per request. Summarize the headers instead
					// of dumping them: the response may carry Set-Cookie or
					// provider token headers that must never reach the log.
					log.Printf("[%s] account %s got %d, penalty now %.1f, body=%s, resp_headers=%v",
						reqID, acc.ID, resp.StatusCode, penaltyNow, safeText(errBody), debugHeaderSummary(resp.Header))
				}

			case ErrorClassPayment:
				// Non-deactivated payment error — heavy penalty but not dead.
				acc.mu.Lock()
				acc.Penalty += 50.0
				acc.mu.Unlock()

			case ErrorClassTransient:
				acc.mu.Lock()
				acc.Penalty += 0.3
				acc.mu.Unlock()

			case ErrorClassNotFound:
				acc.mu.Lock()
				acc.Penalty += 0.1
				acc.mu.Unlock()
			}

			if errClass.Retryable() {
				if len(errBody) > 0 {
					// lastErr reaches h.recent and the client-facing error, so
					// the upstream body must be redacted here as well as in logs.
					lastErr = fmt.Errorf("upstream %s: %s", resp.Status, safeText(errBody))
				} else {
					lastErr = fmt.Errorf("upstream %s", resp.Status)
				}
				h.recent.add(lastErr.Error())
				if h.cfg.debug.Load() {
					log.Printf("[%s] attempt %d/%d account=%s status=%d class=%s refreshFailed=%v",
						reqID, attempt, attempts, acc.ID, resp.StatusCode, errClass, refreshFailed)
				}
				continue
			}

			// Chat/Messages errors are translated once, when writing the response.
			if translateDir != wire.TranslateNone {
				if translateDir == wire.TranslateClaudeToResponses && inputs.targetFormat == wire.FormatOpenAI && inputs.sourceFormat == wire.FormatClaude {
					var parsed map[string]any
					if json.Unmarshal(errBody, &parsed) == nil {
						errBody = wire.OpenAIErrorToClaude(parsed, errBody)
					}
				}
				resp.Body = io.NopCloser(bytes.NewReader(errBody))
			} else {
				resp.Body = io.NopCloser(strings.NewReader(errBodyStr))
			}
		}

		response.account, response.sample = acc, sampleBuf
		response.requiredPlan = requiredPlan
		response.attempt, response.attempts = attempt, attempts
		if h.writeProxyHTTPResponse(ctx, cancel, w, r, resp, response) {
			cyberAccessRetry = true
			continue
		}
		return
	}

	// All attempts failed.
	if ctx.Err() != nil {
		// Client disconnected — no point sending an error response.
		return
	}
	status := http.StatusBadGateway
	if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	}
	if lastErr == nil {
		lastErr = errors.New("all attempts failed")
	}
	http.Error(w, lastErr.Error(), status)
}

type proxyHTTPAccountRouting struct {
	request          routing.Request
	requestID        string
	imageGeneration  bool
	imageFanout      bool
	imageFanoutIndex int
}

func (h *proxyHandler) selectProxyHTTPAccount(options proxyHTTPAccountRouting, attempt int, cyberAccessRetry bool) routeSelection {
	request := options.request
	if options.imageGeneration && attempt == 1 {
		if options.imageFanout {
			acc := h.pool.selectRoute(routing.Request{
				Mode: routing.MatchImageFanout, Index: options.imageFanoutIndex, Exclude: request.Exclude,
				RequiredPlan: request.RequiredPlan, ClientIP: request.ClientIP,
			}).Account
			if acc != nil {
				return routeSelection{Policy: "balanced", Account: acc}
			}
		} else {
			const preferredImageCodexAccountID = "neon"
			acc := h.pool.selectRoute(routing.Request{
				Mode: routing.MatchAccountID, AccountID: preferredImageCodexAccountID, UserID: request.UserID,
				Provider: request.Provider, RequiredPlan: request.RequiredPlan, ClientIP: request.ClientIP,
			}).Account
			if acc != nil {
				if h.cfg.debug.Load() {
					log.Printf("[%s] routing image generation request to codex account %s", options.requestID, acc.ID)
				}
				return routeSelection{Policy: "balanced", Account: acc}
			}
		}
	}
	if cyberAccessRetry {
		acc := h.pool.selectRoute(routing.Request{
			Mode: routing.MatchCyberAccess, UserID: request.UserID, Exclude: request.Exclude,
			Provider: request.Provider, RequiredPlan: request.RequiredPlan, ClientIP: request.ClientIP,
		}).Account
		if acc != nil && h.cfg.debug.Load() {
			log.Printf("[%s] routing cyber_policy retry to %s account %s", options.requestID, request.Provider, acc.ID)
		}
		return routeSelection{Policy: "balanced", Account: acc}
	}
	return h.pool.selectRoute(request)
}
