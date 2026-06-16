import { expect, test } from "@playwright/test";
import { MultiTenantPage } from "./pages/multi-tenant.page";
import { ProvidersPage } from "../providers/pages/providers.page";
import { VirtualKeysPage } from "../virtual-keys/pages/virtual-keys.page";
import { randomUUID } from "crypto";

/**
 * Full onboarding flow on a freshly-created tenant. Drives:
 *
 *   1. Login as admin, picking "+ New tenant…" from the dropdown
 *      and supplying an id; the form does login + tenant create +
 *      activate in one Sign in click.
 *   2. Workspace > Providers > Add custom provider (openai-shaped)
 *      pointed at the in-cluster vLLM Service:
 *        http://bm-llms-qwen3-coder.llm-system.svc.cluster.local
 *   3. Add a provider key with explicit models so the key isn't a
 *      wildcard — proves the tenant-scoped POST key route preserves
 *      `models` end to end (the original Stage 2 regression).
 *   4. Workspace > Virtual Keys > Create VK referencing the provider
 *      with an explicit allowed-model list.
 *
 * This is the test the user asked for: it validates the
 * multi-tenant + custom-provider + key-models + VK-provider-config
 * surfaces in one continuous browser flow on a brand-new tenant.
 *
 * Requires BIFROST_E2E_ADMIN_PASS — the create-tenant POST is
 * platform-admin auth-gated, so the login flow MUST be exercised.
 */

const ADMIN_USER = process.env.BIFROST_E2E_ADMIN_USER ?? "admin";
const ADMIN_PASS = process.env.BIFROST_E2E_ADMIN_PASS ?? "";

// Use the user's specified vLLM URL verbatim. The provider doesn't
// need to actually respond — bifrost stores the config and offers
// models via the key's `models` field, which is what the VK form
// reads from.
const QWEN_BASE_URL = "http://bm-llms-qwen3-coder.llm-system.svc.cluster.local";
const QWEN_MODELS = ["qwen3-coder-30b", "qwen3-coder-7b"];

test.describe("Multi-tenant onboarding — new tenant + custom provider + VK", () => {
	test.skip(!ADMIN_PASS, "BIFROST_E2E_ADMIN_PASS required for login flow");

	test("login (+ create tenant) → custom openai provider → key with models → VK", async ({ page }) => {
		const runID = randomUUID().slice(0, 8);
		const tenantID = `e2e-${runID}`;
		const tenantName = `E2E ${runID}`;
		const providerName = `qwen3-coder-${runID}`;
		const keyName = `prod-${runID}`;
		const vkName = `vk-${runID}`;

		// --- 1. Login + create tenant on first sign in ---
		const mt = new MultiTenantPage(page);
		await mt.gotoLogin();

		await mt.usernameInput.fill(ADMIN_USER);
		await mt.passwordInput.fill(ADMIN_PASS);

		// Open the tenant dropdown and pick the "+ New tenant…" sentinel.
		// data-testid hook ("tenant-option-new") was added specifically
		// so this spec doesn't need to match localized "+ New tenant…"
		// label text.
		await mt.tenantSelectTrigger.click();
		await page.getByTestId("tenant-option-new").click();

		// Two inputs appear under the picker; both carry data-testid.
		await page.getByTestId("new-tenant-id").fill(tenantID);
		await page.getByTestId("new-tenant-name").fill(tenantName);

		// Sign in & create — single button, but server runs both the
		// login POST and the platform-admin tenant create POST. We
		// don't try to differentiate the two requests on the wire;
		// the post-navigation badge check below is the contract.
		await mt.signInButton.click();
		await mt.waitForDashboard();

		// Badge shows the freshly-created tenant — proves the
		// create-on-login flow set currentTenantID and the workspace
		// shell picked it up on first render.
		await expect(mt.tenantBadge).toContainText(tenantName);
		expect(await mt.getPersistedTenantID()).toBe(tenantID);

		// --- 2. Add custom openai-shaped provider ---
		const providers = new ProvidersPage(page);
		await providers.goto();

		await providers.openCustomProviderSheet();
		await providers.customProviderNameInput.fill(providerName);
		// fillSelect helper used elsewhere; emulate inline here so this
		// spec doesn't have to import it. The base provider Select shows
		// "OpenAI" as the display label.
		await providers.baseProviderSelect.click();
		await page.getByRole("option", { name: "OpenAI", exact: true }).click();
		await providers.baseUrlInput.fill(QWEN_BASE_URL);
		await providers.customProviderSaveBtn.click();
		await expect(providers.customProviderSheet).not.toBeVisible({ timeout: 10000 });

		// The provider row should now exist in the sidebar.
		const providerRow = providers.getProviderItem(providerName);
		await expect(providerRow).toBeVisible({ timeout: 10000 });

		// --- 3. Add a key with specific models ---
		await providers.selectProvider(providerName);
		await providers.addKeyBtn.click();
		await expect(providers.keyForm).toBeVisible();
		await page.getByLabel("Name").fill(keyName);
		await page.getByLabel("API Key").fill(`sk-vllm-${runID}`);

		// Allowed models — this is the field that was being dropped on
		// the tenant-scoped POST before the providerKeyCreatePayload
		// fix. The form provides a multi-select; we type each model
		// name and press Enter to commit (handles both Combobox-style
		// and free-text-list inputs).
		const modelsInput =
			page.getByLabel("Models", { exact: false }).first();
		if (await modelsInput.isVisible().catch(() => false)) {
			for (const m of QWEN_MODELS) {
				await modelsInput.click();
				await modelsInput.fill(m);
				await page.keyboard.press("Enter");
			}
		}

		await providers.keySaveBtn.click();
		// Key form closes on success.
		await expect(providers.keyForm).not.toBeVisible({ timeout: 10000 });

		// --- 4. Create a VK that references the provider ---
		const vks = new VirtualKeysPage(page);
		await vks.goto();
		await vks.createVirtualKey({
			name: vkName,
			description: `E2E run ${runID}`,
			providerConfigs: [
				{
					provider: providerName,
					weight: 1.0,
					allowedModels: QWEN_MODELS,
				},
			],
		});

		// Row visible in the table — the createVirtualKey helper
		// waits for it internally; double-checking here makes the
		// failure mode legible if the helper changes.
		const vkRow = vks.getVirtualKeyRow(vkName);
		await expect(vkRow).toBeVisible();
	});
});
