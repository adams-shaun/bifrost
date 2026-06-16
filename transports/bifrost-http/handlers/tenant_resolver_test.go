package handlers

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

// stubVKResolver is a multitenant.VKResolver that returns canned answers
// keyed by exact VK match.
type stubVKResolver struct {
	mapping map[string]multitenant.TenantID
	err     error
	calls   int
}

func (s *stubVKResolver) ResolveVK(_ context.Context, vk string) (multitenant.TenantID, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	tid, ok := s.mapping[vk]
	if !ok {
		return "", multitenant.ErrUnknownVK
	}
	return tid, nil
}

// newResolverTestCtx builds a properly-initialized RequestCtx with the given
// path + optional VK header. Path defaults to /api/inference if empty.
func newResolverTestCtx(t *testing.T, path string, hdr map[string]string) *fasthttp.RequestCtx {
	t.Helper()
	var req fasthttp.Request
	if path == "" {
		path = "/api/inference/chat"
	}
	req.SetRequestURI(path)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&req, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, nil)
	return ctx
}

// nextHandler is a trivial fasthttp handler that records whether it was
// called and exposes the ctx so tests can inspect UserValues after the
// middleware ran.
type nextHandler struct {
	called bool
}

func (n *nextHandler) handle(ctx *fasthttp.RequestCtx) { n.called = true }

func TestTenantResolverMiddleware_HappyPath(t *testing.T) {
	resolver := &stubVKResolver{mapping: map[string]multitenant.TenantID{
		"sk-bf-acme": "acme",
	}}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	ctx := newResolverTestCtx(t, "", map[string]string{"x-bf-vk": "sk-bf-acme"})
	mw(ctx)

	if !nh.called {
		t.Fatal("next handler should have been called")
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver should be called exactly once, got %d", resolver.calls)
	}
	tid, _ := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)).(string)
	if tid != "acme" {
		t.Fatalf("tenant id should be set on ctx; got %q", tid)
	}
}

func TestTenantResolverMiddleware_ReadsBearer(t *testing.T) {
	resolver := &stubVKResolver{mapping: map[string]multitenant.TenantID{
		"sk-bf-acme": "acme",
	}}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	ctx := newResolverTestCtx(t, "", map[string]string{"Authorization": "Bearer sk-bf-acme"})
	mw(ctx)

	tid, _ := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)).(string)
	if tid != "acme" {
		t.Fatalf("Bearer-token VK should be picked up; got tid=%q", tid)
	}
}

func TestTenantResolverMiddleware_BearerNonVKIgnored(t *testing.T) {
	resolver := &stubVKResolver{}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	// Authorization without the sk-bf- prefix is some other auth scheme,
	// not a virtual key. Don't even call the resolver.
	ctx := newResolverTestCtx(t, "", map[string]string{"Authorization": "Bearer ya29-google-token"})
	mw(ctx)

	if resolver.calls != 0 {
		t.Fatalf("non-VK Authorization should not trigger resolver, got %d calls", resolver.calls)
	}
	if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		t.Fatalf("no tenant_id should be set; got %v", v)
	}
}

func TestTenantResolverMiddleware_MissingVKPassesThrough(t *testing.T) {
	resolver := &stubVKResolver{}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	ctx := newResolverTestCtx(t, "", nil)
	mw(ctx)

	if !nh.called {
		t.Fatal("next handler should still run when VK is absent")
	}
	if resolver.calls != 0 {
		t.Fatal("resolver should not be called when no VK is present")
	}
	if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		t.Fatalf("no tenant_id should be set; got %v", v)
	}
}

func TestTenantResolverMiddleware_UnknownVKPassesThrough(t *testing.T) {
	resolver := &stubVKResolver{mapping: map[string]multitenant.TenantID{}}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	ctx := newResolverTestCtx(t, "", map[string]string{"x-bf-vk": "sk-bf-bogus"})
	mw(ctx)

	if !nh.called {
		t.Fatal("next handler should still run on unknown VK; governance owns the 401")
	}
	if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		t.Fatalf("no tenant_id should be set on unknown VK; got %v", v)
	}
}

func TestTenantResolverMiddleware_ResolverErrorPassesThrough(t *testing.T) {
	resolver := &stubVKResolver{err: errors.New("db borked")}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	ctx := newResolverTestCtx(t, "", map[string]string{"x-bf-vk": "sk-bf-anything"})
	mw(ctx)

	if !nh.called {
		t.Fatal("DB outage should not block the request — fail open at this layer")
	}
	if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		t.Fatalf("no tenant_id should be set on resolver error; got %v", v)
	}
}

func TestTenantResolverMiddleware_SkipsPlatformAndHealth(t *testing.T) {
	resolver := &stubVKResolver{mapping: map[string]multitenant.TenantID{
		"sk-bf-acme": "acme",
	}}
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(resolver)(nh.handle)

	for _, path := range []string{"/health", "/api/platform/tenants", "/api/platform/tenants/acme"} {
		ctx := newResolverTestCtx(t, path, map[string]string{"x-bf-vk": "sk-bf-acme"})
		mw(ctx)
		if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
			t.Fatalf("path %s should be skipped; resolved tenant=%v", path, v)
		}
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver should not be called on skip paths, got %d calls", resolver.calls)
	}
}

func TestTenantResolverMiddleware_NilResolverIsNoop(t *testing.T) {
	nh := &nextHandler{}
	mw := TenantResolverMiddleware(nil)(nh.handle)

	ctx := newResolverTestCtx(t, "", map[string]string{"x-bf-vk": "sk-bf-acme"})
	mw(ctx)

	if !nh.called {
		t.Fatal("nil resolver should pass through")
	}
	if v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		t.Fatalf("no tenant_id should be set when resolver is nil; got %v", v)
	}
}
