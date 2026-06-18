#!/usr/bin/env bash
# Apply the mt-customprovider-and-strict-routing series.
#
# Two narrowly-scoped fixes for the inference path:
#
# Subfix B — catalog upsert accepts bare model IDs for custom
# providers. Upstream's framework/modelcatalog/models.go filters out
# every model whose /v1/models response ID isn't prefixed with the
# queried provider name. vLLM and OpenAI-compatible custom backends
# return BARE ids (e.g. {"id":"qwen3.6-coder"}), so the catalog stays
# empty and auto-resolve at /v1/chat/completions returns
#   "no providers found for model X in model catalog to auto-resolve"
# even though discovery succeeded. The fix threads an isCustom bool
# through UpsertModelDataForProvider / UpsertUnfilteredModelDataForProvider
# so the parsedProvider==""/bare-ID case is accepted as belonging to
# the queried provider — for CUSTOM providers only. Built-in providers
# still pass isCustom=false → upstream behaviour unchanged.
#
# Subfix A — inference path: strict tenant-header rejection
# (env-gated by BIFROST_STRICT_TENANT_ROUTING, default false →
# behaviour unchanged). When set, requests with no / unknown tenant
# header are fast-failed with 401 / 404 instead of silently falling
# through to the root runtime (which Manager.Acquire's loader
# happily produces for any tid since the loader returns an
# empty-but-valid BifrostConfig for unknown tenants).
#
# Depends on patches 1, 2, 3, 4, 7 (multitenant package, tenant_id
# columns, GORM scope callback, resolver middleware, runtime
# isolation + dispatcher middleware).
#
# Bug 1 from the earlier audit ("discovery never re-fires after key
# add") turned out to be a false positive — the re-trigger already
# happens via attemptModelDiscovery on key add/update/delete + provider
# update in upstream. The "no keys configured" warning we chased was
# just the first-attempt log at provider create; the subsequent key
# POST re-fires discovery. Documented in the commit message body.
#
# Invoked from the top-level Makefile via `make apply-patches`. Can
# also be run standalone from the repo root:
#     bash f5xc-patches/patch17/mt-customprovider-and-strict-routing/apply.sh

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
