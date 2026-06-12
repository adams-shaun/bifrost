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

### Phase 0 — multitenant package scaffold (spike → product)

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0001 | multitenant: scaffold per-tenant Bifrost runtime registry        | `multitenant/` Go module — `Manager` (lazy load + refcount + LRU), `VKResolver`, `StaticAccount`, runtime/tenant/account types.                |
| 0002 | multitenant: add cmd/spike binary and Phase 2 findings           | `multitenant/cmd/spike` binary + `SPIKE_FINDINGS.md` (per-tenant cost measurements — heap, goroutines, cold start).                            |
| 0003 | multitenant: tests for Manager concurrency, eviction, …          | 9 race-clean tests covering concurrent Acquire/Release, LRU eviction under refcount pressure, expiration.                                      |
| 0004 | multitenant: measure per-tenant cost with telemetry plugin       | Spike result: +telemetry plugin per tenant ≈ 36 KiB heap, 148 µs cold-start, 5 goroutines.                                                    |
| 0005 | multitenant: measure per-tenant cost with governance plugin      | Spike result: per-tenant SQLite — 888 KiB / 60 ms (rejected). Shared SQLite — 24 KiB / 178 µs (accepted).                                     |
| 0006 | multitenant: shared-configstore governance spike validation      | End-to-end harness proving shared store + per-tenant resolver/tracker/engine is viable.                                                       |

### Phase 1 — ConfigStore schema (tenant_id everywhere)

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0007 | configstore: add TableTenant and customer.tenant_id              | New `bf_tenants` table; `DefaultTenantID = "default"`; gormigrate migration adding tenant_id to TableCustomer with backward-compat default.    |
| 0008 | configstore: extend tenant_id to teams, VKs, budgets, rate …     | `tenant_id` on TableTeam, TableVirtualKey, TableBudget, TableRateLimit + migration.                                                            |
| 0009 | configstore: extend tenant_id to providers, keys, MCP clients    | `tenant_id` on TableProvider, TableKey, TableMCPClient + migration.                                                                            |
| 0010 | configstore: tenant-scope Name uniqueness on providers, keys, …  | Composite unique indexes `(tenant_id, name)` so two tenants can both have a provider named "openai".                                          |
| 0011 | configstore: add TenantRepository CRUD on ConfigStore            | `TenantRepository` interface + GORM impl: Create/Get/List/Update/Delete tenants.                                                              |

### Phase 2 — Admin API + tenant resolution

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0012 | transports: add /api/platform/tenants admin handlers             | REST CRUD at `/api/platform/tenants` for platform admins to create/list/update/delete tenants.                                                 |
| 0013 | transports+multitenant: tenant resolver middleware + …           | `ConfigStoreVKResolver` (VK→tenant lookup via repo), fasthttp middleware that resolves tenant_id from the request VK and lifts it into ctx.   |

### Phase 3 — Per-request runtime dispatch

| #    | Subject                                                          | What it adds                                                                                                                                  |
| ---- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 0014 | transports: introduce BifrostRouter interface + SingleTenant…    | `BifrostRouter.Acquire(ctx) → (*bifrost.Bifrost, release, error)`. `SingleTenantRouter` returns the shared client with a no-op release.       |
| 0015 | configstore: add GetProvidersConfigByTenant for per-tenant …     | Tenant-scoped provider config loader the per-tenant runtime uses to populate its Account.                                                     |
| 0016 | transports: TenantScopedAccount + MultiTenantRouter + ctx …      | `TenantScopedAccount` wraps the OSS Account interface with a tenant_id; `MultiTenantRouter` reads tenant_id from ctx, falls through to `FallbackTenant` then `FallbackClient`. |
| 0017 | transports: wire MultiTenantRouter + resolver middleware into …  | `buildInferenceRouter` in server.go picks SingleTenantRouter or MultiTenantRouter based on `BIFROST_MULTI_TENANT_ENABLED`.                    |
| 0018 | transports/handlers: route inference through BifrostRouter       | All HTTP inference entry points (`inference.go`, `asyncinference.go`, `mcpinference.go`) now `router.Acquire(ctx) + defer release` instead of `h.client.X`. Streaming threads release through `handleStreamingResponse` for SSE goroutine lifetime; async closures acquire at execution time. |

