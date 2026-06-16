#!/usr/bin/env bash
# Apply the mt-log-telemetry-tenant series.
#
# Closes the cross-tenant data leak in /api/logs and Prometheus
# metrics: adds tenant_id to logstore.Log + the matching column
# migration, registers the GORM tenant-scope callback on the
# logstore *gorm.DB (the mirror of what patch2 did for the
# configstore), stamps tenant on each log row from BifrostContext at
# write time, and adds a `tenant_id` label to the Prometheus metrics.
#
# Depends on patch1 (multitenant package) + patch3 (resolver
# middleware that stamps the tenant onto ctx).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch6/mt-log-telemetry-tenant/apply.sh

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
