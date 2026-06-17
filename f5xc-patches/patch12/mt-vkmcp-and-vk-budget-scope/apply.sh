#!/usr/bin/env bash
# Apply the mt-vkmcp-and-vk-budget-scope series.
#
# Two more JOIN-scope follow-ups in the same risk class as patches
# 10 + 11:
#
#   (1) GetVirtualKeyMCPConfigsByMCPClientStringIDs — was missing both
#       an explicit .Model() pin and a tenant predicate on the JOINed
#       config_mcp_clients side. Closed: cross-tenant VK→MCP
#       enumeration via known client_id.
#
#   (2) GetVirtualKeysPaginated budget subquery — self-limiting today
#       via outer JOIN, but stamp the predicate inside the subquery
#       too so a refactor can't accidentally open it.
#
# Depends on patches 1-2 (tenantFromContext + tenant columns on
# governance_virtual_key_mcp_configs, config_mcp_clients, governance_budgets).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch12/mt-vkmcp-and-vk-budget-scope/apply.sh

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
