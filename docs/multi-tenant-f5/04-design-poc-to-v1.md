# Multi-tenant Bifrost (f5xc) — design from API down to runtime, plus path to v1 EA

> **Status:** living design doc. Patches 1-10 land the POC; sections 4.2-4.4
> list the remaining known work to get to an EA-quality v1 build.
>
> **Reading order:**  section 0 (TL;DR) → section 1 (API) → 2 (data) →
> 3 (runtime) → 4 (patch + TODO summary). The detailed "things we
> missed" runbook is the companion doc
> [03-things-we-missed.md](./03-things-we-missed.md); read alongside
> sections 2 and 3 for the actual failure modes that motivated each
> decision.

---

## 0. TL;DR

Bifrost upstream is a single-process, single-tenant LLM gateway: one
`*bifrost.Bifrost` engine reads one shared `Account` (provider configs
+ keys), holds one MCP manager, runs one plugin chain. This fork adds
**header-driven multi-tenancy** that preserves that shape:

```
┌── Edge ───────┐   ┌── HTTP / middleware ────┐   ┌── Data plane ─────────────┐
│ x-f5xc-tenant │ → │  TenantResolver         │ → │  GORM scope callback      │
│  on request   │   │  TenantDispatcher       │   │   WHERE tenant_id = ?     │
└───────────────┘   │  (per-request *Bifrost) │   │  INSERT stamps tenant_id  │
                    └─────────────────────────┘   │                            │
                              │                   │  multitenant.Manager       │
                              ▼                   │   LRU of per-tenant        │
                       inference handler          │   *bifrost.Bifrost runtimes│
                       reads bf from ctx          └────────────────────────────┘
```

Three layers of isolation, each independent:

| Layer | Mechanism | Patch series |
|---|---|---|
| Data scope (rows) | GORM tenant-scope callback on `Query`/`Update`/`Delete`/`Create` + explicit handler stamps + `BeforeCreate` strict-mode assertions | 1, 2, 4, 9, 10 |
| Runtime isolation (engines) | `multitenant.Manager` with LRU+refcount; per-tenant `StaticAccount` snapshot; admin-write eviction | 7 |
| Admin/UI surface | Tenant directory CRUD at `/api/tenants`; UI tenant picker stamps the header on every Redux call | 3, 5, 8 |

Single-tenant deployments: header absent → callback short-circuits →
runtime falls back to root → behavior **identical to OSS Bifrost**.

---

## 1. API surface — the header carries tenancy

### 1.1 The choice: `x-f5xc-tenant` header, not URL path

The earliest spike of this work used URL-path tenancy (the strawman:
`POST /api/tenants/{tid}/providers`, etc.). That branch landed
**~42 patches** because every API handler — providers, keys, virtual
keys, MCP, governance, batch, file, async, integrations, etc. — got a
new sibling route with `tid` extracted from the path and threaded
through. It tripled the API surface area and made upstream re-syncs
expensive: any time OSS Bifrost adds a route, the path-based fork has
to add the tenant-scoped sibling.

The header model gets the same isolation in 6 patches that **don't
touch existing handler signatures**:

```go
// Single new middleware in the inference + admin chain.
// All existing handler code is unchanged.
tenantMW := handlers.TenantResolverMiddleware(disableDefaultTenant)
chained := append([]schemas.BifrostHTTPMiddleware{tenantMW, dispatchMW},
                  middlewares...)
```

The resolver:
1. Reads `x-f5xc-tenant` off the inbound request.
2. Stamps it onto `*fasthttp.RequestCtx` under both the typed
   `multitenant.BifrostContextKeyTenantID` key AND the stringified
   form (fasthttp UserValue stores by `interface{}` equality, so
   typed-key lookups miss string-key stores — both forms guarantee
   downstream `tenantFromContext(ctx)` lookups hit).
3. Returns 400 if the header is missing AND
   `BIFROST_DISABLE_DEFAULT_TENANT_CONFIG=true` (the strict-mode
   safeguard for production multi-tenant deployments).
4. Exempts the admin surface from that block: `/api/platform/*`
   (legacy) and `/api/tenants*` (canonical) — admin clients need to
   create tenants without already having one.

### 1.2 Wire shape

```
POST /api/governance/virtual-keys HTTP/1.1
x-f5xc-tenant: acme
authorization: Basic admin:...
content-type: application/json

{ "name": "engineering-vk", "team_id": "...", ... }
```

Inference path:

```
POST /v1/chat/completions HTTP/1.1
x-f5xc-tenant: acme
authorization: Bearer sk-bf-<acme-vk>
content-type: application/json

{ "model": "openai/gpt-4o-mini", ... }
```

