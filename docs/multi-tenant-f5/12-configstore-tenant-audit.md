# Configstore per-tenant scope audit

**Status:** read-only audit. No code in this commit — this is the
gap-list that drives future patch series. Companion docs already
cover specific slices in depth: [06-plugin-isolation.md](./06-plugin-isolation.md),
[09-telemetry-tenant-config.md](./09-telemetry-tenant-config.md),
[10-semantic-cache-namespace.md](./10-semantic-cache-namespace.md),
[11-hierarchical-policy-resolution.md](./11-hierarchical-policy-resolution.md).
This doc steps back and asks: across **every** persisted table, where
do we stand?

---

## TL;DR

| Bucket | Count | Notes |
| --- | ---: | --- |
| Scoped (S) — has `TenantID` column | 8 | All added by patch1; all enforced by patch2's callbacks |
| Global-intentional (GI) | 12 | System / registry / sync-state tables; correct to be global |
| Global-suspicious (GS) — gap | 14 | Business data without tenant scope; cross-tenant read/write today |
| Mixed (M) — scoped via FK only | 5 | Inherit scope from parent; explicit `TenantID` would harden them |

**Headline:** patch1 picked the right 8 tables for "round 1" (the
governance / VK / customer / provider trunk). Everything attached to
the **prompt, routing, pricing-override, OAuth, plugin, and
session-token** sub-systems is still global. The semantic-cache
vectorstore config row is also global (doc 10 covers the runtime
namespace gap separately).

---

## Methodology

Mechanism we are auditing against: the GORM callbacks in
[framework/configstore/tenant_scope.go](../../framework/configstore/tenant_scope.go)
auto-stamp `WHERE tenant_id = ?` on every SELECT/UPDATE/DELETE and
auto-populate `tenant_id` on INSERT for any model whose struct carries
a field named `TenantID`. Models without that field flow through
globally — no compile error, no warning, no test failure. The audit is
therefore a per-struct grep: does the field exist or not?

Files surveyed: every `*.go` under
[framework/configstore/tables/](../../framework/configstore/tables/)
except `utils.go` and `*_test.go`.

For each struct: classify as **S** (scoped), **GI** (intentionally
global), **GS** (suspiciously global — gap), or **M** (mixed: scoped
only via FK, no direct column).

---

## Scoped (S) — `TenantID` column present

These 8 tables were added by
[patch1/mt-foundation-schema](../../f5xc-patches/patch1/mt-foundation-schema/).

| Table | File | Composite-unique index |
| --- | --- | --- |
| `TableBudget` | [budget.go](../../framework/configstore/tables/budget.go) | plain index (rows attached to scoped parents) |
| `TableCustomer` | [customer.go](../../framework/configstore/tables/customer.go) | plain index |
| `TableKey` | [key.go](../../framework/configstore/tables/key.go) | `idx_key_tenant_name` |
| `TableMCPClient` | [mcp.go](../../framework/configstore/tables/mcp.go) | `idx_mcp_tenant_name` |
| `TableProvider` | [provider.go](../../framework/configstore/tables/provider.go) | `idx_providers_tenant_name` |
| `TableRateLimit` | [ratelimit.go](../../framework/configstore/tables/ratelimit.go) | plain index |
| `TableTeam` | [team.go](../../framework/configstore/tables/team.go) | plain index |
| `TableVirtualKey` | [virtualkey.go](../../framework/configstore/tables/virtualkey.go) | `idx_virtual_keys_tenant_name` |

The join tables under `virtualkey.go` (`TableVirtualKeyMCPConfig`,
`TableVirtualKeyProviderConfig`) do **not** carry their own
`TenantID` — they inherit scope via the FK to `TableVirtualKey`. See
**Mixed (M)** below.

---

## Global-intentional (GI) — correct to be global

