#!/usr/bin/env bash
# Apply the all-tests patch series to the current bifrost-aigw checkout.
#
# This series lives under f5xc-patches/patchz-tests/ so it sorts AFTER every
# patchN series under GNU make's $(sort), guaranteeing the tests are added
# only once every source patch has been applied. The naming convention is
# the project's escape hatch for "always last" — patch99 / patch100 would
# sort BEFORE patch2 / patch3 in string order, so we use a leading-z prefix
# at the patchN level instead.
#
# Patches in this series, ordered to mirror the source-patch dependency
# chain (user → VK → budget/rate-limit):
#   0001: user governance store + budget enforcement tests (covers patch1)
#   0002: /api/governance/users HTTP handler tests           (covers patch1)
#   0003: VK ID secondary index + header parser tests        (covers patch2)
#   0004: VK ID HTTP handler tests (PUT-as-upsert)           (covers patch2)
#   0005: delta-dump + refresh worker tests                  (covers patch3)
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also be
# run standalone from the repo root:
#   bash f5xc-patches/patchz-tests/all-tests/apply.sh

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
