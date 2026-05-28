#!/usr/bin/env bash
# Apply the delta-counter-dump patch series to the current bifrost-aigw
# checkout.
#
# Changes DumpRateLimits / DumpBudgets in plugins/governance/store.go from
# blind SETs to DB-side increments so concurrent dumps from sibling pods
# sum cleanly instead of last-writer-wins clobbering each other. Required
# for multi-replica deployments backed by a shared Postgres without a
# gossip layer. The companion refresh worker (0002) pulls cluster totals
# back into each pod's in-memory counter every 60s, bounding enforcement
# drift to one refresh interval.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also be
# run standalone from the repo root:
#   bash f5xc-patches/patch3/delta-counter-dump/apply.sh

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
