# Dynamic plugin-chain — policy-selected variants per request

**Status:** design doc backed by a POC at
[`plugins/governance/chainpolicy_poc_test.go`](../../plugins/governance/chainpolicy_poc_test.go)
on 2026-06-17. Not yet integrated into the plugin execution path; not
yet emitted as a patch in `f5xc-patches/`. The POC validates the
selector pattern + CEL evaluation + audit trail; this doc proposes how
it integrates with the per-tenant plugin instance work
([06-plugin-isolation.md](./06-plugin-isolation.md)).

Extends Phase 3 from
[04-design-poc-to-v1.md](./04-design-poc-to-v1.md) and answers the
"how do we keep this from turning into spaghetti?" question that comes
up the moment a single plugin needs to behave differently based on
who's calling.

---

## 1. The shift this doc addresses

[06-plugin-isolation.md](./06-plugin-isolation.md) handles **per-tenant
CONFIG**: different tenants want different settings on the same plugin
(different cache TTL, different telemetry sink). That's static at
tenant Acquire time — solved by per-tenant plugin instances built by
the TenantLoader.

This doc handles **per-request chain SHAPE**: same tenant, but
different requests get different chains. The trigger example is:

> Virtual key X is for the red team. Their requests should:
> - bypass the AI-guardrails plugin entirely;
> - divert telemetry away from the local SQL / OTEL→Grafana sink, into
>   a protected S3 bucket;
> - use a protected audit log instead of the standard one.

Hand-waving this gets you spaghetti fast (see §4). The pattern in §3
keeps it lasagna.

---

## 2. Customization axes per plugin

The customization surface, mapped against the plugin set, is roughly
two-dimensional: tenant-axis (settled at TenantLoader time) vs.
request-axis (settled by chain policy at request time).

| Plugin | Tenant-axis (config snapshot) | Request-axis (chain selection) |
|---|---|---|
| governance | policy version, default budget, hierarchical scope | per-VK quotas (today), per-VK routing rules (today) |
| telemetry | sink (Prom/Datadog/OTLP), sample rate, label allowlist | per-VK metric prefix, **per-VK divert to S3** |
| semantic cache | namespace, TTL, threshold, embedding model | per-VK opt-out, per-model opt-out |
| guardrails | rule set, strictness, scan provider (CalypsoAI/F5/Azure) | **per-VK skip (red team)**, per-model skip |
| audit / log | sink (local SQL/S3/SIEM), redaction policy | **per-VK redaction profile, per-VK sink override** |
| prompts | template library, version pin | per-VK template allowlist |
| MCP | client allowlist, tool allowlist | per-VK tool allowlist (already in schema) |
| OTel | exporter, sampling, propagator | per-VK trace attributes |
| maxim | endpoint, auth token | per-VK auth scoping (if maxim supports it) |
| mocker, logging, jsonparser, compat | none meaningful | none meaningful |

The bolded entries are the ones the red-team example touches. The
pattern below addresses *all* request-axis customization, not just
those three.

---

## 3. The pattern: separate selector from execution

One function, called once per request, very early (after VK resolve,
before any `PreLLMHook`):

```go
type ChainPolicy interface {
    Select(ctx, in PolicyInput) (ChainPlan, error)
}

type ChainPlan struct {
    Steps []ChainStep  // ordered; each is (plugin name, variant name, why)
    Why   []string     // top-level audit: which rule fired, what fell through
}
```

The selector returns a flat, auditable plan. Every plugin in the plan
runs unconditionally — the *decision* about what runs is made upstream,
not inside the plugins. The plugins themselves stay independent and
know nothing about each other, about tenants, or about VK tags.

### 3.1 Variants

