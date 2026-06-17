#!/usr/bin/env bash
# Apply the mt-semanticcache-tenant-scope series.
#
# Closes the I7 isolation bug from docs/multi-tenant-f5/04-design-poc-to-v1.md:
# the semantic-cache plugin is a singleton (one *Plugin instance shared
# across all tenant runtimes by the shared-LLM-plugin shim from
# patch7) and its resolveCacheKey was returning the raw per-request
# CacheKey (or the configured DefaultCacheKey) with no tenant
# qualifier. Two tenants whose VK / default config produced the same
# cache key would therefore read each other's cached chat-completion
# responses out of the shared VectorStore namespace.
#
# Adds a single tweak in plugins/semanticcache/main.go:
#   * declare local typed-key tenantContextKey ("x-f5xc-tenant",
#     same string value as multitenant.BifrostContextKeyTenantID)
#   * resolveCacheKey now prepends "<tenant>:" when the context
#     carries a non-empty tenant id, so direct hash lookups and
#     semantic-search metadata properties stay tenant-isolated.
#
# Depends on patch7 (which puts the tenant id on the BifrostContext
# via the dispatcher middleware and shares LLM plugins across tenant
# runtimes).
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch15/mt-semanticcache-tenant-scope/apply.sh

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
