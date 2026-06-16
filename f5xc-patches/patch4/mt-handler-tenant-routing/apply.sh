#!/usr/bin/env bash
# Apply the mt-handler-tenant-routing series.
#
# Routes the legacy provider / provider-key / governance admin handlers
# through the tenant-scoped configstore (dbStore) so writes land via the
# GORM callback rather than the shared in-memory map. Also adds the
# cross-tenant customer_id guard on team create/update and the
# `BIFROST_ALLOW_PRIVATE_NETWORK` cluster-wide override on URL
# validation. mt_governance_isolation_test.go is the regression suite.
#
# Depends on patch3 — the resolver provides the ctx the dbStore reads.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also
# be run standalone from the repo root:
#     bash f5xc-patches/patch4/mt-handler-tenant-routing/apply.sh

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