| Table | File | Why it stays global |
| --- | --- | --- |
| `TableTenant` | [tenant.go](../../framework/configstore/tables/tenant.go) | The registry **of** tenants. Cannot be scoped to a tenant. |
| `TableConfigHash` | [confighash.go](../../framework/configstore/tables/confighash.go) | Schema-version state for `config.json` resync. Deployment-wide. |
| `TableDistributedLock` | [dlock.go](../../framework/configstore/tables/dlock.go) | Cluster-coordination mutex. Deployment-wide. |
| `TableFeatureFlag` | [featureflag.go](../../framework/configstore/tables/featureflag.go) | Server-wide toggle overrides. If per-tenant flags become a requirement, this becomes a gap — but not today. |
| `TableFrameworkConfig` | [framework.go](../../framework/configstore/tables/framework.go) | Global URLs (pricing feed, model-parameters datasheet). |
| `TableClientConfig` | [clientconfig.go](../../framework/configstore/tables/clientconfig.go) | CORS, header allowlist, pool size — process-global. |
| `TableGovernanceConfig` | [config.go](../../framework/configstore/tables/config.go) | Key-value system config (admin username, auth flags). |
| `TableEnvKey` | [env.go](../../framework/configstore/tables/env.go) | Audit trail for env-var-backed config keys. |
| `TableModel` | [model.go](../../framework/configstore/tables/model.go) | Model identity registry (provider + model). The catalog itself is global; per-tenant *access* to models is a runtime/governance concern, not a schema one. |
| `TableModelParameters` | [modelparameters.go](../../framework/configstore/tables/modelparameters.go) | Synced from external datasheet API. Capabilities are intrinsic to the model, not the tenant. |
| Encryption-key tables | [encryption.go](../../framework/configstore/tables/encryption.go) | KEK material. Process-global by design. |
| `TableLogStoreConfig` | [logstore.go](../../framework/configstore/tables/logstore.go) | Today the log sink is global. [Doc 06](./06-plugin-isolation.md) and [Doc 09](./09-telemetry-tenant-config.md) propose making log/telemetry routing per-tenant. If/when that lands as a *persisted* config (rather than a runtime plugin policy), this row moves to GS. |

> The "if/when" notes on `TableFeatureFlag` and `TableLogStoreConfig`
> are deliberate: both are correct as global *today* but become gaps
> the moment we ship per-tenant flags / per-tenant log sinks. Re-audit
> these two if either feature is greenlit.

---

## Global-suspicious (GS) — gaps to close

For each row below: purpose, leak vector, recommendation.

### Prompt subsystem (5 tables — single biggest gap)

#### `TablePrompt` ([prompts.go](../../framework/configstore/tables/prompts.go))
- **Purpose:** mutable prompt definitions; the user-facing AI/ML
  authoring surface.
- **Leak vector:** any tenant's dashboard can list, edit, or delete
  any other tenant's prompts. There is no scope filter at the row
  level; only the API layer separates them, and only if every handler
  remembers to filter — which the GORM callback would do for free if
  the column existed.
- **Recommendation:** add `TenantID` with `default:default` and
  `uniqueIndex:idx_prompts_tenant_name` (drop bare unique on `name`).

#### `TablePromptVersion` ([promptVersions.go](../../framework/configstore/tables/promptVersions.go))
- **Purpose:** immutable version snapshots of a prompt; rollback /
  audit history.
- **Leak vector:** cross-tenant version-id collision; version GETs
  bypass any handler-level prompt scope check because they key on
  version_id alone.
- **Recommendation:** add `TenantID`. Validate parent prompt's
  `TenantID` matches on insert.

#### `TablePromptSession` ([promptSessions.go](../../framework/configstore/tables/promptSessions.go))
- **Purpose:** mutable working sessions during prompt authoring.
- **Leak vector:** session-id navigation across tenants — one
  tenant's UI can resume another tenant's editing session.
- **Recommendation:** add `TenantID`. Match parent prompt on load.

#### `TablePromptSessionMessage`, `TablePromptVersionMessage`
- **Purpose:** message-content fragments owned by sessions / versions.
- **Status:** these are M-class (inherit via FK to a GS parent). They
  become S-class transitively when the parents are fixed, but adding
  an explicit `TenantID` column hardens them against accidental
  joinless queries.

#### `TableFolder` ([folders.go](../../framework/configstore/tables/folders.go))
- **Purpose:** organizational container for prompts.
- **Leak vector:** folder hierarchy listed globally; cascade-delete on
  a folder could touch another tenant's prompts.
- **Recommendation:** add `TenantID` with `uniqueIndex` on
  `(tenant_id, name)`.

