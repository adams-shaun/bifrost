#!/usr/bin/env bash
# Apply the vk-id-header patch series to the current bifrost-aigw checkout.
#
# Patches 3 and 4 are tightly coupled — patch 3 changes the signature of
# parseVirtualKeyFromHTTPRequest / ParseVirtualKeyFromFastHTTPRequest from
# `*string` to `(*string, string)`. Patch 4 updates every caller for the
# new return shape. The tree does not compile between patches 3 and 4;
# apply both before building.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also be
# run standalone from the repo root: `bash f5xc-patches/vk-id-header/apply.sh`.

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
