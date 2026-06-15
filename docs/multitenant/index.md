# Multi-tenant Bifrost (F5XC fork)

Internal engineering plan for the SaaS multi-tenant overlay on top of vanilla
OSS Bifrost. Tracks the patch series under
`f5xc-patches/patch1/multi-tenant/patches/`, the architecture it implements,
and the work still outstanding.

- Jira: https://jira.f5net.com/browse/XC-25496
- Fork branch: `vesdev-multi-tenant` (overlay) / `tmp-XC-25496-mt-phase1` (workspace)
- Feature flag: `BIFROST_MULTI_TENANT_ENABLED` (default off — single-tenant
  deployments observe no behavior change)

## Goal

Run N independent 3rd-party tenants — each bringing their own provider tokens,
governance rules, MCP clients, etc. — inside one Bifrost process. Vanilla OSS
Bifrost is single-tenant per process; this fork wraps it without rewriting
core.

## Architecture (decisions locked in)

- **Tenancy shape:** per-tenant Bifrost runtime registry (shape B). Each
  tenant gets a vanilla `*bifrost.Bifrost` instance with its own Account /
  plugins / MCP manager. Lazy-loaded on first request, LRU-evictable on
  memory pressure. Sidesteps the partitioning problem in `core/bifrost.go`'s
  global `sync.Map`/atomic state by giving each tenant its own runtime.
- **Tenant resolver:** VK encodes the tenant via DB lookup, **not** a prefix
  in the VK string (no tenant identity leakage). LRU cache
  `hash(vk) → (tenant_id, vk_record_id, expires_at)` populated at HTTP
  middleware. TTL invalidation (60s) for v1; Postgres LISTEN/NOTIFY deferred.
- **HA target v1:** stateless. Postgres ConfigStore + LogStore, Redis
  KVStore, N replicas behind LB. Per-node rate limits documented as
  approximate. Coordinated counter sync deferred to v2.
- **ConfigStore strategy:** shared store with `tenant_id` row scoping on every
  governance/config table. Per-tenant SQLite files were measured at ~888 KiB
  heap + 60 ms cold-start per tenant — not viable past ~1000 tenants. Shared
  store is ~36× cheaper.
- **Governance plugin shape:** hybrid. Shared `LocalGovernanceStore` (sync.Maps
  keyed by `tenant_id`), per-tenant `BudgetResolver` / `UsageTracker` /
  `RoutingEngine`. Per-tenant runtime owns the per-tenant pieces; the store
  survives across runtime evictions.

## Patch series so far

Each patch lives at `f5xc-patches/patch1/multi-tenant/patches/NNNN-*.patch`
and carries a `Ref: https://jira.f5net.com/browse/XC-25496` trailer.

> **Note:** the series was renumbered from 18 patches to 14 after dropping
> the spike research patches (see cleanup section below). Old patch numbers
> are referenced in some prior commits and the project memory.

### Phase 0 — multitenant package scaffold

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0001 | multitenant: scaffold per-tenant Bifrost runtime registry        | `multitenant/` Go module — `Manager` (lazy load + refcount + LRU), `VKResolver`, `StaticAccount`, runtime/tenant/account types.                |
| 0002 | multitenant: tests for Manager concurrency, eviction, …          | 9 race-clean tests covering concurrent Acquire/Release, LRU eviction under refcount pressure, expiration.                                      |

### Phase 1 — ConfigStore schema (tenant_id everywhere)

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0003 | configstore: add TableTenant and customer.tenant_id              | New `bf_tenants` table; `DefaultTenantID = "default"`; gormigrate migration adding tenant_id to TableCustomer with backward-compat default.    |
| 0004 | configstore: extend tenant_id to teams, VKs, budgets, rate …     | `tenant_id` on TableTeam, TableVirtualKey, TableBudget, TableRateLimit + migration.                                                            |
| 0005 | configstore: extend tenant_id to providers, keys, MCP clients    | `tenant_id` on TableProvider, TableKey, TableMCPClient + migration.                                                                            |
| 0006 | configstore: tenant-scope Name uniqueness on providers, keys, …  | Composite unique indexes `(tenant_id, name)` so two tenants can both have a provider named "openai".                                          |
| 0007 | configstore: add TenantRepository CRUD on ConfigStore            | `TenantRepository` interface + GORM impl: Create/Get/List/Update/Delete tenants.                                                              |

### Phase 2 — Admin API + tenant resolution

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0008 | transports: add /api/platform/tenants admin handlers             | REST CRUD at `/api/platform/tenants` for platform admins to create/list/update/delete tenants.                                                 |
| 0009 | transports+multitenant: tenant resolver middleware + …           | `ConfigStoreVKResolver` (VK→tenant lookup via repo), fasthttp middleware that resolves tenant_id from the request VK and lifts it into ctx. Also wires `framework/configstore` into `multitenant/go.mod`. |

