const { test, expect } = require('@playwright/test');
const { openAdmin, adminCsrf, createProvider, createClient, clearActivity, mockAddModel, mockFailModel } = require('./helpers');

// Activity living pane: starts empty, materializes client → tiller model →
// real-model target legs from live `activity` deltas, and `outcome` settles
// the colour. Legs fade 30s after quiet (not asserted — expensive/flaky);
// the empty state and live legs are asserted here. Snapshot eviction of
// lost-delta legs is not covered (no test hook to inject a synthetic
// snapshot). Written alongside the feature; NOT RUN here (browser tier needs
// an explicit go-ahead per AGENTS.md — run via ./tests/browser/run.sh).
test('activity graph starts empty, lights live legs, and settles on outcome', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
  const csrf = await adminCsrf(page);
  await clearActivity(page, csrf);

  await mockAddModel(page, 'mock-model-b');
  const providerName = 'activity-graph-provider';
  const provider = await createProvider(page, csrf, providerName);
  const modelsRes = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsRes.ok()).toBeTruthy();
  const models = (await modelsRes.json()).data;
  const failingModel = models.find(m => m.upstream_model_id === 'mock-model');
  const healthyModel = models.find(m => m.upstream_model_id === 'mock-model-b');
  expect(failingModel).toBeTruthy();
  expect(healthyModel).toBeTruthy();
  await mockFailModel(page, 'mock-model');

  const client = await createClient(page, csrf, 'activity-graph-client');

  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'activity-graph-group' } });
  expect(groupRes.status()).toBe(201);
  const groupId = (await groupRes.json()).id;
  const vRes = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: groupId, name: 'graph', routing_mode: 'ordered_fallback', targets: [{ provider_model_id: failingModel.id, enabled: true }, { provider_model_id: healthyModel.id, enabled: true }] } });
  expect(vRes.status()).toBe(201);
  const virtualId = (await vRes.json()).id;
  const grantRes = await page.request.put(`/api/admin/client-keys/${client.id}/permissions`, { headers: { 'X-CSRF-Token': csrf }, data: { defaults: [], permissions: [{ kind: 'real', model_id: failingModel.id, enabled: true }, { kind: 'real', model_id: healthyModel.id, enabled: true }, { kind: 'virtual', model_id: virtualId, enabled: true }] } });
  expect(grantRes.status()).toBe(204);

  // Active-only: the pane starts empty — no catalogue nodes, no idle legs.
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await expect(page.locator('#view-activity')).toBeVisible();
  await expect(page.locator('#activity-pane')).toBeVisible();
  await expect(page.locator('#graph-empty')).toBeVisible();
  await expect(page.locator('#activity-pane path.edge')).toHaveCount(0);

  // Drive a real fallback request: first target 500s, second serves.
  const post = await page.request.post('/v1/chat/completions', { headers: { Authorization: `Bearer ${client.secret}` }, data: { model: 'activity-graph-group/graph', messages: [{ role: 'user', content: 'e2e' }] } });
  expect(post.status()).toBe(200);

  // Legs materialize: client → virtual in the middle lane, virtual → each
  // tried real model in the right lane. No provider nodes exist.
  await expect(page.locator('#graph-empty')).toBeHidden({ timeout: 15000 });
  await expect.poll(async () => page.locator('#activity-pane path.edge').count(), { timeout: 15000 }).toBeGreaterThanOrEqual(3);
  await expect(page.locator('#activity-pane text', { hasText: 'activity-graph-client' }).first()).toBeVisible();
  await expect(page.locator('#activity-pane text', { hasText: 'activity-graph-group/graph' }).first()).toBeVisible();
  await expect(page.locator('#activity-pane text', { hasText: `${providerName}/mock-model-b` }).first()).toBeVisible();
  const graphNode = label => page.locator('#activity-pane g[role="button"]').filter({ hasText: label }).first();
  await graphNode('activity-graph-client').click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-title')).toHaveText('activity-graph-client activity');
  await page.getByRole('button', { name: 'Done' }).click();
  await graphNode('activity-graph-group/graph').click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-title')).toHaveText('activity-graph-group/graph activity');
  await page.getByRole('button', { name: 'Done' }).click();
  await graphNode(`${providerName}/mock-model-b`).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-title')).toHaveText(`${providerName}/mock-model-b activity`);
  await page.getByRole('button', { name: 'Done' }).click();
  // The served target roundel settles green, the failed target roundel red.
  // Outcome colour lives on the node ring; edges are flow-only.
  await expect(page.locator('#activity-pane .node-ring.st-ok').first()).toBeVisible({ timeout: 15000 });
  await expect(page.locator('#activity-pane .node-ring.st-failed').first()).toBeVisible({ timeout: 15000 });

  // Direct real-model request: a single client → real-model leg in the
  // middle lane, lit from `activity` deltas alone.
  const direct = await page.request.post('/v1/chat/completions', { headers: { Authorization: `Bearer ${client.secret}` }, data: { model: `${providerName}/mock-model-b`, messages: [{ role: 'user', content: 'direct' }] } });
  expect(direct.status()).toBe(200);
  // The direct real-model leg lights the pane (client → real target) and the
  // served roundel stays green (the resolution feed was removed).
  await expect(page.locator('#activity-pane .node-ring.st-ok').first()).toBeVisible({ timeout: 15000 });
  await expect(page.locator('#activity-pane text', { hasText: 'activity-graph-client' }).first()).toBeVisible();
});

test('activity graph shows empty state with no traffic', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await expect(page.locator('#view-activity')).toBeVisible();
  await expect(page.locator('#activity-pane')).toBeVisible();
  // Empty until first traffic; the pane is the only content.
  await expect(page.locator('#graph-empty')).toBeVisible();
});
