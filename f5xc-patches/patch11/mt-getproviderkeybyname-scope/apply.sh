#!/usr/bin/env bash
# Apply the mt-getproviderkeybyname-scope series.
#
# Audit follow-up to patch10: same defensive scoping on the sibling
# JOIN in getProviderKeyByName. Works correctly today by accident
# (primary table is config_keys so the scope callback's qualified
# WHERE lands on the right side) but fragile to refactor — add an
# explicit predicate so the scope is independent of which side GORM
# treats as primary.
#
# Depends on patches 1-2 (tenantFromContext + scope callback + tenant
# columns).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch11/mt-getproviderkeybyname-scope/apply.sh

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