### Pricing subsystem (2 tables)

#### `TableModelPricing` ([modelpricing.go](../../framework/configstore/tables/modelpricing.go))
- **Purpose:** per-(model, provider, mode) input/output/cache token
  prices used during cost calculation.
- **Why this is contested:** prices are partly intrinsic to the
  upstream provider (and partly synced from a global feed via
  `TableFrameworkConfig`), so the "global table is correct" argument
  has merit. But: F5XC offers tenants negotiated rates, surcharges, or
  reseller markups; today there is no schema room for that.
- **Recommendation:** add a **nullable** `TenantID` and a
  unique-fallback strategy: a tenant-scoped row wins, else the global
  (`tenant_id IS NULL`) row. This is the pattern that lets the upstream
  feed keep populating global rows while admin-side patches insert
  per-tenant overrides. Pair with the resolver model from
  [doc 11](./11-hierarchical-policy-resolution.md).

#### `TablePricingOverride` ([pricingoverride.go](../../framework/configstore/tables/pricingoverride.go))
- **Purpose:** governance pricing overrides keyed by (scope_kind,
  vk_id | provider_id | provider_key_id, match_type, pattern).
- **Leak vector:** the override row references a VK or Provider by ID;
  if a tenant fabricates an ID belonging to another tenant, the
  override applies cross-tenant. The FK lookup happens in the
  governance plugin runtime, **after** the row is loaded — there's no
  GORM-level guard.
- **Recommendation:** add `TenantID` with `default:default`,
  `uniqueIndex:idx_pricing_override_tenant_name`. Validate
  `VirtualKeyID` / `ProviderID` resolve to a row with matching
  `TenantID` on save.

### Routing subsystem (2 tables)

#### `TableRoutingRule` ([routing_rules.go](../../framework/configstore/tables/routing_rules.go))
- **Purpose:** CEL-based routing rules with `Scope ∈ {global, team,
  customer, virtual_key}` and a `ScopeID` referencing the entity.
- **Leak vector:** `ScopeID` references a scoped entity (team /
  customer / VK), but there is no enforced cross-check that the
  routing rule's caller belongs to the same tenant as `ScopeID`.
- **Recommendation:** add `TenantID` with `default:default`. Keep the
  `idx_routing_rule_scope_name` index; extend to
  `(tenant_id, scope, scope_id, name)`. Validate `ScopeID` resolves
  within tenant on save.

#### `TableRoutingTarget` ([routing_rules.go](../../framework/configstore/tables/routing_rules.go))
- **Status:** M (inherits scope from parent rule). Becomes S
  transitively. An explicit column is defense-in-depth.

### Auth / session subsystem (5 tables)

#### `SessionsTable` ([sessions.go](../../framework/configstore/tables/sessions.go))
- **Purpose:** dashboard / admin session tokens.
- **Leak vector:** if a token is issued for tenant A, today nothing in
  the row binds it to tenant A — the validation path has to remember
  to check.
- **Recommendation:** add `TenantID` with `uniqueIndex` on
  `(tenant_id, token_hash)`. Forces every session lookup to be
  tenant-scoped at the DB level.

#### OAuth tables (`TableOauthConfig`, `TableOauthToken`, `TableOauthUserSession`, `TableOauthUserToken` — [oauth.go](../../framework/configstore/tables/oauth.go))
- **Purpose:** OAuth credentials and session/token state for MCP
  client auth flows.
- **Leak vector:** OAuth credentials belong to a tenant's MCP
  integrations; today they're a global pool. A misuse of an MCP
  client_id across tenants can resolve to another tenant's stored
  token.
- **Recommendation:** add `TenantID` to all four. Tighter:
  `TableOauthUserToken` and `TableOauthUserSession` should also gain
  composite unique indexes that include `tenant_id`.

#### `TempToken` ([temp_token.go](../../framework/configstore/tables/temp_token.go))
- **Purpose:** short-lived narrow-scope credentials (e.g., MCP-auth
  flow tokens).
- **Leak vector:** lookup is by `token_hash` alone. If a tenant gets
  hold of (or guesses) another tenant's plaintext token, the row will
  resolve and the bound `resource_id` will be exposed.
