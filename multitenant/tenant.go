// Package multitenant — header-model multi-tenant runtime.
//
// Identity model
// --------------
// Tenant identity reaches every layer via the `x-f5xc-tenant` request
// header.  No URL path segment, no body field — a single source of
// truth that:
//
//   - admin requests carry verbatim ("x-f5xc-tenant: acme" on every
//     /api/* call from a tenant-aware client),
//   - inference requests carry verbatim ("x-f5xc-tenant: acme" on every
//     /v1/* call) PLUS the VK bearer for auth,
//   - middleware extracts and stamps onto request context exactly once,
//     so every handler / configstore method / plugin hook reads the
//     same value via BifrostContextKeyTenantID.
//
// Single-tenant deployments are multi-tenant with N=1: every request
// carries `x-f5xc-tenant: default` and the schema's NOT NULL default
// "default" makes that the implicit value when the column wasn't
// populated by an older write path.
//
// Runtime model
// -------------
// Each tenant gets its own *bifrost.Bifrost instance (own Account, own
// MCP manager, own request queues) lazy-loaded via Manager.Acquire.
// Plugins (logging, governance, telemetry) are shared with the root
// runtime via the no-cleanup-on-shutdown shim in plugin_share.go so a
// tenant evict doesn't tear down the global logstore writer.
package multitenant

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
)

// HeaderName is the canonical HTTP header for tenant identity.
const HeaderName = "x-f5xc-tenant"

// TenantID is the opaque per-process tenant identifier — string so DB
// IDs, ULIDs, UUIDs, or slugs can all be used by the deployer.
type TenantID string

// String renders the underlying value.
func (t TenantID) String() string { return string(t) }

// DefaultTenantID is the tenant attributed to all pre-multi-tenant
// data and to callers in single-tenant deployments.  Mirrors the
// constant of the same name in framework/configstore/tables to avoid
// pulling the configstore import into call sites that only need the
// literal.
const DefaultTenantID TenantID = "default"

// BifrostContextKeyTenantID is the BifrostContext key under which the
// resolved tenant id is stored.  The tenant-resolver middleware sets
// it after reading the `x-f5xc-tenant` header; downstream plugins /
// the multi-tenant dispatcher read it via
// ctx.Value(BifrostContextKeyTenantID).
const BifrostContextKeyTenantID schemas.BifrostContextKey = "x-f5xc-tenant"

// TenantLoader is the user-supplied callback that produces a
// BifrostConfig for a given tenant.  Invoked exactly once per tenant
// runtime per process lifetime (or until the runtime is evicted and
// re-acquired).
//
// The returned BifrostConfig.Account is required; other fields are
// optional and follow bifrost.Init's semantics.
//
// The ctx passed to the loader is the Manager's root context; it is
// cancelled when the runtime is being evicted or the Manager is
// shutting down.  Loaders that block on I/O should respect
// cancellation.
type TenantLoader func(ctx context.Context, tid TenantID) (schemas.BifrostConfig, error)
