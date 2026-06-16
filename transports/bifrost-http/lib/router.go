package lib

import (
	"context"

	bifrost "github.com/maximhq/bifrost/core"
)

// BifrostRouter resolves a per-request *bifrost.Bifrost to use for an
// inbound inference call.
//
// In OSS / single-tenant deployments there's exactly one Bifrost runtime
// per process; Acquire is a trivial pass-through. In multi-tenant
// deployments Acquire reads the resolved tenant id off the context and
// returns the per-tenant runtime, pinning it via refcount until the
// returned release function is invoked.
//
// Calling code must ALWAYS pair Acquire with a release(), typically via
// defer — failing to release will leak refcount holds and prevent
// the multi-tenant Manager from evicting idle tenants.
//
// The contract is intentionally minimal — one method, returning the
// runtime and the release. Anything richer (per-method routing,
// streaming-friendly release semantics) is layered on top inside the
// router implementations rather than the interface.
type BifrostRouter interface {
	// Acquire returns the *bifrost.Bifrost to use for this request and a
	// release function that the caller MUST invoke (via defer) when done.
	//
	// The release semantics differ per implementation. SingleTenantRouter
	// returns a noop release. MultiTenantRouter decrements the per-tenant
	// runtime refcount so the LRU can evict.
	//
	// Errors from Acquire are terminal for the request: the handler
	// should reply with 5xx without invoking the runtime. The OSS path
	// never returns an error; the multi-tenant path may (DB outage,
	// tenant suspended, etc.).
	Acquire(ctx context.Context) (*bifrost.Bifrost, func(), error)
}

// SingleTenantRouter is the trivial BifrostRouter used by OSS / single-
// tenant deployments. It always returns the same Bifrost runtime and a
// noop release.
type SingleTenantRouter struct {
	client *bifrost.Bifrost
}

// NewSingleTenantRouter returns a router that always serves the given
// Bifrost runtime. Passing nil panics — every deployment has at least
// one Bifrost.
func NewSingleTenantRouter(client *bifrost.Bifrost) *SingleTenantRouter {
	if client == nil {
		panic("lib: nil *bifrost.Bifrost passed to NewSingleTenantRouter")
	}
	return &SingleTenantRouter{client: client}
}

// Acquire implements BifrostRouter.
func (r *SingleTenantRouter) Acquire(_ context.Context) (*bifrost.Bifrost, func(), error) {
	return r.client, noopRelease, nil
}

// SetClient swaps the underlying runtime. Used at hot-reload time when
// the Bifrost runtime is rebuilt (e.g. provider config changes); not
// safe to call concurrently with Acquire, so callers must coordinate
// via the existing reload locks.
func (r *SingleTenantRouter) SetClient(client *bifrost.Bifrost) {
	if client == nil {
		panic("lib: nil *bifrost.Bifrost passed to SingleTenantRouter.SetClient")
	}
	r.client = client
}

// Client returns the current underlying runtime. Used by code paths
// that need the raw runtime for non-routed operations (Shutdown,
// ReloadConfig). Should NOT be used as a back-door around Acquire in
// the request path.
func (r *SingleTenantRouter) Client() *bifrost.Bifrost { return r.client }

func noopRelease() {}
