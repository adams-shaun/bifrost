package lib

import (
	"os"
	"strings"

	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

// AdminAuthz resolves the calling identity for admin HTTP endpoints into
// (caller_tenant_id, is_platform_admin). It is a deliberately small interface
// so v1 can ship a stub and Phase 4's real AuthzPlugin (OIDC-backed) can
// swap in without touching every admin handler.
//
// Semantics:
//   - callerTenant: the tenant the caller belongs to. A tenant-admin's
//     mutations to /api/<entity> apply to this tenant only. Empty string is
//     only valid when isPlatformAdmin is true AND the route reads the target
//     tenant from a path param (e.g. /api/tenants/{tenant_id}/...).
//   - isPlatformAdmin: true if the caller is allowed to invoke
//     /api/platform/* and /api/tenants/{tenant_id}/* with any tenant_id.
//   - err: any error returned causes the calling middleware to write a 401.
//     v1 stubs never error.
type AdminAuthz interface {
	ResolveCaller(ctx *fasthttp.RequestCtx) (callerTenant multitenant.TenantID, isPlatformAdmin bool, err error)
}

// StubPlatformAdminAuthz is the v1 stub: every admin caller is treated as a
// platform-admin whose own tenant is FallbackTenant. This preserves the
// current single-tenant operator experience while letting us ship the
// tenant-scoped routing now. Phase 4 replaces this with an OIDC-backed
// AdminAuthz that reads the caller's identity claims.
//
// FallbackTenant defaults to multitenant.DefaultTenantID; override only for
// tests that want to assert tenant-admin behavior.
type StubPlatformAdminAuthz struct {
	FallbackTenant multitenant.TenantID
}

// NewStubPlatformAdminAuthz returns a StubPlatformAdminAuthz with the
// fallback tenant resolved from the BIFROST_ADMIN_DEFAULT_TENANT env var,
// falling back to multitenant.DefaultTenantID. The env var lets a single
// process serve as a fixed tenant-admin in single-tenant deployments
// (e.g. for CI fixtures or local-dev runs).
func NewStubPlatformAdminAuthz() *StubPlatformAdminAuthz {
	tid := multitenant.TenantID(strings.TrimSpace(os.Getenv("BIFROST_ADMIN_DEFAULT_TENANT")))
	if tid == "" {
		tid = multitenant.DefaultTenantID
	}
	return &StubPlatformAdminAuthz{FallbackTenant: tid}
}

func (s *StubPlatformAdminAuthz) ResolveCaller(_ *fasthttp.RequestCtx) (multitenant.TenantID, bool, error) {
	tid := s.FallbackTenant
	if tid == "" {
		tid = multitenant.DefaultTenantID
	}
	return tid, true, nil
}
