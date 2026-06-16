#!/usr/bin/env bash
# Apply the mt-foundation-schema series.
#
# Lays down the `multitenant/` module, the `tenants` table + `tenant_id`
# columns on the eight existing governance / per-tenant config tables,
# the consolidated tenant migration, and the Dockerfile / go.work wiring
# the rest of the series depends on. Must run BEFORE patch2 (the GORM
# tenant-scope callback) because the callback reads from columns that
# only exist after this series lands.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also
# be run standalone from the repo root:
#     bash f5xc-patches/patch1/mt-foundation-schema/apply.sh

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
