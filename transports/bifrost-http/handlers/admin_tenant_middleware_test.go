package handlers

import (
	"errors"
	"net"
	"testing"

	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

// stubAdminAuthz is a test double for lib.AdminAuthz so this test file does
// not pull in the lib package's import cycle considerations.
type stubAdminAuthz struct {
	caller          multitenant.TenantID
	isPlatformAdmin bool
	err             error
}

func (s *stubAdminAuthz) ResolveCaller(_ *fasthttp.RequestCtx) (multitenant.TenantID, bool, error) {
	return s.caller, s.isPlatformAdmin, s.err
}

func newAdminCtx(t *testing.T, path string, tenantParam string) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	if path == "" {
		path = "/api/tenants/acme/providers"
	}
	req.SetRequestURI(path)
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	if tenantParam != "" {
		ctx.SetUserValue("tenant_id", tenantParam)
	}
	return ctx
}

func TestRequireTenantPathMiddleware_PlatformAdminAnyTenant(t *testing.T) {
	authz := &stubAdminAuthz{caller: "operator", isPlatformAdmin: true}
	nh := &nextHandler{}
	mw := RequireTenantPathMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "", "acme")
	mw(ctx)

	if !nh.called {
		t.Fatal("next should be called for platform-admin")
	}
	if ctx.Response.StatusCode() == fasthttp.StatusForbidden {
		t.Fatal("platform-admin must not get 403 targeting another tenant")
	}
	tid, _ := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)).(string)
	if tid != "acme" {
		t.Fatalf("tenant_id should be set to path param; got %q", tid)
	}
}

func TestRequireTenantPathMiddleware_TenantAdminOwnTenant(t *testing.T) {
	authz := &stubAdminAuthz{caller: "acme", isPlatformAdmin: false}
	nh := &nextHandler{}
	mw := RequireTenantPathMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "", "acme")
	mw(ctx)

	if !nh.called {
		t.Fatal("tenant-admin must be allowed on their own tenant path")
	}
	tid, _ := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)).(string)
	if tid != "acme" {
		t.Fatalf("tenant_id should be set; got %q", tid)
	}
}

func TestRequireTenantPathMiddleware_TenantAdminCrossTenant(t *testing.T) {
	authz := &stubAdminAuthz{caller: "acme", isPlatformAdmin: false}
	nh := &nextHandler{}
	mw := RequireTenantPathMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "", "globex")
	mw(ctx)

	if nh.called {
		t.Fatal("tenant-admin must NOT be allowed to act on another tenant")
	}
	if got, want := ctx.Response.StatusCode(), fasthttp.StatusForbidden; got != want {
		t.Fatalf("status got %d want %d", got, want)
	}
}

func TestRequireTenantPathMiddleware_MissingTenantID(t *testing.T) {
	authz := &stubAdminAuthz{caller: "acme", isPlatformAdmin: true}
	nh := &nextHandler{}
	mw := RequireTenantPathMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "", "")
	mw(ctx)

	if nh.called {
		t.Fatal("must reject when tenant_id is missing")
	}
	if got, want := ctx.Response.StatusCode(), fasthttp.StatusBadRequest; got != want {
		t.Fatalf("status got %d want %d", got, want)
	}
}

func TestRequireTenantPathMiddleware_AuthzError(t *testing.T) {
	authz := &stubAdminAuthz{err: errors.New("upstream IdP down")}
	nh := &nextHandler{}
	mw := RequireTenantPathMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "", "acme")
	mw(ctx)

	if nh.called {
		t.Fatal("authz error must short-circuit")
	}
	if got, want := ctx.Response.StatusCode(), fasthttp.StatusUnauthorized; got != want {
		t.Fatalf("status got %d want %d", got, want)
	}
}

func TestLegacyAdminTenantScopeMiddleware_LiftsCallerTenant(t *testing.T) {
	authz := &stubAdminAuthz{caller: "acme", isPlatformAdmin: false}
	nh := &nextHandler{}
	mw := LegacyAdminTenantScopeMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "/api/providers", "")
	mw(ctx)

	if !nh.called {
		t.Fatal("next should be called on legacy admin route")
	}
	tid, _ := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)).(string)
	if tid != "acme" {
		t.Fatalf("tenant_id should be lifted from caller; got %q", tid)
	}
}

func TestLegacyAdminTenantScopeMiddleware_FallbackToDefaultOnEmpty(t *testing.T) {
	authz := &stubAdminAuthz{caller: "", isPlatformAdmin: true}
	nh := &nextHandler{}
	mw := LegacyAdminTenantScopeMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "/api/providers", "")
	mw(ctx)

	tid, _ := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)).(string)
	if tid != string(multitenant.DefaultTenantID) {
		t.Fatalf("empty caller should fall back to DefaultTenantID; got %q", tid)
	}
}

func TestRequirePlatformAdminMiddleware_RejectsTenantAdmin(t *testing.T) {
	authz := &stubAdminAuthz{caller: "acme", isPlatformAdmin: false}
	nh := &nextHandler{}
	mw := RequirePlatformAdminMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "/api/platform/tenants", "")
	mw(ctx)

	if nh.called {
		t.Fatal("tenant-admin must not reach platform routes")
	}
	if got, want := ctx.Response.StatusCode(), fasthttp.StatusForbidden; got != want {
		t.Fatalf("status got %d want %d", got, want)
	}
}

func TestRequirePlatformAdminMiddleware_AllowsPlatformAdmin(t *testing.T) {
	authz := &stubAdminAuthz{caller: "operator", isPlatformAdmin: true}
	nh := &nextHandler{}
	mw := RequirePlatformAdminMiddleware(authz)(nh.handle)

	ctx := newAdminCtx(t, "/api/platform/tenants", "")
	mw(ctx)

	if !nh.called {
		t.Fatal("platform-admin must pass")
	}
}
