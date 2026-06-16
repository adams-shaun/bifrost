package handlers

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// RequireTenantPathMiddleware reads the {tenant_id} fasthttp URL param,
// authorizes via the supplied lib.AdminAuthz, and lifts the tenant id onto
// the request context under multitenant.BifrostContextKeyTenantID so all
// downstream handlers / repo calls scope to that tenant.
//
// Policy:
//   - {tenant_id} must be present and non-empty -> 400 otherwise.
//   - AdminAuthz failure -> 401.
//   - !isPlatformAdmin AND {tenant_id} != callerTenant -> 403.
//   - Otherwise: set ctx user-value BifrostContextKeyTenantID = {tenant_id}
//     and call next. Platform-admins targeting their own tenant fall through
//     too — the policy is "platform-admin can target any tenant" not "only
//     other tenants".
//
// Mounted on all /api/tenants/{tenant_id}/... routes.
func RequireTenantPathMiddleware(authz lib.AdminAuthz) schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			tidVal, _ := ctx.UserValue("tenant_id").(string)
			tid := multitenant.TenantID(strings.TrimSpace(tidVal))
			if tid == "" {
				SendError(ctx, fasthttp.StatusBadRequest, "tenant_id is required")
				return
			}

			caller, isPlatformAdmin, err := authz.ResolveCaller(ctx)
			if err != nil {
				SendError(ctx, fasthttp.StatusUnauthorized, "admin authz failed: "+err.Error())
				return
			}
			if !isPlatformAdmin && caller != tid {
				SendError(ctx, fasthttp.StatusForbidden, "tenant-admin cannot target a different tenant")
				return
			}

			ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), string(tid))
			next(ctx)
		}
	}
}

// LegacyAdminTenantScopeMiddleware resolves the caller via AdminAuthz and
// lifts the caller's own tenant id onto the request context. Mount this on
// the existing /api/<entity> admin routes so tenant-admins see only their
// own data even though the URL has no {tenant_id} param.
//
// Platform-admins also get scoped to their own (FallbackTenant) tenant under
// this middleware — they should use /api/tenants/{tenant_id}/... when they
// need to act on a specific tenant.
func LegacyAdminTenantScopeMiddleware(authz lib.AdminAuthz) schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			caller, _, err := authz.ResolveCaller(ctx)
			if err != nil {
				SendError(ctx, fasthttp.StatusUnauthorized, "admin authz failed: "+err.Error())
				return
			}
			if caller == "" {
				caller = multitenant.DefaultTenantID
			}
			ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), string(caller))
			next(ctx)
		}
	}
}

// RequirePlatformAdminMiddleware rejects any caller that isn't a platform
// admin. Mount on /api/platform/* routes (cross-tenant admin: tenants CRUD,
// global feature flags, audit log, etc.).
func RequirePlatformAdminMiddleware(authz lib.AdminAuthz) schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			_, isPlatformAdmin, err := authz.ResolveCaller(ctx)
			if err != nil {
				SendError(ctx, fasthttp.StatusUnauthorized, "admin authz failed: "+err.Error())
				return
			}
			if !isPlatformAdmin {
				SendError(ctx, fasthttp.StatusForbidden, "platform-admin only")
				return
			}
			next(ctx)
		}
	}
}