The bearer token (virtual key) is itself globally unique; the gateway
*could* resolve tenant by VK lookup. We chose to require the header
anyway because:

- The edge (Envoy / cloudflared / whatever rewrites traffic) already
  knows the tenant before the request even reaches the gateway —
  injecting the header is cheap. Doing a DB lookup on VK→tenant for
  every inference adds DB pressure and a startup race for cold
  per-tenant runtimes (you need the tenant id to Acquire the runtime
  that owns the VK).
- It keeps the auth path (VK→authorization) and the routing path
  (tenant→engine selection) orthogonal. Tomorrow's per-team VK
  collisions don't break dispatch.
- Single-tenant OSS deployments don't carry the header, the callback
  short-circuits, behavior is identical to upstream.

### 1.3 Admin surface and exemptions

The `/api/tenants` surface is the **only** part of the API that's
explicitly NOT tenant-scoped. A platform admin reading the tenant
directory needs to see every tenant, not just their own. Two
exemptions encode this:

- `TenantResolverMiddleware.isAdminPath` returns true for
  `/api/platform/*` and `/api/tenants*`, so requests without a
  header are allowed even when default-tenant writes are otherwise
  blocked.
- The GORM scope callback's `hasTenantColumn` check returns false for
  the `tenants` table (it doesn't carry `tenant_id` — it IS the
  tenant), so the callback's WHERE is never appended on tenant CRUD.

### 1.4 Tradeoffs and known limitations

| Pro | Con |
|---|---|
| 6 patches vs ~42; every OSS handler keeps its signature | Tenancy isn't visible in URLs → harder to do route-level RBAC at the edge |
| Easy to re-sync from `maximhq/bifrost` upstream | A misconfigured edge that doesn't strip cross-tenant headers is a silent bypass — relies on edge correctness |
| Single resolver, single exemption list, single test surface | Logs need an explicit `tenant_id` field (patch6 does this) — URLs would have made it grep-trivial |
| Header is opt-in: zero overhead for single-tenant OSS deployments | Inference clients with `curl` can spoof the header in a misconfigured deployment. Edge MUST strip incoming `x-f5xc-tenant` and re-inject. |

---

## 2. Object store — shared DB, request context layered around `core`

### 2.1 The architecture in one paragraph

There's still one `*gorm.DB` and one config schema. The multi-tenant
overlay adds (a) a `tenant_id` column to 8 tables, (b) a single GORM
callback registration that scopes `SELECT`/`UPDATE`/`DELETE` by
`WHERE tenant_id = ?` and stamps `INSERT` with the tenant from the
request context, and (c) per-model `BeforeCreate` hooks as a
strict-mode safety net. Three independent layers; each catches what
the others miss.

```
                            ┌── Layer 1 — explicit handler stamps ──┐
                            │   createVirtualKey():                   │
                            │     vk.TenantID = TenantIDFromCtx(ctx)  │
                            │   AddProvider() / etc. — same           │
                            └─────────────────────────────────────────┘
                                              │
                                              ▼
                            ┌── Layer 2 — GORM scope callback ──┐
                            │   Before("gorm:before_create"):     │
                            │     fill TenantID from ctx if empty │
                            │   Before("gorm:query"/update/delete):│
                            │     append WHERE tenant_id = ?      │
                            └─────────────────────────────────────┘
                                              │
                                              ▼
                            ┌── Layer 3 — BeforeCreate strict assert ┐
                            │  EnsureTenantIDOnCreate(&f, tableName):  │
                            │   if BIFROST_ASSERT_TENANT_ON_INSERT     │
                            │     and *f == "": error                  │
                            │   else (non-strict): fill with "default" │
                            └──────────────────────────────────────────┘
                                              │
                                              ▼
                                       INSERT runs
```

### 2.2 Schema before — single tenant by accident

The 8 OSS tables that hold per-deployment config:

```
config_providers       PK id (auto), UNIQUE(name)
config_keys            PK id, FK provider_id, UNIQUE(name) [global!]
config_mcp_clients     PK id, UNIQUE(client_id)
governance_customers   PK id, UNIQUE(name)
governance_teams       PK id, FK customer_id, UNIQUE(name)
governance_virtual_keys PK id, FK team_id|customer_id|access_profile_id,
                       UNIQUE(name), UNIQUE(value)
governance_budgets     PK id, FK virtual_key_id|team_id|customer_id|...
governance_rate_limits PK id, same FKs as budgets
```

Every uniqueness is global. Two tenants can't both have a provider
named `"openai"`. Two tenants can't both name their primary VK
`"production"`. No tenant column exists; nothing tells the DB or the
handlers that rows belong to a tenant.

