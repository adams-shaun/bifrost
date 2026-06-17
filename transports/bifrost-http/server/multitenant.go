// Package server — multitenant.go.
//
// Stage 2 runtime-isolation glue. Stage 1 (patches 1-6) added data-
// scope isolation via the GORM tenant-scope callback: every SELECT/
// UPDATE/DELETE/INSERT on a tenant-scoped table is implicitly bound
// to the request's `x-f5xc-tenant` header. But the inference path
// still dispatched through a single shared `s.Client *bifrost.Bifrost`,
// whose Account read the global `lib.Config.Providers` in-memory map
// — last-write-wins across tenants. Two tenants with a provider named
// "openai" stomped on each other at request time.
//
// Stage 2 wires the multitenant.Manager (added in patch1) into the
// server bootstrap. On the first inference request for a given
// tenant, Manager.Acquire calls the TenantLoader built here, which:
//
//   1. Reads that tenant's providers + keys from the configstore via
//      a tenant-scoped context (the GORM callback adds
//      `WHERE tenant_id = '<tid>'` automatically).
//   2. Snapshots them into a multitenant.StaticAccount — a per-
//      tenant Account that the per-tenant *bifrost.Bifrost can read
//      without ever touching the shared in-memory map.
//   3. Shares the global plugin chain (logging / governance /
//      telemetry / etc.) via multitenant.ShareLLMPlugins so a per-
//      tenant Bifrost.Shutdown can't tear down plugin state the
//      root runtime still depends on (the share-shim noop's
//      Cleanup, the root runtime owns the real Cleanup).
//   4. Returns a BifrostConfig that the Manager passes into a fresh
//      bifrost.Init — one engine per tenant, own worker pool, own
//      provider queues, own request-side concurrency budget.
//
// Eviction is admin-driven: when /api/providers, /api/providers/*/
// keys, /api/mcp/clients, or /api/governance/virtual-keys mutate a
// tenant's config, the handler calls s.Manager.Evict(tid) so the
// next inference for that tenant Acquires a freshly loaded runtime.
// Inference paths NEVER trigger eviction — Acquire/Release are
// O(1) once the runtime is loaded, and refcounted so eviction blocks
// on in-flight requests draining.
package server

import (
	"context"
	"fmt"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/transports/bifrost-http/handlers"
)

