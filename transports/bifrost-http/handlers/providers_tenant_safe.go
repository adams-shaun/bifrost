// Package handlers — providers_tenant_safe.go.
//
// Multi-tenant-safe variants of the provider-config read/write paths
// that the existing handler code uses against h.inMemoryStore. Three
// concerns motivate this file living separately from providers.go:
//
//  1. h.inMemoryStore.GetProviderConfigRaw / GetProviderConfigRedacted
//     read from the SHARED lib.Config.Providers map, which is global
//     (last-write-wins across tenants). In multi-tenant deployments
//     this returns whoever wrote the same provider name last —
//     potentially a DIFFERENT tenant's NetworkConfig / ProxyConfig /
//     CustomProviderConfig — and downstream code merges those values
//     into the request, then writes the result back to the calling
//     tenant's DB row. Effect: cross-tenant config bleed at write
//     time, plus the global idx_key_id constraint trip documented in
//     [docs/multi-tenant-f5/03-things-we-missed.md] item #7.
//
//  2. h.inMemoryStore.UpdateProviderConfig mutates the same shared
//     map AND triggers a "sync keys" path against the configstore
//     that tries to INSERT any "new" keys (keys that don't appear in
//     the previously-shared in-memory entry) — those INSERTs trip the
//     legacy global idx_key_id when a second tenant updates an
//     existing provider name.
//
//  3. The fix has to be CONDITIONAL — single-tenant OSS deployments
//     still rely on h.inMemoryStore for inference dispatch (the root
//     *bifrost.Bifrost reads Account.GetKeysForProvider against the
//     same map). Swapping unconditionally would break OSS behavior.
//
// Branch by TenantIDFromCtx(ctx): when present, multi-tenant; when
// empty, fall through to the legacy in-memory path. Per-tenant
// runtime cache invalidation is unchanged — patch7's defer
// EvictTenant(ctx) at the top of every mutation handler still owns
// runtime refresh.

package handlers

import (
	"errors"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// loadProviderConfigRaw returns the raw (un-redacted) provider config
// for the request's tenant in multi-tenant mode, falling back to the
// shared in-memory store in single-tenant mode. Errors from the
// tenant-scoped path are translated to lib.ErrNotFound so callers can
// keep their existing errors.Is(err, lib.ErrNotFound) checks.
func (h *ProviderHandler) loadProviderConfigRaw(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider) (*configstore.ProviderConfig, error) {
	if TenantIDFromCtx(ctx) != "" && h.dbStore != nil {
		cfg, err := h.dbStore.GetProviderConfig(ctx, provider)
		if errors.Is(err, configstore.ErrNotFound) {
			return nil, lib.ErrNotFound
		}
		return cfg, err
	}
	return h.inMemoryStore.GetProviderConfigRaw(provider)
}

// loadProviderConfigRedacted is the redacted-output sibling of
// loadProviderConfigRaw. In multi-tenant mode it fetches via dbStore
// and calls .Redacted() on the result; in single-tenant mode the
// in-memory store's GetProviderConfigRedacted already redacts.
func (h *ProviderHandler) loadProviderConfigRedacted(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider) (*configstore.ProviderConfig, error) {
	if TenantIDFromCtx(ctx) != "" && h.dbStore != nil {
		cfg, err := h.dbStore.GetProviderConfig(ctx, provider)
		if errors.Is(err, configstore.ErrNotFound) {
			return nil, lib.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return cfg.Redacted(), nil
	}
	return h.inMemoryStore.GetProviderConfigRedacted(provider)
}

// commitProviderUpdate writes provider config changes to the right
// store for the request's tenant. Multi-tenant mode goes direct to
// dbStore inside a transaction — that path is tenant-scoped via the
// GORM scope callback (patch2) and does NOT mutate the shared
// in-memory map (which would be cross-tenant write amplification) or
// re-enter the legacy key-sync code path (which trips global
// idx_key_id, see #7).
//
// Single-tenant mode keeps the existing h.inMemoryStore.UpdateProviderConfig
// path because the root *bifrost.Bifrost still serves inference
// against the in-memory map and the post-write c.client.UpdateProvider
// inside lib.Config refreshes the account snapshot accordingly. In
// multi-tenant mode the per-tenant runtime is invalidated separately
// via the EvictTenant defer at the top of every mutation handler
// (patch7), so the next inference re-loads the tenant runtime from a
// fresh DB read.
func (h *ProviderHandler) commitProviderUpdate(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider, config configstore.ProviderConfig) error {
	if TenantIDFromCtx(ctx) != "" && h.dbStore != nil {
		return h.dbStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
			if err := h.dbStore.UpdateProvider(ctx, provider, config, tx); err != nil {
				if errors.Is(err, configstore.ErrNotFound) {
					return lib.ErrNotFound
				}
				return err
			}
			return nil
		})
	}
	return h.inMemoryStore.UpdateProviderConfig(ctx, provider, config)
}

// commitProviderAdd is the multi-tenant-safe sibling of
// h.inMemoryStore.AddProvider used only on the upsert branch when a
// provider didn't exist yet for the calling tenant. Same branching
// rationale as commitProviderUpdate.
func (h *ProviderHandler) commitProviderAdd(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider, config configstore.ProviderConfig) error {
	if TenantIDFromCtx(ctx) != "" && h.dbStore != nil {
		if err := h.dbStore.AddProvider(ctx, provider, config); err != nil {
			if errors.Is(err, configstore.ErrAlreadyExists) {
				return lib.ErrAlreadyExists
			}
			return err
		}
		return nil
	}
	return h.inMemoryStore.AddProvider(ctx, provider, config)
}