### Verification gates (all green at 0018)

- `make verify-patches` — strict `git am` replay of all 18 patches.
- `make clean-patches && make apply-patches` — round-trip rebuilds the
  workspace cleanly.
- `go vet` clean on handlers, lib, server, multitenant.
- Test suites green: `transports/bifrost-http/handlers`,
  `transports/bifrost-http/lib`, `transports/bifrost-http/server`,
  `multitenant`, `framework/configstore` (+tables), `plugins/governance`.

## Planned cleanup: drop spike patches (0002, 0004, 0005, 0006)

Audit (confirmed by `grep` against the worktree at HEAD `ec762957e`):

| Patch | Touches                                                                                    | Load-bearing?                  |
| ----- | ------------------------------------------------------------------------------------------ | ------------------------------ |
| 0002  | `multitenant/SPIKE_FINDINGS.md`, `multitenant/cmd/spike/main.go`                           | No — research artifacts only.  |
| 0004  | spike binary + `multitenant/go.mod` (+ telemetry plugin require), `multitenant/go.sum`     | No — measurement only.         |
| 0005  | spike binary + `multitenant/go.mod` (+ governance plugin require), `multitenant/go.sum`    | No — measurement only.         |
| 0006  | spike binary + `multitenant/cmd/spike-shared/main.go`                                      | No — research artifact only.   |

Runtime code (`multitenant/{manager,runtime,tenant,account,resolver,configstore_resolver}.go`)
does **not** import `plugins/governance`, `plugins/telemetry`, or
`prometheus/client_golang` — only the spike binaries do. The findings are
preserved in [[enterprise-multitenant-plan]] (memory) and the OSS
`feat/multitenant-spike` branch.

### Cleanup procedure

1. In `.workspaces/multi-tenant/`, drop the spike commits (rebase -i over
   the four commits or reset to before-spike and re-cherry-pick the rest).
2. Regenerate the series — old 0003 becomes new 0002, old 0007 becomes
   new 0003, and so on. After renumbering the 18-patch series becomes a
   14-patch series:

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

3. Trim `multitenant/go.mod`: drop `plugins/governance`, `plugins/telemetry`,
   and the `prometheus/client_golang` direct require. `go mod tidy` in the
   workspace will prune the indirect transitives.
4. Verify: `make verify-patches && make clean-patches && make apply-patches`
   plus the full test battery.
5. Commit the renumbered series to `f5xc-patches/patch1/multi-tenant/patches/`
   and push.

Risk: zero — the spike code never wired into the inference path. Cost: one
careful rebase. Schedule before any new feature patches so subsequent
numbering stays stable.

## Outstanding items

### Next up

The two priority items for the next sprint:

1. **Per-tenant admin interface across the board** — every entity that has
   admin endpoints today needs a tenant-scoped form. Scope and design notes
   below.
2. **K8s-style E2E test harness** — boot Bifrost + Postgres + Redis,
   provision multiple tenants via the admin API, drive inference + MCP on
   each, assert cross-tenant isolation. Design notes below.

### Next-1: Per-tenant admin interface

The existing OSS admin handlers operate on a single global ConfigStore —
they don't filter by `tenant_id`. Patch 0012 added platform-scope CRUD at
`/api/platform/tenants`; the parallel work is to tenant-scope the rest of
the admin surface.

Entities that need tenant-scoped admin endpoints (each entity already has a
`tenant_id` column from patches 0007–0009; the work is at the handler
layer):

