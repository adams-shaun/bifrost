# Semantic cache — per-tenant namespace, threshold, TTL, enable

**Status:** POC-backed design at
[`plugins/semanticcache/tenant_namespace_poc_test.go`](../../plugins/semanticcache/tenant_namespace_poc_test.go)
on 2026-06-17. Not yet wired into `*Plugin`; not yet emitted as a
patch in `f5xc-patches/`. This doc proposes the production wiring.

Continues the semantic-cache isolation work that started with
[patch15 (mt-semanticcache-tenant-scope)](../../f5xc-patches/patch15/mt-semanticcache-tenant-scope/)
and closes item #3 from the corrected gap list in
[08-outbound-isolation.md §5](./08-outbound-isolation.md#5-correction-to-the-earlier-whats-missing-list):
*"Semantic cache per-tenant namespace (upgrade from patch15's key
prefix — namespace isolation in the vector store, not just key
namespace)."*

---

## 1. What patch15 closed, and what it didn't

[patch15](../../f5xc-patches/patch15/mt-semanticcache-tenant-scope/patches/)
modified
[`resolveCacheKey`](../../plugins/semanticcache/main.go) so the cache
KEY emitted by the plugin is `<tenant>:<base>` when the
`BifrostContext` carries an f5xc tenant id. That gave us:

- Direct (hash-keyed) lookups can't return another tenant's entry,
  because the keys are distinct.
- The metadata recorded on each entry includes the tenant-prefixed
  key, so a Grafana query filtering by `cache_key` already segregates.

