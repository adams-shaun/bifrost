// Package handlers — tenant_resolver.go.
//
// The middleware that turns the `x-f5xc-tenant` request header into a
// fasthttp UserValue (for downstream code that pokes at the request
// context) and a BifrostContext value (for plugins that flow through
// the inference path).  Mount it ONCE near the front of the chain;
// every handler / configstore method / plugin hook downstream then
// sees the same tenant id without further plumbing.
//
// The contract is intentionally narrow: header present → ctx populated,
// header absent → ctx left alone (the configstore tenant-scope
// callback treats that as "single-tenant N=1, no implicit filter").
// We DO NOT reject requests without the header here — that's a policy
// decision the surrounding auth middleware can make if it wants.
package handlers

import (
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TenantResolverMiddleware extracts the `x-f5xc-tenant` header and
// stashes the value under multitenant.BifrostContextKeyTenantID
// (twice — once as a string-keyed fasthttp UserValue so configstore
// callbacks reading from gorm's stmt.Context.Value(...) see it, and
// once on the BifrostContext if one is already attached so inference
// plugins see it).
//
// Skip paths (health probes) bypass the lookup entirely.
func TenantResolverMiddleware() schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			path := string(ctx.Path())
			if shouldSkipTenantResolve(path) {
				next(ctx)
				return
			}
			raw := ctx.Request.Header.Peek(multitenant.HeaderName)
			if len(raw) == 0 {
				next(ctx)
				return
			}
			tid := string(raw)
			// Both forms — fasthttp UserValue (read by configstore
			// callback via gorm stmt.Context which wraps the
			// fasthttp.RequestCtx) and any pre-attached
			// BifrostContext (read by inference plugins).
			ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), tid)
			if bfCtxIface := ctx.UserValue(lib.FastHTTPUserValueBifrostContext); bfCtxIface != nil {
				if bfCtx, ok := bfCtxIface.(*schemas.BifrostContext); ok && bfCtx != nil {
					bfCtx.SetValue(multitenant.BifrostContextKeyTenantID, tid)
				}
			}
			next(ctx)
		}
	}
}

// shouldSkipTenantResolve returns true for paths that have no per-
// tenant dispatch and so don't benefit from a header read.  Keep this
// list short — every entry is a per-request header peek we avoid.
func shouldSkipTenantResolve(path string) bool {
	switch path {
	case "/health", "/api/version", "/favicon.ico":
		return true
	}
	return false
}
