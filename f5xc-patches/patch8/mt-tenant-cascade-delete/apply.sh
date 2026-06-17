#!/usr/bin/env bash
# Apply the mt-tenant-cascade-delete series.
#
# Fixes DELETE /api/tenants/{id} to cascade through every tenant-scoped
# child row (providers, keys, virtual_keys, teams, customers, budgets,
# rate_limits, mcp_clients) inside a single transaction, so the parent
# delete no longer leaves orphans that 409 on the next same-id create.
#
# Depends on patch1 (multitenant package, tenant_id columns) + patch3
# (platform_tenants.go where the handler lives).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch8/mt-tenant-cascade-delete/apply.sh

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
