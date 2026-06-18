#!/usr/bin/env bash
# Apply the mt-discovery-tenant-scope series.
#
# Companion to patch17. Three subfixes around the shared in-memory
# provider map (c.Providers) being keyed by provider NAME only, with
# no per-tenant separation:
#
# Subfix A — boot loader filter (framework/configstore/rdb.go)
#   When BIFROST_DISABLE_DEFAULT_TENANT_CONFIG=true and the ctx has
#   no tenant id (boot loader call from lib/config.go::loadProviders),
#   exclude default-tenant rows so the root client's shared in-memory
#   map doesn't get polluted with cross-tenant config via last-write-
#   wins iteration over the unfiltered SELECT.
#
# Subfix B1 — discovery hasNoKeys check via configstore, not in-memory
#   (transports/bifrost-http/server/server.go::ReloadProvider)
#   The hasNoKeys decision was reading the shared in-memory keys via
#   GetProviderKeysRaw. After ANY tenant DELETEs a provider with the
#   same name, the entry vanishes and the check falsely reports "no
#   keys configured" for EVERY other tenant's subsequent discovery
#   attempts. Fixed by reusing the tenant-scoped configstore lookup
#   already loaded a few lines later.
#
# Subfix B2 — preserve tenant ctx into discovery's timeout context
#   (transports/bifrost-http/handlers/providers.go::attemptModelDiscovery)
#   The timeout context was derived from context.Background(), which
#   drops the tenant id stamped by TenantResolverMiddleware. Fixed by
#   stamping the tenant onto a base context before adding the timeout
#   so the downstream GORM callbacks correctly scope per-tenant.
#
# Tests: framework/configstore/rdb_provider_boot_filter_test.go locks
# in the boot-loader truth table. Runtime subfixes B1/B2 covered by
# live cluster smoke + the existing server-package tests.
#
# Depends on patches 1, 2, 3, 4, 7 (multitenant package + tenant_id
# columns + GORM scope callback + resolver middleware + runtime
# isolation) and patch17 (mt-customprovider-and-strict-routing — pairs
# naturally, not a hard dep).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch18/mt-discovery-tenant-scope/apply.sh

set -euo pipefail

SERIES_DIR=$(cd "$(dirname "$0")" && pwd)
PATCH_DIR="$SERIES_DIR/patches"
REPO_ROOT=$(git rev-parse --show-toplevel)

cd "$REPO_ROOT"

for p in "$PATCH_DIR"/*.patch; do
    echo "    - $(basename "$p")"
    if ! git apply --recount --whitespace=nowarn "$p"; then
        echo "ERROR: $(basename "$p") failed to apply." >&2
        exit 1
    fi
done
