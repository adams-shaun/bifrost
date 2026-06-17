#!/usr/bin/env bash
# Apply the mt-mcp-tenant-scope series.
#
# Closes T5 from docs/multi-tenant-f5/04-design-poc-to-v1.md §4.2:
# MCPManager.GetClientByName was matching on Name across the entire
# clientMap with no tenant qualifier, so two tenants both choosing
# the same MCP client name (e.g. 'echo') would collide at
# AddClient time — the second create returned
#   "MCP client with name 'X' already exists"
# even though their configstore rows have distinct tenant_id
# values (per patch1). Isolation test I6 surfaces this.
#
# Patch16 tenant-scopes the collision check:
#   * adds TenantID to schemas.MCPClientConfig (JSON tag '-' so the
#     public wire shape stays upstream-compatible) and to the
#     in-memory schemas.MCPClientState
#   * changes core/mcp.GetClientByName signature from (name) to
#     (tenantID, name) with empty-tenant fallback for OSS + the
#     CodeMode Starlark sites that have no tenant context at
#     script-execution level
#   * propagates TenantID through the configstore loader and the
#     admin handler so AddClient sees the right tenant value
#
# Trade-off: per-tenant MCPManager INSTANCES (with isolated tool
# server / monitors / OAuth) is a larger refactor tracked as a
# follow-up — patch16 closes the cross-tenant name collision (the
# audit gap that surfaces today) without rebuilding per-tenant MCP
# infrastructure.
#
# Depends on patches 1, 2, 3, 4, 7 (tenant_id columns, GORM scope
# callback, dispatcher middleware that puts tenant on ctx, and the
# shared-plugin shim).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch16/mt-mcp-tenant-scope/apply.sh

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
