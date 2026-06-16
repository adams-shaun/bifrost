import { Locator, Page, expect } from '@playwright/test'
import { BasePage } from '../../../core/pages/base.page'

/**
 * Page object for the multi-tenant flow — login page tenant picker,
 * the in-workspace TenantBadge switcher, and helpers for asserting
 * tenant config isolation across switching.
 *
 * Most fixtures here intentionally use data-testid hooks rather than
 * text matching so the strings can be localized without breaking tests.
 */
export class MultiTenantPage extends BasePage {
  // Login page
  readonly usernameInput: Locator
  readonly passwordInput: Locator
  readonly tenantSelectTrigger: Locator
  readonly signInButton: Locator
  readonly tenantErrorBanner: Locator

  // Workspace shell
  readonly tenantBadge: Locator

  constructor(page: Page) {
    super(page)
    this.usernameInput = page.locator('input#username')
    this.passwordInput = page.locator('input#password')
    this.tenantSelectTrigger = page.locator('button#tenant')
    this.signInButton = page.getByRole('button', { name: /sign in/i })
    this.tenantErrorBanner = page.getByText(/unable to list tenants/i)
    this.tenantBadge = page.getByTestId('tenant-badge')
  }

  async gotoLogin(): Promise<void> {
    await this.page.goto('/login')
    await this.page.waitForLoadState('domcontentloaded')
  }

  /**
   * Sign in with the supplied credentials, picking the tenant from the
   * dropdown if it has more than one entry. The single-tenant case
   * renders a plain "Signing in to X" label instead of a Select, so the
   * caller must pass `tenantID = null` for that case.
   */
  async login(username: string, password: string, tenantID: string | null): Promise<void> {
    await this.usernameInput.fill(username)
    await this.passwordInput.fill(password)
    if (tenantID) {
      // The Select is a Radix component — open it and pick the item by
      // its visible id text (the helper text under the tenant name).
      await this.tenantSelectTrigger.click()
      // Items render as role=option; match the value attribute since the
      // visible label is "Name {status?}" rather than the bare id.
      await this.page.locator(`[role="option"][data-value="${tenantID}"]`).click()
    }
    await this.signInButton.click()
    // Wait for navigation away from /login — the login flow uses
    // navigate({ to: "/workspace" }) which goes through the workspace
    // redirect to /workspace/dashboard.
    await this.page.waitForURL(/\/workspace/, { timeout: 15000 })
  }

  /** Read the persisted tenant id from localStorage. Returns null if unset. */
  async getPersistedTenantID(): Promise<string | null> {
    return this.page.evaluate(() => window.localStorage.getItem('bifrost-current-tenant-id'))
  }

  /**
   * Switch tenants via the in-workspace badge. Asserts the current
   * label changes after selection.
   */
  async switchTenantTo(tenantID: string, expectedLabelSubstring: string): Promise<void> {
    await expect(this.tenantBadge).toBeVisible()
    await this.tenantBadge.click()
    // The dropdown lists the id under the name (10px muted span); match
    // on that to pick the right row even when names duplicate.
    await this.page.getByRole('menuitem').filter({ hasText: tenantID }).click()
    await expect(this.tenantBadge).toContainText(expectedLabelSubstring)
  }

  /**
   * Wait for the dashboard to be the active route. The workspace
   * landing redirects to /workspace/dashboard, so this is a stable
   * marker for "login flow finished".
   */
  async waitForDashboard(): Promise<void> {
    await this.page.waitForURL(/\/workspace\/dashboard/, { timeout: 15000 })
  }
}
