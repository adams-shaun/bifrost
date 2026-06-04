#!/usr/bin/env bash
# Apply the user-support patch series to the current bifrost-aigw checkout.
#
# Adds governance_users as a first-class entity alongside teams/customers/
# access-profiles, with UserID FKs on TableVirtualKey and TableBudget,
# and three idempotent migrations. Subsequent patches (CRUD store, HTTP
# handlers, context middleware, reload/cache) live in the original series
# at ~/AI-Gateway/bifrost-user-support/ and still need to be rebased.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also be
# run standalone from the repo root: `bash f5xc-patches/user-support/apply.sh`.

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
