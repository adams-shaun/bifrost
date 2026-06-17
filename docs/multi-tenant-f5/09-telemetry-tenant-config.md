# Tenant-scoped telemetry config — sample rate + deny lists

**Status:** POC-backed design at
[`plugins/telemetry/tenant_config_poc_test.go`](../../plugins/telemetry/tenant_config_poc_test.go)
on 2026-06-17. Not yet wired into the actual `PrometheusPlugin`
emission points; not yet emitted as a patch in `f5xc-patches/`. This
doc proposes the production wiring.

Closes item #1 from the corrected "what's missing" list in
[08-outbound-isolation.md §5](./08-outbound-isolation.md#5-correction-to-the-earlier-whats-missing-list).
Composes with the chain-policy work in
[07-dynamic-plugin-chain.md](./07-dynamic-plugin-chain.md) — once that
lands, telemetry config can flow through chain-policy variants too;
this doc proposes a more direct path that doesn't depend on the
chain-policy patches landing first.

---

## 1. The gap

Today's [`PrometheusPlugin`](../../plugins/telemetry/main.go) emits 17
metric vectors, all process-global. Every tenant gets the same metrics,
the same labels, the same sample rate (100%). Per-tenant
*aggregation* works at Prometheus query time because every metric
carries a `tenant_id` label (patch6) — but every tenant pays the same
cost and exposes the same data shape.

What tenants actually want differs across two axes:

1. **Volume control** — a test tenant or a noisy red-team tenant can
   blow out cardinality and storage. Today's only knob is "turn off
   the plugin globally."
2. **Data shape** — a privacy-conscious tenant may not want
   `customer_name` recorded; a compliance-conscious tenant may not
   want body-size histograms recorded at all.

Both are tenant-axis CONFIG (per
[06-plugin-isolation.md](./06-plugin-isolation.md)'s framing) —
settled at TenantLoader time, applied per request.

---

## 2. POC findings

The POC ([`plugins/telemetry/tenant_config_poc_test.go`](../../plugins/telemetry/tenant_config_poc_test.go))
defines three pieces:

```go
type TenantTelemetryConfig struct {
    SampleRate     *float64 // nil = default emit; non-nil 0 = drop all;
                            // 0 < r < 1 = per-emission Bernoulli; 1 = always emit
    MetricDenyList []string // metric names to skip entirely
    LabelDenyList  []string // label names whose VALUE gets stripped (slot preserved)
}

type TenantConfigStore interface {
    Get(tenantID string) *TenantTelemetryConfig // nil result = no config
}

type TelemetryPolicy struct { ... }
func (p *TelemetryPolicy) Decide(metricName, tenantID string, labels map[string]string) (emit bool, labelsToUse map[string]string)
```

`Decide` is the single choke point every metric emission runs through.
Returns whether to emit and the (possibly rewritten) label set. When
`emit==false` the caller skips the metric entirely; when `emit==true`
the caller uses `labelsToUse` (which may equal the input or have
denied labels replaced with `""`).

Seven tests cover the surface:

- `TestPolicy_NoConfigEmitsEverything` — regression guard: no config
  row → today's behavior unchanged.
- `TestPolicy_SampleRateZeroDropsAll` — explicit kill switch.
- `TestPolicy_SampleRatePartialKeepsFraction` — 20% rate over 1000
  trials emits within 0.001 of 0.20.
- `TestPolicy_MetricDenyListSkipsNamed` — privacy-strict tenant drops
  specific metrics, others unaffected.
- `TestPolicy_LabelDenyListStripsValues` — denied label values
  replaced with `""`, slot preserved (Prometheus cardinality
  requirement; see §4).
- `TestPolicy_CombinedKnobsCompose` — all three axes apply, with
  short-circuit ordering: metric-deny → sample-drop → label-rewrite.
- `TestPolicy_ConcurrentSafe` — race-detector run with 100 concurrent
  Decides + 10 concurrent Set updates.

All seven pass under `go test -race`.

### Constraints surfaced by the POC

#### Constraint 1 — `SampleRate` must be a pointer

The zero value of `float64` is 0.0, which we WANT to mean "drop all"
(an explicit kill switch). But we also need a separate state meaning
"field not set, use default 1.0." A non-pointer `float64` collapses
those two. The POC discovered this the obvious way (two tests failed
because their configs only set `MetricDenyList`, leaving `SampleRate`
at zero, which silently dropped everything).

The pointer-vs-nil distinction maps cleanly onto a `NULL`-able
database column when this graduates to configstore-backed.

#### Constraint 2 — Label deny strips VALUES, not slots

Prometheus vec metrics require every observation on a vec to carry the
SAME set of label names. If tenant A's emission omits `customer_name`
and tenant B's doesn't, Prom rejects A's observation (different
cardinality on the same vec). The POC keeps the label slot and
replaces the value with `""`. Empty string is the standard Prom
convention for "no value" and aggregates predictably at query time
(`{customer_name=""}` is a valid selector).

#### Constraint 3 — Short-circuit ordering matters for cost

`Decide` orders: metric-deny → sample-drop → label-rewrite. Metric-deny
is the cheapest check (one string match against a small list) and the
most common in privacy-strict configs. Sample-drop short-circuits
before the label allocation. Label rewrite only runs when both
prior checks pass. This keeps the hot path under ~3 string ops + 1
rand call in the common case (tenant has no config), and under ~10
string ops in the busy case.

The hot path runs once per metric emission per request. The
PrometheusPlugin emits to ~5 vecs per LLM request on average, so
budget is ~5 Decide calls per request. Profile-driven optimization
deferred until the bench shows a hot spot.

---

## 3. Proposed wiring

### 3.1 No OSS upstream interface change

Same pattern as [06](./06-plugin-isolation.md) and
[07](./07-dynamic-plugin-chain.md): the policy lives ENTIRELY in the
f5xc telemetry plugin's variant. OSS bifrost's `PrometheusPlugin`
constructor and hook signatures don't change.

The change is internal to `plugins/telemetry`: a new struct field on
`PrometheusPlugin`, populated at `Init` time when a `TenantConfigStore`
is supplied via a new optional `Config.TenantConfigStore` field. When
that field is nil (today's path / OSS path), `Decide` short-circuits
to `(true, labels)` for every call — zero overhead, zero behavior
change.

### 3.2 The configstore-backed implementation

Production swaps in:

```go
// framework/configstore/tables/telemetry.go
type TableTelemetryConfig struct {
    TenantID       string   `gorm:"primaryKey;type:varchar(255)"`
    SampleRate     *float64 `json:"sample_rate"`
    MetricDenyList []string `gorm:"serializer:json"`
    LabelDenyList  []string `gorm:"serializer:json"`
    UpdatedAt      time.Time
}
```

with a tiny `RDBConfigStore.GetTenantTelemetryConfig(ctx, tid)` method
behind a `configstoreTenantConfigStore` adapter that satisfies the
`TenantConfigStore` interface. The plugin caches the lookup result
keyed by tenant id, with a refresh on tenant evict (same hook as
governance — `multitenant.Manager.EvictTenant`).

Cache invariants: read-mostly map, RWMutex; write on Set (admin
endpoint), Delete on tenant DELETE cascade, full reload on tenant
evict (which the chain-policy invalidation path already triggers).

### 3.3 Admin endpoints

Two endpoints, parallel to the rest of the tenant-scoped admin surface
that patches 0023-0027 established:

- `GET  /api/tenants/{tid}/telemetry-config` — return the row or
  `404` if no config.
- `PUT  /api/tenants/{tid}/telemetry-config` — upsert the row.
  Triggers `EvictTenant` defer hook on success so the plugin reloads
  the policy.

`DELETE` collapses into `PUT` with the empty/null payload to keep the
API surface small.

---

## 4. Patch series outline

Two patches, in order:

- **patch17 — `telemetry-tenant-config-foundation`**: the POC types
  promoted into `plugins/telemetry/` as production code (drop the
  `_test.go` suffix; export the types). Adds the `Config.TenantConfigStore`
  optional field. Modifies emission sites in `PrometheusPlugin` to
  route through `Decide`. Behavior unchanged when `TenantConfigStore`
  is nil. Unit-tested with the same seven POC tests against the
  in-memory store + a smoke test using the real `PrometheusPlugin`.

- **patch18 — `telemetry-config-configstore-+-admin`**: adds
  `TableTelemetryConfig`, migration, configstore CRUD methods, admin
  endpoints, EvictTenant hook. Wires `Config.TenantConfigStore` to the
  configstore-backed adapter in
  [`server/plugins.go`](../../transports/bifrost-http/server/plugins.go).

No OSS upstream change in either patch.

---

## 5. Open dimensions for review

The POC validates the model. These are explicitly deferred for review
before patch17 work starts:

- **Cardinality cap as a tenant-axis knob** — should
  `TenantTelemetryConfig` also carry a `MaxDistinctLabelValues int`
  cap that drops emissions when a tenant exceeds N distinct values
  for a specified label (e.g. `model`)? The POC doesn't model this;
  v1 could add it as a fourth field. Probably yes, but the policy
  needs a small per-tenant counter that adds memory cost.

- **Tenant-scoped push gateway destination** — the
  `PushGatewayConfig` is per-plugin today. Per-tenant push targets
  (different deliverability requirements per customer) would mean N
  active `*push.Pusher` instances. Bigger surface, defer past v1.

- **OTel sibling** — the otel plugin
  ([`plugins/otel/`](../../plugins/otel/)) emits the same metric set
  via OTLP exporter. Should `TenantTelemetryConfig` apply to both
  plugins via a shared `Decide` helper, or stay Prometheus-specific?
  Leaning shared (the policy semantics are identical).

- **Where does the sampling decision live in a streaming response?**
  Streaming responses emit several metrics across multiple hook
  invocations (TTFT, inter-token, total). Should the policy decide
  once at PreLLMHook (entire response either sampled or not), or
  per-emission (some metrics of one response sampled, others not)?
  The POC treats them independently; the production version may want
  a single decision recorded in BifrostContext for consistency.

- **Visibility** — when the plugin drops emissions for a tenant,
  there's no record. Should we emit a special meta-metric
  `telemetry_drops_total{reason=sample|metric_deny|...,tenant_id=...}`
  so operators can see how much is being suppressed? Probably yes —
  cheap and lights up the dashboard.
