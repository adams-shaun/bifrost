// f5xc-overlay (patch17): tests for the inference-path tenant
// strictness middleware. Covers the four behaviour quadrants in the
// truth table:
//
//                 strict=false | strict=true
//   header absent       pass   |   401
//   header unknown      pass   |   404
//   header known        pass   |   pass
//   header "default"    pass   |   pass (short-circuit, no DB hit)
//
// The validator is a small in-memory mock so tests don't need a sqlite
// DB.

package handlers

import (
	"context"
	"net"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/valyala/fasthttp"
)

// mockTenantValidator is the test double for TenantValidator.
type mockTenantValidator struct {
	known   map[string]bool
	calls   int
	failErr error
}

func (m *mockTenantValidator) TenantExists(_ context.Context, tid string) (bool, error) {
	m.calls++
	if m.failErr != nil {
		return false, m.failErr
	}
	return m.known[tid], nil
}

// runMiddleware mounts the middleware in front of a "200 OK" handler
// and returns the resulting status + next-was-called flag.
func runMiddleware(t *testing.T, mw schemas.BifrostHTTPMiddleware, headerVal string) (int, bool) {
	t.Helper()
	nextCalled := false
	next := func(ctx *fasthttp.RequestCtx) {
		nextCalled = true
		ctx.SetStatusCode(fasthttp.StatusOK)
	}
	wrapped := mw(next)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI("/v1/chat/completions")
	if headerVal != "" {
		// Stamp the ctx the way TenantResolverMiddleware does (UserValue —
		// TenantIDFromCtx reads from here). This isolates the strict
		// middleware from the resolver middleware's parsing details.
		ctx.SetUserValue(string(multitenant.BifrostContextKeyTenantID), headerVal)
	}
	wrapped(ctx)
	return ctx.Response.StatusCode(), nextCalled
}

func TestTenantStrictRouting_Disabled_IsNoOp(t *testing.T) {
	// strict=false → middleware MUST be a transparent pass-through
	// regardless of header presence. Validates the OSS-callflow
	// preservation promise in the file header.
	mw := TenantStrictRoutingMiddleware(nil, false)

	for _, tid := range []string{"", "unknown-tenant", "acme"} {
		status, called := runMiddleware(t, mw, tid)
		if !called {
			t.Fatalf("strict=false, tid=%q: next not invoked (got status=%d)", tid, status)
		}
		if status != fasthttp.StatusOK {
			t.Fatalf("strict=false, tid=%q: want 200, got %d", tid, status)
		}
	}
}

func TestTenantStrictRouting_HeaderAbsent_Returns401(t *testing.T) {
	mw := TenantStrictRoutingMiddleware(&mockTenantValidator{}, true)
	status, called := runMiddleware(t, mw, "")
	if called {
		t.Fatal("next should not be invoked when header is absent in strict mode")
	}
	if status != fasthttp.StatusUnauthorized {
		t.Fatalf("want 401 when header absent in strict mode, got %d", status)
	}
}

func TestTenantStrictRouting_HeaderUnknown_Returns404(t *testing.T) {
	v := &mockTenantValidator{known: map[string]bool{"acme": true}}
	mw := TenantStrictRoutingMiddleware(v, true)
	status, called := runMiddleware(t, mw, "globex")
	if called {
		t.Fatal("next should not be invoked for unknown tenant in strict mode")
	}
	if status != fasthttp.StatusNotFound {
		t.Fatalf("want 404 for unknown tenant in strict mode, got %d", status)
	}
	if v.calls != 1 {
		t.Fatalf("validator should be hit exactly once, got %d calls", v.calls)
	}
}

func TestTenantStrictRouting_HeaderKnown_PassesThrough(t *testing.T) {
	v := &mockTenantValidator{known: map[string]bool{"acme": true}}
	mw := TenantStrictRoutingMiddleware(v, true)
	status, called := runMiddleware(t, mw, "acme")
	if !called {
		t.Fatal("next should be invoked for known tenant in strict mode")
	}
	if status != fasthttp.StatusOK {
		t.Fatalf("want 200 for known tenant, got %d", status)
	}
}

func TestTenantStrictRouting_DefaultTenant_ShortCircuits(t *testing.T) {
	// The 'default' tenant is always-exists in MT deployments (seeded by
	// patch1). The ConfigStoreTenantValidator short-circuits the DB hit
	// for tid == default. This test uses the real validator wired with a
	// nil store to prove the short-circuit happens BEFORE the store is
	// touched (any DB access would crash on the nil store).
	v := &ConfigStoreTenantValidator{Store: nil}
	mw := TenantStrictRoutingMiddleware(v, true)
	status, called := runMiddleware(t, mw, string(multitenant.DefaultTenantID))
	if !called {
		t.Fatal("default tenant should pass through (short-circuit)")
	}
	if status != fasthttp.StatusOK {
		t.Fatalf("want 200 for default tenant short-circuit, got %d", status)
	}
}

func TestTenantStrictRouting_ValidatorError_Returns503(t *testing.T) {
	// A lookup failure (DB unreachable, etc.) should be distinguishable
	// from a "tenant not found" — operators reading the log want to
	// know which one to chase. 503 vs 404 carries that signal.
	v := &mockTenantValidator{failErr: &netDownErr{}}
	mw := TenantStrictRoutingMiddleware(v, true)
	status, called := runMiddleware(t, mw, "acme")
	if called {
		t.Fatal("next should not be invoked when validator errors")
	}
	if status != fasthttp.StatusServiceUnavailable {
		t.Fatalf("want 503 when validator errors, got %d", status)
	}
}

func TestTenantStrictRouting_StrictWithoutValidator_Panics(t *testing.T) {
	// strict=true with a nil validator would silently reject every
	// request. Construction MUST panic so the misconfiguration is
	// caught at boot rather than at first inference.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on strict=true with nil validator")
		}
	}()
	_ = TenantStrictRoutingMiddleware(nil, true)
}

// netDownErr is a synthetic error used by the validator-error test.
type netDownErr struct{}

func (netDownErr) Error() string   { return "simulated DB unreachable" }
func (netDownErr) Timeout() bool   { return false }
func (netDownErr) Temporary() bool { return true }

// Sanity: netDownErr satisfies net.Error so it would behave the same
// in production where the configstore wraps gorm errors.
var _ net.Error = netDownErr{}
