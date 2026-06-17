# Stage 2 — per-tenant runtime isolation + hardening

**Status:** code wired in `/home/sadams/projtmp/bifrost/.workspaces/patch-dev`
on 2026-06-16. Build is clean (`go build ./transports/bifrost-http/...
./multitenant/... ./framework/...` green). Not yet emitted as a patch in
`f5xc-patches/`; not yet deployed.

Continues the Stage 2 work plan from
[00-review-and-plan.md §"Concerns / debt" item 1](./00-review-and-plan.md):
> *The runtime isn't actually multi-tenant yet. `multitenant.Manager`
> exists with LRU + refcount + `initOnce`, but server.go never calls
> Acquire. Inference still dispatches through the single root s.Client.*

This document describes the wiring that closes that gap, plus the
hardening rolled in alongside it (three layers of defense against silent
`tenant_id` corruption; URL move; cluster bootstrap SDK).

---

## 1. Runtime isolation wiring

### Goal

Every inference request dispatched against a `*bifrost.Bifrost` engine
that was constructed **for that tenant**, snapshotting that tenant's
providers + keys at Acquire time, with its own worker pool, queues, and
request-side concurrency budget. Single-tenant deployments (no header)
keep zero overhead.

### Architecture

```
                 ┌─────────────────────────────────────────────────────┐
                 │  *fasthttp.RequestCtx                                │
HTTP request  ──▶│   x-f5xc-tenant: acme                                │
                 └─────────────────────────────────────────────────────┘
                                       │
                                       ▼
                    ┌───────────────────────────────────┐
                    │ TenantResolverMiddleware          │  patches 1-3
                    │   reads header → stamps tid       │
                    │   on ctx UserValue                │
                    └───────────────────────────────────┘
                                       │
                                       ▼
                    ┌───────────────────────────────────┐
                    │ TenantDispatcherMiddleware        │  patch7 (new)
                    │   tid := TenantIDFromCtx(ctx)     │
                    │   bf, release := disp.BifrostFor( │
                    │                  ctx, tid)        │
                    │   ctx.SetUserValue(               │
                    │     "__bifrost_client", bf)       │
                    │   defer release()                 │
                    └───────────────────────────────────┘
                                       │
                                       ▼
                    ┌───────────────────────────────────┐
                    │ inference handlers                │
                    │   h.bf(ctx).ChatCompletionRequest │
                    │   (reads ctx UserValue → per-     │
                    │   tenant *bifrost.Bifrost,        │
                    │   falls back to h.client root)    │
                    └───────────────────────────────────┘
```

### Pieces added

| File | Role |
|---|---|
| `transports/bifrost-http/server/multitenant.go` | `initializeManager(ctx)`, `newTenantLoader()`, `providerConfigForBifrost()`, `BifrostFor(ctx, tid)`, `EvictTenantRuntime(tid)` |
| `transports/bifrost-http/handlers/tenant_dispatcher.go` | `BifrostDispatcher` interface, `TenantDispatcherMiddleware`, `TenantEvict` package-level hook, `EvictTenant(ctx)` helper |
| `transports/bifrost-http/lib/ctx.go` | `FastHTTPUserValueBifrostClient` constant, `BifrostClientFromCtx(ctx)` reader |
| `transports/bifrost-http/handlers/inference.go` | `h.bf(ctx)` helper; all 46 `h.client.X(bifrostCtx, ...)` call sites refactored to `h.bf(ctx).X(...)` |
| `transports/bifrost-http/handlers/mcpinference.go` | `h.bf(ctx)` helper; both `ExecuteChat/ResponsesMCPTool` sites refactored |
| `transports/bifrost-http/handlers/asyncinference.go` | `SetDispatcher`, `acquireBifrost`; all 11 async submission handlers acquire a fresh refcount that the closure releases (request lifetime ≠ closure lifetime) |
| `transports/bifrost-http/server/server.go` | `Manager *multitenant.Manager` on struct; bootstrap calls `s.initializeManager(ctx)`; `asyncHandler.SetDispatcher(s)`; `RegisterInferenceRoutes` prepends `dispatchMW` after `tenantMW` |
| `transports/bifrost-http/handlers/providers.go` + `provider_keys.go` | `defer EvictTenant(ctx)` at the top of `addProvider`, `updateProvider`, `deleteProvider`, `createProviderKey`, `updateProviderKey`, `deleteProviderKey` |

### How the TenantLoader runs

