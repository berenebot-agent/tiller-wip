const { test, expect } = require('@playwright/test');
const { login, adminCsrf, createProvider, createClient, clearActivity, mockAddModel, mockFailModel, seedActivity } = require('./helpers');

// E2E: prove real inference lands in Activity. Only this test makes real proxy
// calls (a handful). Pagination/search/volume are covered by the seeded tests
// below, which use fixturectl to insert rows directly and therefore run in
// milliseconds instead of ~300 ms per proxy call.
test('activity records real inference: success, upstream failure, and ordered fallback', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  await clearActivity(page, csrf);

  // Catalogue gets a second, healthy model; mock-model will be made to fail.
  await mockAddModel(page, 'mock-model-b');
  const providerName = 'activity-e2e-provider';
  const provider = await createProvider(page, csrf, providerName);
  const modelsRes = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsRes.ok()).toBeTruthy();
  const models = (await modelsRes.json()).data;
  const failingModel = models.find(m => m.upstream_model_id === 'mock-model');
  const healthyModel = models.find(m => m.upstream_model_id === 'mock-model-b');
  expect(failingModel).toBeTruthy();
  expect(healthyModel).toBeTruthy();
  await mockFailModel(page, 'mock-model');

  const client = await createClient(page, csrf, 'activity-e2e-client');

  // Ordered fallback virtual model: mock-model (500) -> mock-model-b (ok).
  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'activity-e2e-group' } });
  expect(groupRes.status()).toBe(201);
  const groupId = (await groupRes.json()).id;
  const vRes = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: groupId, name: 'fallback', routing_mode: 'ordered_fallback', targets: [{ provider_model_id: failingModel.id, enabled: true }, { provider_model_id: healthyModel.id, enabled: true }] } });
  expect(vRes.status()).toBe(201);
  const virtualId = (await vRes.json()).id;
  const grantRes = await page.request.put(`/api/admin/client-keys/${client.id}/permissions`, { headers: { 'X-CSRF-Token': csrf }, data: { defaults: [], permissions: [{ kind: 'real', model_id: failingModel.id, enabled: true }, { kind: 'real', model_id: healthyModel.id, enabled: true }, { kind: 'virtual', model_id: virtualId, enabled: true }] } });
  expect(grantRes.status()).toBe(204);

  const post = async (model, expectedStatus) => {
    const res = await page.request.post('/v1/chat/completions', { headers: { Authorization: `Bearer ${client.secret}` }, data: { model, messages: [{ role: 'user', content: 'e2e' }] } });
    expect(res.status()).toBe(expectedStatus);
  };
  await post(`${providerName}/mock-model-b`, 200); // direct success
  await post(`${providerName}/mock-model`, 500); // direct upstream failure
  await post('activity-e2e-group/fallback', 200); // fallback: first target 500, second ok

  const attemptsOf = async id => {
    const res = await page.request.get(`/api/admin/activity/${id}/attempts`);
    expect(res.ok()).toBeTruthy();
    return (await res.json()).data;
  };

  const actRes = await page.request.get(`/api/admin/client-keys/${client.id}/activity?limit=10`);
  expect(actRes.ok()).toBeTruthy();
  const rows = (await actRes.json()).data;
  expect(rows).toHaveLength(3);

  const fallback = rows.find(r => r.http_status === 200 && r.fallback_used);
  const success = rows.find(r => r.http_status === 200 && !r.fallback_used);
  const failure = rows.find(r => r.http_status === 500);
  expect(fallback).toBeTruthy();
  expect(success).toBeTruthy();
  expect(failure).toBeTruthy();

  // Success row: resolved target, one success attempt.
  expect(success.resolved_provider).toBe(providerName);
  expect(success.resolved_model).toBe('mock-model-b');
  const sAttempts = await attemptsOf(success.id);
  expect(sAttempts).toHaveLength(1);
  expect(sAttempts[0].result).toBe('success');
  expect(sAttempts[0].model).toBe('mock-model-b');

  // Failure row: 500 from upstream, error text, no resolved target, one failed attempt.
  expect(failure.error_text).toBeTruthy();
  expect(failure.resolved_provider).toBeFalsy();
  expect(failure.resolved_model).toBeFalsy();
  const fAttempts = await attemptsOf(failure.id);
  expect(fAttempts).toHaveLength(1);
  expect(fAttempts[0].result).toBe('failed');
  expect(fAttempts[0].http_status).toBe(500);
  expect(fAttempts[0].model).toBe('mock-model');

  // Fallback row: 200, fallback reason, two attempts (failed then success).
  expect(fallback.fallback_reason).toBeTruthy();
  expect(fallback.resolved_provider).toBe(providerName);
  expect(fallback.resolved_model).toBe('mock-model-b');
  const fbAttempts = await attemptsOf(fallback.id);
  expect(fbAttempts).toHaveLength(2);
  expect(fbAttempts[0].attempt_number).toBe(1);
  expect(fbAttempts[0].result).toBe('failed');
  expect(fbAttempts[0].model).toBe('mock-model');
  expect(fbAttempts[1].attempt_number).toBe(2);
  expect(fbAttempts[1].result).toBe('success');
  expect(fbAttempts[1].model).toBe('mock-model-b');

  // The Activity UI renders the three real-inference rows (global view; the
  // lane was cleared at the start so these are the only rows).
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await expect(page.locator('#view-settings')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Global activity' })).toBeVisible();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(3);
});

