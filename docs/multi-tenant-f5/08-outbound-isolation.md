# Outbound HTTP isolation — the noisy-neighbor topology trade-off

**Status:** benchmark-backed analysis at
[`multitenant/outbound_isolation_bench_test.go`](../../multitenant/outbound_isolation_bench_test.go)
on 2026-06-17. No patches proposed — the v1 recommendation is to keep
the existing topology and revisit once tenant count + bench numbers
say it hurts.

This doc supersedes the "outbound HTTP is the biggest under-discussed
gap" line in [my earlier status pass](#) — that was wrong. Patch7's
per-tenant `*bifrost.Bifrost` already gives per-tenant HTTP clients
**by accident**. The real question is the resource/contention
trade-off across topologies, which the benchmark below quantifies.

---

## 1. What patch7 actually buys us

Each `*bifrost.Bifrost` constructs its own provider instances
([core/bifrost.go:340-365](../../core/bifrost.go#L340-L365) → `prepareProvider`),
and each provider constructs its own `*fasthttp.Client` with its own
connection pool
([core/providers/openai/openai.go:44-52](../../core/providers/openai/openai.go#L44-L52)).
In the patch7 multi-tenant model, that means:

- Tenant A's `*Bifrost` → tenant A's `*OpenAIProvider` → tenant A's
  `*fasthttp.Client_A` → tenant A's connection pool to the upstream
- Tenant B's `*Bifrost` → tenant B's `*OpenAIProvider` → tenant B's
  `*fasthttp.Client_B` → tenant B's connection pool to the upstream

Different `MaxConnsPerHost` allocations. Different idle-conn caches.
Different goroutine scheduling. **Outbound HTTP is per-tenant isolated
today.** No patch needed for the isolation property itself.

The cost: N tenants sharing one upstream host = N separate connection
pools, each with `MaxConnsPerHost` slots, against the same upstream
server. At 100 tenants × 10 conns each = 1000 simultaneous sockets to
api.openai.com. Eventually wasteful, eventually rate-limited by the
upstream's per-IP limits, eventually a memory cost on bifrost itself.

---

## 2. The benchmark

[`multitenant/outbound_isolation_bench_test.go`](../../multitenant/outbound_isolation_bench_test.go)
fires 50 simultaneous requests per tenant against one upstream host.
Tenant A's requests are slow (server-side 200ms sleep simulating an
overloaded vLLM); tenant B's return immediately. `MaxConnsPerHost=4`
so contention is visible at small concurrency.

Three topologies:

**(A) Per-tenant clients** — patch7 today. Each tenant gets its own
`*http.Transport` with its own pool.

**(B) Shared client** — counterfactual: both tenants funnel through one
`*http.Client`, one pool, no quotas.

**(C) Shared client + per-tenant semaphore (cap=2)** — hybrid: one
pool shared, but each tenant's in-flight call count is bounded so
neither can hog the whole pool.

### Numbers

Two consecutive runs on the workstation that produced them, latencies
to ms precision:

```
upstream: 127.0.0.1:*  MaxConnsPerHost=4  tenant A sleep=200ms  50 req/tenant

--- (A) PER-TENANT clients (patch7 today) ---
  tenant A (slow upstream)   p50=1.404s  p95=2.405s  p99=2.605s  max=2.605s
  tenant B (fast upstream)   p50=   1ms  p95=   2ms  p99=   2ms  max=   2ms

--- (B) SHARED client (consolidation, no quotas) ---
  tenant A (slow upstream)   p50=1.404s  p95=2.406s  p99=2.606s  max=2.606s
  tenant B (fast upstream)   p50= 201ms  p95=1.604s  p99=2.005s  max=2.406s

--- (C) SHARED client + per-tenant semaphore (hybrid) ---
  tenant A (slow upstream)   p50=2.605s  p95= 4.81s  p99= 5.01s  max= 5.01s
  tenant B (fast upstream)   p50=   1ms  p95=   1ms  p99=   1ms  max=   1ms
```

Reading the table:

- **(A) tenant B p50 = 1ms** — gets the actual upstream's latency
  unaffected by tenant A. This is the isolation property.
- **(B) tenant B p50 = 201ms** — **200× regression on tenant B's
  median**. Even though tenant B's upstream answers in ~1ms, the
  shared connection pool is full of tenant A's in-flight slow requests.
  Tenant B blocks acquiring a conn slot until one frees up — and
  tenant A's slots free up 200ms at a time. p99 = 2 seconds. Classic
  noisy neighbor.
- **(C) tenant B p50 = 1ms** — semaphore caps tenant A at 2 in-flight,
  so 2 slots in the shared pool always stay available for tenant B.
  Same latency profile as (A). Tenant A's wall-clock is somewhat worse
  (p50 1.4s → 2.6s) because A can't burst beyond its semaphore quota,
  but A's p99 floor was already set by the upstream's per-request
  latency, not the gateway's pool — so the regression is bounded.

### What the table says about the trade-off

- (A) is the simplest and gives perfect isolation. The cost is N pools
  × M conns per pool when N tenants share an upstream host.
- (B) is what you get if someone proposes "let's just share the client
  for efficiency." Catastrophic for the well-behaved tenant. Do not.
- (C) is the hybrid: keeps tenant B's latency identical to (A), bounds
  tenant A so it can't dominate, and saves N pools' worth of sockets.
  Costs: a tenant-axis semaphore at the request entrypoint and the
  lookup table for it.

For comparison: the same bench at `MaxConnsPerHost=4` with the
semaphore set generously (`cap=10`, larger than the pool) collapses to
behave like (B) — the semaphore stops being a quota at that point.
Choose the cap < `MaxConnsPerHost / N tenants` for meaningful isolation;
the per-tenant tail still depends on the upstream's response time, not
the gateway.

---

## 3. v1 recommendation

**Keep topology (A) — per-tenant clients — for v1.** Reasons:

1. It's the default the patch7 model already gives us. Nothing to
   write, nothing to patch, nothing to keep against upstream.
2. The isolation property is bench-confirmed clean (tenant B
   p99 = 2ms even while tenant A's upstream burns 200ms per request).
3. The resource cost (N pools per upstream host) is acceptable at the
   tenant counts we're realistically targeting. At 10 tenants × 10
   conns × 1 upstream = 100 sockets. We're nowhere near a limit.

**Revisit when at least one of these is true:**

- The bench framework's `mt-balanced` scenario at 50+ tenants pegs
  bifrost CPU on TLS handshakes (per-tenant clients amplify handshake
  cost).
- The upstream provider rate-limits us by source IP because we have
  too many concurrent conns from one bifrost pod.
- Per-tenant memory cost from idle conn pools becomes noticeable in
  the `mt-cold` scenario's RSS growth.

At any of those, (C) is the path forward: one shared client per
*upstream host* (not per tenant), with a per-tenant semaphore at the
request entry that bounds simultaneous in-flight calls per tenant.
The semaphore lives on the tenant runtime (one allocation per
`Manager.Acquire`) and is acquired/released in the same Pre/Post
LLM hook flow as the rest of governance.

The benchmark in this lane should be run again whenever the topology
changes — the test exists at
[multitenant/outbound_isolation_bench_test.go](../../multitenant/outbound_isolation_bench_test.go)
and runs in `make cmd-unittest` (no live network deps).

---

## 4. What this does NOT cover

The benchmark fixes the question of CONNECTION POOL contention. Five
related outbound-axis isolation concerns are out of scope here:

- **Per-tenant proxy config** — some tenants need to egress through a
  specific HTTP proxy. Today `config.ProxyConfig` is per-provider; in
  patch7 each tenant's provider already gets its own ConfigureProxy
  call, so this works. No gap.
- **Per-tenant TLS material (mTLS to upstream)** — same story:
  per-provider `ConfigureTLS` runs at `NewOpenAIProvider` time. Per-
  tenant TLS material wires through `config.NetworkConfig` already.
- **Per-tenant outbound IP (egress IP allocation)** — k8s-level, not
  a bifrost concern. Tenants needing dedicated egress IPs run in
  dedicated bifrost pods (separate `multitenant.Manager` per pod).
- **Per-tenant DNS resolution** — fasthttp uses the process resolver.
  Per-tenant DNS would require swapping out `Transport.DialContext`
  per provider. Not impossible but not asked-for; defer.
- **Per-tenant outbound rate limit** — different from inbound rate
  limit (which is per-VK in governance). Outbound "stop burning
  OpenAI quota when tenant X hits N req/s" is a tenant-axis concern
  not modeled today. Likely a governance plugin extension.

---

## 5. Correction to the earlier "what's missing" list

In an earlier status review I called out outbound HTTP as "the biggest
under-discussed gap." That was wrong. The patch7 model gave us the
isolation property for free; the gap I was worried about doesn't exist
under the current topology.

The actual gaps still standing (re-ordered by impact):

1. Per-tenant log REDACTION policy (different tenants, different PII
   rules)
2. Per-tenant metric label cardinality cap (defensive, before patch7
   gets to 100+ tenants)
3. Semantic cache per-tenant *namespace* (upgrade from patch15's
   key prefix — namespace isolation in the vector store, not just key
   namespace)
4. Per-tenant goroutine + in-flight gauges (observability, not
   enforcement)
5. Per-tenant outbound rate limit (eventually, governance extension)

Outbound HTTP topology drops off the list. It's measured, the property
holds, and the contention only appears if someone proposes to
consolidate — at which point this benchmark is the rebuttal.
