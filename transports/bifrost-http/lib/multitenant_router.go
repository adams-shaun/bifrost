package lib

import (
	"context"
	"fmt"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/multitenant"
)

// MultiTenantRouter dispatches inference requests to a per-tenant
// *bifrost.Bifrost runtime, fetched on demand from the supplied
// multitenant.Manager.
//
// The router reads the resolved tenant id off the supplied context
// (under multitenant.BifrostContextKeyTenantID, set by the
// TenantResolverMiddleware after the VK→tenant lookup) and acquires a
// Handle from the Manager. The returned release function decrements
// the per-tenant refcount via Handle.Release so the Manager's LRU can
// evict idle tenants once no requests are in flight against them.
//
// When the context carries no tenant id — health probes, /api/platform
// admin endpoints, or any request the middleware decided to skip — the
// router routes to the configured FallbackTenant. Production
// deployments will set that to multitenant's DefaultTenant slug; the
// single-tenant smoke path leaves it empty and falls back to the
// process-global *bifrost.Bifrost via the FallbackClient option
// instead. That second path is kept as a safety net for the
// transition window between this patch landing and every entry point
// being routed through the tenant resolver.
type MultiTenantRouter struct {
	manager        *multitenant.Manager
	fallbackTenant multitenant.TenantID
	// fallbackClient is the legacy single-tenant runtime used only when
	// fallbackTenant is empty AND the request carries no tenant id. It
	// lets the router serve infrastructure paths (e.g. /health) that
	// pre-date multi-tenancy without forcing them through a Manager
	// acquire. nil when the deployment is fully migrated.
	fallbackClient *bifrost.Bifrost
}

// MultiTenantRouterConfig is the constructor input. Manager is required;
// the two fallback fields together describe the single-tenant fallback
// behaviour (see MultiTenantRouter docs).
type MultiTenantRouterConfig struct {
	// Manager owns the per-tenant *bifrost.Bifrost runtimes.
	Manager *multitenant.Manager

	// FallbackTenant is acquired when the request context carries no
	// tenant id. Empty disables tenant-fallback and falls through to
	// FallbackClient. Production deployments should set this to the
	// DefaultTenant slug so unscoped requests still get a real tenant
	// runtime.
	FallbackTenant multitenant.TenantID

	// FallbackClient is the legacy single-tenant runtime. Used only
	// when FallbackTenant is empty AND the request lacks a tenant id.
	// nil disables the legacy fallback — every request must carry a
	// tenant id or use FallbackTenant.
	FallbackClient *bifrost.Bifrost
}

// NewMultiTenantRouter constructs a MultiTenantRouter. Manager must be
// non-nil — Acquire has no useful behaviour without one. Panics if it's
// nil, matching the SingleTenantRouter convention.
func NewMultiTenantRouter(cfg MultiTenantRouterConfig) *MultiTenantRouter {
	if cfg.Manager == nil {
		panic("lib: nil multitenant.Manager passed to NewMultiTenantRouter")
	}
	return &MultiTenantRouter{
		manager:        cfg.Manager,
		fallbackTenant: cfg.FallbackTenant,
		fallbackClient: cfg.FallbackClient,
	}
}

// Acquire implements BifrostRouter.
//
// Resolution order:
//
//  1. Read tenant id from ctx (multitenant.BifrostContextKeyTenantID).
//  2. If present, Manager.Acquire(tid). The returned release calls
//     Handle.Release.
//  3. If absent and FallbackTenant is set, Manager.Acquire(fallback).
//  4. If absent and FallbackClient is set, serve that with noopRelease.
//  5. Otherwise, return an error — the request cannot be routed.
func (r *MultiTenantRouter) Acquire(ctx context.Context) (*bifrost.Bifrost, func(), error) {
	if tid := tenantIDFromCtx(ctx); tid != "" {
		return r.acquireTenant(ctx, tid)
	}
	if r.fallbackTenant != "" {
		return r.acquireTenant(ctx, r.fallbackTenant)
	}
	if r.fallbackClient != nil {
		return r.fallbackClient, noopRelease, nil
	}
	return nil, nil, fmt.Errorf("multi-tenant router: no tenant id on context and no fallback configured")
}

// Manager returns the underlying multitenant.Manager. Used by
// non-routed code paths that need to evict tenants directly (e.g.
// admin handlers that suspend a tenant or apply a config change).
func (r *MultiTenantRouter) Manager() *multitenant.Manager { return r.manager }

// acquireTenant is the shared post-resolution path: ask the Manager
// for a Handle and wire its Release into the BifrostRouter contract.
func (r *MultiTenantRouter) acquireTenant(ctx context.Context, tid multitenant.TenantID) (*bifrost.Bifrost, func(), error) {
	h, err := r.manager.Acquire(ctx, tid)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire tenant %q: %w", tid, err)
	}
	return h.Bifrost(), h.Release, nil
}

// tenantIDFromCtx reads the resolver middleware's hand-off from the
// context. The middleware writes a string-typed UserValue keyed by the
// stringified BifrostContextKeyTenantID; ctx.Value handles fasthttp
// RequestCtx UserValues transparently for fast-path inference.
func tenantIDFromCtx(ctx context.Context) multitenant.TenantID {
	if ctx == nil {
		return ""
	}
	if v := ctx.Value(multitenant.BifrostContextKeyTenantID); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return multitenant.TenantID(s)
		}
	}
	// Resolver middleware writes the UserValue under the bare string
	// form of the key (so fasthttp's UserValue lookup is happy);
	// production code reading via the typed BifrostContextKey would
	// miss it without this second probe.
	if v := ctx.Value(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return multitenant.TenantID(s)
		}
	}
	return ""
}
