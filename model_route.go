package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"codex-pool-proxy/internal/requestjson"
)

func stripCodexModelSuffixes(model string) (base string, reasoningEffort string, serviceTier string) {
	base = strings.TrimSpace(model)
	lower := strings.ToLower(base)
	for _, tier := range []string{"fast", "flex"} {
		suffix := "-" + tier
		if strings.HasSuffix(lower, suffix) {
			serviceTier = tier
			if serviceTier == "fast" {
				serviceTier = "priority"
			}
			base = strings.TrimSpace(base[:len(base)-len(suffix)])
			lower = strings.ToLower(base)
			break
		}
	}
	// Order matters: longer suffixes first so -xhigh wins over -high, etc.
	// GPT-5.6 adds max (above xhigh). ultra is multi-agent product mode;
	// map suffix to max for single-request routing through the pool.
	for _, effort := range []string{"xhigh", "minimal", "medium", "ultra", "none", "high", "max", "low"} {
		suffix := "-" + effort
		if strings.HasSuffix(lower, suffix) {
			reasoningEffort = effort
			if effort == "ultra" {
				reasoningEffort = "max"
			}
			base = strings.TrimSpace(base[:len(base)-len(suffix)])
			break
		}
	}
	return base, reasoningEffort, serviceTier
}

func applyCodexModelSuffixControls(body []byte, originalModel string) ([]byte, string) {
	base, effort, serviceTier := stripCodexModelSuffixes(originalModel)
	if base == "" || (base == originalModel && effort == "" && serviceTier == "") {
		return body, originalModel
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, originalModel
	}
	obj["model"] = base
	if effort != "" {
		reasoning, _ := obj["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		if _, ok := reasoning["summary"]; !ok {
			reasoning["summary"] = "auto"
		}
		reasoning["effort"] = effort
		obj["reasoning"] = reasoning
	}
	if serviceTier != "" {
		obj["service_tier"] = serviceTier
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body, originalModel
	}
	return rewritten, base
}

func modelRequiresCodexPro(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), "gpt-5.3-codex-spark")
}

const requiredPlanClaudePremium = "claude_premium"

func claudeRequestRequiresPremium(r *http.Request, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	// Opus is available on Pro. Only explicit extended-context requests use
	// this plan filter; regular model access is decided by Claude upstream.
	if strings.Contains(model, "[1m]") {
		return true
	}
	if r == nil {
		return false
	}
	for _, beta := range r.Header.Values("anthropic-beta") {
		if strings.Contains(strings.ToLower(beta), "context-1m-") {
			return true
		}
	}
	return false
}

func requiredPlanForRequest(accountType AccountType, r *http.Request, requestedModel string) string {
	if accountType == AccountTypeCodex && modelRequiresCodexPro(requestedModel) {
		return "pro"
	}
	if accountType == AccountTypeClaude && claudeRequestRequiresPremium(r, requestedModel) {
		return requiredPlanClaudePremium
	}
	return ""
}

func (h *proxyHandler) modelRouteOverride(path, model string, body []byte) (ProviderDriver, *url.URL, []byte) {
	provider, rewritten := h.registry.resolveBufferedModel(h.pool, model, body)
	if provider == nil {
		return nil, nil, nil
	}
	return provider, provider.UpstreamURL(path), rewritten
}

const streamedModelRoutePeekBytes = 64 * 1024

func shouldPeekStreamedModelRoute(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || !isStreamedModelRoutePath(r.URL.Path) {
		return false
	}
	// Chunked Responses bodies never advertise a size. Peek anyway so a leading
	// "model" field can divert Grok/Kimi/etc. before Codex streaming fallthrough.
	if r.ContentLength < 0 {
		return isCodexResponsesPath(r.URL.Path)
	}
	return r.ContentLength >= streamedModelRoutePeekBytes
}

func (h *proxyHandler) applyStreamedModelRoute(r *http.Request, provider ProviderDriver, targetBase *url.URL, reqID string) (ProviderDriver, *url.URL, error) {
	if r == nil || r.Body == nil || h == nil || h.registry == nil {
		return provider, targetBase, nil
	}
	if r.Method != http.MethodPost || !isStreamedModelRoutePath(r.URL.Path) {
		return provider, targetBase, nil
	}
	if strings.TrimSpace(r.Header.Get("Content-Encoding")) != "" {
		return provider, targetBase, nil
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "json") {
		return provider, targetBase, nil
	}

	prefix, err := requestjson.ReadBodyPrefix(r.Body, streamedModelRoutePeekBytes)
	if err != nil {
		r.Body = &requestjson.PrefixReadCloser{R: bytes.NewReader(prefix), C: r.Body}
		return provider, targetBase, err
	}
	restoreBody := func(replacement []byte) {
		r.Body = &requestjson.PrefixReadCloser{R: io.MultiReader(bytes.NewReader(replacement), r.Body), C: r.Body}
	}

	model, valueStart, valueEnd, ok := requestjson.FindTopLevelStringField(prefix, "model")
	if !ok {
		restoreBody(prefix)
		return provider, targetBase, nil
	}

	requestedModel := strings.TrimSpace(model)
	if resolved, aliased := h.aliases.Resolve(requestedModel); aliased {
		if h.cfg != nil && h.cfg.debug.Load() {
			log.Printf("[%s] streamed model alias: %s -> %s", reqID, requestedModel, resolved)
		}
		requestedModel = resolved
	}
	routeProvider, routeBase, canonicalModel := h.resolveStreamedModelRoute(r.URL.Path, requestedModel)
	if routeProvider == nil {
		restoreBody(prefix)
		return provider, targetBase, nil
	}
	if admission := policyAdmissionFromRequest(r); admission != nil {
		if err := admission.CheckModel(canonicalModel); err != nil {
			restoreBody(prefix)
			return provider, targetBase, err
		}
		if err := admission.CheckProvider(routeProvider.Type()); err != nil {
			restoreBody(prefix)
			return provider, targetBase, err
		}
	}
	rewrittenPrefix := prefix
	delta := 0
	if canonicalModel != requestedModel {
		rewrittenPrefix, delta, err = requestjson.ReplaceStringToken(prefix, valueStart, valueEnd, canonicalModel)
	}
	if err != nil {
		restoreBody(prefix)
		return provider, targetBase, err
	}
	if r.ContentLength >= 0 && delta != 0 {
		r.ContentLength += int64(delta)
		r.Header.Del("Content-Length")
	}
	restoreBody(rewrittenPrefix)
	return routeProvider, routeBase, nil
}

func (h *proxyHandler) resolveStreamedModelRoute(path, model string) (ProviderDriver, *url.URL, string) {
	provider, canonical := h.registry.resolveStreamedModel(h.pool, model)
	if provider == nil {
		return nil, nil, canonical
	}
	return provider, provider.UpstreamURL(path), canonical
}
