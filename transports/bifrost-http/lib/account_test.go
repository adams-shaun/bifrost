package lib

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

func TestTenantScopedAccount_GetConfiguredProviders(t *testing.T) {
	providers := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.OpenAI:    {Keys: []schemas.Key{{ID: "k1"}}},
		schemas.Anthropic: {Keys: []schemas.Key{{ID: "k2"}}},
	}
	a := NewTenantScopedAccount(providers)

	got, err := a.GetConfiguredProviders()
	if err != nil {
		t.Fatalf("GetConfiguredProviders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 providers, got %d (%v)", len(got), got)
	}
	seen := map[schemas.ModelProvider]bool{}
	for _, p := range got {
		seen[p] = true
	}
	if !seen[schemas.OpenAI] || !seen[schemas.Anthropic] {
		t.Fatalf("missing expected providers: %v", seen)
	}
}

func TestTenantScopedAccount_EmptyMap(t *testing.T) {
	a := NewTenantScopedAccount(nil)

	provs, err := a.GetConfiguredProviders()
	if err != nil {
		t.Fatalf("GetConfiguredProviders: %v", err)
	}
	if len(provs) != 0 {
		t.Fatalf("expected empty providers, got %v", provs)
	}

	if _, err := a.GetKeysForProvider(context.Background(), schemas.OpenAI); err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
	if _, err := a.GetConfigForProvider(schemas.OpenAI); err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
}

func TestTenantScopedAccount_GetKeysForProvider(t *testing.T) {
	want := []schemas.Key{{ID: "k1"}, {ID: "k2"}, {ID: "k3"}}
	providers := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.OpenAI: {Keys: want},
	}
	a := NewTenantScopedAccount(providers)

	got, err := a.GetKeysForProvider(context.Background(), schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d keys, got %d", len(want), len(got))
	}
}

func TestTenantScopedAccount_GetKeysForProvider_IncludeOnlyFilter(t *testing.T) {
	all := []schemas.Key{{ID: "k1"}, {ID: "k2"}, {ID: "k3"}}
	providers := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.OpenAI: {Keys: all},
	}
	a := NewTenantScopedAccount(providers)

	// IncludeOnlyKeys filter — same shape governance plugin uses for
	// per-VK key allow-listing.
	ctx := context.WithValue(context.Background(),
		schemas.BifrostContextKeyGovernanceIncludeOnlyKeys, []string{"k1", "k3"})
	got, err := a.GetKeysForProvider(ctx, schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 filtered keys, got %d (%v)", len(got), got)
	}
	seen := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !seen["k1"] || !seen["k3"] {
		t.Fatalf("expected k1+k3 to survive filter, got %v", seen)
	}

	// Empty list means "no keys allowed" (matches BaseAccount behaviour).
	ctxEmpty := context.WithValue(context.Background(),
		schemas.BifrostContextKeyGovernanceIncludeOnlyKeys, []string{})
	got, err = a.GetKeysForProvider(ctxEmpty, schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if got != nil {
		t.Fatalf("empty IncludeOnly should return nil, got %v", got)
	}
}

func TestTenantScopedAccount_GetConfigForProvider_DefaultsAndOverrides(t *testing.T) {
	providers := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.OpenAI: {
			SendBackRawRequest: true,
			// NetworkConfig + ConcurrencyAndBufferSize left nil — should
			// fall back to schemas defaults.
		},
	}
	a := NewTenantScopedAccount(providers)

	got, err := a.GetConfigForProvider(schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetConfigForProvider: %v", err)
	}
	if !got.SendBackRawRequest {
		t.Fatal("SendBackRawRequest should round-trip")
	}
	// NetworkConfig contains a map field so we can't compare with == ;
	// check a few representative scalars instead.
	if got.NetworkConfig.MaxConnsPerHost != schemas.DefaultNetworkConfig.MaxConnsPerHost {
		t.Fatal("nil NetworkConfig should fall back to DefaultNetworkConfig")
	}
	if got.ConcurrencyAndBufferSize != schemas.DefaultConcurrencyAndBufferSize {
		t.Fatal("nil ConcurrencyAndBufferSize should fall back to default")
	}
}
