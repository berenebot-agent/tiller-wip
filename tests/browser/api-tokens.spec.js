const { test, expect, request } = require('@playwright/test');
const { openAdmin } = require('./helpers');

const BASE = process.env.TILLER_BROWSER_BASE_URL || 'http://127.0.0.1:18080';

async function openManagementApi(page) {
  await openAdmin(page);
  await page.locator('a[data-view="settings"]').first().click();
  await page.locator('[data-settings-tab="api"]').click();
  await expect(page.locator('#settings-panel-api')).toBeVisible();
}

test('create an API token with provider access, use it headlessly, rotate and delete it', async ({ page }) => {
  await openManagementApi(page);

  await page.locator('#create-api-token').click();
  await page.locator('#form-dialog [name="token_name"]').fill('e2e agent');
  await page.locator('#form-dialog [name="token_description"]').fill('playwright');
  // Escalating toggles: read implies nothing above; write lights read+write.
  const providersRow = page.locator('[data-scope-area="providers"]');
  await providersRow.locator('[data-scope-tier="2"]').click();
  await expect(providersRow.locator('[data-scope-tier="1"]')).toHaveAttribute('aria-pressed', 'true');
  await expect(providersRow.locator('[data-scope-tier="2"]')).toHaveAttribute('aria-pressed', 'true');
  await expect(providersRow.locator('[data-scope-tier="3"]')).toHaveAttribute('aria-pressed', 'false');
  await page.locator('#dialog-submit').click();

  const secret = page.locator('#secret-value');
  await expect(secret).toHaveText(/^mt-tr-[A-Za-z0-9_-]{12}\.[A-Za-z0-9_-]{43}$/);
  // The dialog points at the management API landing page, which serves the
  // usage guide (HTML for people, markdown for agents).
  await expect(page.locator('#secret-base-url-block')).toBeVisible();
  await expect(page.locator('#secret-base-url-label')).toHaveText('Management API');
  await expect(page.locator('#api-base-url')).toContainText('/api/admin');
  const token = (await secret.textContent()).trim();
  await page.locator('#close-secret').click();

  const guide = await page.request.get('/api/admin');
  expect(guide.status()).toBe(200);
  expect(guide.headers()['content-type']).toContain('text/html');
  expect(await guide.text()).toContain('Authorization: Bearer');
  const guideMd = await page.request.get('/api/admin', { headers: { Accept: 'text/markdown' } });
  expect(guideMd.status()).toBe(200);
  expect(guideMd.headers()['content-type']).toContain('text/markdown');
  expect(await guideMd.text()).toContain('# Tiller Router — Management API');
  const discovery = await page.request.get('/api.json');
  expect(discovery.status()).toBe(200);
  expect(discovery.headers()['content-type']).toContain('application/json');
  const doc = await discovery.json();
  expect(doc.api_base).toBe('/api/admin');
  expect(doc.auth.key_prefix).toBe('mt-tr-');

  const row = page.locator('#api-tokens-body tr').filter({ hasText: 'e2e agent' });
  await expect(row).toHaveCount(1);
  await expect(row).toContainText('Providers · write');

  // The token authenticates the admin API headlessly, with no cookie jar.
  const api = await request.newContext({ baseURL: BASE });
  try {
    const ok = await api.get('/api/admin/providers', { headers: { Authorization: `Bearer ${token}` } });
    expect(ok.status()).toBe(200);
    // Cross-area access is refused with a machine-readable scope error.
    const denied = await api.get('/api/admin/settings', { headers: { Authorization: `Bearer ${token}` } });
    expect(denied.status()).toBe(403);
    expect((await denied.json()).error.code).toBe('insufficient_scope');
    // The self endpoint reports the token without any secret material.
    const self = await api.get('/api/admin/api-tokens/self', { headers: { Authorization: `Bearer ${token}` } });
    expect(self.status()).toBe(200);
    const selfBody = await self.json();
    expect(selfBody.name).toBe('e2e agent');
    expect(selfBody.token).toBeUndefined();

    // Rotate in the UI: the old token dies immediately.
    await row.locator('[data-api-token-rotate]').click();
    await page.locator('#confirm-action').click();
    await expect(secret).toHaveText(/^mt-tr-/);
    const rotated = (await secret.textContent()).trim();
    expect(rotated).not.toBe(token);
    await page.locator('#close-secret').click();
    const dead = await api.get('/api/admin/providers', { headers: { Authorization: `Bearer ${token}` } });
    expect(dead.status()).toBe(401);
    const alive = await api.get('/api/admin/providers', { headers: { Authorization: `Bearer ${rotated}` } });
    expect(alive.status()).toBe(200);

    // Edit: rename and escalate to delete.
    await row.locator('[data-api-token-edit]').click();
    await page.locator('#form-dialog [name="token_name"]').fill('e2e agent renamed');
    await providersRow.locator('[data-scope-tier="3"]').click();
    await page.locator('#dialog-submit').click();
    const renamed = page.locator('#api-tokens-body tr').filter({ hasText: 'e2e agent renamed' });
    await expect(renamed).toHaveCount(1);
    await expect(renamed).toContainText('Providers · delete');

    // Delete: the rotated token is refused afterwards.
    await renamed.locator('[data-api-token-delete]').click();
    await page.locator('#confirm-action').click();
    await expect(page.locator('#api-tokens-body tr').filter({ hasText: 'e2e agent' })).toHaveCount(0);
    const gone = await api.get('/api/admin/providers', { headers: { Authorization: `Bearer ${rotated}` } });
    expect(gone.status()).toBe(401);
  } finally {
    await api.dispose();
  }

  await expect(page.locator('#api-tokens-empty')).toBeVisible();
});

test('environment-managed token has no lifecycle controls', async ({ page }) => {
  await page.route('**/api/admin/api-tokens', async route => {
    if (route.request().method() !== 'GET') return route.continue();
    const response = await route.fetch();
    const body = await response.json();
    body.data.push({ id: 'boot-admin', name: 'boot-admin', description: 'Environment-managed credential', fingerprint: 'fixture', scopes: { providers: 3 }, enabled: true, environment_managed: true, created_at: new Date().toISOString() });
    await route.fulfill({ response, json: body });
  });
  await openManagementApi(page);
  const row = page.locator('#api-tokens-body tr').filter({ hasText: 'boot-admin' });
  await expect(row).toContainText('Environment-managed');
  await expect(row.locator('button')).toHaveCount(0);
});
