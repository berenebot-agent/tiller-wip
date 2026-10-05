// first-run.spec.js — the local first-run lane. This shard starts against a
// router with NO admin credentials (see run.sh), so the setup page replaces
// the login form. The spec claims the instance, then drives the reused
// onboarding wizard end to end against the mock upstream.
const { test, expect } = require('@playwright/test');

const ADMIN_USER = 'first-run-admin';
const ADMIN_PASS = 'first-run-password';
const MOCK_BASE = process.env.TILLER_BROWSER_MOCK_BASE_URL || 'http://127.0.0.1:18081/v1';

test('first-run setup claims the instance, opens the wizard, and completes onboarding', async ({ page }) => {
  // Boot lands on the setup page, not login.
  await page.goto('/');
  const setupForm = page.locator('#setup-form');
  await expect(setupForm).toBeVisible();
  await expect(page.locator('#login-form')).toBeHidden();
  await setupForm.getByLabel('Admin username').fill(ADMIN_USER);
  await setupForm.getByLabel('Password', { exact: true }).fill(ADMIN_PASS);
  await setupForm.getByLabel('Confirm password').fill(ADMIN_PASS);

  // Mismatched confirm stays on the page with a clean error.
  await setupForm.getByLabel('Confirm password').fill('something-else');
  await setupForm.getByRole('button', { name: 'Create admin and continue' }).click();
  await expect(page.locator('#setup-error')).toHaveText('Passwords do not match.');
  await setupForm.getByLabel('Confirm password').fill(ADMIN_PASS);

  // The claim mints a session and lands in the app; the wizard auto-opens.
  await setupForm.getByRole('button', { name: 'Create admin and continue' }).click();
  await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();
  const wizard = page.locator('#wizard-dialog');
  await expect(wizard).toBeVisible();
  await expect(wizard).toContainText('Connect a provider');

  // Runtime now reports configured; the setup page can never come back, and
  // the session minted by setup reaches an admin endpoint with the stored
  // username.
  const runtime = await page.request.get('/api/runtime');
  expect(runtime.ok()).toBeTruthy();
  const runtimeBody = await runtime.json();
  expect(runtimeBody.setup_required).toBe(false);
  expect(runtimeBody.wizard_enabled).toBe(true);
  const replay = await page.request.post('/api/admin/setup', { data: { username: 'intruder', password: 'intruder-password' } });
  expect(replay.status()).toBe(404);
  const session = await page.request.get('/api/admin/session');
  expect(session.ok()).toBeTruthy();
  expect((await session.json()).username).toBe(ADMIN_USER);

  // Wizard step 1 — provider. Open the shared provider form, pick the generic
  // OpenAI-compatible type, point it at the mock upstream.
  await wizard.getByRole('button', { name: 'Add provider' }).click();
  const providerDialog = page.locator('#form-dialog');
  await expect(providerDialog).toBeVisible();
  await providerDialog.getByLabel('Search provider types').fill('generic');
  await providerDialog.getByRole('option', { name: /Generic OpenAI-compatible/ }).click();
  await providerDialog.getByLabel('Provider name').fill('first-run-provider');
  await providerDialog.getByLabel('Base URL').fill(MOCK_BASE);
  await providerDialog.getByRole('button', { name: 'Add & discover' }).click();
  await expect(providerDialog).toBeHidden();
  // The provider callback advances the wizard to the client-key step.
  await expect(wizard).toContainText('Create a client key');

  // Wizard step 2 — client key with all-model access (the provider callback
  // already advanced the wizard here). Creating it advances the wizard while
  // the one-time secret dialog is open.
  await wizard.getByRole('button', { name: 'All models' }).click();
  const clientDialog = page.locator('#form-dialog');
  await expect(clientDialog).toBeVisible();
  await clientDialog.getByLabel('Client name').fill('first-run-client');
  await clientDialog.getByRole('button', { name: 'Create & show key' }).click();
  const secretDialog = page.locator('#secret-dialog');
  await expect(secretDialog).toBeVisible();
  await expect(page.locator('#secret-value')).toHaveText(/^sk-tr-/);
  await page.getByRole('button', { name: 'I have stored the key' }).click();
  await expect(secretDialog).toBeHidden();

  // Wizard step 3 — virtual route optional; skip to Connect.
  await expect(wizard).toContainText('Create a virtual route');
  await wizard.getByRole('button', { name: 'Continue' }).click();

  // Wizard step 4 — connect: the curl snippet names an available model.
  await expect(wizard).toContainText('Point your tool at Tiller');
  await expect(wizard.locator('.secret-box code')).toContainText('/v1/chat/completions');

  // Done hides the wizard and the Get started button: the account now has a
  // provider and a client key, so onboarding is complete.
  await wizard.getByRole('button', { name: 'Done' }).click();
  await expect(wizard).toBeHidden();
  await expect(page.locator('#open-wizard')).toBeHidden();
  const onboarding = await page.request.get('/api/auth/onboarding');
  expect(onboarding.ok()).toBeTruthy();
  const state = await onboarding.json();
  expect(state.needs_onboarding).toBe(false);
  expect(state.configured).toBe(true);
});

test('wizard-created credential logs in after logout and reload', async ({ page }) => {
  await page.goto('/');
  // Order-independent: claim the instance if a previous test has not (a
  // one-worker shard runs these in file order, but this keeps the test robust
  // to reordering).
  if (await page.locator('#setup-form').isVisible()) {
    await page.locator('#setup-form').getByLabel('Admin username').fill(ADMIN_USER);
    await page.locator('#setup-form').getByLabel('Password', { exact: true }).fill(ADMIN_PASS);
    await page.locator('#setup-form').getByLabel('Confirm password').fill(ADMIN_PASS);
    await page.locator('#setup-form').getByRole('button', { name: 'Create admin and continue' }).click();
    await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();
    // The wizard auto-opens for a wizard-created admin; close it (X, not Skip)
    // so logout is clickable and onboarding stays pending.
    if (await page.locator('#wizard-dialog').isVisible()) await page.locator('#close-wizard').click();
    await page.locator('#logout').click();
  }
  await expect(page.locator('#login-form')).toBeVisible();
  await expect(page.locator('#setup-form')).toBeHidden();
  await page.getByLabel('Tiller username').fill(ADMIN_USER);
  await page.locator('#login-form').getByLabel('Password').fill(ADMIN_PASS);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();
  // Onboarding is still outstanding (this test does not add a provider), so
  // the wizard auto-opens as a modal; close it before using the top bar.
  if (await page.locator('#wizard-dialog').isVisible()) await page.locator('#close-wizard').click();

  // Wrong password is rejected and does not fall back to any default.
  await page.locator('#logout').click();
  await expect(page.locator('#login-form')).toBeVisible();
  await page.getByLabel('Tiller username').fill(ADMIN_USER);
  await page.locator('#login-form').getByLabel('Password').fill('wrong-password');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.locator('#login-error')).toContainText('Invalid administrator credentials.');
});
