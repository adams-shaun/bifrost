import { expect, test } from '@playwright/test'
import { MultiTenantPage } from './pages/multi-tenant.page'

/**
 * Multi-tenant UI E2E coverage.
 *
 * Assumes a bifrost-http running on BASE_URL with multi-tenancy enabled
 * (BIFROST_MULTI_TENANT_ENABLED=true) and auth enabled. The test seeds
 * tenants via the admin API as a setup step. Credentials come from env:
 *
 *   BIFROST_E2E_ADMIN_USER  (default "admin")
 *   BIFROST_E2E_ADMIN_PASS  (required when auth is on)
 *   BIFROST_E2E_API_BASE    (default `${BASE_URL}` — bifrost serves /api
 *                            on the same origin)
 *
 * The seed step is idempotent: each create POST treats 409 as "already
 * exists" so the suite can rerun without manual cleanup.
 */

const ADMIN_USER = process.env.BIFROST_E2E_ADMIN_USER ?? 'admin'
const ADMIN_PASS = process.env.BIFROST_E2E_ADMIN_PASS ?? ''
const API_BASE = process.env.BIFROST_E2E_API_BASE ?? process.env.BASE_URL ?? 'http://localhost:3000'

const TENANT_A = { id: 'e2e-acme', name: 'E2E Acme' }
const TENANT_B = { id: 'e2e-globex', name: 'E2E Globex' }

// authHeader generates the Basic header bifrost expects on /api/* when
// auth is enabled. We hit the admin API directly for seeding — the UI
// path is what the tests are actually about; this is plumbing.
function authHeader(): Record<string, string> {
  if (!ADMIN_PASS) return {}
  const creds = Buffer.from(`${ADMIN_USER}:${ADMIN_PASS}`).toString('base64')
  return { Authorization: `Basic ${creds}` }
}

async function ensureTenant(id: string, name: string): Promise<void> {
  const resp = await fetch(`${API_BASE}/api/platform/tenants`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeader() },
    body: JSON.stringify({ id, name, status: 'active' }),
  })
  // 201 = created, 409 = already there — both are acceptable for an
  // idempotent setup. Anything else is a real failure.
  if (resp.status !== 201 && resp.status !== 409) {
    throw new Error(`ensureTenant(${id}): ${resp.status} ${await resp.text()}`)
  }
}

async function ensureProviderForTenant(tid: string, provider: string, baseURL: string): Promise<void> {
  const resp = await fetch(`${API_BASE}/api/tenants/${encodeURIComponent(tid)}/providers`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeader() },
    body: JSON.stringify({
      provider,
      network_config: { base_url: baseURL, default_request_timeout_in_seconds: 30 },
      concurrency_and_buffer_size: { concurrency: 10, buffer_size: 100 },
    }),
  })
  if (resp.status !== 201 && resp.status !== 409) {
    throw new Error(`ensureProviderForTenant(${tid}, ${provider}): ${resp.status} ${await resp.text()}`)
  }
}

test.describe('Multi-tenant — login picker', () => {
  test.beforeAll(async () => {
    await ensureTenant(TENANT_A.id, TENANT_A.name)
    await ensureTenant(TENANT_B.id, TENANT_B.name)
  })

  test('login page shows tenant dropdown listing seeded tenants', async ({ page }) => {
    const mt = new MultiTenantPage(page)
    await mt.gotoLogin()

    // The dropdown only renders as a Select when there are 2+ tenants.
    // Seed step guarantees this so the test isn't environment-dependent.
    await expect(mt.tenantSelectTrigger).toBeVisible()
    await mt.tenantSelectTrigger.click()

    // Both seeded tenants should show up as options. Use data-value
    // (the id) rather than the visible name so the assertion survives
    // renames.
    await expect(page.locator(`[role="option"][data-value="${TENANT_A.id}"]`)).toBeVisible()
    await expect(page.locator(`[role="option"][data-value="${TENANT_B.id}"]`)).toBeVisible()
  })

  test('logging in with a selected tenant persists it to localStorage', async ({ page }) => {
    test.skip(!ADMIN_PASS, 'BIFROST_E2E_ADMIN_PASS required for login flow')
    const mt = new MultiTenantPage(page)
    await mt.gotoLogin()
    await mt.login(ADMIN_USER, ADMIN_PASS, TENANT_B.id)
    await mt.waitForDashboard()

    expect(await mt.getPersistedTenantID()).toBe(TENANT_B.id)
    await expect(mt.tenantBadge).toContainText(TENANT_B.name)
  })

  test('an unauthenticated visit to /api/session/tenants returns minimal info only', async ({ request }) => {
    // The public picker endpoint must NOT leak admin metadata (no
    // description, timestamps, etc.) — only id/name/status. Guards
    // against accidentally pointing the picker at the protected
    // /api/platform/tenants and forgetting to lock down the auth bypass.
    const resp = await request.get(`${API_BASE}/api/session/tenants`)
    expect(resp.status()).toBe(200)
    const body = await resp.json()
    expect(Array.isArray(body.tenants)).toBe(true)
    for (const t of body.tenants) {
      const allowedKeys = new Set(['id', 'name', 'status'])
      for (const k of Object.keys(t)) {
        expect(allowedKeys.has(k), `unexpected key "${k}" leaked from /api/session/tenants`).toBe(true)
      }
    }
  })
})

