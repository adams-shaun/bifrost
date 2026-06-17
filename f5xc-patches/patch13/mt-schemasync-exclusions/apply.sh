#!/usr/bin/env bash
# Apply the mt-schemasync-exclusions series.
#
# Closes the failing TestConfigSchemaSync test (lib package) by adding
# the multi-tenant overlay's tenant_id / source_id columns to the
# per-table excludedGoFields list. These columns are runtime-stamped
# from request ctx (or parent row) — they never round-trip through
# config.json, so the OSS schema shouldn't carry them.
#
# Depends on patch1 (which adds the columns).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch13/mt-schemasync-exclusions/apply.sh

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
