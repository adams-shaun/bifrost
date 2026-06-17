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
	"strings"

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
// When disableDefaultTenant is true, any request whose tenant header
// resolves to "default" gets rejected with 403 before any handler
// runs. Operators opt into this when their cluster is meant to be
// multi-tenant only and the seeded `default` tenant should not be
// usable as a write target. /v1/* inference paths still flow through
// even with the flag on — the resolver's job is admin-surface
// isolation; inference-time policy lives in the governance plugin.
//
// Skip paths (health probes) bypass the lookup entirely.
func TenantResolverMiddleware(disableDefaultTenant bool) schemas.BifrostHTTPMiddleware {
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
			if disableDefaultTenant && tid == string(multitenant.DefaultTenantID) && isAdminPath(path) && isUnsafeMethod(string(ctx.Method())) {
				SendError(ctx, fasthttp.StatusForbidden,
					"the 'default' tenant is disabled on this cluster (BIFROST_DISABLE_DEFAULT_TENANT_CONFIG=true); pick a different tenant via the x-f5xc-tenant header")
				return
			}
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

// isUnsafeMethod returns true for HTTP methods that mutate server
// state. GET / HEAD / OPTIONS are read-only and never count as writes
// so the UI can render bootstrap reads (e.g. GET /api/config) from
// the default tenant even when the disable flag is on; only POST /
// PUT / PATCH / DELETE are gated by the flag.
func isUnsafeMethod(method string) bool {
	switch method {
	case fasthttp.MethodPost, fasthttp.MethodPut, fasthttp.MethodPatch, fasthttp.MethodDelete:
		return true
	}
	return false
}

// isAdminPath returns true for the per-tenant admin surfaces where a
// "default tenant write" is in scope of the disable flag.
//
// /api/tenants* and /api/platform/* are intentionally NOT tenant-
// scoped surfaces — the tenants table itself has no tenant_id column,
// and the only way for an operator sitting on the seeded `default`
// tenant to provision their first real tenant is to POST to one of
// those endpoints. If the resolver blocked that based on the caller's
// header, the flag would be a chicken-and-egg trap: you can't get off
// `default` without already being off `default`. (Both prefixes are
// exempt — `/api/tenants` is the canonical surface; `/api/platform/*`
// is the legacy alias retained until UI clients have migrated.)
//
// Inference (/v1/*) is also excluded — governance owns admit/deny
// there. So the flag only fires on /api/<entity>/... (providers,
// virtual-keys, mcp/clients, config, etc.).
func isAdminPath(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	if strings.HasPrefix(path, "/api/platform/") {
		return false
	}
	if path == "/api/tenants" || strings.HasPrefix(path, "/api/tenants/") {
		return false
	}
	return true
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

// TenantIDFromCtx is the read side of the resolver — handlers use it to
// stamp `TenantID:` on every tenant-scoped struct they construct,
// belt-and-braces with the GORM tenant-scope callback in framework/
// configstore. Returns the tenant id stashed by TenantResolverMiddleware,
// or "" when the request didn't carry the `x-f5xc-tenant` header.
//
// Why handlers stamp explicitly when the GORM callback already
// stamps from ctx: the callback's reflection path failed silently in
// production (the `default:default` GORM tag pre-fills "default" and
// the callback respected it). Explicit stamping at construction is
// the reliable source of truth; the callback is the safety net for
// any future code path that forgets.
func TenantIDFromCtx(ctx *fasthttp.RequestCtx) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
