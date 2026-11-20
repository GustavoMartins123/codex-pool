package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

type providerUsageParityCase struct {
	Provider string `json:"provider"`
	Input    string `json:"input"`
	Output   string `json:"output"`
}

type providerLoadParityCase struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	Output   string `json:"output"`
	Err      string `json:"err"`
}

type providerAuthParityCase struct {
	Provider string            `json:"provider"`
	Headers  map[string]string `json:"headers"`
	Output   string            `json:"output"`
}

type providerRoutingParityCase struct {
	Provider    string `json:"provider"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Upstream    string `json:"upstream"`
	Matches     bool   `json:"matches"`
	Normalized  string `json:"normalized"`
	SSE         bool   `json:"sse"`
	RefreshErr  string `json:"refresh_err"`
}

type providerParityFixture struct {
	Usage   []providerUsageParityCase   `json:"usage"`
	Load    []providerLoadParityCase    `json:"load"`
	Auth    []providerAuthParityCase    `json:"auth"`
	Routing []providerRoutingParityCase `json:"routing"`
}

func providerParityProviders(t *testing.T) map[string]ProviderDriver {
	t.Helper()
	base := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	return map[string]ProviderDriver{
		"claude":      NewClaudeProvider(base("https://claude.example.test")),
		"kimi":        NewKimiProvider(base("https://kimi.example.test/coding")),
		"minimax":     NewMinimaxProvider(base("https://minimax.example.test/anthropic")),
		"zai":         NewZAIProvider(base("https://zai.example.test/api/anthropic")),
		"xiaomi":      NewXiaomiProvider(base("https://xiaomi.example.test/anthropic")),
		"adverserial": NewAdverserialProvider(base("https://adverserial.example.test")),
		"opencode_go": NewOpencodeGoProvider(base("https://opencode.example.test/zen/go")),
	}
}

func providerParityJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func runProviderUsageParity(t *testing.T, providers map[string]ProviderDriver, tc providerUsageParityCase) providerUsageParityCase {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(tc.Input), &obj); err != nil {
		t.Fatal(err)
	}
	got := tc
	usage := providers[tc.Provider].ParseUsage(obj)
	if usage != nil {
		usage.Timestamp = time.Time{}
	}
	got.Output = providerParityJSON(t, usage)
	return got
}

func runProviderLoadParity(t *testing.T, providers map[string]ProviderDriver, tc providerLoadParityCase) providerLoadParityCase {
	t.Helper()
	got := tc
	got.Output, got.Err = "", ""
	account, err := providers[tc.Provider].LoadAccount(tc.Name, "pool/"+tc.Provider+"/"+tc.Name, []byte(tc.Data))
	if account != nil {
		account.Usage.RetrievedAt = time.Time{}
	}
	got.Output = providerLoadedAccountView(t, account)
	if err != nil {
		got.Err = err.Error()
	}
	return got
}

func providerLoadedAccountView(t *testing.T, account *Account) string {
	t.Helper()
	if account == nil {
		return "null"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(providerParityJSON(t, account)), &fields); err != nil {
		t.Fatal(err)
	}
	accountID := account.codex.AccountID
	if account.Type == AccountTypeGrok {
		accountID = account.grok.TokenEndpoint
	}
	providerFields := map[string]any{
		"AccountID":               accountID,
		"IDTokenChatGPTAccountID": account.codex.IDTokenChatGPTAccountID,
		"ImageGenerationSupport":  account.codex.ImageGenerationSupport,
		"ImageGenerationFailures": account.codex.ImageGenerationFailures,
		"ImageGenerationRetryAt":  account.codex.ImageGenerationRetryAt,
		"AccountUUID":             account.claude.UUID,
		"CyberAccess":             account.codex.CyberAccess,
		"CodexCookies":            account.codex.Cookies,
		"RateLimitResetCredits":   account.codex.RateLimitResetCredits,
		"ResetCreditsAvailable":   account.codex.ResetCreditsAvailable,
		"ResetCreditsRetrievedAt": account.codex.ResetCreditsRetrievedAt,
		"ResetCreditRedeeming":    account.codex.ResetCreditRedeeming,
		"ProjectID":               account.google.ProjectID,
		"ModelRateLimits":         account.google.ModelRateLimits,
		"QwenTokenPlan":           account.qwen.TokenPlan,
		"QwenCredits":             account.qwen.Credits,
		"QwenCreditsError":        account.qwen.CreditsError,
	}
	for name, value := range providerFields {
		fields[name] = json.RawMessage(providerParityJSON(t, value))
	}
	return providerParityJSON(t, fields)
}

func runProviderAuthParity(t *testing.T, providers map[string]ProviderDriver, tc providerAuthParityCase) providerAuthParityCase {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	for name, value := range tc.Headers {
		req.Header.Set(name, value)
	}
	providers[tc.Provider].SetAuthHeaders(req, &Account{AccessToken: "upstream-key"})
	got := tc
	got.Output = providerParityJSON(t, req.Header)
	return got
}

func runProviderRoutingParity(t *testing.T, providers map[string]ProviderDriver, tc providerRoutingParityCase) providerRoutingParityCase {
	t.Helper()
	provider := providers[tc.Provider]
	got := tc
	got.Upstream = provider.UpstreamURL(tc.Path).String()
	got.Matches = provider.MatchesPath(tc.Path)
	got.Normalized = provider.NormalizePath(tc.Path)
	got.SSE = provider.DetectsSSE(tc.Path, tc.ContentType)
	got.RefreshErr = ""
	if err := provider.RefreshToken(context.Background(), &Account{}, nil); err != nil {
		got.RefreshErr = err.Error()
	}
	return got
}

func TestProviderSharedBehaviorParity(t *testing.T) {
	data, err := os.ReadFile("testdata/provider_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture providerParityFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	providers := providerParityProviders(t)
	for i, tc := range fixture.Usage {
		t.Run(fmt.Sprintf("usage/%s/%d", tc.Provider, i), func(t *testing.T) {
			if got := runProviderUsageParity(t, providers, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
	for i, tc := range fixture.Load {
		t.Run(fmt.Sprintf("load/%s/%d", tc.Provider, i), func(t *testing.T) {
			var expected map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.Output), &expected); err != nil {
				t.Fatal(err)
			}
			tc.Output = providerParityJSON(t, expected)
			if got := runProviderLoadParity(t, providers, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
	for i, tc := range fixture.Auth {
		t.Run(fmt.Sprintf("auth/%s/%d", tc.Provider, i), func(t *testing.T) {
			if got := runProviderAuthParity(t, providers, tc); got.Output != tc.Output {
				t.Fatalf("got  %s\nwant %s", got.Output, tc.Output)
			}
		})
	}
	for i, tc := range fixture.Routing {
		t.Run(fmt.Sprintf("routing/%s/%d", tc.Provider, i), func(t *testing.T) {
			if got := runProviderRoutingParity(t, providers, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
}
