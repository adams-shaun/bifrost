// Package lib provides core functionality for the Bifrost HTTP service,
// including context propagation, header management, and integration with monitoring systems.
package lib

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

// BaseAccount implements the Account interface for Bifrost.
// It manages provider configurations using a in-memory store for persistent storage.
// All data processing (environment variables, key configs) is done upfront in the store.
type BaseAccount struct {
	store *Config // store for in-memory configuration
}

// NewBaseAccount creates a new BaseAccount with the given store
func NewBaseAccount(store *Config) *BaseAccount {
	return &BaseAccount{
		store: store,
	}
}

// GetConfiguredProviders returns a list of all configured providers.
// Implements the Account interface.
func (baseAccount *BaseAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	if baseAccount.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	return baseAccount.store.GetAllProviders()
}

// GetKeysForProvider returns the API keys configured for a specific provider.
// Keys are already processed (environment variables resolved) by the store.
// Implements the Account interface.
func (baseAccount *BaseAccount) GetKeysForProvider(ctx context.Context, providerKey schemas.ModelProvider) ([]schemas.Key, error) {
	if baseAccount.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	config, err := baseAccount.store.GetProviderConfigRaw(providerKey)
	if err != nil {
		return nil, err
	}
	keys := config.Keys
	if v := ctx.Value(schemas.BifrostContextKeyGovernanceIncludeOnlyKeys); v != nil {
		if includeOnlyKeys, ok := v.([]string); ok {
			if len(includeOnlyKeys) == 0 {
				// header present but empty means "no keys allowed"
				keys = nil
			} else {
				set := make(map[string]struct{}, len(includeOnlyKeys))
				for _, id := range includeOnlyKeys {
					set[id] = struct{}{}
				}
				filtered := make([]schemas.Key, 0, len(keys))
				for _, key := range keys {
					if _, ok := set[key.ID]; ok {
						filtered = append(filtered, key)
					}
				}
				keys = filtered
			}
		}
	}
	return keys, nil
}

// GetConfigForProvider returns the complete configuration for a specific provider.
// Configuration is already fully processed (environment variables, key configs) by the store.
// Implements the Account interface.
func (baseAccount *BaseAccount) GetConfigForProvider(providerKey schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if baseAccount.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	config, err := baseAccount.store.GetProviderConfigRaw(providerKey)
	if err != nil {
		return nil, err
	}
	providerConfig := &schemas.ProviderConfig{}
	if config.ProxyConfig != nil {
		providerConfig.ProxyConfig = config.ProxyConfig
	}
	if config.NetworkConfig != nil {
		providerConfig.NetworkConfig = *config.NetworkConfig
	} else {
		providerConfig.NetworkConfig = schemas.DefaultNetworkConfig
	}
	if config.ConcurrencyAndBufferSize != nil {
		providerConfig.ConcurrencyAndBufferSize = *config.ConcurrencyAndBufferSize
	} else {
		providerConfig.ConcurrencyAndBufferSize = schemas.DefaultConcurrencyAndBufferSize
	}
	providerConfig.SendBackRawRequest = config.SendBackRawRequest
	providerConfig.SendBackRawResponse = config.SendBackRawResponse
	providerConfig.StoreRawRequestResponse = config.StoreRawRequestResponse
	if config.CustomProviderConfig != nil {
		providerConfig.CustomProviderConfig = config.CustomProviderConfig
	}
	if config.OpenAIConfig != nil {
		providerConfig.OpenAIConfig = config.OpenAIConfig
	}
	return providerConfig, nil
}

// TenantScopedAccount implements the schemas.Account interface against a
// pre-loaded slice of providers belonging to a single tenant. The
// MultiTenantBifrost loader builds one of these per tenant via
// configstore.GetProvidersConfigByTenant and hands it to bifrost.Init —
// the resulting *bifrost.Bifrost only ever sees that tenant's keys.
//
// In contrast to BaseAccount (which reads from the live *Config and so
// shares state with every other in-process consumer), a
// TenantScopedAccount holds an immutable snapshot of the tenant's
// providers taken at runtime acquire time. Hot-reload of provider
// configs for a tenant therefore requires evicting that tenant's
// runtime from the multi-tenant Manager, which then re-loads from the
// store on the next Acquire.
type TenantScopedAccount struct {
	providers map[schemas.ModelProvider]configstore.ProviderConfig
}

