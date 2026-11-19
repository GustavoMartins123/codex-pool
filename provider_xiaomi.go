package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// XiaomiProvider handles Xiaomi MiMo Token Plan accounts through the Anthropic-compatible API.
type XiaomiProvider struct {
	apiKeyUpstream
	bearerAuth
}

// NewXiaomiProvider creates a new Xiaomi MiMo provider.
func NewXiaomiProvider(xiaomiBase *url.URL) *XiaomiProvider {
	return &XiaomiProvider{apiKeyUpstream: apiKeyUpstream{base: xiaomiBase}}
}

func (p *XiaomiProvider) Type() AccountType {
	return AccountTypeXiaomi
}

type XiaomiAuthJSON struct {
	APIKey   string `json:"api_key"`
	Dead     bool   `json:"dead"`
	Disabled bool   `json:"disabled"`
}

func (p *XiaomiProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var xj XiaomiAuthJSON
	if err := json.Unmarshal(data, &xj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if xj.APIKey == "" {
		return nil, nil
	}

	acc := newAPIKeyAccount(AccountTypeXiaomi, "xiaomi", name, path, xj.APIKey)
	acc.Dead = xj.Dead
	acc.Disabled = xj.Disabled
	return acc, nil
}

func (p *XiaomiProvider) ParseUsage(obj map[string]any) *RequestUsage {
	return anthropicCombinedUsage(obj)
}

func (p *XiaomiProvider) ParseUsageHeaders(acc *Account, headers http.Header) {
}

func isXiaomiModel(model string) bool {
	_, ok := modelForProvider(AccountTypeXiaomi, model)
	return ok
}

func xiaomiCanonicalModel(model string) string {
	if found, ok := modelForProvider(AccountTypeXiaomi, model); ok {
		return found.ID
	}
	return model
}
