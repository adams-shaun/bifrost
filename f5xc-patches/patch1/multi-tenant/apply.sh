#!/usr/bin/env bash
# Apply the multi-tenant patch series to the current bifrost-aigw checkout.
#
# Adds the per-tenant Bifrost runtime registry (new multitenant/ Go module
# with Manager/lazy load/LRU/Handle refcount), Phase 2 spike binaries that
# quantify per-tenant cost across no-plugin / telemetry / per-tenant SQLite /
# shared SQLite configurations, manager tests, and the first Phase 1 schema
# migration: TableTenant + tenant_id on governance_customers.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also be
# run standalone from the repo root: `bash f5xc-patches/patch1/multi-tenant/apply.sh`.

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