Plugins declare named variants. The variant name is the only thing the
policy needs to know — the variant's config is owned by the plugin
author (much like Go's typed enum convention vs. open string maps).

Concrete examples:

| Plugin | Variants |
|---|---|
| `guardrails` | `enforce` (default), `skip` (red team), `audit-only` |
| `telemetry` | `local` (default), `s3-divert` (red team), `none` |
| `audit` | `standard` (default), `protected` (red team), `tamperproof` |
| `semantic-cache` | `standard` (default), `off` (per-VK opt-out) |
| `prompts` | `team-templates`, `personal`, `none` |

A plugin with one variant is operationally identical to today's "no
variants" plugin — backwards compatible.

### 3.2 The CEL rule engine

Rules are CEL expressions over a small set of variables. The POC
([chainpolicy_poc_test.go::newChainPolicyCELEnv](../../plugins/governance/chainpolicy_poc_test.go))
exposes:

```
tenant_id   string
vk_id       string
vk_name     string
vk_tags     list<string>
vk_role     string
```

Rule shape:

```go
Rule{
    Name: "red-team-divert",
    Expr: `"red-team" in vk_tags`,
    Plan: []ChainStep{
        {Plugin: "guardrails", Variant: "skip"},
        {Plugin: "telemetry",  Variant: "s3-divert"},
        {Plugin: "audit",      Variant: "protected"},
    },
},
```

Rules are tried in order; **first match wins**. No layered overlays in
v1 — that introduces composition rules the admin has to reason about.
A rule fully replaces the base plan when it fires.

The POC test
[TestChainPolicy_RedTeamBypassesAndDiverts](../../plugins/governance/chainpolicy_poc_test.go)
proves the red-team request gets `skip`/`s3-divert`/`protected` while a
standard prod VK falls through to `enforce`/`local`/`standard`.

### 3.3 Audit trail — visible by construction

Every `ChainPlan` carries:

- `ChainPlan.Why` — top-level: *which* rule matched (with the
  expression text), or `"no rule matched, using default chain"`.
- `ChainStep.Why` — per-step: the rule that picked this step.

A request-time hook can attach the plan to the request log so a
security audit sees:

```
request_id: r-abc
tenant_id:  acme
vk_id:      vk-red-001
chain_plan:
  why:
    - "matched rule: red-team-divert (\"red-team\" in vk_tags)"
  steps:
    - guardrails:skip       why=red-team-divert
    - telemetry:s3-divert   why=red-team-divert
    - audit:protected       why=red-team-divert
```

This is the property security reviews actually ask for: *"show me what
ran for this request and why."* Today's plugin chain can't answer that
honestly because the chain doesn't change between requests.

### 3.4 Failure semantics

- **Compile-time** — a syntactically broken rule fails
  `NewCELChainPolicy` (POC test
  [TestChainPolicy_MalformedExpressionFailsAtPolicyLoad](../../plugins/governance/chainpolicy_poc_test.go)).
  Admins can't deploy a broken rule and discover it at request time.
- **Runtime** — a rule that errors during evaluation (e.g. unexpected
  type, missing variable) is **fail-open**: log it, treat the rule as
  not-matched, try the next rule (POC test
  [TestChainPolicy_RuleEvalErrorDoesNotBreakRequest](../../plugins/governance/chainpolicy_poc_test.go)).
  Mirrors the F5 Guardrails plugin's fail-open default for unreachable
  scan service.
- **Divert sink unreachable** — if the variant the policy selected
  (`telemetry:s3-divert`) can't write, the plugin variant chooses its
  own fallback. Likely: buffer locally + log loudly; surface the failure
  via a `chain_divert_failed` metric. Per-plugin decision; not the
  policy layer's problem.

---

## 4. Anti-patterns the pattern explicitly avoids

Each of these is a real shape "dynamic plugin chains" can take that
turns the lasagna into spaghetti within ~3 PRs:

1. **Plugin-internal `if vk.HasTag("red-team")`**. Every plugin grows
   its own tag table. The chain *looks* clean but the logic is
   scattered across N plugins. Removing the concept of red-team is a
   12-file PR.

2. **`ctx.SetValue("skip_telemetry", true)`**. Invisible coupling.
   Reading the plugin chain doesn't tell you what actually runs —
   you'd have to chase context keys across every plugin.

3. **Plugin-ordering tricks**. A "router" plugin reorders the rest
   based on VK metadata. Fragile. Breaks every time someone adds a
   plugin, because new plugins don't know where they should sit in the
   reordering.

4. **N tenant runtimes × M chain variants**. If we materialize a
   distinct runtime per (tenant, chain-variant), `multitenant.Manager`'s
   LRU cache becomes a chain cache and capacity planning gets ugly. The
   selector pattern reuses one tenant runtime per tenant; the *plan*
   varies per request, not the runtime.

5. **Chain mutation mid-flight**. Plugins removing themselves from the
   chain during `PreLLMHook`. State after one plugin's mutation is
   non-obvious; tests can't reason about it.

The selector pattern avoids all five by collapsing the per-request
decision into one function call, with one output artifact (the plan),
that's both inspectable and audit-loggable.

---

## 5. How this composes with the per-tenant plugin isolation work

[06-plugin-isolation.md](./06-plugin-isolation.md) lands per-tenant
plugin INSTANCES. This doc lands per-request VARIANT selection.

The composition:

- `TenantLoader` (per-tenant runtime construction) builds per-tenant
  plugin instances *with all their variants*. Each tenant's plugin
  instance holds the full variant catalog the tenant is allowed to use.
  Forbidden variants (e.g. tenant doesn't have an S3 bucket configured)
  simply don't exist in the catalog.
- `ChainPolicy` (per-request chain selection) picks variants per
  request from that catalog. The policy lives ON the tenant
  configuration — different tenants can have different rule sets.
- `bifrost.Init`'s plugin-chain execution becomes: for each step in the
  plan, call the named plugin's named variant. Plugin authors expose a
  `RunVariant(ctx, req, variant string, hook stage)` entry point on
  top of (or as a thin wrapper around) `PreLLMHook` / `PostLLMHook`.

No interface change at OSS upstream — same pattern as
[06](./06-plugin-isolation.md) §3.1. The whole dynamic-chain layer is
f5xc-side machinery: the policy lives in `multitenant/chainpolicy/`
(promoted from the POC), the variant dispatch lives in each plugin's
own code (additive — single-variant plugins keep their existing hook
signature).

---

## 6. Patch series outline (after the patch17/18 series from doc 06)

Three patches, in order. None earlier than patch19 since they depend on
per-tenant plugin instances + per-tenant config landing first.

- **patch19 — `chainpolicy-foundation`**: promote the POC's types into
  `multitenant/chainpolicy/` as a real package. Adds `cel-go` as a
  direct dep on the `multitenant` module (already an indirect dep via
  governance through the workspace). Unit-tested with the same four
  tests the POC has, plus a property-style test for rule-ordering
  semantics.
- **patch20 — `variant-dispatch-shim`**: define the
  `RunVariant(ctx, req, variant, stage)` convention. Existing
  single-variant plugins gain a default `RunVariant` that ignores the
  variant string and calls the regular hook — so OSS plugins continue
  to work unchanged. Multi-variant plugins (guardrails, telemetry) own
  their dispatch table.
- **patch21 — `tenant-loader-chainpolicy-wire`**: load the
  `ChainPolicy` from the tenant's config (alongside the existing
  governance config), construct the policy at TenantLoader time, attach
  to the tenant runtime. Wire the per-request selector call into
  `bifrost.Bifrost.PreLLMHook` so the per-request plan is computed and
  the plugin chain executes accordingly. The audit trail (the
  `ChainPlan.Why`) gets attached to the request log at this layer.

Each patch follows the established overlay convention (source committed
to `f5xc-mt-overlay` + `f5xc-patches/patchN/`).

---

## 7. Open dimensions for review

The POC validates the core pattern (CEL rules → variant plan → audit
trail). It explicitly leaves these as open dimensions to nail down
before patch21:

- **Selection-time scope**: the POC selects at request entry, with only
  VK + tenant context. A v2 axis would re-select *after* the request
  body is parsed (model + provider + prompt content known). Re-selecting
  late means some plugins have already run with the wrong variant
  — re-classification needs careful Pre vs. Post hook split.

- **Variant config overrides**: the POC treats variants as named
  lookups. A real-world ask is "telemetry: standard, but sample at
  0.01" — a config override that the policy supplies. Two options:
  (a) variant catalog grows finer-grained (`standard-1pct`,
  `standard-100pct`); (b) policy supplies a `config map[string]any`
  alongside the variant. (a) keeps the catalog explicit + auditable;
  (b) is more flexible but the policy now needs schema knowledge.

- **Rule layering / overlays**: the POC's rules are full-chain
  replacements. Real policies might want "default chain + override
  guardrails to skip if red-team." Layered rules introduce composition
  semantics admins must reason about; deferred until needed.

- **Where the policy is stored**: today the POC compiles rules from
  Go literals. Production wants them in the configstore alongside the
  existing routing rules table. New `chain_policy_rules` table, or
  reuse `routing_rules` with a `kind` column? Lean reuse for consistency.

- **Plugin variants discovery**: how does the policy author know what
  variants a plugin offers? Each plugin declares them via a
  `Variants() []string` method that the admin UI surfaces, or a static
  catalog in the bifrost config? Lean toward static for v1 (one fewer
  plugin-interface change).

- **Performance**: CEL eval at every request adds latency. The
  governance plugin already does this for routing rules (~µs per rule);
  bench the worst case (10+ rules) with the `mt-balanced` scenario once
  patch21 lands.
