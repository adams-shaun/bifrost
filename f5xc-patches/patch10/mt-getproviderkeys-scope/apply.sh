#!/usr/bin/env bash
# Apply the mt-getproviderkeys-scope series.
#
# Plugs a cross-tenant data leak in
# RDBConfigStore.GetProviderKeys: the JOIN-based query was returning
# keys from every tenant when the request was scoped to one. The GORM
# scope callback qualified the WHERE on config_providers.tenant_id but
# in practice the WHERE wasn't propagating to the joined config_keys
# rows. Explicitly stamp the predicate on the JOINed side.
#
# Depends on patches 1-2 (tenantFromContext + tenant column on
# config_keys + GORM scope callback already present).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch10/mt-getproviderkeys-scope/apply.sh

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