- **Recommendation:** add `TenantID`; constrain the
  `idx_temp_token_hash` unique index to `(tenant_id, token_hash)`. The
  token-hash entropy makes the practical risk small but the
  defense-in-depth value is high.

### Plugin / governance config (2 tables)

#### `TablePlugin` ([plugin.go](../../framework/configstore/tables/plugin.go))
- **Purpose:** persisted plugin config; the persisted form of what
  [doc 06](./06-plugin-isolation.md) and
  [doc 07](./07-dynamic-plugin-chain.md) discuss.
- **Leak vector:** the unique index is on `name` alone, so two tenants
  cannot both have a "guardrails" plugin with different configs.
- **Recommendation:** add `TenantID` with
  `uniqueIndex:idx_plugin_tenant_name`. Required prerequisite for the
  per-tenant plugin manager described in doc 06.

#### `TableModelConfig` ([modelconfig.go](../../framework/configstore/tables/modelconfig.go))
- **Purpose:** per-model rate-limit and budget assignments (FK to
  `TableBudget` and `TableRateLimit`, both of which are scoped).
- **Leak vector:** the row has no `TenantID`; today the *parent*
  budget/ratelimit IDs do, but no DB-level guard prevents tenant A
  from assigning tenant B's budget to a model.
- **Recommendation:** add `TenantID`. Validate `BudgetID` and
  `RateLimitID` resolve within tenant on save. This was *not* fixed by
  patch1 even though both parent FKs are scoped — easy to miss because
  the upstream catalog (`TableModel`) is correctly global.

### Vectorstore config (1 table)

#### `TableVectorStoreConfig` ([vectorstore.go](../../framework/configstore/tables/vectorstore.go))
- **Purpose:** vector backend (Weaviate / Qdrant / Pinecone / Redis)
  connection config used by the semantic cache plugin.
- **Leak vector:** today the whole deployment shares one
  vector-backend config row. Doc 10's runtime namespace fix gives
  per-tenant *data* isolation; if v2 wants per-tenant *backends* (e.g.,
  one tenant on managed Pinecone, another on local Redis), this row
  needs to be tenant-scoped.
