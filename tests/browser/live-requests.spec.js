const { test, expect } = require('@playwright/test');
const { openAdmin, adminCsrf, createProvider, createClient, mockAddModel, mockFailModel } = require('./helpers');

test('live requests integrate with fallback, logging switches, mobile, and fresh navigation', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
  const csrf = await adminCsrf(page);
  await mockAddModel(page, 'live-healthy');
  const provider = await createProvider(page, csrf, 'live-table-provider');
  const models = (await (await page.request.get(`/api/admin/providers/${provider.id}/models`)).json()).data;
  const first = models.find(m => m.upstream_model_id === 'mock-model');
  const second = models.find(m => m.upstream_model_id === 'live-healthy');
  await mockFailModel(page, 'mock-model');
  const client = await createClient(page, csrf, 'live-table-client');
  const group = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-table-group' } });
  expect(group.status()).toBe(201);
  const virtual = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: (await group.json()).id, name: 'route', routing_mode: 'ordered_fallback', targets: [{ provider_model_id: first.id, enabled: true }, { provider_model_id: second.id, enabled: true }] } });
  expect(virtual.status()).toBe(201);
  const permissions = await page.request.put(`/api/admin/client-keys/${client.id}/permissions`, { headers: { 'X-CSRF-Token': csrf }, data: { defaults: [], permissions: [{ kind: 'virtual', model_id: (await virtual.json()).id, enabled: true }] } });
  expect(permissions.status()).toBe(204);
  await page.locator('#nav-links a[data-view="activity"]').click();
  await expect(page.locator('#live-requests-empty')).toBeVisible();
  // Reflow depends only on viewport width, not the client or request lifecycle.
  for (const width of [1900, 1920]) {
    await page.setViewportSize({ width, height: 1080 });
    const graph = await page.locator('.graph-stage').boundingBox();
    const table = await page.locator('.live-requests-panel').boundingBox();
    expect(table.x).toBeGreaterThanOrEqual(graph.x + graph.width);
    // Sub-pixel layout rounding differs between Chromium builds (a 1.28px
    // delta has been observed on CI for identical CSS), so compare whole
    // pixels: this still catches a real misalignment, which is tens of pixels.
    expect(Math.abs(Math.round(table.y) - Math.round(graph.y))).toBeLessThanOrEqual(1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
  }
  await page.setViewportSize({ width: 1899, height: 1080 });
  const graph = await page.locator('.graph-stage').boundingBox();
  const table = await page.locator('.live-requests-panel').boundingBox();
  expect(table.y).toBeGreaterThanOrEqual(graph.y + graph.height);
  await expect(page.locator('[data-live-connection]').first()).toHaveText('Live');
  const call = () => page.request.post('/v1/chat/completions', { headers: { Authorization: `Bearer ${client.secret}` }, data: { model: 'live-table-group/route', messages: [{ role: 'user', content: 'fixture' }] } });
  const response = await call();
  expect(response.status()).toBe(200);
  const id = response.headers()['x-tiller-request-id'];
  const row = page.locator(`#live-requests-body tbody[data-live-request="${id}"]`);
  await expect(row.locator('.live-model-box .attempt-status')).toContainText('HTTP 200');
  await expect(row).toContainText('live-table-provider/live-healthy');
  await expect(row.locator('.live-model-box')).toHaveClass(/attempt-success/);
  await expect(row.locator('.live-model-target > .live-fallback')).toHaveCount(1);
  await expect(row.locator('.live-fallback')).toHaveCount(1);
  await expect(row.locator('.live-request-attempts .activity-attempt')).toHaveCount(0);
  await row.getByRole('button', { name: /Fallback/ }).click();
  await expect(row.locator('.live-request-attempts .activity-attempt')).toHaveCount(2);
  await expect(row.locator('.live-request-attempts .activity-attempt').first()).toContainText('failed');
  await expect(row.locator('.live-request-attempts .activity-attempt').last()).toContainText('success');

  await page.setViewportSize({ width: 390, height: 844 });
  const card = page.locator(`#mobile-activity-body [data-live-request="${id}"]`);
  await expect(card).toBeVisible();
  await expect(page.locator('.live-requests-panel')).toBeHidden();
  await expect(card.locator('.live-request-attempts .activity-attempt')).toHaveCount(2);
  await expect(card.locator('.live-model-box')).toHaveClass(/attempt-success/);
  await expect(card.locator('.live-model-target > .live-fallback')).toBeVisible();
  await expect(page.locator('#open-mobile-history')).toBeVisible();
  const switched = await page.request.patch(`/api/admin/client-keys/${client.id}`, { headers: { 'X-CSRF-Token': csrf }, data: { logging_enabled: false } });
  expect(switched.ok()).toBeTruthy();
  const unlogged = await call();
  expect(unlogged.status()).toBe(200);
  await expect(page.locator(`[data-live-request="${unlogged.headers()['x-tiller-request-id']}"]`)).toHaveCount(0);

  await page.setViewportSize({ width: 1440, height: 900 });
  await page.locator('#nav-links a[data-view="clients"]').click();
  await page.locator('#nav-links a[data-view="activity"]').click();
  await expect(page.locator('#live-requests-empty')).toBeVisible();
  await expect(page.locator('#live-requests-body tbody')).toHaveCount(0);
});

