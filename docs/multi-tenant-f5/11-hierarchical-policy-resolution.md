# Hierarchical policy resolution + Redis client isolation gap

**Status:** answers two follow-up questions raised after doc 10. Q1
(Redis isolation) is analytical with no patches proposed yet — the
bench-and-recommend pattern from
[08-outbound-isolation.md](./08-outbound-isolation.md) applies
directly, just to a different transport. Q2 (hierarchical resolution)
is backed by a POC at
[`plugins/semanticcache/hierarchical_resolver_poc_test.go`](../../plugins/semanticcache/hierarchical_resolver_poc_test.go).

---

## Q1 — Redis client isolation: NO

### What today's wiring gives us

The Redis client is constructed **exactly once** at server boot.
[`transports/bifrost-http/lib/config.go:784`](../../transports/bifrost-http/lib/config.go#L784)
calls `vectorstore.NewVectorStore`, which lands in
[`framework/vectorstore/redis.go:1700-1740`](../../framework/vectorstore/redis.go#L1700)
constructing one `redis.UniversalClient` with one `PoolSize`. The
returned handle goes into `config.VectorStore`; the semantic cache
plugin singleton holds the pointer.

Every tenant shares:
- the connection pool (`PoolSize` connection slots, all draining the
  same pool)
- the dialer + TLS handshake (one set of credentials, one
  identity to Redis)
- the per-conn timeouts (`ReadTimeout`, `WriteTimeout`, etc.)
- `MaxActiveConns`, `MaxIdleConns` (process-global caps)
- the FT.SEARCH workload pattern (one client, one query plan cache on
  the Redis side, one execution thread queue)

### Why this is worse than the HTTP case in doc 08

Doc 08 showed that outbound HTTP is per-tenant isolated for free
because each tenant's `*bifrost.Bifrost` constructs its own
`*fasthttp.Client`. For Redis we don't get that property because the
`VectorStore` is constructed **above** bifrost (at the
`BifrostHTTPServer` layer) and passed in. Per-tenant `*bifrost.Bifrost`
instances don't help — they all dereference the same shared
`config.VectorStore`.

Beyond the wiring, Redis is structurally **harder** to isolate than HTTP:

| Property | HTTP via fasthttp | Redis via go-redis |
|---|---|---|
| Connection lifetime | per-request (mostly) | persistent, pooled |
| Slow command behavior | TCP timeout via `ReadTimeout` | blocks the conn until reply |
| Per-host conn cap | `MaxConnsPerHost` | `PoolSize`/`MaxActiveConns` |
| Per-pool wait queue | yes | yes (`PoolTimeout`) |
| Pipelining | no | yes — one slow command stalls the rest on its conn |

The same noisy-neighbor scenarios doc 08 bench'd for HTTP
(scenario B "shared client") apply with the same shape to Redis.
Tenant A's slow `FT.SEARCH` (say a 500ms search on a large
result set) parks a connection. With `PoolSize=10`, ten slow tenant-A
searches saturate the pool; tenant B's tiny `HGET` waits in
`PoolTimeout`.

### Doc 06's per-tenant plugin instances don't fix this for free

The proposal in [06-plugin-isolation.md](./06-plugin-isolation.md) is
"`TenantLoader` constructs per-tenant plugin instances." For the
semantic cache that means each per-tenant `*Plugin` gets its own
configuration and lifecycle — but the `*VectorStore` is supplied to
the plugin's `Init` by whoever calls it. The plugin doesn't construct
the vector store.

So the per-tenant plugin decision propagates upstream: should the
**`TenantLoader`** call `vectorstore.NewVectorStore` per tenant (giving
each tenant their own Redis client) or pass the shared root one?

Same three-way trade-off as doc 08 §3:

- **(A) Per-tenant Redis client** — full isolation, but N pools × M conns
  per pod against the same Redis. With cluster mode and 100 tenants
  we'd have hundreds of TCP conns to one Redis cluster. Redis itself
  starts to complain via `maxclients`.
- **(B) Shared Redis client** — what we have today. Cheap, noisy
  neighbor.
- **(C) Shared client + per-tenant semaphore** — bound each tenant's
  simultaneous Redis ops. Bench-able with the same pattern as doc 08's
  [`outbound_isolation_bench_test.go`](../../multitenant/outbound_isolation_bench_test.go).

### Recommendation for v1

Same as doc 08 §3 for HTTP: **keep (B) for now, document the trade,
revisit when bench shows a hot spot.** The triggers to revisit:

- The bench framework's `mt-balanced` scenario shows tenant-B p99
  growing with tenant-A's Redis traffic
- Redis `maxclients`/`PoolTimeout` errors start appearing in logs at
  realistic tenant counts
- A high-compliance tenant requires data isolation guaranteed at the
  network level, not at the namespace level (which is doc 10's
  property)

The bench-style demonstration is portable from doc 08 — same Go test
shape, same three scenarios, just go-redis instead of `net/http`.
Defer to a follow-up commit when we need the data.

### What patch15 + doc 10 still buy us

Despite the shared client, the cache key (patch15) + the per-tenant
namespace (doc 10) **do still isolate the DATA**. A Redis FT.SEARCH
scoped to `BifrostSemanticCachePlugin_acme` cannot return entries from
`BifrostSemanticCachePlugin_globex` regardless of how many tenants
share the client. The Redis namespace separation is enforced
**inside Redis**; the noisy-neighbor problem is about latency, not
about cross-tenant data exposure.

---

## Q2 — Hierarchical policy resolution

### Goal

Doc 10's `TenantSemanticCacheConfig` lifted three knobs (Threshold,
TTL, Enabled) to the tenant axis. The real-world ask goes further:

```
plugin defaults                                      (process-global; today)
  ← tenant config             (whole-tenant defaults; doc 10's POC)
    ← customer / team config  (a group of VKs under one business unit)
      ← VK-specific config    (a single integration's specialized policy)
```

Each layer overrides ONLY the fields it sets. Unset fields fall through
to the next-less-specific layer.

This pattern is **already established** in the codebase for budgets —
[`plugins/governance/resolver.go::BudgetResolver`](../../plugins/governance/resolver.go)
does provider → model → customer → team → user → VK with the same
"most specific wins" semantics. Adopting it for semantic cache is
mostly typing.

### POC findings

[`plugins/semanticcache/hierarchical_resolver_poc_test.go`](../../plugins/semanticcache/hierarchical_resolver_poc_test.go)
defines:

```go
type PluginDefaults struct {
    Threshold float64
    TTL       time.Duration
    Enabled   bool
}

type VKResolverInput struct {
    VKID       string
    TenantID   string
    CustomerID *string // VK's owning customer, if any
    TeamID     *string // VK's owning team, if any
}

type ResolvedSemanticCachePolicy struct {
    Threshold       float64
    TTL             time.Duration
    Enabled         bool
    ResolutionTrace []string // "threshold: vk", "ttl: customer", "enabled: tenant"
}

type PolicyResolver struct {
    defaults  PluginDefaults
    tenants   PerTenantConfigStore
    customers PerCustomerConfigStore
    teams     PerTeamConfigStore
    vks       PerVKConfigStore
}

func (r *PolicyResolver) Resolve(in VKResolverInput) ResolvedSemanticCachePolicy
```

Nine tests cover the surface, all passing under `go test -race`:

| Test | What it proves |
|---|---|
| `PluginDefaultsOnly` | OSS regression guard: no tenant/VK → plugin defaults reach the call site verbatim |
| `TenantOverridesDefaults` | Tenant sets only Threshold → TTL/Enabled stay at plugin defaults |
| `CustomerOverridesTenant` | Customer's TTL wins; Threshold falls through to tenant; Enabled to plugin default |
| `VKOverridesEverything` | VK-set fields win over team/customer/tenant/defaults — most specific |
| `PartialOverridesCompose` | Each layer sets one field → resolved policy carries one field from each layer; trace reads `threshold: vk`, `ttl: customer`, `enabled: tenant` |
| `VKDisableCascades` | `Enabled=false` at the VK layer overrides tenant/customer `Enabled=true` — single-VK opt-out |
| `NoTenantStillResolvesPluginDefaults` | Resolver doesn't NPE when stores are empty or rows missing |
| `ConcurrencySafe` | Race-detector run with 50 concurrent `Resolve` + 30 concurrent `Set` writes across all 4 stores |
| `NamespacePlusHierarchy` | The namespace resolver (doc 10) and the hierarchical resolver compose orthogonally — namespace is tenant-derived; leaf-config resolution walks the hierarchy |

### The trace field — the operator-facing payoff

Every `ResolvedSemanticCachePolicy` carries a `ResolutionTrace` saying
which layer set each field. That's the answer to "why is my cache
disabled for this VK?":

```
threshold: tenant
ttl: customer
enabled: vk            ← oh, the VK itself has Enabled=false
```

Operators reading the trace know exactly where to edit. Same shape as
doc 07's `ChainPlan.Why` — making the decision visible is the property
the operator wants.

### Customer vs. team ordering

The POC walks customer **before** team, on the theory that customer is
usually the higher-level business unit and team is the sub-unit
(matches the existing budget hierarchy in governance — customer
budgets are coarser than team budgets). A team override of a customer
override is the more specific case; team wins.

The reverse ordering is also defensible (especially in setups where
teams cut across customers). The POC picks one; the production patch
should document the choice prominently and surface it in
`ResolutionTrace`.

---

## 3. Proposed wiring

### 3.1 Configstore tables

Four config layers need DB-backed tables. Three already exist; the new
one is the cache-config column.