| Entity         | Current single-tenant route                        | Tenant-scoped form                                  |
| -------------- | -------------------------------------------------- | --------------------------------------------------- |
| Providers      | `/api/providers`                                   | `/api/tenants/{tenant_id}/providers`                |
| Provider keys  | `/api/providers/{provider}/keys`                   | `/api/tenants/{tenant_id}/providers/{provider}/keys`|
| MCP clients    | `/api/mcp/clients`                                 | `/api/tenants/{tenant_id}/mcp/clients`              |
| Customers      | `/api/customers`                                   | `/api/tenants/{tenant_id}/customers`                |
| Teams          | `/api/teams`                                       | `/api/tenants/{tenant_id}/teams`                    |
| Virtual keys   | `/api/virtual-keys`                                | `/api/tenants/{tenant_id}/virtual-keys`             |
| Budgets        | `/api/budgets`                                     | `/api/tenants/{tenant_id}/budgets`                  |
| Rate limits    | `/api/rate-limits`                                 | `/api/tenants/{tenant_id}/rate-limits`              |

Design constraints:

- **Authorization layers.** Two callers: platform-admin (can target any
  `tenant_id`) and tenant-admin (locked to their own tenant_id resolved
  from the calling VK). The middleware that resolves the calling identity
  must reject tenant-admin requests that target a different tenant_id
  than they belong to. Phase 4's AuthzPlugin is the right home for this
  policy — we can ship the routes with a stub authz check first and swap
  it in later.
- **Repo-layer changes.** Most repos already accept `tenant_id`; audit
  needed to confirm every Read/List filters by it and every Create
  enforces it. Patch 0010 already gave us composite `(tenant_id, name)`
  uniqueness so name collisions across tenants are fine.
- **UI surface.** The bundled UI assumes a single tenant. v1 can punt on
  UI changes — operators use the API directly. Phase 4 (RBAC + UI) will
  add a tenant picker.
- **Runtime invalidation.** When a tenant's provider/MCP/VK config changes
  via the admin API, the per-tenant Bifrost runtime in the registry must
  be evicted (or reloaded) so the next request picks up the new config.
  Today `Manager` has no hook for this; we'll need to add an `Evict(tenantID)`
  call and wire it into every mutating admin handler. Possibly: pub/sub
  via the existing dlock primitives so multi-replica deployments evict in
  sync.

Patch breakdown (rough):

- One patch per entity-group (providers, governance entities, MCP) keeps
  diffs reviewable.
- One patch wiring `Manager.Evict` into the mutation paths.
- One patch adding the AuthzPlugin stub + tenant-admin-vs-platform-admin
  middleware split.

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

### In scope for the HTTP REST inference plane (close-out for phase 3)

- **End-to-end integration test.** Unit tests cover each layer in isolation;
  no test yet exercises the full
  middleware → `MultiTenantRouter.Acquire` → handler → release flow with a
  real tenant_id and a multi-tenant ConfigStore. Need a fixture that boots
  two tenants and asserts cross-tenant request isolation.
- **Verify `*HandlerWithRouter` wiring is actually used.** Patch 0018 added
  the constructors and the handlers consume the router, but server.go should
  be re-read end-to-end to confirm `BIFROST_MULTI_TENANT_ENABLED=1` causes
  `NewAsyncHandlerWithRouter` / `NewMCPInferenceHandlerWithRouter` /
  `NewCompletionHandlerWithRouter` to be invoked (not just constructed).
- **Resolver cache invalidation.** Today the VK→tenant cache uses a 60 s
  TTL. For VK revocation latency we'll want Postgres `LISTEN`/`NOTIFY` or
  a versioned invalidation header from the admin API.

### Out of scope of patch 0018 — separate patches needed

- **Realtime / WebSocket inference.** `wsrealtime.go`, `wsresponses.go`,
  `webrtc_realtime.go`, `realtime_client_secrets.go` still call
  `h.client.X` directly. They run real inference, so multi-tenant support
  here needs the same Acquire/release pattern plus a WebSocket-lifetime
  model for the handle (longer-lived than HTTP, can survive eviction
  pressure).
- **Admin handlers.** `providers.go` and `mcp.go` operate on the fallback
  single-tenant runtime. For SaaS, admin operations must be tenant-scoped
  so a tenant-admin can only see/edit their own providers and MCP clients.
- **Platform-admin vs tenant-admin authorization.** Patch 0012 added the
  CRUD endpoints; no AuthzPlugin yet enforces that only platform-admins
  can call `/api/platform/tenants`. Phase 4 work.

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