// Exercise the actual module with deterministic clocks and metadata-only fake
// transport; no test hooks or timing knobs enter the production application.
async function harness(page) {
  await page.route('**/__live-request-test', route => route.fulfill({ contentType: 'text/html', body: '<link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/live-requests.css"><main id="fixture"><span data-live-connection></span><p data-live-request-notice></p><table id="live-requests-body"><thead><tr><th>Age</th></tr></thead></table><div id="live-requests-empty"></div><div id="mobile-activity-body"></div><div id="mobile-activity-empty"></div></main>' }));
  await page.goto('/__live-request-test');
  const time = new Date('2026-10-14T12:00:00Z');
  await page.clock.install({ time });
  // The fake clock keeps ticking in real time until it is paused, so pausing at
  // `time` itself races the install (and throws "Cannot fast-forward to the
  // past" once a single millisecond has passed). Pause ahead of it instead, then
  // park the wall clock exactly at `time`.
  await page.clock.pauseAt(new Date(time.getTime() + 60000));
  await page.clock.setSystemTime(time);
  await page.evaluate(async () => {
    const { LiveRequests } = await import('/live-requests.js');
    const h = v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
    window.recovery = { data: [], missing: [] };
    window.calls = [];
    window.panel = new LiveRequests(document.querySelector('#fixture'), { api: async (path, options) => { window.calls.push({ path, options }); return options ? window.recovery : { data: [] }; }, h, date: h, when: ms => `${Math.floor(ms / 1000)} sec ago`, requestIdentity: row => h(row.requested_model), rowCache: row => `${row.input_tokens ?? '—'} / ${row.output_tokens ?? '—'}` });
    await window.panel.start();
    window.makeRow = (id, extra = {}) => ({ id, client_name: 'Client', requested_model: 'route/model', created_at: new Date(Date.now()).toISOString(), state: 'running', revision: 1, attempts: [], ...extra });
  });
}

test('rows update in place, sort running first, expire, and hold expanded fallback chains', async ({ page }) => {
  await harness(page);
  await page.evaluate(() => panel.onRequest(makeRow('older')));
  await expect(page.locator('#live-requests-body .live-model-box').first()).toHaveClass(/attempt-neutral/);
  await page.clock.fastForward(1000);
  await page.evaluate(() => panel.onRequest(makeRow('newer')));
  await expect(page.locator('#live-requests-body tbody').first()).toHaveAttribute('data-live-request', 'newer');
  await expect(page.locator('[data-live-elapsed="older"]').first()).toContainText('1000 ms');
  await page.evaluate(() => panel.onRequest({ ...panel.rows.get('newer'), revision: 2, state: 'success', completed_at: new Date(Date.now()).toISOString(), latency_ms: 123, input_tokens: 8, output_tokens: 2, fallback_used: true, attempts: [{ provider: 'a', model: 'one', result: 'skipped', failure_class: 'cooldown' }, { provider: 'b', model: 'two', result: 'success' }] }));
  await expect(page.locator('#live-requests-body tbody').first()).toHaveAttribute('data-live-request', 'older');
  const finished = page.locator('#live-requests-body tbody[data-live-request="newer"]');
  await expect(finished).toContainText('8 / 2');
  await finished.getByRole('button', { name: /Fallback/ }).click();
  await expect(finished.locator('.live-request-attempts .activity-attempt').first()).toContainText('cooldown');
  await page.clock.fastForward(31000);
  await expect(finished).toHaveCount(1);
  await finished.getByRole('button', { name: /Fallback/ }).click();
  await expect(finished).toHaveCount(0);
  await page.evaluate(() => panel.onRequest({ ...panel.rows.get('older'), revision: 2, state: 'failed', completed_at: new Date(Date.now()).toISOString(), latency_ms: 32000 }));
  await expect(page.locator('#live-requests-body .live-model-box')).toHaveClass(/attempt-failed/);
  await page.clock.fastForward(31000);
  await expect(page.locator('#live-requests-body tbody')).toHaveCount(0);
});

