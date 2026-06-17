#!/usr/bin/env bash
# Apply the mt-runtime-isolation series.
#
# Closes the Stage 2 runtime-isolation gap from patches 1-6: wires
# multitenant.Manager into the inference path so each tenant gets its
# own *bifrost.Bifrost engine, with a per-tenant StaticAccount snapshot
# of providers+keys. Sync inference dispatches through a per-request
# UserValue stashed by TenantDispatcherMiddleware; async inference holds
# an extra refcount across the goroutine boundary. Provider/key admin
# writes evict the per-tenant runtime so the next inference re-loads
# fresh DB state.
#
# Depends on patch1 (multitenant package + Manager) + patch3 (resolver
# middleware) + patches 2/5 (tenant-scoped configstore reads via GORM
# callback).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch7/mt-runtime-isolation/apply.sh

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