On the first inference request for tenant `acme` after process start (or
after an Evict), `Manager.Acquire(ctx, "acme")` calls
`newTenantLoader()`'s closure:

1. `scopedCtx := context.WithValue(ctx, BifrostContextKeyTenantID, "acme")`
   — pushes the tenant onto ctx so the GORM tenant-scope callback
   appends `WHERE tenant_id = 'acme'` automatically on every read.
2. `providers := s.Config.ConfigStore.GetProvidersConfig(scopedCtx)` —
   reads providers for the tenant.
3. For each provider: `keys := s.Config.ConfigStore.GetProviderKeys(scopedCtx, name)`
   — reads keys for the tenant.
4. Builds a `multitenant.StaticAccount(tid)` snapshot; the per-tenant
   `*bifrost.Bifrost` reads from this snapshot instead of the shared
   `lib.Config.Providers` in-memory map.
5. Returns a `schemas.BifrostConfig` with `LLMPlugins` and `MCPPlugins`
   wrapped via `multitenant.ShareLLMPlugins`/`ShareMCPPlugins` so the
   per-tenant `Shutdown()` can't `Cleanup` plugin state the root depends
   on (`OAuth2Provider`, `Logger`, `KVStore` are reused as-is — they're
   per-process globals).

### Single-tenant fallback

`BifrostFor(ctx, tid)` returns `s.Client, noop` when:
- `tid == ""` (no header), OR
- `tid == "default"`, OR
- `s.Manager == nil` (no multitenant config), OR
- `Manager.Acquire` fails (loader error → log warning, serve from root)

The dispatcher middleware then stamps the root client on ctx; the
handler reads it via `lib.BifrostClientFromCtx`. Net effect: no
behavior change for OSS-style deployments.

### Async case (request lifetime ≠ closure lifetime)

The sync inference path is straightforward because
`defer release()` in the dispatcher middleware fires after the request
handler returns — even for SSE / streaming responses, since fasthttp
handlers are synchronous within a single request.

Async (`/v1/async/*`) is different: `executor.SubmitJob` fires
`go executeJob(...)` and returns immediately. The closure runs in a
background goroutine that outlives the request. If we let the dispatch
middleware's `defer release()` fire on request return, the runtime could
be evicted (refcount drops to 0) before the closure runs.

Resolution:
- `AsyncHandler` holds its own `BifrostDispatcher` reference
  (`SetDispatcher` wired at boot).
- Each async submit method calls `bf, release := h.acquireBifrost(ctx)`
  **before** `SubmitJob`. This holds an additional refcount that
  survives past request return.
- The closure does `defer release()` so the refcount drops when the
  job completes (success or panic).
- If `SubmitJob` returns an error (VK lookup / DB write failed) the
  closure never runs, so the err path explicitly calls `release()`
  before `SendError`.

### Eviction

`EvictTenantRuntime(tid)` is wired as the package-level
`handlers.TenantEvict` at boot (`initializeManager`). Mutation handlers
fire `defer EvictTenant(ctx)` at the top of the function. The next
Acquire for that tenant runs the loader and re-snapshots fresh provider
+ key state from the DB.

Over-eager evict on error paths (e.g. bad JSON, validation failure) is
acceptable: the next request rebuilds an identical snapshot, which is
the same cost as startup. Net cost = one DB round-trip per attempted
admin write to that tenant.

VK / team / customer / MCP mutations do **not** evict — none of those
appear in the StaticAccount snapshot. (Per-tenant MCP isolation is
out of scope for v1; see "Known limitations" below.)

---

## 2. Hardening: three-layer defense against silent `tenant_id` corruption

A category of bugs surfaced in the deployed cluster where rows landed
with `tenant_id = ""` (orphaned from every tenant scope) or
`tenant_id = "default"` (cross-tenant leak). The runbook for one such
incident is captured in
[01-runbook-updateproviderkey-tenant-corruption.md](./01-runbook-updateproviderkey-tenant-corruption.md);
this section describes the prophylactic that prevents the whole class.

### Root cause class

