# Bifrost multi-tenant E2E test harness

Two layers of multi-tenant end-to-end testing live in this repo. This
directory is the **outer layer** — the one this README scopes.

## Two layers

### Layer 1 — in-process integration test (already implemented)

[`transports/bifrost-http/handlers/mt_integration_test.go`](../../transports/bifrost-http/handlers/mt_integration_test.go)

Boots an in-memory SQLite ConfigStore via the production `NewConfigStore`
constructor (so the real migration chain runs), wires every multi-tenant
handler together (platform-admin tenants CRUD, tenant-scoped provider
admin, tenant-scoped VK admin), and drives the full happy path with two
tenants. Asserts:

- Per-tenant provider config is isolated by `tenant_id`.
- VK resolver maps each minted VK to the right tenant.
- `multitenant.Manager.Acquire` lazy-loads a per-tenant runtime whose
  `Account` only sees that tenant's providers.
- `RequireTenantPathMiddleware` rejects tenant-admin cross-tenant
  attempts with 403.

Runs in seconds with `go test ./transports/bifrost-http/handlers/...`,
gates every MR, no Docker dependency.

### Layer 2 — k8s-style E2E (this directory; not yet implemented)

The outer layer. Boots the actual `bifrost-http` binary against a real
Postgres + Redis, behind an LB, in a `kind` cluster or via `docker
compose`. Drives via plain HTTP from `go test`. Validates everything
Layer 1 does plus:

- The Helm chart deploys correctly and wires multi-tenancy on.
- The Postgres-backed ConfigStore (not just SQLite) is consistent across
  replicas.
- The 60-second VK resolver cache TTL behaves correctly on a cold
  process.
- Streaming inference (SSE) holds the per-tenant runtime open for the
  goroutine lifetime under real network conditions.
- Async inference closures pick the right tenant runtime at execution
  time (not submit time) when the resolver state has changed in between.
- Multi-replica admin writes propagate (eviction signals or polled TTL).

## Proposed layout for Layer 2

```
test/e2e/
├── README.md                ← this file
├── go.mod                   ← separate module (testcontainers-go + helm/k8s deps are heavy)
├── harness/
│   ├── postgres.go          ← testcontainers Postgres helper
│   ├── bifrost.go           ← docker-compose / kind boot
│   └── http.go              ← typed HTTP client (admin + inference paths)
├── fixtures/
│   └── mock_provider/       ← in-process server impersonating OpenAI / Anthropic
├── helm/                    ← Helm chart values for the multi-tenant test
└── scenarios/
    ├── admin_sanity_test.go
    ├── tenant_isolation_test.go
    ├── inference_per_tenant_test.go
    ├── mcp_per_tenant_test.go
    ├── streaming_per_tenant_test.go
    ├── async_per_tenant_test.go
    ├── hot_reconfig_test.go
    └── ha_replication_test.go
```

## Why this isn't built yet

- The OSS Helm chart (if any) ships in [`docs/deployment-guides`](../../docs/deployment-guides);
  it has not been audited for multi-tenant-on configuration.
- testcontainers-go pulls in a large dependency tree; the e2e module
  should live in its own `go.mod` so the main workspace stays lean.
- The mock provider shape (OpenAI vs Anthropic vs both) depends on
  which providers we want to assert tenant isolation against; that
  call should be made by whoever owns the e2e suite long-term.
- The CI integration question (every MR vs nightly) hasn't been
  decided. Per-MR cadence rules out kind (~60s cluster boot); nightly
  rules out testcontainers as the only signal.

## How to add a scenario today, before Layer 2 lands

Add it to Layer 1 first
([`mt_integration_test.go`](../../transports/bifrost-http/handlers/mt_integration_test.go)).
If the scenario can be expressed purely in terms of "drive the admin
HTTP API, then drive the inference HTTP API, then assert" — Layer 1
covers it.

Layer 1 cannot cover: real network failures, multi-replica
coordination, Helm/k8s config drift, real-provider rate limits. Those
are the scenarios Layer 2 exists to test.

## Tracking

Jira: https://jira.f5net.com/browse/XC-25496
