// Package handlers — tenant_dispatcher.go.
//
// Stage 2 runtime-isolation middleware: turns the `x-f5xc-tenant`
// header (stamped on ctx by TenantResolverMiddleware) into a per-
// request *bifrost.Bifrost. Inference handlers read that runtime
// reference off ctx via lib.BifrostClientFromCtx — they don't need
// to know whether they're hitting the shared root runtime (single-
// tenant fallback) or a per-tenant runtime owned by the Manager.
//
// Without this middleware mounted, every inference handler still
// works (it falls back to its constructor-time h.client), but
// per-tenant runtime isolation is lost — every tenant shares the
// same provider workers + queues + account snapshot. Data scope
// (the GORM tenant callback) still keeps row visibility correct.

package handlers

import (
	"context"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// BifrostDispatcher abstracts the per-tenant runtime registry the
// middleware looks up against. Implemented by *server.BifrostHTTPServer
// in the production wiring (see server/multitenant.go BifrostFor).
// Lives as an interface here so the handlers package doesn't import
// server (which would cycle).
type BifrostDispatcher interface {
	// BifrostFor returns the *bifrost.Bifrost that should serve a
	// request from the given tenant, plus a release function the
	// caller must defer to allow eventual eviction. For empty / default
	// tid (single-tenant fallback) the implementation returns the root
	// runtime and a no-op release.
	BifrostFor(ctx context.Context, tid string) (*bifrost.Bifrost, func())
}

// TenantEvict is the package-level eviction hook admin handlers call
// after a successful write that mutates a tenant's runtime config
// (provider / key / VK / MCP). Wired at boot from
// server/multitenant.go via *BifrostHTTPServer.EvictTenantRuntime.
// nil during single-tenant deployments (no Manager) — calls are
// no-ops via EvictTenant below.
//
// Global var rather than per-handler field because the eviction surface
// touches half a dozen handlers across providers / governance / mcp;
// threading an interface through every constructor would churn signatures
// for a single line of behavior. The dispatcher (BifrostDispatcher) is
// per-request and DOES belong on a struct — eviction is a side-effect.
var TenantEvict func(tid string)

// EvictTenant fires the configured eviction hook with the tenant id read
// off the request ctx. Safe to call when no manager is configured
// (TenantEvict nil) or when no tenant header was present (empty tid).
// Call AFTER a successful mutation — pre-success calls would evict
// state that's about to be re-loaded on the very next request,
// pointlessly thrashing the cache on a no-op admin call.
func EvictTenant(ctx *fasthttp.RequestCtx) {
	if TenantEvict == nil {
		return
	}
	tid := TenantIDFromCtx(ctx)
	if tid == "" {
		return
	}
	TenantEvict(tid)
}

// TenantDispatcherMiddleware wraps every request: it reads the tenant
// id off the resolver-stamped ctx, asks the dispatcher for the right
// *bifrost.Bifrost, stashes it on the fasthttp UserValue under
// FastHTTPUserValueBifrostClient (so handlers fetch via
// lib.BifrostClientFromCtx), and defers the release.
//
// MUST mount AFTER TenantResolverMiddleware in the chain — that's the
// middleware that puts the tenant id on ctx for us to read.
//
// Streaming-safe: fasthttp request handlers are synchronous within a
// single request, so `defer release` fires after the SSE / streamed
// response has fully drained back to the client.
func TenantDispatcherMiddleware(dispatcher BifrostDispatcher) schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			if dispatcher == nil {
				next(ctx)
				return
			}
			tid := TenantIDFromCtx(ctx)
			bf, release := dispatcher.BifrostFor(ctx, tid)
			defer release()
			if bf != nil {
				ctx.SetUserValue(lib.FastHTTPUserValueBifrostClient, bf)
			}
			next(ctx)
		}
	}
}