### 2.3 Schema after — additive, idempotent, single migration

patch1's consolidated migration:

1. Creates a new `tenants` table (id, name, status, timestamps).
2. Seeds the row `(id='default', name='Default Tenant', status='active')` —
   single-tenant deployments and pre-upgrade rows belong here.
3. Adds a `tenant_id varchar(255) NOT NULL DEFAULT 'default'` column
   to each of the 8 tables, backfilling existing rows to `'default'`.
4. Adds composite `(tenant_id, name)` unique indexes alongside the
   legacy single-column ones:

| Table | Composite index added |
|---|---|
| `config_providers` | `idx_providers_tenant_name` |
| `config_keys` | `idx_key_tenant_name` |
| `config_mcp_clients` | `idx_mcp_tenant_name` |
| `governance_virtual_keys` | `idx_virtual_keys_tenant_name` |
| `governance_teams` | `idx_governance_teams_tenant_name` |

Note: `governance_budgets`, `governance_rate_limits`, and
`governance_customers` only get the column + index, not composite
uniqueness — they're addressed by FK or by id.

Per-row stamping at the struct level:

```go
type TableProvider struct {
    ID                       uint      `gorm:"primaryKey;autoIncrement"`
    Name                     string    `gorm:"type:varchar(50);uniqueIndex:idx_providers_tenant_name;not null"`
    TenantID                 string    `gorm:"type:varchar(255);not null;default:default;uniqueIndex:idx_providers_tenant_name"`
    // ...
}
```

The `default:default` matters: GORM pre-fills the field to the literal
string `"default"` BEFORE callbacks fire. This was actually the source
of bug class #5 (see [03-things-we-missed.md](./03-things-we-missed.md))
because the original `setTenantIfEmpty` only overrode `""`, not
`"default"`. The fix made the callback treat both as overridable.

### 2.4 Migration is idempotent and reversible-in-spirit

Each step in patch1's migration is wrapped with
`HasColumn`/`HasIndex` guards so re-running on a partially-migrated DB
is safe. The rollback is intentionally a **no-op**: rolling back the
`tenant_id` column without also reverting the resolver middleware
would break request routing, so the operator must coordinate a code
rollback first. This is documented inline.

