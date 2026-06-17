package multitenant

import (
	"context"
	"fmt"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
)

// StaticAccount is a minimal schemas.Account implementation backed by a fixed
// per-tenant config map. Use only for tests / the spike — real tenants would
// resolve providers and keys from Postgres on Init (and ideally hot-reload).
//
// All methods are safe for concurrent read.
type StaticAccount struct {
	tenantID TenantID
	mu       sync.RWMutex
	providers map[schemas.ModelProvider]staticProviderEntry
}

type staticProviderEntry struct {
	keys   []schemas.Key
	config *schemas.ProviderConfig
}

// NewStaticAccount returns an empty StaticAccount for the given tenant.
// Use AddProvider to register provider configs before Init.
func NewStaticAccount(tid TenantID) *StaticAccount {
	return &StaticAccount{
		tenantID:  tid,
		providers: make(map[schemas.ModelProvider]staticProviderEntry),
	}
}

// AddProvider registers a provider entry. cfg may be nil to use defaults.
func (a *StaticAccount) AddProvider(p schemas.ModelProvider, keys []schemas.Key, cfg *schemas.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers[p] = staticProviderEntry{keys: keys, config: cfg}
}

func (a *StaticAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]schemas.ModelProvider, 0, len(a.providers))
	for p := range a.providers {
		out = append(out, p)
	}
	return out, nil
}

func (a *StaticAccount) GetKeysForProvider(_ context.Context, providerKey schemas.ModelProvider) ([]schemas.Key, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	e, ok := a.providers[providerKey]
	if !ok {
		return nil, fmt.Errorf("provider %s not configured for tenant %s", providerKey, a.tenantID)
	}
	return e.keys, nil
}

func (a *StaticAccount) GetConfigForProvider(providerKey schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	e, ok := a.providers[providerKey]
	if !ok {
		return nil, fmt.Errorf("provider %s not configured for tenant %s", providerKey, a.tenantID)
	}
	if e.config != nil {
		return e.config, nil
	}
	return &schemas.ProviderConfig{
		NetworkConfig:            schemas.DefaultNetworkConfig,
		ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize,
	}, nil
}
