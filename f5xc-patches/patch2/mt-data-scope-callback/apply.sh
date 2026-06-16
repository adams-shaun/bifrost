#!/usr/bin/env bash
# Apply the mt-data-scope-callback series.
#
# Installs the GORM callback that turns every existing SELECT/UPDATE/
# DELETE on a tenant-scoped table into `WHERE tenant_id = ?` and stamps
# tenant_id on every INSERT — all read from the request context, so
# upstream handlers become tenant-aware without per-handler edits. Also
# carries the rdb.go config-sync edits that explicitly stamp the default
# tenant on startup-time provider/key writes (no request ctx available)
# and the team-name composite-unique migration. Depends on patch1's
# schema.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can also
# be run standalone from the repo root:
#     bash f5xc-patches/patch2/mt-data-scope-callback/apply.sh

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
