package main

import "strings"

type providerModelRoute struct {
	accountType AccountType
	matches     func(string) bool
	canonical   func(string) string
	rewrite     func([]byte, string) []byte
	discover    bool
}

func (route providerModelRoute) modelID(model string) string {
	if route.canonical == nil {
		return model
	}
	return route.canonical(model)
}

func configuredModelRoutes() (buffered, streamed []providerModelRoute) {
	qwen := providerModelRoute{
		accountType: AccountTypeQwen, matches: isQwenModel, canonical: strings.TrimSpace,
		rewrite: rewriteModelInBody, discover: true,
	}
	claude := providerModelRoute{
		accountType: AccountTypeClaude, matches: isClaudeModel,
		canonical: claudeCanonicalModel, rewrite: rewriteModelInBody,
	}
	kimi := providerModelRoute{accountType: AccountTypeKimi, matches: isKimiModel}
	minimax := providerModelRoute{
		accountType: AccountTypeMinimax, matches: isMinimaxModel,
		canonical: minimaxCanonicalModel, rewrite: rewriteModelInBody,
	}
	zai := providerModelRoute{
		accountType: AccountTypeZAI, matches: isZAIModel,
		canonical: zaiCanonicalModel, rewrite: rewriteModelInBody,
	}
	xiaomi := providerModelRoute{
		accountType: AccountTypeXiaomi, matches: isXiaomiModel,
		canonical: xiaomiCanonicalModel, rewrite: rewriteModelInBody,
	}
	grok := providerModelRoute{
		accountType: AccountTypeGrok, matches: isGrokModel,
		canonical: grokCanonicalModel, rewrite: rewriteAndSanitizeGrokRequestBody,
	}
	adverserial := providerModelRoute{
		accountType: AccountTypeAdverserial, matches: isAdverserialModel, rewrite: rewriteAndClampAdverserialRequestBody,
	}
	opencodeGo := providerModelRoute{
		accountType: AccountTypeOpencodeGo, matches: isOpencodeGoModel, rewrite: opencodeGoUpstreamBody,
	}
	opencodeGoStreamed := opencodeGo
	opencodeGoStreamed.canonical = opencodeGoStreamCanonicalModel
	codex := providerModelRoute{accountType: AccountTypeCodex, matches: isOpenAIModel}

	buffered = []providerModelRoute{qwen, kimi, minimax, zai, xiaomi, adverserial, grok, opencodeGo, codex, claude}
	streamed = []providerModelRoute{qwen, claude, kimi, minimax, zai, xiaomi, grok, adverserial, opencodeGoStreamed}
	return buffered, streamed
}

func (r *ProviderRegistry) resolveBufferedModel(pool *poolState, model string, body []byte) (ProviderDriver, []byte) {
	if provider, canonical, ok := r.ResolveModel(model); ok {
		return provider, rewriteModelInBody(body, canonical)
	}
	for _, route := range r.bufferedModelRoutes {
		if !route.matches(model) {
			continue
		}
		provider := r.ForType(route.accountType)
		if provider == nil || route.rewrite == nil {
			return provider, nil
		}
		canonical := route.modelID(model)
		if pool != nil && route.discover {
			if discovered, ok := pool.discoveredCanonicalModel(route.accountType, model); ok {
				canonical = discovered
			}
		}
		return provider, route.rewrite(body, canonical)
	}
	return nil, nil
}

func (r *ProviderRegistry) resolveStreamedModel(pool *poolState, model string) (ProviderDriver, string) {
	if provider, canonical, ok := r.ResolveModel(model); ok {
		return provider, canonical
	}
	for _, route := range r.streamedModelRoutes {
		var canonical string
		var discovered bool
		if pool != nil {
			canonical, discovered = pool.discoveredCanonicalModel(route.accountType, model)
		}
		if !discovered && !route.matches(model) {
			continue
		}
		provider := r.ForType(route.accountType)
		if provider == nil {
			return nil, model
		}
		if !discovered {
			canonical = route.modelID(model)
		}
		return provider, canonical
	}
	return nil, model
}
