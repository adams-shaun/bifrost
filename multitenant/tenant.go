// Package multitenant provides a SaaS-style multi-tenant wrapper around the
// single-tenant core/bifrost runtime. Each tenant gets its own *bifrost.Bifrost
// instance (own Account, own plugins, own MCP manager, own request queues).
// Tenants are lazy-loaded on first request and tracked in a registry; an
// optional active-tenant cap drives LRU-style eviction with graceful drain.
//
// This is a spike: hot-path is intentionally simple, no clustering, no DB.
package multitenant

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
)

// TenantID uniquely identifies a tenant within the process. Opaque string so
// downstream code (DB IDs, ULIDs, UUIDs, slugs) can pick a representation.
type TenantID string

// DefaultTenantID is the tenant attributed to all pre-multi-tenant data and
// to callers in single-tenant deployments. It mirrors the constant of the
// same name in framework/configstore/tables to avoid pulling the configstore
// import into this package's call sites that just need the literal.
const DefaultTenantID TenantID = "default"

// BifrostContextKeyTenantID is the BifrostContext key under which the resolved
// tenant id is stored. The tenant-resolver middleware sets it after looking
// up the inbound virtual key; downstream plugins / the multi-tenant
// dispatcher read it via ctx.Value(BifrostContextKeyTenantID).
const BifrostContextKeyTenantID schemas.BifrostContextKey = "bf-tenant-id"

// TenantLoader is the user-supplied callback that produces a BifrostConfig for
// a given tenant. The loader is invoked exactly once per tenant runtime per
// process lifetime (or until the runtime is evicted and re-acquired).
//
// The returned BifrostConfig.Account is required; other fields are optional
// and follow the same semantics as bifrost.Init.
//
// The ctx passed to the loader is the Manager's root context; it is cancelled
// when the runtime is being evicted or the Manager is shutting down. Loaders
// that block on I/O should respect cancellation.
type TenantLoader func(ctx context.Context, tid TenantID) (schemas.BifrostConfig, error)
