#!/usr/bin/env bash
# Apply the mt-discovery-via-tenant-runtime series.
#
# Final piece of the discovery-tenant-scope work started in patch17/18.
# Patch17 fixed the catalog upsert filter (accept bare IDs from custom
# backends). Patch18 fixed the boot loader (exclude default-tenant
# rows from the shared in-memory map) + the discovery hasNoKeys check
# + preserved tenant ctx through attemptModelDiscovery's timeout
# wrapping. Patch19 closes the last gap: the actual ListModelsRequest
# call in ReloadProvider still used s.Client (the root bifrost
# runtime), whose account reads the shared in-memory map and so gets
# the wrong tenant's keys / URL when multiple tenants have a provider
# with the same name.
#
# The fix: route ListModelsRequest through the per-tenant runtime
# acquired via s.BifrostFor(ctx, tid). The tenant runtime's account
# is a tenant-scoped StaticAccount built by multitenant.Manager's
# TenantLoader from a tenant-scoped configstore read, so its account
# has the right keys + URL by construction.
#
# Single-tenant fallback: BifrostFor returns s.Client + a no-op
# release when tid is empty (no header) or s.Manager is nil (OSS
# single-tenant deployment). Behaviour for those paths is unchanged.
#
# NOT in scope (deferred to a follow-up):
#   populateModelPoolWithListModels (server.go ~line 902) and the
#   boot-time loop at ~line 1530 still iterate s.Config.Providers
#   and call s.Client.ListModelsRequest. Boot-time bulk discovery is
#   structurally incompatible with the per-tenant runtime model — it
#   either needs to iterate tenants explicitly or be skipped entirely
#   in strict-MT mode. Best handled as patch20.
#
# Depends on patches 1, 2, 3, 4, 7, 17, 18.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch19/mt-discovery-via-tenant-runtime/apply.sh

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
