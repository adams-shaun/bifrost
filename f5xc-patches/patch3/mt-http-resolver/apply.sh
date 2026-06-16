#!/usr/bin/env bash
# Apply the mt-http-resolver series.
#
# Adds the HTTP-edge half of the multi-tenant story:
#  * TenantResolverMiddleware reads `x-f5xc-tenant`, stamps it onto the
#    request context, and (when BIFROST_DISABLE_DEFAULT_TENANT_CONFIG is
#    set) rejects write attempts targeting the seeded `default` tenant.
#  * /api/platform/tenants CRUD handler for the cross-tenant tenant
#    directory.
#  * VK-bearer pass-through in the auth middleware so a Bifrost virtual
#    key (sk-bf-...) is a valid /v1/* credential.
#  * server.go wires both into the inference + admin route chains.
#
# Depends on patch2 — the resolver stamps a value the GORM callback
# needs to be able to read.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also
# be run standalone from the repo root:
#     bash f5xc-patches/patch3/mt-http-resolver/apply.sh

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
