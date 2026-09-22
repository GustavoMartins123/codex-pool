package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFederatedProviderAnnouncesAndRoutesRemoteModels(t *testing.T) {
	t.Setenv("FEDERATION_TEST_TOKEN", "remote-client-secret")
	provider, err := NewFederatedProvider("home", FederationNodeConfig{
		BaseURL: "https://home.pool.example", TokenEnv: "FEDERATION_TEST_TOKEN", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	account := provider.syntheticAccount()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/pool/models" {
			t.Fatalf("catalog path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer remote-client-secret" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		body := `{"schema_version":1,"models":[{"id":"gpt-5.6-sol","name":"GPT-5.6 Sol","protocol":"openai","contextWindow":372000,"max_output_tokens":128000,"modalities":["text","image"],"capabilities":{"reasoning":true,"tools":true},"available_now":true}]}`
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header: http.Header{"Content-Type": {"application/json"}},
			Body:   io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	if err := provider.syncModels(context.Background(), transport, account); err != nil {
		t.Fatal(err)
	}
	publicID := "federation/home/gpt-5.6-sol"
	if canonical, ok := provider.ResolveModel(publicID); !ok || canonical != "gpt-5.6-sol" {
		t.Fatalf("ResolveModel = %q, %v", canonical, ok)
	}
	if account.Dead || account.Disabled || account.HealthError != "" {
		t.Fatalf("account health = dead:%v disabled:%v error:%q", account.Dead, account.Disabled, account.HealthError)
	}
	pool := newPoolState([]*Account{account}, false)
	descriptors := poolModelDescriptors(pool)
	found := false
	for _, descriptor := range descriptors {
		if descriptor.ID == publicID {
			found = descriptor.Provider == "federation:home" && descriptor.AvailableNow
		}
	}
	if !found {
		t.Fatalf("federated model missing from descriptors: %#v", descriptors)
	}
}

func TestFederationDisabledCreatesNoProviders(t *testing.T) {
	providers, accounts, err := buildFederatedProviders(FederationConfig{
		Enabled: false,
		Nodes: map[string]FederationNodeConfig{
			"home": {Enabled: true, BaseURL: "https://home.pool.example", TokenEnv: "TOKEN"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 || len(accounts) != 0 {
		t.Fatalf("providers=%d accounts=%d", len(providers), len(accounts))
	}
}

func TestFederationSecretIsNotInModelCatalog(t *testing.T) {
	t.Setenv("FEDERATION_TEST_TOKEN", "federation-private-secret")
	provider, err := NewFederatedProvider("lab", FederationNodeConfig{
		BaseURL: "https://lab.pool.example", TokenEnv: "FEDERATION_TEST_TOKEN", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	account := provider.syntheticAccount()
	encoded := string(genericProviderConfigJSON(GenericProviderConfig{BaseURL: provider.base.String()}))
	if strings.Contains(encoded, "federation-private-secret") || account.Label == "federation-private-secret" {
		t.Fatal("federation secret leaked into public metadata")
	}
}

func TestUnavailableFederationNodeOnlyDisablesItsAccount(t *testing.T) {
	account := &Account{Type: AccountType("federation:lab"), ID: "federation-lab"}
	updateFederationHealth(account, errors.New("node offline"))
	if !account.Dead || account.HealthError != "node offline" {
		t.Fatalf("account = %#v", account)
	}
}
