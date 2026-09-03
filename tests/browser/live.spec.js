const { test, expect } = require('@playwright/test');
const {
  login, adminCsrf, createProvider,
  mockFailModel, mockOkModel
} = require('./helpers');

test('live refresh: transient errors retain one reconnecting EventSource', async ({ page }) => {
  await page.addInitScript(() => {
    window.__liveEventSources = [];
    window.EventSource = class {
      constructor(url) {
        this.url = url;
        this.closed = false;
        window.__liveEventSources.push(this);
      }
      addEventListener() {}
      close() { this.closed = true; }
    };
  });
  await login(page);

  const result = await page.evaluate(() => {
    const source = window.__liveEventSources[0];
    source.onerror();
    return {
      count: window.__liveEventSources.length,
      closed: source.closed,
    };
  });
  expect(result.count).toBe(1);
  expect(result.closed).toBeFalsy();
});

// Live SSE refresh: the Virtual Models page's per-target resolution icons and
// token/cache counters update in place (no navigation) as the router serves
// requests, and DOM writes pause while a dialog is open.
test('live refresh: resolution icon and token counter update without navigation', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'live-refresh';
  const clientName = 'live-refresh-client';
  // The mock upstream is shared across the shard; reset mock-model to healthy
  // so this test starts from a clean baseline regardless of prior tests.
  await mockOkModel(page, 'mock-model');
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  // Virtual model targeting the real model.
  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);
  const virtual = await virtualResponse.json();

  // Single client bound to the virtual model so requests route through it.
  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'virtual', single_target_id: virtual.id }
  });
  expect(createRes.status()).toBe(201);
  const secret = (await createRes.json()).secret;

  // Navigate to Virtual Models.
  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await expect(page.getByRole('heading', { name: 'Virtual Models', exact: true })).toBeVisible();

  const row = page.locator(`tr[data-virtual-id="${virtual.id}"]`);
  await expect(row).toBeVisible();
  const targetLine = row.locator(`[data-target-key="${real.id}"]`);
  await expect(targetLine).toBeVisible();
  const icon = targetLine.locator('.resolution-indicator');

  // Initially no activity -> neutral.
  await expect(icon).toHaveClass(/resolution-neutral/);

  // Fire a successful request; the icon must flip to good WITHOUT navigation.
  // Fire a request and return its status (the caller asserts per phase).
  const fire = async () => {
    const res = await page.request.post('/v1/chat/completions', {
      headers: { Authorization: `Bearer ${secret}` },
      data: { model: 'main', messages: [{ role: 'user', content: 'live' }] }
    });
    return res.status();
  };
  // Success phase: 200 and the icon flips to good WITHOUT navigation.
  expect(await fire()).toBe(200);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');

  // Fail phase: upstream 500 -> virtual unavailable (503), icon flips to bad.
  await mockFailModel(page, 'mock-model');
  expect(await fire()).toBe(503);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-bad');

  // Restore; the icon flips back to good.
  await mockOkModel(page, 'mock-model');
  expect(await fire()).toBe(200);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');

  // The 1h token counter reflects the traffic (no longer a dash) via the
  // snapshot cadence.
  const tokCell = row.locator('.tok[data-window="1h"]').first();
  await expect.poll(() => tokCell.textContent(), { timeout: 8000 }).not.toContain('—');
});

test('live refresh: DOM writes pause while a dialog is open and reconcile on close', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'live-dialog';
  const clientName = 'live-dialog-client';
  await mockOkModel(page, 'mock-model');
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-dialog-vg' } });
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  const virtual = await virtualResponse.json();
  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'virtual', single_target_id: virtual.id }
  });
  const secret = (await createRes.json()).secret;

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  const row = page.locator(`tr[data-virtual-id="${virtual.id}"]`);
  await expect(row).toBeVisible();
  const icon = row.locator(`[data-target-key="${real.id}"] .resolution-indicator`);
  await expect(icon).toHaveClass(/resolution-neutral/);

  // Open the Capabilities dialog (a modal) and fire a request behind it.
  await row.getByRole('button', { name: 'Capabilities' }).click();
  await expect(page.locator('#capabilities-dialog')).toBeVisible();

  const res = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${secret}` },
    data: { model: 'main', messages: [{ role: 'user', content: 'behind-dialog' }] }
  });
  expect(res.status()).toBe(200);

  // While the dialog is open the icon must NOT be patched (DOM writes paused).
  await page.waitForTimeout(1500);
  await expect(icon).toHaveClass(/resolution-neutral/);

  // Closing the dialog reconciles the pending state.
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#capabilities-dialog')).toBeHidden();
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');
});