- **Recommendation:** keep as GI for v1 (matches today's behavior);
  flip to GS the moment per-tenant backend selection is on the
  roadmap. Documented as a watch-item rather than a blocking gap.

---

## Mixed (M) — scoped via FK, no explicit column

| Table | Parent FK | Note |
| --- | --- | --- |
| `TableVirtualKeyMCPConfig` | `VirtualKeyID` → `TableVirtualKey` | Hardens with explicit `TenantID` but not blocking. |
| `TableVirtualKeyProviderConfig` | `VirtualKeyID` → `TableVirtualKey` | Same. |
| `TableRoutingTarget` | `RuleID` → `TableRoutingRule` | Becomes M once routing rule is scoped; otherwise gap. |
| `TablePromptVersionMessage` | `VersionID` → `TablePromptVersion` | Becomes M once version is scoped. |
| `TablePromptSessionMessage` | `SessionID` → `TablePromptSession` | Becomes M once session is scoped. |

Pattern recommendation: as we close each parent gap, **also** add the
column on the child rather than relying on the FK transitively. The
cost is one column; the win is that any future raw-SQL or
`db.Table()` query that bypasses the schema is still protected.

---

## Patch history — what got `tenant_id` and when

| Patch | Series | What it did |
| --- | --- | --- |
| patch1 | `mt-foundation-schema` | Added `TenantID` columns to the 8 S-class tables above; introduced `multitenant.TenantID` |
| patch2 | `mt-data-scope-callback` | Registered the GORM callbacks that make the column actually do something |
| patch3 | `mt-http-resolver` | Resolved `x-f5xc-tenant` header → ctx |
| patch4 | `mt-handler-tenant-routing` | Per-tenant routing in HTTP handlers |
| patch5 | `mt-ui-tenant-picker` | Dashboard tenant selector |
| patch6 | `mt-log-telemetry-tenant` | Per-tenant log isolation (runtime, not schema) |
| patch7 | `mt-runtime-isolation` | Per-tenant `*bifrost.Bifrost` runtimes |
| patch8 | `mt-tenant-cascade-delete` | Cascade-delete on tenant removal |
| patch9 | `mt-scope-callback-order` | Fixed callback ordering vs. BeforeCreate hooks |
| patch10-14 | scope hardening | Various `GetProviderKeys` / VK budget / updateprovider scope fixes |
| patch15 | `mt-semanticcache-tenant-scope` | Semantic-cache key-prefix scope ([doc 10](./10-semantic-cache-namespace.md) explains why this didn't close the ANN-graph gap) |
| patch16 | `mt-mcp-tenant-scope` | MCPManager name-collision fix |

**No patch has added `tenant_id` to any of the GS-class tables above.**
Every gap in this audit is greenfield.

---

## API-surface gap follow-ups

Adding `TenantID` to the schema is necessary but not sufficient — the
admin/management handlers also have to authenticate the request's
tenant and reject mismatches.

Handler directory:
[transports/bifrost-http/handlers/](../../transports/bifrost-http/handlers/).
Handlers operating on GS-class tables (and therefore needing
write-side `ValidateTenantContext` checks once the columns exist):

- `prompts.go` — `TablePrompt`, `TablePromptVersion`,
  `TablePromptSession`
- `governance.go` (or wherever pricing-override/routing-rule CRUD
  lives) — `TablePricingOverride`, `TableRoutingRule`
- `plugins.go` — `TablePlugin`
- `mcp_sessions.go` / oauth — OAuth tables, `TempToken`
- `sessions.go` — `SessionsTable`

(Not enumerated route-by-route here; an "is the handler scope-clean?"
sweep belongs in its own audit once the columns exist.)

---

## Recommended next steps (prioritized by blast radius)

1. **Prompt subsystem patch series.** Highest blast radius (most
   visible in the UI, most user-facing data). One patch series adding
   `TenantID` to `TablePrompt`, `TablePromptVersion`,
   `TablePromptSession`, the two message tables, and `TableFolder`,
   plus handler validation. Estimate: ~6 table edits + ~3 handler
   touch-ups.

2. **Plugin + Governance patch series.** Tied for second on blast
   radius (plugin config can run code; routing rules / pricing
   overrides shape every request). Patch series:
   `TablePlugin`, `TableRoutingRule`, `TableRoutingTarget`,
   `TablePricingOverride`, `TableModelConfig`. Pairs with
   [doc 06](./06-plugin-isolation.md) (manager wiring) and
   [doc 07](./07-dynamic-plugin-chain.md) (chain selection).

3. **Auth/session patch series.** OAuth tables + `SessionsTable` +
   `TempToken`. Defense-in-depth — the entropy of token hashes makes
   the practical risk lower than the prompt or routing gaps, but
   shipping this closes the audit story cleanly.

4. **Pricing override hierarchy.** `TableModelPricing` is the only
   table that genuinely wants the **nullable-tenant fallback** pattern
   rather than a strict `default:default` column. Worth a separate
   design pass (likely a small extension to
   [doc 11](./11-hierarchical-policy-resolution.md)'s resolver).

5. **Re-audit triggers.** `TableFeatureFlag` becomes GS the day
   per-tenant feature flags ship; `TableLogStoreConfig` becomes GS
   the day per-tenant persisted log sinks ship;
   `TableVectorStoreConfig` becomes GS the day per-tenant vector
   backends ship. Add these to the project re-audit checklist rather
   than fixing them speculatively now.

---

## What this audit does **not** cover

- Runtime-side leaks. Doc 03 (`03-things-we-missed.md`) and the docs
  06-11 series cover plugin instances, outbound HTTP clients, log
  sinks, telemetry, and Redis client isolation. Those are
  *non-configstore* state.
- The handler scope-cleanliness sweep. Per-handler grep for
  `db.Table(...).Where(...)` patterns that bypass model-bound queries
  (and thus the callback) deserves its own audit once the columns
  exist.
- Migrations. None of the recommendations above include the
  GORM-AutoMigrate vs. manual-migration choice for a `default:default`
  fill-down on the existing rows; that's per-patch detail, not audit
  scope.
- External systems (Redis, vector backends, log sinks). Doc 11 Q1
  covers Redis-side isolation; the same pattern applies to any
  external shared resource.
