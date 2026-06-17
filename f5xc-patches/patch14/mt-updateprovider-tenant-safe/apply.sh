#!/usr/bin/env bash
# Apply the mt-updateprovider-tenant-safe series.
#
# Closes T3 from docs/multi-tenant-f5/04-design-poc-to-v1.md §4.2:
# updateProvider was reading provider config (raw + redacted) from
# the SHARED in-memory map and writing via
# h.inMemoryStore.UpdateProviderConfig — both of which are global,
# last-write-wins across tenants, and the latter trips the legacy
# idx_key_id unique on a second tenant updating an existing provider
# name.
#
# Adds providers_tenant_safe.go with three branching helpers:
#   loadProviderConfigRaw / loadProviderConfigRedacted / commitProviderUpdate /
#   commitProviderAdd
#
# Each branches by TenantIDFromCtx(ctx): multi-tenant mode reads and
# writes go direct through h.dbStore (tenant-scoped via the GORM
# callback installed in patch2); single-tenant OSS path is unchanged.
#
# Depends on patches 1-4 (tenant_id columns, scope callback,
# h.dbStore wired into ProviderHandler) and patch7 (defer
# EvictTenant on every mutation handler — runtime invalidation).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch14/mt-updateprovider-tenant-safe/apply.sh

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
