# Plugin isolation — per-tenant plugin instances

**Status:** design doc backed by a POC unit test at
[`plugins/governance/tenant_isolation_poc_test.go`](../../plugins/governance/tenant_isolation_poc_test.go)
on 2026-06-17. Not yet wired into the production TenantLoader; not yet
emitted as a patch in `f5xc-patches/`. The POC validates the mechanism
is sound; this doc proposes the production wiring.

Continues the Phase 3 stretch from
[04-design-poc-to-v1.md §"Phase 3 — Plugin framework"](./04-design-poc-to-v1.md)
and closes the I10 stub in `examples/isolation/main.go`:
> *stateful plugins (semantic_cache, governance) get per-tenant
> instances — BifrostConfig.LLMPlugins is shared via ShareLLMPlugins
> shim, plugin state is global.*

---

## 1. The shape of the problem

Today every tenant runtime — `*bifrost.Bifrost` per tenant via
`multitenant.Manager.Acquire` — receives the **same** plugin instance list,
wrapped in the `multitenant.ShareLLMPlugins` shim
([multitenant/plugin_share.go:61-82](../../multitenant/plugin_share.go#L61-L82)).
The shim no-ops `Cleanup()` so a per-tenant `bifrost.Shutdown()` can't tear
down the singleton's worker pool, but every other hook is forwarded
verbatim to the **single shared instance**.

The instance holds tenant-scoped state masquerading as global state:

| Plugin | Per-tenant state | Where it lives |
|---|---|---|
| `governance` | VK cache, team/customer/budget/rate-limit maps, usage baselines, CEL routing programs | [plugins/governance/store.go:25-46](../../plugins/governance/store.go#L25-L46) |
| `semanticcache` | Cache keys (FIXED in patch15 by tenant-prefixing the key) | [plugins/semanticcache/main.go::resolveCacheKey](../../plugins/semanticcache/main.go) |
| `prompts` | Prompt versions + cached templates | `plugins/prompts/` |
| `telemetry` | Per-VK counters aggregated into Prometheus | `plugins/telemetry/` |

We solved semanticcache cheaply in patch15 by prefixing the cache key
with the tenant id — a workaround at the **key namespace** level. That
pattern doesn't generalize: governance's hierarchical resolver
(team → customer → org budgets) needs per-tenant *graph* state, not
just a key prefix. Likewise the routing engine's compiled CEL programs
are tenant-scoped policies.

The real fix is **per-tenant plugin instances**.

---

## 2. POC findings

The POC at
[`plugins/governance/tenant_isolation_poc_test.go`](../../plugins/governance/tenant_isolation_poc_test.go)
constructs two `GovernancePlugin` instances side-by-side from the same
seed config and exercises three scenarios:

1. **Two tenants, same budget id** — `BumpBudgetUsage` on instance A
   leaves instance B's snapshot untouched. The `sync.Map`s inside
   `LocalGovernanceStore` are per-instance; no key namespace collision
   even when admins copy templates across tenants.

2. **`UpsertBudgetConfig` preserves per-instance counters** — admin edit
   of `MaxLimit` on tenant A keeps tenant A's `CurrentUsage` AND leaves
   tenant B's whole snapshot unchanged.

3. **Full plugin instances, not just stores** — two `Init()`-constructed
   `*GovernancePlugin`s isolate end-to-end. `Cleanup()` on instance A
   does not affect B's background workers; B keeps serving after A is
   torn down.

All three tests pass in
`go test ./plugins/governance/... -run TestPluginIsolationPOC` (0.009s).

### Constraints surfaced by the POC

The POC succeeded, but the act of writing it exposed three real
constraints the production wiring must address:

#### Constraint 1 — `NewUsageTracker` uses `context.Background()` for workers

[`tracker.go:63`](../../plugins/governance/tracker.go#L63) creates the
background reset-worker context with `context.Background()`, not from
the plugin's tenant-scoped `Init` ctx. Two instances thus get two
independent worker pools (good for isolation), but the workers
themselves run with an unscoped context.

**Implication:** if/when the tracker's periodic reset worker does a DB
read through `configStore`, the GORM tenant-scope callback can't filter
by tenant — the ctx carries no tenant id. This is *fine* for the
per-tenant model **only if** every worker operation is bounded by an
already-tenant-scoped id (budget id, rate-limit id, VK id), because
those ids are unique across tenants by accident (UUID).

It is **not fine** if any worker ever does `gs.configStore.GetVirtualKeys(ctx)`
(unscoped enumeration) — that returns every tenant's data. The wiring
proposal in §3 addresses this by deriving the tracker's worker context
from a tenant-scoped parent.

#### Constraint 2 — `Init` has 8 dependencies; the loader needs them all

`governance.Init` takes
`(ctx, *Config, logger, configStore, *GovernanceConfig, *modelCatalog, *mcpCatalog, InMemoryStore)`
([main.go:132-141](../../plugins/governance/main.go#L132-L141)). The
`TenantLoader` closure at
[server/multitenant.go:104](../../transports/bifrost-http/server/multitenant.go#L104)
is built over `s` (the `BifrostHTTPServer`), so it has access to every
one of those handles via `s.Config` and friends — same as the root
server.go path that constructs the singleton today. No abstraction
needed; the loader just calls `governance.Init(scopedCtx, s.Config.GovernanceConfig, …)`
inline.

The only thing it doesn't have at construction time is `Config` (the
plugin's own `*governance.Config` — boolean flags like `IsVkMandatory`,
`IsEnterprise`). That comes from the root runtime's plugin-config map
keyed by plugin name. Easy to look up; not a real constraint, just
plumbing.

**Implication:** direct in-loader construction is straightforward.
There's no need to introduce a factory/closure abstraction or a new
`BifrostConfig` field — the loader knows enough at acquire-time to call
the constructor itself.

#### Constraint 3 — `InMemoryStore` is currently shared via the shim

`InMemoryStore` is a `governance.InMemoryStore` interface that wraps
the root bifrost config's provider registry
([server/plugins.go:85-93](../../transports/bifrost-http/server/plugins.go#L85-L93)).
It's a window into "what providers exist," used by the resolver to
short-circuit checks against unconfigured providers.

In the per-tenant model, each tenant runtime ALREADY has its own
`StaticAccount` snapshot scoped to its tenant
([multitenant/account.go](../../multitenant/account.go)). So the
per-tenant governance instance can read providers from the tenant's
own account rather than the global config. Whether this is a strict
improvement (tenant truly can't see other tenants' provider names) or
a regression (admins lose the "global view" for diagnostics) is a
trade-off called out in §3.

---

## 3. Proposed wiring

### 3.1 No OSS upstream interface change

Plugin instances are long-lived — one per tenant runtime, constructed at
`multitenant.Manager.Acquire` time and torn down via `Cleanup()` at
`EvictTenant`. That's the same lifetime as the runtime itself. There's
no per-request construction in this design.

So the existing `BifrostConfig.LLMPlugins []schemas.LLMPlugin` field is
sufficient. The TenantLoader already runs at the right lifecycle point
(once per tenant, with a tenant-scoped runtime ctx in hand). It builds
per-tenant plugin instances *directly* and passes them through the
standard `LLMPlugins` field.

**No `BifrostConfig` change. No new `bifrost.Init` code path. Zero OSS
upstream surface to maintain.** The per-tenant model is achieved entirely
by what the multitenant `TenantLoader` puts in `LLMPlugins`.

This is also why the `ShareLLMPlugins` shim
([multitenant/plugin_share.go:61-82](../../multitenant/plugin_share.go#L61-L82))
**goes away** for stateful plugins: it only existed to no-op `Cleanup()`
so a per-tenant `bifrost.Shutdown()` couldn't kill the singleton's
worker pool. With per-tenant instances, `Cleanup()` running on eviction
is the *correct* behavior — it's how the per-tenant tracker flushes its
in-memory budget snapshots back to the DB before the goroutine pool
exits. (Today's `tracker.Cleanup` already does
`DumpBudgets(context.Background(), nil)` at shutdown; per-tenant
instances inherit that for free.)

The shim still wraps any *stateless* shared plugins that genuinely want
process-global lifetime (logging, otel, jsonparser, compat). The opt-in
boundary is "is this plugin per-tenant or shared?" — set at TenantLoader
construction time, not deep in the plugin interface.

### 3.2 TenantLoader builds per-tenant plugin instances directly

In
[`server/multitenant.go::newTenantLoader`](../../transports/bifrost-http/server/multitenant.go#L104),
replace the current `LLMPlugins: ShareLLMPlugins(...)` line with a
direct construction:

```go
perTenantPlugins, err := s.buildPerTenantLLMPlugins(scopedCtx, tid)
if err != nil {
    return schemas.BifrostConfig{}, fmt.Errorf("multitenant: build per-tenant plugins for %q: %w", tid, err)
}

return schemas.BifrostConfig{
    Account:            acct,
    InitialPoolSize:    s.Config.ClientConfig.InitialPoolSize,
    DropExcessRequests: s.Config.ClientConfig.DropExcessRequests,
    LLMPlugins:         perTenantPlugins,
    MCPPlugins:         multitenant.ShareMCPPlugins(s.Config.GetLoadedMCPPlugins()),
    OAuth2Provider:     s.Config.OAuthProvider,
    Logger:             logger,
    KVStore:            s.Config.KVStore,
}, nil
```

`buildPerTenantLLMPlugins` is a small helper on the server that
iterates `s.Config.GetLoadedLLMPlugins()` and, for each entry:

- **Stateful & per-tenant** (governance, eventually prompts) — calls the
  plugin's constructor directly (e.g.
  `governance.Init(scopedCtx, cfg, logger, s.Config.ConfigStore, ...)`).
  `scopedCtx` already carries the tenant id, so `loadFromDatabase` filters
  via the GORM scope callback (patch2). Resulting instance goes into the
  returned slice unwrapped.
- **Stateless or singleton-OK** (logging, otel, semanticcache,
  jsonparser, compat, mocker) — wraps the root's instance with
  `ShareLLMPlugins`'s shim entry, same as today. `Cleanup` stays no-op
  for these.

The branching lives in `buildPerTenantLLMPlugins`. The classification is
intentionally a small switch table; adding prompts later (after that
plugin gets its own per-tenant constructor parity) is a one-line addition.

### 3.3 Constraint 1 fix: derive the tracker ctx from the plugin ctx

Change [`tracker.go:63`](../../plugins/governance/tracker.go#L63) from:

```go
tracker.trackerCtx, tracker.trackerCancel = context.WithCancel(context.Background())
```

to:

```go
tracker.trackerCtx, tracker.trackerCancel = context.WithCancel(ctx)
```

where `ctx` is the parent already passed into `NewUsageTracker`. The
parent comes from `Init`'s ctx, which in the per-tenant case carries
the tenant id, so worker DB reads pick up the GORM scope callback
automatically.

This is a one-line change that's safe even in OSS single-tenant mode
(parent ctx is `context.Background()` there → behaviour identical to
today).

### 3.4 Constraint 3 decision: per-tenant `InMemoryStore`

Recommended: per-tenant `InMemoryStore` reading from the tenant's own
`StaticAccount` rather than the global config. The tenant's runtime
already only ever sees its own providers (per
[02-stage2-runtime-isolation.md](./02-stage2-runtime-isolation.md));
hiding the provider name list from cross-tenant view is consistent.

Admin "global view" diagnostics already go through
`/api/platform/tenants` + the admin-tenant context, not through the
governance plugin, so this isn't an admin regression.

---

## 4. Scope: which plugins get per-tenant instances

| Plugin | Per-tenant? | Rationale |
|---|---|---|
| `governance` | **Yes** | Hierarchical state, audit-critical isolation. POC validates the model. |
| `semanticcache` | **No (already fixed)** | patch15 prefixes the cache key with the tenant id. No state graph, just key namespace. Cheaper than per-tenant instances. |
| `prompts` | **Yes** | Tenant-owned prompt versions; cross-tenant template visibility is a data leak. |
| `telemetry` | **Probably no** | Aggregates to Prometheus; per-tenant labels (added in patch6) already give isolation at the metric level. Going further means N metric registries, which is wasteful. |
| `mocker` | **No** | Stateless. |
| `logging` | **No** | Per-VK already, no shared graph. |
| `otel` | **No** | Per-request tracing; the span attributes already include tenant id (patch6). |
| `compat` | **No** | Translation only. |
| `jsonparser` | **No** | Stateless. |
| `maxim` | **TBD** | Holds a shared HTTP client to maxim.ai; isolation depends on whether maxim's API supports tenant-scoped tokens. Punt to a follow-up. |

So the **first** per-tenant plugin is governance (the POC target);
prompts is the natural second. Telemetry / maxim are deferred.

---

## 5. Patch series outline

Two patches, in order:

- **patch17 — `governance-tenant-init`**: tracker ctx derivation
  (Constraint 1 fix at [`tracker.go:63`](../../plugins/governance/tracker.go#L63)),
  per-tenant `InMemoryStore` (Constraint 3 decision). All-plugin-side;
  no TenantLoader wiring yet. The OSS single-tenant path stays
  bit-identical (tracker parent ctx is still `context.Background()`
  when called from server.go's root construction). Unit-tested by
  extending [tenant_isolation_poc_test.go](../../plugins/governance/tenant_isolation_poc_test.go)
  with a tracker-ctx-cancellation assertion (cancel the parent → workers
  exit).
- **patch18 — `tenant-loader-per-tenant-governance`**: add
  `buildPerTenantLLMPlugins` to
  `server/multitenant.go`, switch `newTenantLoader` to use it. The
  classifier wraps stateless plugins via `ShareLLMPlugins` (today's
  behaviour) and constructs stateful plugins via direct `Init` calls
  with the tenant-scoped ctx. Flips the singleton model to per-tenant
  for governance specifically; closes I10 for governance. Same
  classifier extends to prompts later.

No OSS upstream interface change in either patch — both modifications
stay inside the f5xc multitenant layer + the governance plugin.

Each patch follows the established overlay convention (source effects
committed to `f5xc-mt-overlay` + `f5xc-patches/patchN/` for downstream
consumers).

---

## 6. Open questions for review

- **Eviction lifecycle**: when `multitenant.Manager` evicts a tenant
  runtime, does the per-tenant plugin's `Cleanup()` flush in-flight
  usage updates before the goroutine pool dies? The POC test confirms
  `Cleanup()` is non-blocking for siblings but doesn't stress the
  "in-flight UsageUpdate vs Cleanup race" path. Likely fine because
  `tracker.Cleanup` already does
  `DumpBudgets(context.Background(), nil)` before exiting, but
  worth verifying with a focused test alongside patch19.

- **Cold-start cost**: per-tenant governance instances each build their
  own routing engine + CEL environment. CEL env construction is
  non-trivial (compiles AST grammar). For 100 tenants, that's 100 CEL
  envs. Mitigation: the `routingCELEnv` field is already a
  per-store singleton inside the store, so it's one per tenant rather
  than one per request — manageable. Bench it with the
  `mt-cold` scenario (see [bifrost-bench-and-mockllm memory](../../../.claude/projects/-home-sadams-projtmp-bifrost/memory/bifrost-bench-and-mockllm.md))
  once patch19 lands.

- **Telemetry**: should patch19 also emit per-tenant
  `governance_plugin_instances_total` so we can spot leaks (instances
  not getting cleaned up after evict)? Probably yes — cheap to add and
  the bench harness benefits from the visibility.