What patch15 did NOT change: every entry still lives in **one
physical vector store namespace** (`config.VectorStoreNamespace`,
default `"BifrostSemanticCachePlugin"`). The
[VectorStore interface](../../framework/vectorstore/store.go#L82) is
namespace-keyed, but the plugin only ever passes the single configured
value to it.

That leaves two concrete leak paths:

### 1.1 ANN search runs across the whole namespace

`GetNearest` is "find the closest vectors to this one within a
namespace, return ones within `threshold` distance." Hyperplane
partitioning structures (HNSW, IVF for Qdrant/Pinecone/Weaviate's ANN
indices) build ONE graph spanning all entries in the namespace. So:

- Tenant A's embeddings shape the graph tenant B's queries traverse.
- An adversarial or malformed entry can pollute the ANN structure
  (rare but real for some backends).
- Loose similarity thresholds can return cross-tenant matches even
  though the cache KEYS are distinct, because `GetNearest` filters by
  vector distance, not by the `cache_key` property.

The POC's
[`TestNamespacePOC_GetNearestScopedToTenant`](../../plugins/semanticcache/tenant_namespace_poc_test.go)
demonstrates this: with two tenants' vectors at distance 0.05 and a
threshold of `1.0` (max), the per-tenant namespace returns exactly one
hit — the same threshold against a shared namespace would return both.

### 1.2 Eviction is per-namespace at the backend

Today, "clear all of tenant A's cache" means iterating entries and
matching the `<tenant>:` key prefix. With per-tenant namespaces,
`store.DeleteNamespace(tenantNamespace)` is the operation — one call
to the backend, atomic at the backend's level, no scan-and-match.

The POC's
[`TestNamespacePOC_DeleteNamespaceClearsOnlyOneTenant`](../../plugins/semanticcache/tenant_namespace_poc_test.go)
demonstrates this: three tenants seeded, one namespace deleted, the
other two intact.

---

### 1.3 ... and any other per-request knob that's process-global today

patch15 fixed the cache KEY axis. Doc 09's `TenantTelemetryConfig`
pattern showed that other per-tenant knobs follow the same shape
(pointer-typed fields on a struct; a `Get(tenantID)` interface; a
resolver function per knob). For the semantic cache that means at
least three more dimensions worth lifting to per-tenant:

| Knob today | Today's source | What it controls | Why per-tenant |
|---|---|---|---|
| `config.Threshold` | Plugin-global float64 | Cosine-distance ceiling for a hit | Sensitive tenants want strict (0.05); chatty tenants want loose (0.99) |
| `config.TTL` | Plugin-global Duration | How long entries live | Compliance tenants want long retention; sensitive tenants want short |
| `config.DefaultCacheKey` + on/off | Plugin-global; no per-tenant disable | Whether to cache at all | Some tenants don't want their prompts retained in any form |

The POC extends the namespace resolver with three more resolvers
(`resolveThreshold`, `resolveTTL`, `isCacheEnabled`) and three more
tests demonstrating each knob is observable per tenant. Same pattern
as doc 09's `TenantTelemetryConfig`.

---

## 2. POC findings

The POC adds one namespace resolver next to patch15's
`resolveCacheKey` AND three knob resolvers next to it:

```go
// plugins/semanticcache/main.go (proposed location alongside resolveCacheKey)
const tenantNamespaceSep = "_"

func resolveNamespace(ctx *schemas.BifrostContext, baseNamespace string) string {
    if ctx == nil { return baseNamespace }
    if v := ctx.Value(tenantContextKey); v != nil {
        if tid, ok := v.(string); ok && tid != "" {
            return baseNamespace + tenantNamespaceSep + tid
        }
    }
    return baseNamespace
}
```

Every site that today passes `plugin.config.VectorStoreNamespace`
becomes a call to `resolveNamespace(ctx, plugin.config.VectorStoreNamespace)`.
A grep of [main.go](../../plugins/semanticcache/main.go) lists ~5
namespace-arg call sites — small surface.

Alongside the namespace resolver, the POC defines a per-tenant config
shape modeled on doc 09's `TenantTelemetryConfig`:

```go
type TenantSemanticCacheConfig struct {
    Threshold *float64       // nil = use plugin default
    TTL       *time.Duration // nil = use plugin default; 0 = explicit "no expiry"
    Enabled   *bool          // nil = enabled; false = bypass cache for this tenant
}

type TenantCacheConfigStore interface {
    Get(tenantID string) *TenantSemanticCacheConfig
}

// Three resolvers, one per knob. Each returns the per-tenant value
// when set, or the plugin's base when not.
func resolveThreshold(ctx *schemas.BifrostContext, store TenantCacheConfigStore, baseThreshold float64) float64
func resolveTTL(ctx *schemas.BifrostContext, store TenantCacheConfigStore, baseTTL time.Duration) time.Duration
func isCacheEnabled(ctx *schemas.BifrostContext, store TenantCacheConfigStore) bool
```

Eight tests in the POC, all passing under `go test -race`:

1. `TwoTenantsAddToDistinctNamespaces` — same `cache_key` from two
   tenants lands in two different physical namespaces.
2. `GetNearestScopedToTenant` — ANN-isolation property: even with
   `threshold=1.0` and two vectors at distance 0.05, a tenant's
   `GetNearest` returns only their own namespace's entry.
3. `NoTenantFallsBackToBase` — OSS single-tenant compatibility: no
   tenant on ctx → base namespace unchanged (regression guard).
4. `PerTenantThreshold` — strict tenant (0.05) gets 1 hit; loose
   tenant (0.99) gets 2 hits on the same seed + same query.
5. `PerTenantTTL` — resolver returns 5m for "sensitive", 30d for
   "compliance", base for everyone else; explicit TTL=0 is respected
   as "no expiry" (NOT collapsed to base).
6. `PerTenantDisable` — `Enabled=false` short-circuits both writes
   and reads; the would-be Add never reaches the store.
7. `AllKnobsCompose` — three tenants ("strict", "loose", "disabled")
   on the same workload produce three observably different behaviors:
   1 hit, 2 hits, 0 hits respectively, plus the right namespace and
   TTL for each.
8. `DeleteNamespaceClearsOnlyOneTenant` — eviction primitive scoped
   to the right namespace; siblings untouched.

### Constraints surfaced by the POC

#### Constraint 1 — Namespace naming restrictions vary by backend

| Backend | Namespace name restrictions |
|---|---|
| Weaviate | Class names: start with `[A-Z]`, then `[A-Za-z0-9]` only |
| Qdrant | Collection names: `[A-Za-z0-9_-]` |
| Pinecone | Namespace strings: arbitrary (almost) |
| Redis | Key prefixes: arbitrary |

The POC uses `_` as the separator
([`tenantNamespaceSep`](../../plugins/semanticcache/tenant_namespace_poc_test.go))
because it's the lowest common denominator. Tenant ids today
([multitenant/tenant.go](../../multitenant/tenant.go)) are validated as
slugs (`[A-Za-z0-9_-]`), so `<base>_<tenant>` is safe across all
backends.

**Open dimension**: what about a tenant id starting with a digit on
Weaviate? Weaviate's class name must START with `[A-Z]`. Today the
base `"BifrostSemanticCachePlugin"` provides the leading letter and
the tenant id is the suffix, so we're fine. If we ever invert to
`<tenant>_<base>`, we'd need a casing/prefix normalization step.

#### Constraint 2 — Lazy vs. eager namespace creation

Production has two options:

- **Eager**: `multitenant.Manager.Acquire(tenantID)` calls
  `plugin.CreateNamespaceIfNotExists(ctx)` with the tenant on ctx.
  Cost: one CreateNamespace round-trip per tenant per gateway boot.
  Pro: first cache write doesn't pay the namespace-create latency.
  Con: tenants who never use the cache still pay for the namespace.

- **Lazy**: the plugin checks namespace existence on first `Add` and
  creates it if missing. Pro: zero cost for tenants who don't use the
  cache. Con: first cache write pays a CreateNamespace round-trip
  (typically tens of ms on Weaviate/Qdrant); needs a once-per-tenant
  cache so the check is fast on subsequent writes.

The POC sidesteps this with a no-op `CreateNamespace` on the recording
store. **Recommendation: lazy**, with a per-process `sync.Map[tenant]bool`
flag that's set after the first successful Add. Matches the per-plugin
pattern; explicit eviction (`DeleteNamespace`) clears the flag.

#### Constraint 3 — Eviction semantics on tenant DELETE

When a tenant is hard-deleted, their cache namespace should go with
it. Today's cascade delete (patch8) doesn't know about the vector
store; it only cascades configstore rows. The integration point is:

- patch8's `tenant_cascade_delete` handler runs the DB cascade.
- The semantic cache plugin needs a hook that fires on
  `multitenant.Manager.EvictTenant` (different from `Acquire`/release;
  this is the "tenant is gone forever" signal). On that hook, the
  plugin calls `store.DeleteNamespace(resolveNamespace(ctx, base))`.

This is one new hook the Manager already half-has (it has eviction
notifications for LRU; we'd add a separate "permanent delete" channel).
Defer to the production patch's design.

---

## 3. Proposed wiring

### 3.1 No OSS upstream change

Same pattern as all the docs in this lane: the resolver lives ENTIRELY
in the f5xc semantic-cache layer. OSS bifrost's `Plugin` constructor
and hook signatures don't change. The change is internal — every
`plugin.config.VectorStoreNamespace` reference becomes
`resolveNamespace(ctx, plugin.config.VectorStoreNamespace)`.

### 3.2 Inside the plugin

Four changes to [`plugins/semanticcache/main.go`](../../plugins/semanticcache/main.go):

1. Add the `tenantNamespaceSep` const and the `resolveNamespace`
   function (~10 LOC; sibling to `resolveCacheKey` from patch15).
2. Add a `TenantCacheConfigStore` interface on the plugin (optional
   field on `Config`; nil = OSS behavior unchanged) and three knob
   resolvers (`resolveThreshold`, `resolveTTL`, `isCacheEnabled`)
   (~50 LOC; sibling to `resolveCacheKey`).
3. Rewrite the ~5 `plugin.config.VectorStoreNamespace` references in
   the hot path to use `resolveNamespace(ctx, plugin.config.VectorStoreNamespace)`.
4. Rewrite the threshold + TTL + on/off sites in the hot path to use
   the three new resolvers (~5 sites for Threshold/TTL; the on/off
   short-circuits live at PreLLMHook entry and PostLLMHook entry —
   2 sites).

Specifically (from the
[grep](../../plugins/semanticcache/main.go) at survey time):

Namespace sites:
   - `store.CreateNamespace(...)` at Init time — covered by the lazy
     pattern (Constraint 2); Init still creates the BASE namespace for
     no-tenant requests.
   - `store.DeleteAll(...)` in cleanup paths
   - `store.Delete(...)` for individual entry cleanup
   - Cache write path (the actual `Add` calls in search.go / state.go)
   - Logging strings that mention the namespace

Threshold sites:
   - `GetNearest(..., threshold, ...)` calls (1-2 sites)
   - Per-request override path (already reads `CacheThresholdKey` from
     ctx; this is the per-tenant DEFAULT before the per-request
     override applies)

TTL sites:
   - State-cache reaper (uses TTL to expire entries)
   - Per-request override path (already reads `CacheTTLKey` from ctx;
     same per-tenant DEFAULT pattern)

Enable sites:
   - PreLLMHook entry: `if !isCacheEnabled(ctx, store) { return req, nil, nil }`
   - PostLLMHook entry: same guard before the async cache write

Plus the lazy CreateNamespace check on first Add per tenant
(Constraint 2).

### 3.3 Tenant lifecycle hook for namespace cleanup

On tenant hard-delete, the plugin's `OnTenantDelete(tenantID)` hook
gets called and runs `store.DeleteNamespace(base + sep + tenantID)`.
Wiring lives in the multitenant Manager — same shape as the
`EvictTenant` notifications today, but a separate event class.

---

## 4. Patch series outline

Two patches, in order:

- **patch17 — `semanticcache-tenant-namespace`**: types + the
  resolver + the hot-path rewrite + lazy CreateNamespace. Self-
  contained inside the plugin. Unit-tested by the four POC tests
  promoted into the plugin's test suite. Behavior unchanged when
  the BifrostContext doesn't carry a tenant (OSS regression guard).

- **patch18 — `tenant-delete-namespace-hook`**: adds
  `Plugin.OnTenantDelete` and wires it into `multitenant.Manager`'s
  tenant-delete notification path. Atomic backend `DeleteNamespace`
  call replaces the current "scan-and-match-prefix" eviction.

No OSS upstream change in either patch.

---

## 5. Composition with prior patches

- **patch15 (key prefix)** stays. The cache KEY remains
  `<tenant>:<base>` for metadata clarity and for backends that don't
  treat the namespace as a hard partition (Redis, for instance, where
  "namespace" is a key prefix). Belt + suspenders.
- **Doc 09 (telemetry config)**: the cache plugin emits a metric on
  every hit/miss; the per-tenant telemetry policy applies to those
  emissions the same way it applies to upstream-request metrics.
  Composes cleanly.
- **Doc 07 (chain policy)**: a `semantic-cache: off` variant becomes
  trivial — the plugin's RunVariant skips both Add and GetNearest
  when the policy supplies that variant for the request. The
  namespace-level isolation is the safety net for the requests that
  DO go through the cache.

---

## 6. Open dimensions for review

- **Cardinality of namespaces** — every tenant ever to use the cache
  creates a namespace. Some backends (Weaviate, Pinecone) have soft
  limits in the thousands of classes/namespaces; verify our tenant
  count target lands under those limits, and add a documented cap
  (`maxNamespacesPerStore`) with an alert.

- **Shared embedding model** — different tenants might want different
  embedding models (sensitive tenants want an in-house model, not the
  default openai-ada). Today the embedding model is plugin-global. A
  per-tenant embedding model would mean per-tenant embedders + per-
  tenant dimension, which forces per-tenant namespaces ANYWAY. This
  doc's plumbing is the foundation; the embedding-model-per-tenant
  patch is a follow-up.

- **Migration of patch15 entries** — when patch17 lands, existing
  entries in the BASE namespace from patch15 will still have keys
  like `acme:default-key`. Two options: leave them (they'll TTL out
  in a few days; new entries land in the per-tenant namespace), or
  write a one-shot migration command that drains the base namespace.
  Lean to leave-them — the migration code costs more than the cache
  cold-start does.

- **Per-tenant TTL / threshold overrides** — patch15 added
  per-request TTL + threshold overrides via context keys. Should a
  per-tenant DEFAULT be settable (e.g. tenant A defaults to 1h TTL,
  tenant B to 24h)? Yes, but compose this with doc 09's
  `TenantTelemetryConfig` pattern — a `TenantSemanticCacheConfig`
  table with the same shape.

- **Reporting** — `cache_hits_total{tenant_id=...}` already exists
  in telemetry. With per-tenant namespaces, also worth emitting
  `cache_namespace_size_bytes{tenant_id=...}` so operators can see
  per-tenant footprint. Backend support varies (Weaviate exposes
  this, Pinecone exposes counts not bytes); defer to follow-up.