test('reconciliation recovers a missed completion without undoing newer state or loading history', async ({ page }) => {
  await harness(page);
  await page.evaluate(() => {
    panel.onRequest(makeRow('tracked'));
    panel.setConnection('reconnecting');
    window.recovery = { data: [makeRow('tracked', { state: 'success', revision: 3, completed_at: new Date(Date.now()).toISOString(), latency_ms: 4 })], missing: [] };
    panel.onSnapshot([]);
  });
  await expect(page.locator('#live-requests-body .live-model-box .attempt-status')).toContainText('success');
  await expect(page.locator('[data-live-connection]')).toHaveText('Reconnecting…');
  await page.evaluate(() => {
    panel.onRequest(makeRow('tracked', { revision: 2 }));
    panel.onSnapshot([makeRow('unrelated-history', { state: 'success', created_at: new Date(Date.now() - 60000).toISOString(), completed_at: new Date(Date.now()).toISOString() })]);
    panel.setConnection('connected');
  });
  await expect(page.locator('#live-requests-body tbody')).toHaveCount(1);
  await expect(page.locator('#live-requests-body .live-model-box .attempt-status')).toContainText('success');
  await expect(page.locator('[data-live-connection]')).toHaveText('Live');
  expect(await page.evaluate(() => JSON.parse(calls.find(call => call.options)?.options.body).ids)).toEqual(['tracked']);
});

test('completed display cap preserves all running rows and expanded rows', async ({ page }) => {
  await harness(page);
  await page.evaluate(() => {
    for (let i = 0; i < 205; i++) panel.upsert(makeRow(`done-${i}`, { state: 'success', completed_at: new Date(Date.now() - i).toISOString(), latency_ms: 1 }));
    panel.expanded.add('done-204');
    panel.upsert(makeRow('running'));
    panel.render();
  });
  await expect(page.locator('#live-requests-body tbody')).toHaveCount(202);
  await expect(page.locator('[data-live-request-notice]')).toContainText('4 older completed rows omitted');
  await expect(page.locator('#live-requests-body tbody').first()).toHaveAttribute('data-live-request', 'running');
});

test('server clock skew does not hide completions or break expiry', async ({ page }) => {
  await harness(page);
  await page.evaluate(() => {
    const serverTime = Date.now() - 3600000;
    panel.onSnapshot([], new Date(serverTime).toISOString());
    panel.onRequest(makeRow('skewed', { created_at: new Date(serverTime).toISOString() }));
  });
  await page.clock.fastForward(1000);
  await expect(page.locator('[data-live-elapsed="skewed"]').first()).toContainText('1000 ms');
  await page.evaluate(() => panel.onRequest({ ...panel.rows.get('skewed'), state: 'success', revision: 2, completed_at: new Date(panel.now()).toISOString(), latency_ms: 1000 }));
  await expect(page.locator('#live-requests-body .live-model-box .attempt-status')).toContainText('success');
  await page.clock.fastForward(31000);
  await expect(page.locator('#live-requests-body tbody')).toHaveCount(0);
});

test('an old missed completion expires immediately on recovery', async ({ page }) => {
  await harness(page);
  await page.evaluate(() => panel.onRequest(makeRow('tracked')));
  await page.clock.fastForward(60000);
  await page.evaluate(() => {
    recovery = { data: [{ ...panel.rows.get('tracked'), state: 'failed', completed_at: new Date(Date.now() - 31000).toISOString(), latency_ms: 29000 }], missing: [] };
    panel.onSnapshot([]);
  });
  await expect(page.locator('#live-requests-body tbody')).toHaveCount(0);
});