GORM struct tag `default:default` (used to give the OSS migration a
non-NULL fallback for `tenant_id`) **pre-fills the field to the literal
string `"default"` before BeforeCreate / scope callbacks run**. The
original `setTenantIfEmpty` callback only overrode the empty string, so
it saw a non-empty pre-filled value and skipped. Net: any INSERT that
didn't pass through a stamp-from-ctx site landed with `tenant_id =
"default"` regardless of the actual request's tenant.

### Layer 1 — explicit stamps at every write site

Every handler / rdb method that builds a `*tables.TableX` from the
request explicitly stamps `TenantID: TenantIDFromCtx(ctx)` (or pulls
the tenant from the related parent row). Sites covered:

- `governance.go`: `createVirtualKey`, `createTeam`, `createCustomer`
- `rdb.go`: `AddProvider` (reads `tenantFromContext`), `UpdateProvider`'s
  key INSERT branch (uses `dbProvider.TenantID`), `AddProvider` key loop
  (uses `dbProvider.TenantID`), `UpdateProviderKey` stub (uses
  `existingKey.TenantID`), `CreateMCPClientConfig` (reads ctx)
- `rdb.go::tableKeyFromSchemaKey`: propagates `provider.TenantID` →
  `dbKey.TenantID`

### Layer 2 — GORM scope-callback post-check

`framework/configstore/tenant_scope.go::setTenantIfEmpty` now treats
both `""` and `"default"` as **overridable** values — if the request ctx
carries a real tenant id, the callback overwrites the pre-filled
`"default"`. Then a `assertEveryRowCarriesTenant(tx)` post-check fires
after the loop; in strict mode it errors out the transaction, in
non-strict mode it logs and continues.

### Layer 3 — per-model `BeforeCreate` hooks

`framework/configstore/tables/tenant.go::EnsureTenantIDOnCreate(field,
tableName)` is called by `BeforeCreate(tx *gorm.DB) error` hooks on all
8 tenant-scoped tables. In strict mode
(`BIFROST_ASSERT_TENANT_ON_INSERT=1`) it errors if the field is still
empty / `"default"` at insert time; in non-strict (default) it auto-fills
`"default"` so OSS tests pass.

Tables instrumented:
`provider.go`, `key.go`, `virtualkey.go`, `team.go`, `customer.go`,
`budget.go`, `ratelimit.go`, `mcp.go`.

### Mode selector

| Env | Behavior |
|---|---|
| unset (default) | Auto-fill `"default"` on missing tenant — OSS-friendly |
| `BIFROST_ASSERT_TENANT_ON_INSERT=1` | Strict: any insert without an explicit tenant or ctx tenant errors. Recommended for production multi-tenant deployments. |

---

## 3. URL move: `/api/platform/tenants` → `/api/tenants`

Moved the tenant-management endpoints to the cleaner top-level path:

- Canonical: `POST/GET/PUT/DELETE /api/tenants` and `/api/tenants/:id`
- Aliases retained: `POST/GET/PUT/DELETE /api/platform/tenants[/...]`
  (no breaking change for existing API clients)

`TenantResolverMiddleware`'s `isAdminPath` exempts both prefixes from
the "disable default tenant" block — admin endpoints must be reachable
even when client requests without a tenant header are otherwise
rejected.

UI updated: `ui/lib/store/apis/tenantsApi.ts` URLs flipped to the new
path.

---

## 4. Cluster bootstrap SDK + seed program

The user repeatedly had to re-create the cluster's config by hand after
each wipe-and-deploy. A background agent updated
`/home/sadams/projtmp/go-bifrost-ai` (the companion Go SDK) and
generated a seed program:

### SDK updates (`/home/sadams/projtmp/go-bifrost-ai`)

- `client.go`: added `TenantHeader = "x-f5xc-tenant"` constant,
  `AuthConfig.Tenant`, `WithTenant`, `SetTenant`, `WithRequestTenant`.
  `DoRequest` injects `x-f5xc-tenant` whenever the field is set
  (per-client default or per-request override).
- `Tenants *tenants.Service` exposed on the client.
- New package `tenants/`: `List`, `Get`, `Create`, `Update`, `Delete`
  with variadic opts for per-request tenant override.
- New `types/tenants.go`: Tenant struct + request/response shapes.
- Tests in `tenants/tenants_test.go` pass (asserts header lands on
  outbound HTTP).

### Seed program (`examples/seed/main.go`)

5-phase bootstrap:
1. **Teardown:** delete all existing VKs / providers / tenants for a
   clean slate.
2. **Tenants:** create `acme` and `globex`.
3. **Providers:** for each tenant, create `openai`, `google_ai_studio`,
   `vllm` with appropriate keys / base URLs (sources: env vars
   `BIFROST_OPENAI_KEY`, `BIFROST_GOOGLE_KEY`, `BIFROST_VLLM_KEY`;
   vLLM bearer is the literal `clowntown123`).
4. **Keys:** provider keys are attached at creation time in phase 3.
5. **VK:** create one virtual key per tenant scoped to that tenant.

The seed program uses `c.SetTenant(...)` between phases for services
that don't yet accept per-request tenant opts (providers, governance);
the new tenants service accepts variadic opts.

Caveats reported by the bootstrap agent (worth documenting for
follow-up):
- `allow_private_network` field omitted from the SDK — the corresponding
  Go field doesn't exist yet in `go-bifrost-ai`.
- Per-request tenant override is only available on the new `tenants`
  service. Other services use `SetTenant` between phases.
- Cascade orphan check (delete-tenant verifying no orphaned children)
  is best-effort.

---

## 5. Known limitations / Stage 3 follow-ups

| Item | Why deferred | Where it shows up |
|---|---|---|
| Per-tenant MCP isolation | `TableMCPClient` already carries `tenant_id` (Stage 1 schema), but the runtime still shares `OAuth2Provider` / `KVStore` / `MCPManager` across tenants. Lower priority than provider/key isolation. | `server/multitenant.go::newTenantLoader` — MCP config reused as-is |
| WebSocket / WebRTC / Realtime handlers | Still dispatch through `h.client` root. Not on the critical inference path, lower traffic. | `wsresponses.go`, `wsrealtime.go`, `webrtc_realtime.go`, `realtime_client_secrets.go` |
| Manager LRU cap unset | `MaxActiveRuntimes` left at unbounded default. Productization knob — each tenant runtime is cheap until it sees traffic, but a hot-but-bounded cap belongs in any deployment > 100 tenants. | `server/multitenant.go::initializeManager` |
| Manager unit tests | LRU eviction under concurrent Acquire, init race, refcount underflow. | `multitenant/` has no test files yet |
| `TestConfigSchemaSync` | Fails because Stage 1 added `tenant_id` / `source_id` fields to 5 governance tables that aren't in `config.schema.json`. Pre-dates Stage 2; needs `excludedGoFields` additions (or schema additions) in `lib/config_test.go`. | `lib/config_test.go::TestConfigSchemaSync` |
| Cross-tenant inference smoke test | Stage 2 is wired but not yet smoke-tested against the live cluster. | k3d `bm`, image `local/bifrost:f5g` |

---

## 6. How to build + deploy this stage

Standard recipe captured in
[01-runbook §Deploy](./01-runbook-updateproviderkey-tenant-corruption.md#deploy).
After this stage lands as a patch (`f5xc-patches/patch7/`), the same
flow applies:

```bash
# from /home/sadams/projtmp/bifrost
make clean-patches
make apply-patches            # picks up patch7