// NewTenantScopedAccount constructs a TenantScopedAccount from the
// provider map a per-tenant query produced. The map is captured by
// reference; callers should treat it as read-only after handing it off.
//
// A nil map is permissible — it produces a no-providers account that
// returns ErrNotConfigured from every accessor, which is the correct
// behaviour for a tenant that has no providers registered yet.
func NewTenantScopedAccount(providers map[schemas.ModelProvider]configstore.ProviderConfig) *TenantScopedAccount {
	return &TenantScopedAccount{providers: providers}
}

// GetConfiguredProviders returns the providers in the captured snapshot.
func (a *TenantScopedAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	out := make([]schemas.ModelProvider, 0, len(a.providers))
	for p := range a.providers {
		out = append(out, p)
	}
	return out, nil
}

// GetKeysForProvider returns the API keys configured for a provider in
// the tenant snapshot. The IncludeOnlyKeys context filter (used by the
// governance plugin for per-VK key allow-listing) is respected — same
// shape as BaseAccount.
func (a *TenantScopedAccount) GetKeysForProvider(ctx context.Context, providerKey schemas.ModelProvider) ([]schemas.Key, error) {
	config, ok := a.providers[providerKey]
	if !ok {
		return nil, fmt.Errorf("provider %s not configured for this tenant", providerKey)
	}
	keys := config.Keys
	if v := ctx.Value(schemas.BifrostContextKeyGovernanceIncludeOnlyKeys); v != nil {
		if includeOnlyKeys, ok := v.([]string); ok {
			if len(includeOnlyKeys) == 0 {
				keys = nil
			} else {
				set := make(map[string]struct{}, len(includeOnlyKeys))
				for _, id := range includeOnlyKeys {
					set[id] = struct{}{}
				}
				filtered := make([]schemas.Key, 0, len(keys))
				for _, key := range keys {
					if _, ok := set[key.ID]; ok {
						filtered = append(filtered, key)
					}
				}
				keys = filtered
			}
		}
	}
	return keys, nil
}

// GetConfigForProvider returns the schemas.ProviderConfig for a given
// provider in the tenant snapshot. The configstore.ProviderConfig →
// schemas.ProviderConfig conversion mirrors BaseAccount.GetConfigForProvider
// so behaviour stays identical for non-tenant call sites that wire a
// TenantScopedAccount as the Account.
func (a *TenantScopedAccount) GetConfigForProvider(providerKey schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	config, ok := a.providers[providerKey]
	if !ok {
		return nil, fmt.Errorf("provider %s not configured for this tenant", providerKey)
	}
	providerConfig := &schemas.ProviderConfig{}
	if config.ProxyConfig != nil {
		providerConfig.ProxyConfig = config.ProxyConfig
	}
	if config.NetworkConfig != nil {
		providerConfig.NetworkConfig = *config.NetworkConfig
	} else {
		providerConfig.NetworkConfig = schemas.DefaultNetworkConfig
	}
	if config.ConcurrencyAndBufferSize != nil {
		providerConfig.ConcurrencyAndBufferSize = *config.ConcurrencyAndBufferSize
	} else {
		providerConfig.ConcurrencyAndBufferSize = schemas.DefaultConcurrencyAndBufferSize
	}
	providerConfig.SendBackRawRequest = config.SendBackRawRequest
	providerConfig.SendBackRawResponse = config.SendBackRawResponse
	providerConfig.StoreRawRequestResponse = config.StoreRawRequestResponse
	if config.CustomProviderConfig != nil {
		providerConfig.CustomProviderConfig = config.CustomProviderConfig
	}
	if config.OpenAIConfig != nil {
		providerConfig.OpenAIConfig = config.OpenAIConfig
	}
	return providerConfig, nil
}