### Phase 3 — Per-request runtime dispatch

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0010 | transports: introduce BifrostRouter interface + SingleTenant…    | `BifrostRouter.Acquire(ctx) → (*bifrost.Bifrost, release, error)`. `SingleTenantRouter` returns the shared client with a no-op release.       |
| 0011 | configstore: add GetProvidersConfigByTenant for per-tenant …     | Tenant-scoped provider config loader the per-tenant runtime uses to populate its Account.                                                     |
| 0012 | transports: TenantScopedAccount + MultiTenantRouter + ctx …      | `TenantScopedAccount` wraps the OSS Account interface with a tenant_id; `MultiTenantRouter` reads tenant_id from ctx, falls through to `FallbackTenant` then `FallbackClient`. |
| 0013 | transports: wire MultiTenantRouter + resolver middleware into …  | `buildInferenceRouter` in server.go picks SingleTenantRouter or MultiTenantRouter based on `BIFROST_MULTI_TENANT_ENABLED`.                    |
| 0014 | transports/handlers: route inference through BifrostRouter       | All HTTP inference entry points (`inference.go`, `asyncinference.go`, `mcpinference.go`) now `router.Acquire(ctx) + defer release` instead of `h.client.X`. Streaming threads release through `handleStreamingResponse` for SSE goroutine lifetime; async closures acquire at execution time. |

### Phase 4 — Per-tenant admin surface

The first thirteen patches (0001–0014) lit up multi-tenant inference;
patches 0015–0027 build the admin surface operators need to actually
provision tenants and their inference-plane state via API.

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0015 | transports/lib+handlers: AdminAuthz stub + tenant-scoped admin middleware | `lib.AdminAuthz` interface + `StubPlatformAdminAuthz` (v1: always-platform-admin, `BIFROST_ADMIN_DEFAULT_TENANT` env-configurable fallback). Three middlewares: `RequireTenantPathMiddleware`, `LegacyAdminTenantScopeMiddleware`, `RequirePlatformAdminMiddleware`. Adds `multitenant.DefaultTenantID = "default"`. |
| 0016 | transports+configstore: POST `/api/tenants/{tenant_id}/providers` | `ConfigStore.AddProviderForTenant` pins tenant_id on the new TableProvider + TableKey rows. `handlers.TenantProviderHandler` POST with Manager.Evict on mutate. Stashes `multitenant.Manager` on `BifrostHTTPServer` for the admin layer. |
| 0017 | transports: POST `/api/tenants/{tenant_id}/governance/virtual-keys` | `handlers.TenantVirtualKeyHandler` POST. Mints a governance-prefixed VK value if the caller omits one. Response carries the value for immediate inference use. |
| 0018 | transports/handlers integration test + e2e Layer-2 scaffold      | `handlers/mt_integration_test.go` boots a real SQLite ConfigStore via `NewConfigStore`, wires every multi-tenant handler, and drives the full happy path for two tenants with assertions at store + VK resolver + Manager.Acquire layers. `test/e2e/README.md` documents the kind/Helm Layer 2. |
| 0019 | transports+configstore: POST `/api/tenants/{tenant_id}/mcp/clients` | `ConfigStore.GetMCPConfigByTenant` + `CreateMCPClientConfigForTenant` (composite uniqueness on `(tenant_id, name)`). Tenant loader in `buildInferenceRouter` now loads MCPConfig per-tenant so a fresh runtime sees its MCP clients. |
| 0020 | transports+configstore: GET list/single + DELETE on `/api/tenants/{tid}/providers/...` | Completes read+delete on providers. Legacy `DeleteProvider` now scopes to `DefaultTenantID` so multi-tenant data can't be wrong-deleted via the single-tenant call shape. |
| 0021 | transports+configstore: GET list/single + DELETE on `/api/tenants/{tid}/{governance/virtual-keys, mcp/clients}` | Mirror of 0020 for VKs and MCP clients. Delegate-then-verify on cross-tenant probes returns ErrNotFound. |
| 0022 | transports+configstore: per-tenant provider keys CRUD            | Nested under `/api/tenants/{tid}/providers/{provider}/keys`. `ConfigStore.GetProviderKeysForTenant` filters both the provider join AND the key rows by tenant_id (fixes a latent cross-tenant leak when two tenants have same-named providers). |
| 0023 | transports+configstore: PUT on tenant VK and MCP                 | `UpdateVirtualKeyForTenant`, `UpdateMCPClientConfigForTenant` with malicious-tenant-id-in-body defense. Handlers patch safe fields only (VK name/description/active; MCP disabled/connection_string/connection_type). |
| 0024 | transports+configstore: PUT on `/api/tenants/{tid}/providers/{provider}` | Refactored `UpdateProvider` through `updateProviderInternal(ctx, tenantID, ...)` — legacy single-tenant entry pins DefaultTenantID; tenant-scoped form filters the provider lookup + VKPC join + new TableKey rows by tenant_id. |
| 0025 | transports+configstore: tenant-scoped teams CRUD                 | Full CRUD under `/api/tenants/{tid}/governance/teams`. First governance-metadata entity (no runtime evictor — teams are tagging, not request-path state). |
| 0026 | transports+configstore: tenant-scoped customers CRUD             | Mirror of 0025 for customers under `/api/tenants/{tid}/governance/customers`. |
| 0027 | transports+configstore: tenant-scoped budgets + rate limits CRUD | Batched in one patch since both share the same shape. Routes under `/api/tenants/{tid}/governance/{budgets, rate-limits}`. Budget create validates `max_limit >= 0` and `reset_duration` via `ParseDuration`; rate limit create requires at least one of token / request limits. |