test.describe('Multi-tenant — in-workspace switcher', () => {
  test.beforeAll(async () => {
    await ensureTenant(TENANT_A.id, TENANT_A.name)
    await ensureTenant(TENANT_B.id, TENANT_B.name)
  })

  test.beforeEach(async ({ page }) => {
    test.skip(!ADMIN_PASS, 'BIFROST_E2E_ADMIN_PASS required for login flow')
    const mt = new MultiTenantPage(page)
    await mt.gotoLogin()
    await mt.login(ADMIN_USER, ADMIN_PASS, TENANT_A.id)
    await mt.waitForDashboard()
  })

  test('badge reflects the tenant picked at login', async ({ page }) => {
    const mt = new MultiTenantPage(page)
    await expect(mt.tenantBadge).toBeVisible()
    await expect(mt.tenantBadge).toContainText(TENANT_A.name)
  })

  test('switching tenants from the badge updates the badge label + localStorage', async ({ page }) => {
    const mt = new MultiTenantPage(page)
    await mt.switchTenantTo(TENANT_B.id, TENANT_B.name)
    expect(await mt.getPersistedTenantID()).toBe(TENANT_B.id)
  })

  test('a hard reload preserves the active tenant', async ({ page }) => {
    const mt = new MultiTenantPage(page)
    await mt.switchTenantTo(TENANT_B.id, TENANT_B.name)
    await page.reload()
    await expect(mt.tenantBadge).toContainText(TENANT_B.name)
  })
})

test.describe('Multi-tenant — config isolation (Stage 2 — these are RED until path rewrites land)', () => {
  // Stage 2 wires the admin pages (providers, VKs, MCP, teams, etc.) to
  // /api/tenants/{tid}/... so a tenant only sees its own rows. Before
  // that lands, these tests fail — they're red-now/green-later contracts.
  // Run them in --grep mode to focus.

  test.beforeAll(async () => {
    await ensureTenant(TENANT_A.id, TENANT_A.name)
    await ensureTenant(TENANT_B.id, TENANT_B.name)
    // Seed ONLY tenant A with a provider. After Stage 2, the providers
    // page on tenant B should show zero rows (provider config is per-
    // tenant) — today, both tenants share the legacy /api/providers
    // surface and would both see the row.
    await ensureProviderForTenant(TENANT_A.id, 'openai', 'http://example.com')
  })

  test.beforeEach(async ({ page }) => {
    test.skip(!ADMIN_PASS, 'BIFROST_E2E_ADMIN_PASS required for login flow')
    const mt = new MultiTenantPage(page)
    await mt.gotoLogin()
    await mt.login(ADMIN_USER, ADMIN_PASS, TENANT_A.id)
    await mt.waitForDashboard()
  })

  test('providers page on tenant A shows the seeded provider', async ({ page }) => {
    await page.goto('/workspace/providers')
    // The row carries the provider name; locator stays generic so the
    // page object doesn't need to ship with this spec.
    await expect(page.getByText('openai', { exact: false })).toBeVisible({ timeout: 10000 })
  })

  test('providers page on tenant B shows zero provider rows (isolation)', async ({ page }) => {
    const mt = new MultiTenantPage(page)
    await mt.switchTenantTo(TENANT_B.id, TENANT_B.name)
    await page.goto('/workspace/providers')
    // After Stage 2 wires /api/tenants/{tid}/providers, tenant B should
    // NOT see the row tenant A created. Today this assertion FAILS
    // because the page still hits /api/providers (global). That failure
    // is the marker for what Stage 2 needs to fix.
    await expect(page.getByText('openai', { exact: false })).toHaveCount(0, { timeout: 10000 })
  })
})
