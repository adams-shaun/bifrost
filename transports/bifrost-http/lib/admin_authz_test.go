package lib

import (
	"net"
	"testing"

	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

func newAuthzCtx(t *testing.T) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	req.SetRequestURI("/api/providers")
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	return ctx
}

func TestStubPlatformAdminAuthz_DefaultFallback(t *testing.T) {
	authz := &StubPlatformAdminAuthz{}
	tid, isPlatformAdmin, err := authz.ResolveCaller(newAuthzCtx(t))
	if err != nil {
		t.Fatalf("stub must never error; got %v", err)
	}
	if !isPlatformAdmin {
		t.Fatal("stub must always say platform-admin")
	}
	if tid != multitenant.DefaultTenantID {
		t.Fatalf("empty FallbackTenant should resolve to DefaultTenantID; got %q", tid)
	}
}

func TestStubPlatformAdminAuthz_CustomFallback(t *testing.T) {
	authz := &StubPlatformAdminAuthz{FallbackTenant: "acme"}
	tid, _, _ := authz.ResolveCaller(newAuthzCtx(t))
	if tid != "acme" {
		t.Fatalf("custom FallbackTenant should win; got %q", tid)
	}
}

func TestNewStubPlatformAdminAuthz_DefaultsToDefault(t *testing.T) {
	t.Setenv("BIFROST_ADMIN_DEFAULT_TENANT", "")
	authz := NewStubPlatformAdminAuthz()
	if authz.FallbackTenant != multitenant.DefaultTenantID {
		t.Fatalf("unset env should default to DefaultTenantID; got %q", authz.FallbackTenant)
	}
}

func TestNewStubPlatformAdminAuthz_RespectsEnvVar(t *testing.T) {
	t.Setenv("BIFROST_ADMIN_DEFAULT_TENANT", "  acme  ")
	authz := NewStubPlatformAdminAuthz()
	if authz.FallbackTenant != "acme" {
		t.Fatalf("env var should be trimmed and used; got %q", authz.FallbackTenant)
	}
}