The migration **explicitly leaves the legacy single-column unique
indexes in place** — including `idx_key_name ON config_keys(name)`.
The reason was upstream-compat: OSS upsert sites (config.json sync)
target the global name as the upsert key. The cost is documented in
[03-things-we-missed.md #8](./03-things-we-missed.md#8) — two tenants
cannot have keys with the same name. The seed driver works around
this by tenant-prefixing key names. The proper v1 fix is to drop the
legacy index AND patch the OSS upsert sites to target the composite.

### 2.5 Read scoping via GORM callback

```go
db.Callback().Query().Before("gorm:query").
    Register("multitenant:scope_query", scopeQueryByTenant)
db.Callback().Update().Before("gorm:update").
    Register("multitenant:scope_update", scopeQueryByTenant)
db.Callback().Delete().Before("gorm:delete").
    Register("multitenant:scope_delete", scopeQueryByTenant)
db.Callback().Create().Before("gorm:before_create").  // patch9: was "gorm:create"
    Register("multitenant:scope_create", populateTenantIDOnCreate)
```

`scopeQueryByTenant`:

```go
func scopeQueryByTenant(tx *gorm.DB) {
    if tx.Error != nil || !hasTenantColumn(tx) { return }
    tid, ok := tenantFromContext(tx.Statement.Context)
    if !ok { return }                                  // single-tenant fallback
    tx.Statement.Where(qualifyTenantColumn(tx)+" = ?", tid)
}
```

`qualifyTenantColumn` uses the primary table name to disambiguate
JOINs: `config_providers.tenant_id = ?` instead of bare `tenant_id`,
so a JOIN involving two tenant-scoped tables doesn't throw "ambiguous
column name".

### 2.6 Misses that surfaced in this area

The detailed write-up is in
[03-things-we-missed.md](./03-things-we-missed.md). Quick map back to
this section:

| Miss | Section / mechanism | Patch |
|---|---|---|
| Callback fired AFTER per-model `BeforeCreate` → strict-mode 500 on nested INSERTs (budgets / rate-limits inside `createCustomer`) | Section 2.1 Layer 2 vs Layer 3 ordering | patch9 |
| Tenant DELETE didn't cascade child rows → orphans collide on re-create | Section 2.3 — no cascade implementation in the original tenant CRUD | patch8 |
| JOIN-based reads (`GetProviderKeys`) returned cross-tenant rows because the callback's WHERE only restricted the primary table | Section 2.5 — `qualifyTenantColumn` was designed for SQL correctness (no ambiguous column), NOT for joined-side scoping | patch10 |
| Sibling JOINs (`getProviderKeyByName`, `GetVirtualKeyMCPConfigsByMCPClientStringIDs`, `GetVirtualKeysPaginated` subquery) have the same risk class | Section 2.5 — pattern enumerated by audit on 2026-06-16 | TODO: patch11, patch12 (see section 4.2) |
| `UpdateProvider` writes via the SHARED `h.inMemoryStore` instead of `h.dbStore` → second tenant updating "openai" trips global `idx_key_id` | Section 2.1 Layer 1 — handler should use dbStore directly in multi-tenant mode | TODO (see section 4.2) |
| Legacy global `idx_key_name` forces tenant-prefixed key names | Section 2.4 — left in place by design | TODO (see section 4.2) |

---

## 3. In-memory & runtime — per-tenant `*bifrost.Bifrost`

### 3.1 OSS Bifrost in one diagram

Stock Bifrost has a single engine per process:

```
                          ┌──────────────── *bifrost.Bifrost ────────────────┐
                          │                                                   │
   admin writes ────────► │  Account (single)                                │
   /api/providers/...     │   ├─ GetConfiguredProviders()                    │
   /api/governance/...    │   ├─ GetKeysForProvider(p) — reads in-memory map │
                          │   └─ GetConfigForProvider(p)                     │
                          │                                                   │
   inference request ───► │  Provider workers (per provider, fixed pool)     │
   /v1/chat/completions   │  Provider queues (per provider)                  │
                          │  Concurrency budget (request-side semaphore)     │
                          │                                                   │
                          │  LLMPlugin chain                                  │
                          │   governance → logging → telemetry → ...          │
                          │  MCPPlugin chain + MCPManager (singleton)         │
                          │                                                   │
                          │  OAuth2Provider, KVStore, Logger (singletons)     │
                          └───────────────────────────────────────────────────┘
```

The `Account` interface is the choke point:

```go
type Account interface {
    GetConfiguredProviders() ([]ModelProvider, error)
    GetKeysForProvider(p ModelProvider) ([]Key, error)
    GetConfigForProvider(p ModelProvider) (*ProviderConfig, error)
}

type BifrostConfig struct {
    Account            Account
    LLMPlugins         []LLMPlugin
    MCPPlugins         []MCPPlugin
    OAuth2Provider     OAuth2Provider
    Logger             Logger
    KVStore            KVStore
    InitialPoolSize    int
    DropExcessRequests bool
}
```

The OSS account impl (`lib.BaseAccount`) reads from a shared in-memory
`lib.Config.Providers map[ModelProvider]ProviderConfig`. That map is
populated at boot from the DB and updated in-memory on every admin
write. In single-tenant deployments this is fine — one tenant, one
map, last-write IS the right answer. In multi-tenant deployments it's
**last-write-wins across tenants**: tenant A's `openai` provider with
its `base_url` and `api_key` gets stomped by tenant B's `openai` with
different config.

### 3.2 Why data-scope alone isn't enough

After patches 1-6 (data-scope isolation only):

- ✅ `GET /api/providers` correctly returns *only* tenant A's
  providers (the GORM callback adds the WHERE).
- ✅ `POST /api/providers` correctly inserts with
  `tenant_id = 'A'` (handler stamps explicitly + scope callback
  fills the column).
- ❌ `POST /v1/chat/completions` for tenant A's request dispatches
  through the SAME `*bifrost.Bifrost` as tenant B's request. The
  `Account.GetKeysForProvider("openai")` reads the shared
  `lib.Config.Providers` map. Whoever wrote `openai` last sets
  the routing for *everyone*.

The data is isolated. The execution isn't.

### 3.3 patch7 — `multitenant.Manager` and per-tenant runtimes

The fix is to give each tenant its own `*bifrost.Bifrost`, lazily
loaded on first inference and refcounted so eviction blocks on
in-flight requests. The manager and the per-tenant `StaticAccount`
both shipped in patch1 but weren't wired into the inference path until
patch7.

```
                            ┌─── multitenant.Manager ───────────────┐
TenantDispatcherMiddleware  │                                       │
  bf, release :=            │  runtimes: LRU<TenantID, *bifrost>    │
    s.BifrostFor(ctx,tid) ─►│   - refcounted via Acquire/Release   │
                            │   - LRU evicts when idle              │
                            │   - explicit Evict(tid) on admin write│
                            │                                       │
ctx.SetUserValue(           │  loader(ctx, tid) BifrostConfig:      │
  "__bifrost_client",       │   1. push tid onto ctx                │
   bf)                      │   2. dbStore.GetProvidersConfig(ctx)  │
defer release()             │      ↳ scope callback restricts       │
                            │   3. dbStore.GetProviderKeys(ctx,p)   │
                            │      for each provider                │
inference handler reads bf  │   4. build StaticAccount snapshot     │
  from ctx via              │   5. share root's plugin chain via    │
  lib.BifrostClientFromCtx  │      ShareLLMPlugins/ShareMCPPlugins  │
                            │   6. return BifrostConfig             │
                            │                                       │
                            │  Manager.Acquire calls bifrost.Init() │
                            │   → fresh engine, fresh workers,      │
                            │     fresh provider queues, fresh      │
                            │     concurrency budget                │
                            └───────────────────────────────────────┘
```

Inference dispatch becomes:

```go
// h is the inference handler with constructor-time h.client (root engine).
func (h *CompletionHandler) bf(ctx *fasthttp.RequestCtx) *bifrost.Bifrost {
    if bf := lib.BifrostClientFromCtx(ctx); bf != nil {
        return bf                  // per-tenant runtime from dispatcher
    }
    return h.client                // single-tenant fallback (no header)
}

// ...
resp, err := h.bf(ctx).ChatCompletionRequest(bifrostCtx, req)
```

Single-tenant deployments pay zero overhead: header absent → BifrostFor
returns the root client + no-op release → the inference path is
identical to OSS.

Async handlers are special — `executor.SubmitJob` fires
`go executeJob(...)` and returns immediately, so the closure runs in a
goroutine that outlives the request. If the dispatcher middleware's
`defer release()` fires on request return, the per-tenant runtime
could be evicted before the closure executes. The async handler holds
its own extra refcount across the goroutine boundary
(`acquireBifrost` → closure `defer release()` → explicit `release()`
on `SubmitJob` error path).

### 3.4 Eviction model: admin-write-driven

Inference paths NEVER trigger eviction (Acquire/Release are O(1) once
the runtime is loaded). Admin writes do, via a package-level hook:

```go
// server/multitenant.go::initializeManager
handlers.TenantEvict = s.EvictTenantRuntime

// handlers/providers.go (and provider_keys.go) — 6 mutation handlers
func (h *ProviderHandler) addProvider(ctx *fasthttp.RequestCtx) {
    defer EvictTenant(ctx)
    // ...
}
```

Over-eager eviction on error paths is acceptable: the next inference
re-loads from the DB, which is identical to the cold-start cost. Net
overhead per attempted admin write: one DB round-trip on the next
inference.

VK / team / customer mutations do **not** evict. None of them appear
in the `StaticAccount` snapshot (the snapshot is providers + keys
only). VK lookups happen via the governance plugin which goes direct
to the DB on every inference — already tenant-scoped via the GORM
callback.

### 3.5 Shared plugins — what we share and what it would take to isolate

This is the heaviest unsolved design question. Today, the per-tenant
runtime **shares** the root runtime's plugin chain via shims:

```go
return schemas.BifrostConfig{
    Account:    acct,           // PER-TENANT
    LLMPlugins: multitenant.ShareLLMPlugins(s.Config.GetLoadedLLMPlugins()),
    MCPPlugins: multitenant.ShareMCPPlugins(s.Config.GetLoadedMCPPlugins()),
    OAuth2Provider: s.Config.OAuthProvider,   // PER-PROCESS
    Logger:         logger,                   // PER-PROCESS
    KVStore:        s.Config.KVStore,         // PER-PROCESS
}
```

`ShareLLMPlugins` wraps each plugin so the per-tenant engine's
`Shutdown()` can't call `Cleanup()` on plugin state the root engine
still depends on. The forwarder is pure: hooks pass through to the
shared plugin instance; only `Cleanup` is a no-op.

**What we share, why we share it, and what it would take to make per-tenant:**

| Plugin | What it holds in-process | Why shared (today) | Cost of per-tenant isolation | Risk if NOT isolated |
|---|---|---|---|---|
| `logging` (`LoggerPlugin`) | Singleton log writer + DB connection. Log rows already carry `tenant_id` (patch6). | Pure I/O sink — splitting it would just open N DB connections. | Either (a) split the log store into N per-tenant tables, or (b) keep shared but enforce reader-side tenant scope (patch6 does this for `/api/logs`). | LOW — log rows are correctly tagged; only the *reader* surface needed scoping (done). |
| `telemetry` (`PrometheusPlugin`) | Prometheus collectors keyed by labels. patch6 added `tenant_id` label. | Same as logging — splitting collectors adds cardinality but no isolation. | Already done at the label level. | LOW — operators query Prometheus with `{tenant_id="acme"}`. |
| `governance` (`GovernancePlugin`) | `store` (DB-backed), `resolver` + `tracker` (in-memory budget counters + rate-limit windows), `engine` (routing rules). | VKs, budgets, rate-limits all live in DB with `tenant_id`. Resolver uses VK→entity DB lookups (tenant-scoped). The in-memory `tracker` counters are keyed by VK id (globally unique) so cross-tenant collisions can't happen — *by accident*. | If we ever change VK ids to be tenant-prefixed instead of UUIDs, this isolation breaks silently. Per-tenant plugin instances would close that gap. | MEDIUM — depends on VK id globality. Audit before changing the id scheme. |
| `semanticcache` (`Plugin`) | A vector store (shared by all tenants), per-request `streamAccumulators` + `cacheStates` (request-id keyed, ephemeral). Cache lookup key = `(provider, model, cache_key, request_hash, params_hash)`. | Single in-process vector store; shared embedding executor. | The cache lookup KEY does not include `tenant_id`. Two tenants that happen to both use the same explicit `cache_key` header would share entries. | **HIGH** if the plugin is enabled and clients control `cache_key`. Today it's opt-in (no caching when no key set), so single-tenant deployments are safe by default; multi-tenant deployments using the cache MUST inject `tenant_id` into the cache key (either via plugin patch or by appending `tenant_id` to the client-side cache_key header). |
| `mocker`, `jsonparser`, `prompts`, etc. | Per-plugin state. | Stateless (jsonparser) or process-scoped (mocker). | n/a or trivial. | LOW. |
| MCP (`MCPManager`, not technically a plugin but per-process) | Registry of named MCP clients, OAuth tokens, tool-execution sessions. | Singleton wired up at boot from `config_mcp_clients` (which IS tenant-scoped at the DB level). The in-memory registry is NOT tenant-scoped — two tenants creating an MCP client with the same name collide. | Tenant-scope the registry: keyed by `(tenant_id, name)` rather than `name`. Or: instantiate one MCPManager per tenant in the per-tenant runtime config. | MEDIUM — name collision is the symptom today (workaround: tenant-prefix MCP names). True cross-tenant tool execution leak hasn't been demonstrated but the surface is large enough to warrant audit. |

**What per-tenant plugin isolation would actually take:**

1. **Plugin interface change.** Today `BifrostConfig.LLMPlugins` is a
   flat `[]LLMPlugin`. Per-tenant plugin state needs either (a)
   per-tenant plugin instances at engine-construction time, or (b)
   a `WithTenant(tid)` constructor on each plugin that returns a
   tenant-scoped view. (a) is cleaner but means each tenant runtime
   pays per-plugin init cost on first Acquire; (b) requires plugin
   authors to opt-in.
2. **Cleanup semantics.** Per-tenant plugin instances need owners.
   When the per-tenant runtime is evicted, plugin Cleanup should fire
   for that tenant's instances — NOT for the shared ones. The
   share-shim today blocks Cleanup entirely because there's nothing
   to clean up at the per-tenant level.
3. **Boot cost.** Each new tenant runtime today does ~1ms of work
   (DB reads + StaticAccount construction). Per-tenant plugins add
   plugin-init time per tenant — for a cold tenant facing semantic
   cache + governance + telemetry, that's seconds of cold-start
   latency for the first inference.
4. **Shared resources.** The vector store backing the semantic cache,
   the Prometheus collector, the log writer — these are infra that
   the plugins HOLD POINTERS TO. Per-tenant plugin instances still
   need to share the infra; isolation is at the plugin's
   request-handling code path, not the storage layer.

**Recommended path for v1 EA:** keep the current
shared-with-share-shim model. Document the cache-key tenant
augmentation as an operator responsibility. Defer per-tenant plugin
instances to v1.1 unless the semantic-cache leak vector is acceptable
in customer deployments (it usually is — caching is opt-in and most
EA customers don't enable it).

---

## 4. POC patches + remaining work to v1 EA

### 4.1 Patches landed

| # | Series | What it does | Lines (rough) |
|---|---|---|---|
| 1 | `mt-foundation-schema` | multitenant package (Manager + StaticAccount + ShareLLMPlugins) + tenant_id columns on 8 tables + consolidated migration + seeded `default` tenant | ~1900 |
| 2 | `mt-data-scope-callback` | GORM scope callbacks (Query/Update/Delete/Create) + tenant_id propagation in rdb fallbacks | ~155 |
| 3 | `mt-http-resolver` | `TenantResolverMiddleware` + `/api/tenants` (originally `/api/platform/tenants`) + VK bearer auth | ~700 |
| 4 | `mt-handler-tenant-routing` | Provider/key handlers via tenant-scoped `dbStore` + customer-FK tenant guard + isolation regression test | ~700 |
| 5 | `mt-ui-tenant-picker` | Redux tenant slice + RTK Query header injection + sidebar picker + branding | ~520 |
| 6 | `mt-log-telemetry-tenant` | `tenant_id` on log rows + per-tenant log-reader scoping + tenant label on Prometheus metrics | ~700 |
| 7 | `mt-runtime-isolation` | Stage 2: `multitenant.Manager` wired into inference path, `h.bf(ctx)` dispatch helper across 48 sites, async refcount handling, admin-write eviction hooks | ~550 |
| 8 | `mt-tenant-cascade-delete` | Tenant DELETE cascades across all 8 tenant-scoped child tables in a single tx | ~40 |
| 9 | `mt-scope-callback-order` | Register `multitenant:scope_create` `Before("gorm:before_create")` (was `Before("gorm:create")`) so it fires before strict-mode `BeforeCreate` hooks | ~10 |
| 10 | `mt-getproviderkeys-scope` | Explicit `WHERE config_keys.tenant_id = ?` predicate in `GetProviderKeys` (the scope callback's qualified WHERE didn't follow the JOIN) | ~20 |
| 11 | `mt-getproviderkeybyname-scope` | Sibling fix for `getProviderKeyByName` JOIN: explicit `config_keys.tenant_id` predicate so the scope is independent of which side GORM treats as primary. Closes T1. | ~20 |
| 12 | `mt-vkmcp-and-vk-budget-scope` | Pins `.Model()` on `GetVirtualKeyMCPConfigsByMCPClientStringIDs` + adds `config_mcp_clients.tenant_id` predicate (T2); belt-and-suspenders predicate inside `GetVirtualKeysPaginated` budget subquery (T7). | ~45 |
| 13 | `mt-schemasync-exclusions` | Adds `tenant_id` / `source_id` to per-table `excludedGoFields` so `TestConfigSchemaSync` recognizes them as runtime-only. Closes T6. | ~12 |

Total: ~5,400 LOC of code; ~1,400 LOC of doc & migration commentary.

Companion artifact (separate repo): `go-bifrost-ai/examples/seed` —
end-to-end lifecycle tester. Creates 5 tenants, full-seeds 3 of them
(providers + keys + customer + team + VK), runs UPDATE on one,
DELETE on another, asserts the isolation matrix on the live cluster.
This driver is what caught patches 8, 9, 10 and the open items below.

### 4.2 Known TODO before EA — confirmed bugs

| # | Item | Severity | Where | Status |
|---|---|---|---|---|
| ~~T1~~ | ~~`getProviderKeyByName` JOIN: add explicit `config_keys.tenant_id = ?` predicate~~ | ~~HIGH~~ | [rdb.go:1307](../../framework/configstore/rdb.go#L1307) | ✅ **Closed by patch11** |
| ~~T2~~ | ~~`GetVirtualKeyMCPConfigsByMCPClientStringIDs` JOIN: pin `.Model()` + add `config_mcp_clients.tenant_id = ?`~~ | ~~HIGH~~ | [rdb.go:3090](../../framework/configstore/rdb.go#L3090) | ✅ **Closed by patch12** |
| T3 | `UpdateProvider` writes via `h.inMemoryStore` (not tenant-safe) → trips `idx_key_id` global unique on second tenant updating "openai" | HIGH | [providers.go:563](../../transports/bifrost-http/handlers/providers.go#L563) | OPEN — 1-2d, needs careful refactor; in-memory store still serves single-tenant inference dispatch, so the swap is conditional on multi-tenant mode |
| T4 | Drop legacy `idx_key_name` global unique on `config_keys(name)` and patch OSS upsert sites to target `idx_key_tenant_name` composite | MEDIUM | [migrations.go:1529](../../framework/configstore/migrations.go#L1529) + upsert sites in rdb.go | OPEN — 1-2d, needs careful sweep of every UPSERT |
| T5 | Tenant-scope the in-memory MCPManager registry: key by `(tenant_id, name)` OR per-tenant MCPManager via runtime config | MEDIUM | `core/mcp/...` (upstream) — easier fix may be per-tenant runtime MCPManager in `multitenant/loader.go` | OPEN — 2-3d depending on approach |
| ~~T6~~ | ~~`TestConfigSchemaSync` fails: add `tenant_id`/`source_id` to `excludedGoFields` or to `config.schema.json`~~ | ~~LOW (test-only)~~ | `transports/bifrost-http/lib/config_test.go` | ✅ **Closed by patch13** |
| ~~T7~~ | ~~`GetVirtualKeysPaginated` budget subquery: add `governance_budgets.tenant_id = ?` for refactor-resilience~~ | ~~LOW~~ | [rdb.go:2555](../../framework/configstore/rdb.go#L2555) | ✅ **Closed by patch12** |

### 4.3 Productization knobs (not bugs, but EA-quality work)

| # | Item | Severity | Effort |
|---|---|---|---|
| P1 | `multitenant.Manager.MaxActiveRuntimes` cap (today: unbounded). Need an LRU bound + admission policy + per-tenant runtime-warm metric. | MEDIUM at scale | 2-3d |
| P2 | Manager unit tests (LRU under concurrent Acquire/Release, init race, refcount underflow). Currently zero coverage. | MEDIUM | 2d |
| P3 | Cascade-delete unit test in `configstore`. Today only the live seed driver exercises it. | MEDIUM | 1d |
| P4 | Tenant directory mutations audit log (who created/deleted which tenant when). | MEDIUM for SOC2-track customers | 1d |
| P5 | Per-tenant runtime warm-up: today first inference for a cold tenant pays loader latency. Optional pre-warm at process boot (read tenant directory, Acquire all). | LOW | 1d |
| P6 | `WS` / `WebRTC` / `Realtime` handlers still dispatch through root `h.client`. Not on the critical inference path; add when those features see multi-tenant traffic. | LOW | 1-2d each |

### 4.4 Open design questions for v1.1+

| # | Question | Notes |
|---|---|---|
| D1 | Per-tenant plugin instances vs share-shim — see section 3.5. Cache-key tenant augmentation is the immediate concern. | Decision point: who owns the per-tenant plugin instance lifecycle? |
| D2 | Per-tenant OAuth providers / KVStores — currently process-globals. Customers running per-tenant SSO would need this. | Pattern is the same as plugins. |
| D3 | Cross-tenant analytics / billing reports. The data-scope callback PREVENTS the platform admin from querying `SELECT COUNT(*) FROM governance_virtual_keys` because no tenant is on ctx. Need an explicit `WithCrossTenant(ctx)` escape hatch with audit. | Pattern: similar to PostgreSQL's `SECURITY DEFINER` — opt-in, audited, narrowly used. |
| D4 | Tenant deletion + GDPR. Cascade-delete (patch8) handles row removal. Log rows in `logstore` need a separate sweep. | Patch8 covers config; logstore tenant-scope was patch6 but cascade isn't wired. |

### 4.5 Operator checklist for EA deployments

Multi-tenant mode is opt-in via env vars (none of these are set by
default — OSS deployments behave exactly as upstream):

| Env var | Value | Purpose |
|---|---|---|
| `BIFROST_MULTI_TENANT_ENABLED` | `true` | Master switch. Enables resolver + dispatcher middleware. |
| `BIFROST_DISABLE_DEFAULT_TENANT_CONFIG` | `true` | Rejects writes without `x-f5xc-tenant` header (except admin surface). Production-grade safety belt. |
| `BIFROST_ASSERT_TENANT_ON_INSERT` | `true` | Strict-mode `BeforeCreate` hooks. INSERTs without tenant in ctx OR explicit stamp error out at the DB layer. Belt-and-suspenders for layers 1+2. |

**Edge configuration (critical):** the deployment's edge proxy
(Envoy, cloudflared, AGW, etc.) MUST:

1. **Strip** any inbound `x-f5xc-tenant` from external callers
   (otherwise it's a trivial spoofing bypass).
2. **Inject** the authenticated tenant id as the new value (typically
   derived from the OAuth claim or mTLS cert subject).

This is the same trust boundary as any header-based auth gateway —
the gateway trusts the edge to be correct. The fork itself can't
validate that boundary; that's an operator responsibility documented
in the deployment runbook.

**Single-tenant OSS deployments:** set NONE of these env vars.
Header absent → callbacks short-circuit → behavior is identical to
upstream Bifrost. The patches add ~5,300 LOC of zero-cost code paths
to the single-tenant configuration.

---

## Companion docs

- [00-review-and-plan.md](./00-review-and-plan.md) — original patch-1-6
  review and the productization plan that motivated this design.
- [01-runbook-updateproviderkey-tenant-corruption.md](./01-runbook-updateproviderkey-tenant-corruption.md)
  — one specific bug runbook (the `TenantID` corruption on
  UpdateProviderKey that caused early field outages).
- [02-stage2-runtime-isolation.md](./02-stage2-runtime-isolation.md)
  — detail on patch7's runtime isolation wiring.
- [03-things-we-missed.md](./03-things-we-missed.md) — living
  retrospective of every miss the original patch series didn't catch,
  with detailed root-cause write-ups for each.