// initializeManager constructs the per-tenant runtime manager and
// attaches it to s.Manager. Safe to call multiple times — subsequent
// calls are a no-op once s.Manager is non-nil. Must run AFTER s.Client
// (the root runtime) is up, because the TenantLoader reuses the root
// runtime's plugin chain + MCP config via the share-shim.
func (s *BifrostHTTPServer) initializeManager(ctx context.Context) error {
	if s.Manager != nil {
		return nil
	}
	if s.Client == nil {
		return fmt.Errorf("multitenant: initializeManager called before root Client is up")
	}
	if s.Config == nil || s.Config.ConfigStore == nil {
		return fmt.Errorf("multitenant: initializeManager requires Config.ConfigStore")
	}
	mgr, err := multitenant.NewManager(ctx, multitenant.ManagerConfig{
		Loader: s.newTenantLoader(),
		// MaxActiveRuntimes left at the default (unbounded). Each
		// tenant runtime is cheap until it sees traffic; the LRU cap
		// is a productization knob (Stage 2 follow-up).
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("multitenant: NewManager: %w", err)
	}
	s.Manager = mgr
	// Wire the package-level eviction hook so admin write handlers
	// (providers / keys / VK / MCP) can call handlers.EvictTenant(ctx)
	// without taking a direct dependency on this server struct.
	handlers.TenantEvict = s.EvictTenantRuntime
	logger.Info("multitenant: per-tenant runtime manager initialized")
	return nil
}

// newTenantLoader returns the TenantLoader the Manager invokes on the
// first Acquire for each tenant (and on every Acquire after an
// Evict). The loader is a closure over s.Config (configstore +
// plugins + MCP) and s.Client (for the shared plugin instances).
//
// Behaviour:
//   - Reads providers + keys for the tenant via tenant-scoped ctx (the
//     GORM callback in framework/configstore appends `WHERE tenant_id
//     = '<tid>'`). Skips tenants with no providers — returns a
//     BifrostConfig with an empty StaticAccount, so the engine starts
//     cleanly and serves "provider not configured" errors at request
//     time rather than crashing at boot.
//   - Builds a multitenant.StaticAccount snapshot. The snapshot is
//     captured at Acquire time and re-captured on every Evict, so
//     admin writes hit the wire via Manager.Evict(tid) (see
//     evictTenantOnAdminWrite hooks in the provider/key handlers).
//   - Wraps the root runtime's plugin chains via ShareLLMPlugins /
//     ShareMCPPlugins so the per-tenant engine's Shutdown() can't
//     Cleanup plugin state the root still depends on.
func (s *BifrostHTTPServer) newTenantLoader() multitenant.TenantLoader {
	return func(ctx context.Context, tid multitenant.TenantID) (schemas.BifrostConfig, error) {
		// Push the tenant onto ctx so every configstore read filters
		// to this tenant via the GORM scope callback. The Manager
		// passed its rootCtx in; we layer the tenant on top.
		scopedCtx := context.WithValue(ctx, multitenant.BifrostContextKeyTenantID, string(tid))

		providers, err := s.Config.ConfigStore.GetProvidersConfig(scopedCtx)
		if err != nil {
			return schemas.BifrostConfig{}, fmt.Errorf("multitenant: load providers for tenant %q: %w", tid, err)
		}

		acct := multitenant.NewStaticAccount(tid)
		for name, cfg := range providers {
			keys, err := s.Config.ConfigStore.GetProviderKeys(scopedCtx, name)
			if err != nil {
				return schemas.BifrostConfig{}, fmt.Errorf("multitenant: load keys for tenant %q provider %q: %w", tid, name, err)
			}
			acct.AddProvider(name, keys, providerConfigForBifrost(cfg))
		}

		return schemas.BifrostConfig{
			Account:            acct,
			InitialPoolSize:    s.Config.ClientConfig.InitialPoolSize,
			DropExcessRequests: s.Config.ClientConfig.DropExcessRequests,
			// Plugins shared via share-shim so per-tenant Shutdown
			// can't Cleanup state the root depends on.
			LLMPlugins: multitenant.ShareLLMPlugins(s.Config.GetLoadedLLMPlugins()),
			MCPPlugins: multitenant.ShareMCPPlugins(s.Config.GetLoadedMCPPlugins()),
			// MCP / OAuth / KVStore: per-process globals reused across
			// tenants. Per-tenant MCP isolation is a productization
			// follow-up — the schema already carries tenant_id on
			// TableMCPClient, so the same loader pattern can layer it
			// when needed.
			OAuth2Provider: s.Config.OAuthProvider,
			Logger:         logger,
			KVStore:        s.Config.KVStore,
		}, nil
	}
}

// providerConfigForBifrost lifts the configstore representation of a
// provider config into the schemas.ProviderConfig that
// bifrost.Init / Account.GetConfigForProvider expects. Mirrors the
// mapping in lib/account.go's BaseAccount.GetConfigForProvider so the
// per-tenant runtime sees identical shape — modulo source (snapshot
// at TenantLoader time vs. live in-memory read).
func providerConfigForBifrost(cfg configstore.ProviderConfig) *schemas.ProviderConfig {
	pc := &schemas.ProviderConfig{
		SendBackRawRequest:      cfg.SendBackRawRequest,
		SendBackRawResponse:     cfg.SendBackRawResponse,
		StoreRawRequestResponse: cfg.StoreRawRequestResponse,
		ProxyConfig:             cfg.ProxyConfig,
		CustomProviderConfig:    cfg.CustomProviderConfig,
		OpenAIConfig:            cfg.OpenAIConfig,
	}
	if cfg.NetworkConfig != nil {
		pc.NetworkConfig = *cfg.NetworkConfig
	} else {
		pc.NetworkConfig = schemas.DefaultNetworkConfig
	}
	if cfg.ConcurrencyAndBufferSize != nil {
		pc.ConcurrencyAndBufferSize = *cfg.ConcurrencyAndBufferSize
	} else {
		pc.ConcurrencyAndBufferSize = schemas.DefaultConcurrencyAndBufferSize
	}
	return pc
}

// BifrostFor returns the *bifrost.Bifrost that should serve this
// request, plus a release function the caller MUST defer to allow
// eventual eviction of the underlying runtime.
//
// Single-tenant fallback: when the request carries no
// `x-f5xc-tenant` header (or the header is "default", or the Manager
// isn't initialized) the root s.Client is returned with a no-op
// release. Single-tenant deployments thus pay zero overhead and
// behave exactly as upstream OSS.
//
// Multi-tenant path: Manager.Acquire returns a Handle whose
// .Bifrost() is the per-tenant runtime. The release function decrements
// the runtime's refcount; the runtime is eligible for eviction once
// refcount reaches zero (LRU policy + explicit Evict on admin writes).
//
// On Acquire failure (loader error, manager closed, etc.) we fall
// back to the root s.Client and log — better to serve the request
// from the shared runtime than to 5xx the user; the data-scope
// callback still keeps row visibility correct, only the runtime is
// shared. The log lets operators spot loader regressions.
func (s *BifrostHTTPServer) BifrostFor(ctx context.Context, tid string) (*bifrost.Bifrost, func()) {
	noopRelease := func() {}
	if tid == "" || tid == "default" || s.Manager == nil {
		return s.Client, noopRelease
	}
	handle, err := s.Manager.Acquire(ctx, multitenant.TenantID(tid))
	if err != nil {
		logger.Warn("multitenant: Acquire failed for tenant %q (%v); falling back to root runtime — data scope still applies but inference workers are shared", tid, err)
		return s.Client, noopRelease
	}
	return handle.Bifrost(), handle.Release
}

// EvictTenantRuntime is the public hook admin handlers call after a
// successful write that changes a tenant's runtime config (provider
// add/update/delete, key add/update/delete, MCP client mutation,
// etc.). The next inference for that tenant will re-Acquire and the
// loader will re-snapshot fresh config from the DB.
//
// Safe to call when Manager is nil (single-tenant deployments) — just
// a no-op. Safe to call with an empty tid (no header present on the
// admin request — defensive only; the admin path SHOULD always carry
// one in production).
func (s *BifrostHTTPServer) EvictTenantRuntime(tid string) {
	if s.Manager == nil || tid == "" {
		return
	}
	if evicted := s.Manager.Evict(multitenant.TenantID(tid)); evicted {
		logger.Info("multitenant: evicted runtime for tenant %q on admin write", tid)
	}
}
