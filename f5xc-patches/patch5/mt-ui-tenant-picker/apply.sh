#!/usr/bin/env bash
# Apply the mt-ui-tenant-picker series.
#
# Front-end half of the multi-tenant header story: a Redux slice holds
# the selected tenant id, an RTK Query `prepareHeaders` interceptor on
# the shared baseApi stamps `x-f5xc-tenant` onto every outgoing admin
# request, and a sidebar picker lets the operator switch tenants
# without a page reload. Also flips DefaultNetworkConfig.allow_private_network
# to true so cluster-internal *.svc URLs validate in a k8s deployment.
#
# Pure UI — independent of the Go-side series. Can be reordered or
# dropped without affecting back-end behaviour (admin requests just
# never get the header and fall back to single-tenant N=1).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also
# be run standalone from the repo root:
#     bash f5xc-patches/patch5/mt-ui-tenant-picker/apply.sh

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