### Verification gates (all green at 0027)

- `make verify-patches` — strict `git am` replay of all 27 patches.
- `make clean-patches && make apply-patches` — round-trip rebuilds the
  workspace cleanly.
- `go vet` clean on handlers, lib, server, multitenant.
- Test suites green: `transports/bifrost-http/handlers`,
  `transports/bifrost-http/lib`, `transports/bifrost-http/server`,
  `multitenant`, `framework/configstore` (+tables), `plugins/governance`.

### Per-tenant admin surface matrix (post-0027)

The admin API is feature-complete for all nine entity groups. Every
entity ships under `/api/tenants/{tenant_id}/...` (platform-admin only
via the v1 AdminAuthz stub). Inference-plane entities additionally
evict the per-tenant runtime on mutate so the next request reloads.

| Entity                      | POST | GET list | GET single | PUT | DELETE | Runtime evict on mutate |
| --------------------------- | :--: | :------: | :--------: | :-: | :----: | :---------------------: |
| Tenants (`/api/platform/tenants`) |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           —             |
| **Inference plane** |       |          |            |     |        |                         |
| Providers                   |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           ✓             |
| Provider keys (nested)      |  ✓   |    ✓     |     ✓      |  —¹  |   ✓    |           ✓             |
| Virtual keys                |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           ✓             |
| MCP clients                 |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           ✓             |
| **Governance metadata** |       |          |            |     |        |                         |
| Teams                       |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           —²            |
| Customers                   |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           —²            |
| Budgets                     |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           —²            |
| Rate limits                 |  ✓   |    ✓     |     ✓      |  ✓  |   ✓    |           —²            |

¹ Provider keys rotate via POST + DELETE (avoids the cascade machinery in
the legacy `UpdateProviderKey`). Operator workflow: POST a new key with
a new `id`, smoke-test, DELETE the old one.

² Governance metadata is configuration data attached to other entities
(VKs reference budget/rate-limit/team/customer IDs), not request-path
state. A mutation shows up on subsequent admin reads + on any
inference-plane row whose pointer is updated.

## History: spike-patch cleanup (done)

The series was originally 18 patches; patches 0002, 0004, 0005, 0006 were
Phase 2 spike research — `cmd/spike` binaries, `SPIKE_FINDINGS.md`, and
the `plugins/governance` / `plugins/telemetry` / `prometheus/client_golang`
go.mod requires they pulled in for cost measurement. None of that code was
reachable from the inference path; the runtime
(`multitenant/{manager,runtime,tenant,account,resolver,configstore_resolver}.go`)
imports none of those deps. The findings live in
[[enterprise-multitenant-plan]] (memory) and the OSS
`feat/multitenant-spike` branch.

The cleanup commit dropped the four spike patches, renumbered the 14
survivors, and folded the `framework/configstore` go.mod require (originally
bundled with old spike patch 0005) into the resolver patch (new 0009),
where the dependency was actually introduced.

Old → new mapping:

| Old  | New  | Subject                                                          |
| ---- | ---- | ---------------------------------------------------------------- |
| 0001 | 0001 | multitenant: scaffold per-tenant Bifrost runtime registry        |
| 0003 | 0002 | multitenant: tests for Manager concurrency, eviction, …          |
| 0007 | 0003 | configstore: add TableTenant and customer.tenant_id              |
| 0008 | 0004 | configstore: extend tenant_id to teams, VKs, budgets, rate …     |
| 0009 | 0005 | configstore: extend tenant_id to providers, keys, MCP clients    |
| 0010 | 0006 | configstore: tenant-scope Name uniqueness                        |
| 0011 | 0007 | configstore: add TenantRepository CRUD                           |
| 0012 | 0008 | transports: add /api/platform/tenants admin handlers             |
| 0013 | 0009 | transports+multitenant: tenant resolver middleware + ConfigStoreVKResolver |
| 0014 | 0010 | transports: introduce BifrostRouter interface                    |
| 0015 | 0011 | configstore: add GetProvidersConfigByTenant                      |
| 0016 | 0012 | transports: TenantScopedAccount + MultiTenantRouter              |
| 0017 | 0013 | transports: wire MultiTenantRouter + resolver middleware         |
| 0018 | 0014 | transports/handlers: route inference through BifrostRouter       |

Dropped patches (no longer in the series): old 0002, 0004, 0005, 0006.

## Outstanding items

### Next up

The two priority items remaining now that patches 0001–0027 have landed:

1. **Layer 2 E2E** — kind cluster, Helm, Postgres, multi-replica HA,
   streaming/async/hot-reconfig. Design notes below; Layer 1 (in-process
   integration test exercising every multi-tenant handler) is already
   shipped at `transports/bifrost-http/handlers/mt_integration_test.go`.
2. **Phase 4 AuthzPlugin / OIDC** — replace the v1 `StubPlatformAdminAuthz`
   with a real, identity-aware authorization plugin so tenant-admins can
   call the admin API on their own tenant via `/api/tenants/{tid}/...`
   without first becoming platform-admins. Until this lands the
   per-tenant routes are platform-admin-only.

### Next-2: K8s-style E2E test harness

Goal: prove the whole stack — admin API, tenant resolver, multi-tenant
router, per-tenant runtime — works end-to-end with realistic infrastructure
and is isolation-correct.

Options for the harness:

1. **Go `testcontainers-go`** — Spin up Postgres + Redis + a Bifrost
   container, drive via HTTP from `go test`. Fastest feedback loop, runs
   on dev laptops and CI without a cluster. Doesn't validate Helm/k8s
   manifests.
2. **`kind` + a smoke-test script** — Boot a kind cluster, apply Helm /
   kustomize manifests, run a Go or shell driver. Validates manifests +
   real networking but slower (~60s cluster boot). CI runs need
   docker-in-docker or a kind GH Action.
3. **Both** — testcontainers for fast inner loop, kind for nightly
   integration. This is probably where we land long-term.

Test scenarios (must-have for v1):

- **Admin sanity.** POST `/api/platform/tenants` for tenant A and B.
  GET / list / DELETE each. Idempotency on retries.
- **Per-tenant config sanity.** As tenant-admin A, POST a provider + a VK.
  As tenant-admin B, do the same. Confirm A's GET of providers shows only
  A's providers (cross-tenant isolation at admin layer).
- **Per-tenant inference.** Using tenant A's VK, POST
  `/v1/chat/completions` — assert the request lands on tenant A's
  provider (mock provider that echoes a tenant marker). Repeat for tenant
  B. Assert tenant A's VK gets 401/403 if it tries to invoke a model only
  configured on tenant B.
- **Per-tenant MCP.** Configure an MCP client on tenant A only. Call
  `/v1/mcp/tool/execute` with tenant A's VK → success. Same call with
  tenant B's VK → 404 (tool not found / not configured for this tenant).
- **Streaming inference.** SSE `chat/completions stream=true` per tenant
  — confirm the streaming goroutine holds the per-tenant runtime for the
  full stream lifetime (covers the release-callback path added in patch
  0018).
- **Async inference.** POST `/v1/async/chat/completions` per tenant,
  retrieve job, assert the executor acquired the correct tenant's
  runtime at execution time (covers the closure-time Acquire pattern
  from patch 0018).
- **Hot reconfig.** PUT a new provider for tenant A via admin API while
  tenant A has active traffic; assert subsequent requests use the new
  config (validates the Manager eviction hook from Next-1).
- **HA shape.** Run 2 Bifrost replicas behind a simple LB; admin write
  on replica 1 must propagate to replica 2's runtime (validates
  cross-replica invalidation).

Layout proposal:

```
test/e2e/
├── README.md
├── go.mod                 # separate module, depends on the workspace
├── fixtures/
│   ├── mock-provider/     # tiny HTTP server impersonating an LLM provider
│   └── tenants.yaml       # seed configs for tenant A and B
├── helm/                  # Bifrost Helm chart (or kustomize overlay)
├── harness/
│   ├── testcontainers.go  # docker-based harness
│   └── kind.go            # kind-based harness, optional
└── scenarios/
    ├── admin_sanity_test.go
    ├── tenant_isolation_test.go
    ├── inference_per_tenant_test.go
    ├── mcp_per_tenant_test.go
    ├── streaming_per_tenant_test.go
    ├── async_per_tenant_test.go
    └── hot_reconfig_test.go
```

Open questions before we start the harness:

- **Helm chart status.** Does the OSS Bifrost repo already ship a Helm
  chart, or do we need to write one from scratch as part of this work?
  (Check `docs/deployment-guides/` first.)
- **Mock provider strategy.** Do we want a hand-written mock, or use an
  existing OpenAI-mock fixture from the OSS test suite?
- **CI integration.** Where does the e2e suite run — Gitlab pipeline as a
  separate stage, or a nightly job? Testcontainers is cheap enough to
  gate every MR; kind is probably nightly.

### Inference plane close-out items still pending

- **Layer 2 E2E coverage of the close-out scenarios.** Layer 1
  integration test exercises admin-side isolation; what's still missing
  is a real-server-binding test that drives `/v1/chat/completions`
  streaming (SSE goroutine holds the per-tenant runtime open through
  the full stream), `/v1/async/chat/completions` (closure-time
  Acquire), and a hot-reconfig flow (PUT a new provider mid-traffic,
  verify next inference picks up the change via the evictor).
- **Resolver cache invalidation.** Today the VK→tenant cache uses a 60 s
  TTL. For VK revocation latency we'll want Postgres `LISTEN`/`NOTIFY` or
  a versioned invalidation header from the admin API. The PUT/DELETE
  handlers already evict the per-tenant runtime, but the **resolver
  cache** on each replica is still independently TTL'd.
- **Realtime / WebSocket inference.** `wsrealtime.go`, `wsresponses.go`,
  `webrtc_realtime.go`, `realtime_client_secrets.go` still call
  `h.client.X` directly. They run real inference, so multi-tenant support
  here needs the same Acquire/release pattern plus a WebSocket-lifetime
  model for the handle (longer-lived than HTTP, can survive eviction
  pressure).
- **Tenant-scoping the legacy `/api/<entity>` route family.** Patch 0015
  shipped `LegacyAdminTenantScopeMiddleware` which lifts the caller's
  own tenant onto ctx, but the legacy handlers don't read it yet — they
  still operate on the global ConfigStore. Wiring them through requires
  modifying every legacy handler to filter by ctx tenant_id and is gated
  on Phase 4's AuthzPlugin resolving a real tenant-admin caller.

### Later phases (per the memory plan)

- **Phase 3 (parallel) — per-built-in plugin scope audit.** Decide for each
  built-in plugin whether it lives per-tenant or globally:
  - Likely per-tenant: governance, semantic cache, maxim, compat, prompts.
  - Likely global: telemetry, OTEL, logging-store.
- **Phase 4 — RBAC.** Add `AuthzPlugin` to OSS core; build enterprise impl
  with OIDC. Two-level model: platform-admin (cross-tenant) vs tenant-admin
  (within-tenant). UI feature flags.
- **Phase 5 — stateless HA hardening.** Force Postgres/Redis backends,
  3-replica soak test, document per-node rate-limit drift.

### Open risks worth spiking before deeper investment

- Per-tenant runtime memory budget (5–50 MB est) × LRU size = node memory
  ceiling. Cold-start latency on eviction unmeasured under realistic
  warm-tenant traffic.
- MCP subprocess explosion if per-tenant MCP managers spawn processes
  (current spike used in-process MCP only).
- `sync.Pool` benefit dilution across many runtimes.
- Plugin external connections (Redis / Postgres pools) need cross-tenant
  sharing to avoid FD exhaustion at scale.

## Workflow notes for contributors

- Source tree stays **vanilla** — all fork changes live as
  `git format-patch` files. Never hand-edit `.patch` files.
- Work in worktrees under `.workspaces/<name>`, generated by
  `make apply-patches`. Commit there, then `git format-patch` and copy to
  `f5xc-patches/patch1/multi-tenant/patches/`.
- Every commit needs `Ref: https://jira.f5net.com/browse/XC-25496`. No
  `Co-Authored-By` trailers on this fork.
- Before committing a patch series change, run `make verify-patches`
  (strict `git am` replay) and `make clean-patches && make apply-patches`
  (round-trip).
- Workspace builds use a workspace-local `go.work.local`; set
  `GOWORK=$(pwd)/go.work.local` when running `go test` / `go build` from
  inside `.workspaces/multi-tenant/`.