// Data-volume: 55 rows seeded directly (28 client-one + 27 client-two) exercise
// the same global-activity pagination/search surface the test used to reach
// with 55 real proxy calls.
test('global activity renders across clients, searches, and pages', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  await clearActivity(page, csrf);
  const client1Name = 'activity-client-one';
  const client2Name = 'activity-client-two';
  const client1 = await createClient(page, csrf, client1Name);
  const client2 = await createClient(page, csrf, client2Name);
  seedActivity(client1.id, 28);
  seedActivity(client2.id, 27);
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await expect(page.locator('#view-settings')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Global activity' })).toBeVisible();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(50);
  await expect(page.locator('#global-activity-count')).toHaveText('1–50');
  await expect(page.locator('#global-activity-prev')).toBeDisabled();
  await expect(page.locator('#global-activity-next')).toBeEnabled();
  await page.locator('#global-activity-search').fill(client2Name);
  await expect(page.locator('#global-activity-body tr')).toHaveCount(27);
  await expect(page.locator('#global-activity-empty')).toBeHidden();
  await page.locator('#global-activity-search').fill('zzz-no-match');
  await expect(page.locator('#global-activity-empty')).toBeVisible();
  await page.locator('#global-activity-search').fill('');
  await expect(page.locator('#global-activity-body tr')).toHaveCount(50);
  await page.locator('#global-activity-next').click();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(5);
  await expect(page.locator('#global-activity-count')).toHaveText('51–55');
  await expect(page.locator('#global-activity-prev')).toBeEnabled();
  await expect(page.locator('#global-activity-next')).toBeDisabled();
  await page.locator('#global-activity-prev').click();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(50);
  await expect(page.locator('#global-activity-count')).toHaveText('1–50');
});

// Data-volume + destructive: exactly 50 seeded rows hit the exact-page boundary
// and the clear-activity flow. Also covers the empty per-client and global
// states.
test('activity pagination handles empty results and the exact-page boundary', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  await clearActivity(page, csrf);
  const clientA = await createClient(page, csrf, 'boundary-empty-client');
  const clientB = await createClient(page, csrf, 'boundary-full-client');
  await page.getByRole('link', { name: 'Clients' }).click();
  const clientARow = page.locator('#clients-body tr', { hasText: clientA.name });
  await expect(clientARow).toBeVisible();
  await clientARow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-empty')).toBeVisible();
  await expect(page.locator('#activity-count')).not.toHaveText('1–0');
  await expect(page.locator('#activity-count')).toHaveText('0 results');
  await expect(page.locator('#activity-prev')).toBeDisabled();
  await expect(page.locator('#activity-next')).toBeDisabled();
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await expect(page.locator('#view-settings')).toBeVisible();
  await page.locator('#global-activity-search').fill('zzz-no-match-boundary');
  await expect(page.locator('#global-activity-empty')).toBeVisible();
  await expect(page.locator('#global-activity-count')).not.toHaveText('1–0');
  await expect(page.locator('#global-activity-count')).toHaveText('0 results');
  await expect(page.locator('#global-activity-next')).toBeDisabled();
  await page.locator('#global-activity-search').fill('');
  seedActivity(clientB.id, 50);
  await page.getByRole('link', { name: 'Clients' }).click();
  const clientBRow = page.locator('#clients-body tr', { hasText: clientB.name });
  await expect(clientBRow).toBeVisible();
  await clientBRow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-empty')).toBeHidden();
  await expect(page.locator('#activity-body tr')).toHaveCount(50);
  await expect(page.locator('#activity-count')).toHaveText('1–50');
  await expect(page.locator('#activity-next')).toBeDisabled();
  await expect(page.locator('#activity-prev')).toBeDisabled();
  await page.getByRole('button', { name: 'Clear activity' }).click();
  await expect(page.locator('#confirm-dialog')).toBeVisible();
  await expect(page.locator('#confirm-copy')).toContainText('permanently deleted');
  await page.locator('#confirm-action').click();
  await expect(page.locator('#activity-empty')).toBeVisible();
  await expect(page.locator('#activity-count')).toHaveText('0 results');
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();
});