```go
// framework/configstore/tables/semantic_cache_config.go (NEW)
// One row per (scope_type, scope_id). scope_type ∈ {tenant, customer, team, vk}.
type TableSemanticCacheConfig struct {
    ScopeType      string    `gorm:"primaryKey;type:varchar(32)"` // 'tenant'|'customer'|'team'|'vk'
    ScopeID        string    `gorm:"primaryKey;type:varchar(255)"`
    TenantID       string    `gorm:"type:varchar(255);not null;index"`
    Threshold      *float64  `json:"threshold"`
    TTL            *int64    `json:"ttl_seconds"` // nullable; nil = inherit
    Enabled        *bool     `json:"enabled"`
    UpdatedAt      time.Time
}
```

One table with a scope-type discriminator beats four parallel tables
because the lookup queries are uniform.

### 3.2 In-plugin wiring

The plugin's `Init` accepts an optional `PolicyResolver` (or builds a
default resolver that reads the new table via configstore lookups).
On each request, after VK resolution:

```go
in := VKResolverInput{
    VKID:       vk.ID,
    TenantID:   vk.TenantID,
    CustomerID: vk.CustomerID,
    TeamID:     vk.TeamID,
}
policy := plugin.resolver.Resolve(in)

if !policy.Enabled {
    return req, nil, nil  // cache short-circuited
}
// Use policy.Threshold + policy.TTL for namespace operations
```

The doc 10 resolvers (`resolveThreshold`, `resolveTTL`, `isCacheEnabled`)
collapse into one `Resolve` call; the trace replaces the ad-hoc
sources string.

### 3.3 Admin endpoints

Four CRUD endpoints, parallel to the rest of the tenant-scoped admin
surface:

```
GET    /api/tenants/{tid}/semantic-cache-config
PUT    /api/tenants/{tid}/semantic-cache-config
DELETE /api/tenants/{tid}/semantic-cache-config

GET    /api/tenants/{tid}/customers/{cid}/semantic-cache-config
PUT    /api/tenants/{tid}/customers/{cid}/semantic-cache-config
DELETE /api/tenants/{tid}/customers/{cid}/semantic-cache-config

GET    /api/tenants/{tid}/teams/{tmid}/semantic-cache-config
PUT    /api/tenants/{tid}/teams/{tmid}/semantic-cache-config
DELETE /api/tenants/{tid}/teams/{tmid}/semantic-cache-config

GET    /api/tenants/{tid}/virtual-keys/{vkid}/semantic-cache-config
PUT    /api/tenants/{tid}/virtual-keys/{vkid}/semantic-cache-config
DELETE /api/tenants/{tid}/virtual-keys/{vkid}/semantic-cache-config
```

Each is a thin wrapper around the configstore CRUD on the new table
with the appropriate `scope_type` value. Validation: `Threshold ∈ [0,1]`,
`TTL ≥ 0`, etc.

---

## 4. Patch series outline

After doc 10's patches land:

- **patch19 — `semanticcache-policy-table`**: adds
  `TableSemanticCacheConfig` + migration + configstore CRUD methods.
  Self-contained; no behavior change yet.
- **patch20 — `semanticcache-hierarchical-resolver`**: promotes the
  POC types into the plugin as production code; wires the resolver to
  read from the four configstore-backed adapters; replaces doc 10's
  three resolver functions with one `Resolve` call.
- **patch21 — `semanticcache-policy-admin-endpoints`**: adds the 12
  CRUD endpoints + UI surface (optional for v1; CLI / API-only
  acceptable to ship behind a feature flag).

No OSS upstream change in any patch.

---

## 5. Open dimensions for review

- **Customer vs. team ordering** (called out above). Hard to back out
  once shipped; pick deliberately.
- **What about a "group" layer between team and customer?** The user
  mentioned `groupID` as a parallel to customer/team. Governance has
  the same question (and answers it via the table-by-table
  hierarchy). Defer until configstore actually has a `groups` table.
- **Pre-resolved policy cache** — the resolution walk is 4 store
  lookups per request, all O(1) maps. At 10k req/s that's 40k map
  lookups/sec, which is nothing. But if the resolver gets harder
  (CEL expressions a la doc 07 chain policy), pre-resolving per-VK
  and TTL-caching becomes cheap insurance. Not now.
- **Carry the trace to telemetry** — the resolution trace is the
  best operator-facing field we have. Worth recording on a `cache_lookup`
  span (otel) and on a label of the cache_hits_total counter
  (Prometheus) so dashboards can answer "which policy layer
  produced the hits I'm seeing?" Bigger surface; consider for a
  follow-up commit.
- **What about the chain-policy (doc 07) interaction?** Doc 07's
  `ChainPolicy.Select` can supply a per-request semantic-cache
  variant + config. That's a 5th layer above VK — request-specific
  override. The resolver should accept an optional "request override"
  as the final winner. Easy to add; defer until doc 07 patches land.
- **Redis client isolation revisit trigger** — re-bench when tenant
  count hits ~50 OR when Redis `maxclients` errors appear in logs.
  The bench shape is already worked out in doc 08; porting takes ~1 hr.
