#!/usr/bin/env bash
# Apply the mt-scope-callback-order series.
#
# Moves the multitenant:scope_create GORM callback from
# Before("gorm:create") to Before("gorm:before_create") so it runs
# BEFORE per-model BeforeCreate hooks. Otherwise strict-mode
# (BIFROST_ASSERT_TENANT_ON_INSERT=true) BeforeCreate asserts saw an
# empty tenant_id on child rows (budgets / rate-limits inside
# customer/team/VK create transactions) and rejected the insert before
# the scope callback could stamp from request ctx.
#
# Depends on patch1 (multitenant pkg + RegisterTenantScopes site).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch9/mt-scope-callback-order/apply.sh

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
