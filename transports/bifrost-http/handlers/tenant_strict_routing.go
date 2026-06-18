// Package handlers — tenant_strict_routing.go.
//
// f5xc-overlay (patch17): inference-path strictness gate.
//
// Without this middleware, an inference request with no `x-f5xc-tenant`
// header falls through to the root runtime, and a header carrying an
// id that doesn't resolve to a configured tenant ALSO falls through
// (because Manager.Acquire's loader returns an empty-but-valid
// BifrostConfig for unknown tids — see server/multitenant.go's
// newTenantLoader). The fall-through is correct for OSS single-tenant
// callflows but lets multi-tenant deployments quietly answer requests
// against the wrong runtime when callers mis-type the header or forget
// it entirely.
//
// When the BIFROST_STRICT_TENANT_ROUTING env var is set, this
// middleware fast-fails inference requests under either condition:
//
//   - header absent           → 401 (with a clear "header required")
//   - header set but unknown  → 404 ("tenant not found")
//
// Mount AFTER TenantResolverMiddleware (which stamps the header value
// on ctx) and BEFORE TenantDispatcherMiddleware (which routes to the
// per-tenant runtime). Mount ONLY on the inference chain — admin
// chains use a different mounting at server.go::RegisterAPIRoutes
// and intentionally don't gate on tenant presence (the platform
// /api/tenants/* admin surface needs to operate without a header to
// bootstrap the first non-default tenant).
//
// When the env var is unset (the default), the middleware is a no-op
// — every callflow that worked yesterday still works today.
package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

// TenantValidator answers "does this tenant id exist?" The interface
// (vs. a concrete configstore handle) keeps the middleware testable
// without spinning a sqlite DB in tests.
//
// Implementations MUST be safe for concurrent use.
type TenantValidator interface {
	// TenantExists returns true when tid resolves to a configured
	// tenant. Returns (false, nil) for "doesn't exist". A non-nil
	// error is a lookup failure (DB unreachable, etc.); the middleware
	// treats it as 503 rather than 404 so operators can distinguish.
	TenantExists(ctx context.Context, tid string) (bool, error)
}

// ConfigStoreTenantValidator adapts a configstore.ConfigStore (which
// has DB access) into a TenantValidator. The lookup is a plain
// `SELECT 1 FROM tenants WHERE id = ?` via GORM. Cheap; no caching
// at this layer — if production traffic ever shows the lookup as a
// hot spot, layer a sync.Map cache invalidated by the tenant CREATE/
// DELETE handlers (platform_tenants.go).
type ConfigStoreTenantValidator struct {
	Store configstore.ConfigStore
}

// TenantExists checks the tenants table directly.
func (v *ConfigStoreTenantValidator) TenantExists(ctx context.Context, tid string) (bool, error) {
	// The default tenant always exists (seeded). Short-circuit FIRST so
	// a request landing on /v1/* with x-f5xc-tenant: default skips the
	// DB hit entirely — and so the short-circuit still works in test
	// harnesses that wire a nil configstore.
	if tid == string(multitenant.DefaultTenantID) {
		return true, nil
	}
	if v == nil || v.Store == nil {
		return false, fmt.Errorf("nil configstore")
	}
	var row configstoreTables.TableTenant
	err := v.Store.DB().WithContext(ctx).Select("id").Where("id = ?", tid).First(&row).Error
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// TenantStrictRoutingMiddleware enforces tenant-header strictness on
// the inference chain when strictMode is true. When strictMode is
// false the middleware is a no-op (zero overhead per request) so OSS
// single-tenant deployments are unaffected.
//
// The validator MUST be non-nil when strictMode is true. Pass
// (nil, false) to disable; pass a real validator + true to enforce.
func TenantStrictRoutingMiddleware(validator TenantValidator, strictMode bool) schemas.BifrostHTTPMiddleware {
	if !strictMode {
		return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
			return next
		}
	}
	if validator == nil {
		// Defensive: strictMode without a validator would silently
		// reject every request. Fail loud at construction.
		panic("handlers: TenantStrictRoutingMiddleware: strictMode=true requires a non-nil validator")
	}
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			tid := TenantIDFromCtx(ctx)
			if tid == "" {
				SendError(ctx, fasthttp.StatusUnauthorized,
					"tenant header required (BIFROST_STRICT_TENANT_ROUTING is enabled): set x-f5xc-tenant on the request")
				return
			}
			ok, err := validator.TenantExists(ctx, tid)
			if err != nil {
				SendError(ctx, fasthttp.StatusServiceUnavailable,
					fmt.Sprintf("tenant lookup failed: %v", err))
				return
			}
			if !ok {
				SendError(ctx, fasthttp.StatusNotFound,
					fmt.Sprintf("tenant %q not found; set x-f5xc-tenant to a configured tenant id (see GET /api/tenants)", tid))
				return
			}
			next(ctx)
		}
	}
}