# Local Docker image; emits bifrost:latest tagged for k3d import
make docker-image LOCAL=1
docker tag bifrost:latest local/bifrost:f5g
k3d image import local/bifrost:f5g -c bm

# Roll the deployment
kubectl -n bifrost rollout restart deploy/bifrost
kubectl -n bifrost rollout status deploy/bifrost
```

Then re-seed if you wiped the DB:

```bash
cd /home/sadams/projtmp/go-bifrost-ai
BIFROST_BASE_URL=https://admin.codeburro2.net \
BIFROST_OPENAI_KEY=... BIFROST_GOOGLE_KEY=... \
go run ./examples/seed
```

## 7. Smoke test cheat sheet

```bash
# Tenant A inference
curl -s https://admin.codeburro2.net/v1/chat/completions \
  -H 'x-f5xc-tenant: acme' \
  -H 'Authorization: Bearer <acme-vk>' \
  -H 'content-type: application/json' \
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'

# Tenant B inference — should see ONLY tenant B's providers
curl -s https://admin.codeburro2.net/v1/chat/completions \
  -H 'x-f5xc-tenant: globex' \
  -H 'Authorization: Bearer <globex-vk>' \
  -H 'content-type: application/json' \
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'

# Verify eviction fires on key rotation:
curl -X PUT https://admin.codeburro2.net/api/providers/openai/keys/<id> \
  -H 'x-f5xc-tenant: acme' \
  -d '{...}' \
  && curl ... /v1/chat/completions ...   # next request re-loads acme's runtime
```

Observe the logs for `multitenant: evicted runtime for tenant "acme" on
admin write` on the PUT and the loader info messages on the subsequent
inference.
