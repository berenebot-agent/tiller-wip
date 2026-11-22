const { test, expect } = require('@playwright/test');
const { openAdmin, adminCsrf, createProvider, createClient, clearActivity, mockAddModel, mockFailModel, seedActivity } = require('./helpers');

// E2E: prove real inference lands in Activity. Only this test makes real proxy
// calls (a handful). Pagination/search/volume are covered by the seeded tests
// below, which use fixturectl to insert rows directly and therefore run in
// milliseconds instead of ~300 ms per proxy call.
test('activity records real inference: success, upstream failure, and ordered fallback', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
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
  expect((await page.request.patch(`/api/admin/client-keys/${client.id}`, { headers: { 'X-CSRF-Token': csrf }, data: { log_all_bodies: 'always' } })).status()).toBe(204);

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
  expect(success.request_body).toContain('e2e');
  expect(success.response_body).toBeTruthy();
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
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await page.locator('[data-activity-tab="history"]').click();
  await expect(page.locator('#activity-panel-history')).toBeVisible();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(3);
  await page.locator('#global-activity-body').getByRole('button', { name: 'Message bodies' }).first().click();
  await expect(page.locator('#error-body')).toContainText('Client request body:');
  await expect(page.locator('#error-body')).toContainText('Client response body:');
  await page.locator('#done-error').click();

  // Opening a client Activity dialog renders the newest window immediately and
  // backfills attempt details as rows scroll into view. There is no "Show
  // attempts" button, and hydration must happen without any click.
  let attemptRequests = 0;
  await page.route('**/api/admin/activity/*/attempts', async route => {
    attemptRequests += 1;
    await route.continue();
  });
  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: client.name });
  await expect(clientRow).toBeVisible();
  await clientRow.locator('[data-client-activity]').click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-body tr')).toHaveCount(3);
  await expect(page.locator('#activity-body .activity-attempts-load')).toHaveCount(0);
  await expect.poll(() => attemptRequests).toBeGreaterThan(0);
  await expect(page.locator('#activity-body .attempt-failed').first()).toBeVisible();
});

// Data-volume: 55 rows seeded directly (28 client-one + 27 client-two) exercise
// the same global-activity pagination/search surface the test used to reach
// with 55 real proxy calls.
test('global activity renders across clients, searches, and loads older rows on scroll', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
  const csrf = await adminCsrf(page);
  await clearActivity(page, csrf);
  const client1Name = 'activity-client-one';
  const client2Name = 'activity-client-two';
  const client1 = await createClient(page, csrf, client1Name);
  const client2 = await createClient(page, csrf, client2Name);
  seedActivity(client1.id, 28);
  seedActivity(client2.id, 27);
  const scrollGlobalToBottom = async () => { await page.locator('.global-activity-table-shell').scrollIntoViewIfNeeded(); await page.locator('.global-activity-table-shell').evaluate(el => { el.scrollTop = el.scrollHeight; }); };
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await page.locator('[data-activity-tab="history"]').click();
  await expect(page.locator('#activity-panel-history')).toBeVisible();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(20);
  await expect(page.locator('#global-activity-count')).toHaveText('1–20');
  await scrollGlobalToBottom();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(40);
  await expect(page.locator('#global-activity-count')).toHaveText('1–40');
  await scrollGlobalToBottom();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(55);
  await expect(page.locator('#global-activity-count')).toHaveText('1–55');
  await page.locator('#global-activity-search').fill(client2Name);
  await expect(page.locator('#global-activity-body tr')).toHaveCount(20);
  await expect(page.locator('#global-activity-empty')).toBeHidden();
  await scrollGlobalToBottom();
  await expect(page.locator('#global-activity-body tr')).toHaveCount(27);
  await page.locator('#global-activity-search').fill('zzz-no-match');
  await expect(page.locator('#global-activity-empty')).toBeVisible();
  await page.locator('#global-activity-search').fill('');
  await expect(page.locator('#global-activity-body tr')).toHaveCount(20);
  await expect(page.locator('#global-activity-count')).toHaveText('1–20');
});

// Data-volume + destructive: exactly 50 seeded rows hit the exact-page boundary
// and the clear-activity flow. Also covers the empty per-client and global
// states.
test('activity pagination handles empty results and the exact-page boundary', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
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
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await page.locator('[data-activity-tab="history"]').click();
  await expect(page.locator('#activity-panel-history')).toBeVisible();
  await page.locator('#global-activity-search').fill('zzz-no-match-boundary');
  await expect(page.locator('#global-activity-empty')).toBeVisible();
  await expect(page.locator('#global-activity-count')).not.toHaveText('1–0');
  await expect(page.locator('#global-activity-count')).toHaveText('0 results');
  await page.locator('#global-activity-search').fill('');
  seedActivity(clientB.id, 50);
  await page.getByRole('link', { name: 'Clients' }).click();
  const clientBRow = page.locator('#clients-body tr', { hasText: clientB.name });
  await expect(clientBRow).toBeVisible();
  await clientBRow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await expect(page.locator('#activity-empty')).toBeHidden();
  await expect(page.locator('#activity-body tr')).toHaveCount(20);
  await expect(page.locator('#activity-count')).toHaveText('1–20');
  await page.locator('#activity-dialog .activity-table-shell').evaluate(el => { el.scrollTop = el.scrollHeight; });
  await expect(page.locator('#activity-body tr')).toHaveCount(40);
  await page.locator('#activity-dialog .activity-table-shell').evaluate(el => { el.scrollTop = el.scrollHeight; });
  await expect(page.locator('#activity-body tr')).toHaveCount(50);
  await expect(page.locator('#activity-count')).toHaveText('1–50');
  await page.getByRole('button', { name: 'Clear activity' }).click();
  await expect(page.locator('#confirm-dialog')).toBeVisible();
  await expect(page.locator('#confirm-copy')).toContainText('permanently deleted');
  await page.locator('#confirm-action').click();
  await expect(page.locator('#activity-empty')).toBeVisible();
  await expect(page.locator('#activity-count')).toHaveText('0 results');
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();
});

// The Export CSV button sits on the right of the activity toolbar next to Clear
// activity (not centred), and the export dialog offers both presets and a
// custom from/to range that downloads a CSV. Also covers the Global activity
// card's own Export CSV button.
test('activity export button sits beside clear and custom range downloads CSV', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openAdmin(page);
  const csrf = await adminCsrf(page);
  await clearActivity(page, csrf);
  const client = await createClient(page, csrf, 'export-range-client');
  seedActivity(client.id, 5);

  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: client.name });
  await expect(clientRow).toBeVisible();
  await clientRow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();

  // Export CSV is right-aligned: its right edge matches Clear activity's right
  // edge, and it sits to the left of Clear, well past the toolbar midpoint.
  const exportBox = await page.locator('#export-activity').boundingBox();
  const clearBox = await page.locator('#clear-activity').boundingBox();
  const toolbarBox = await page.locator('#activity-dialog .activity-toolbar').boundingBox();
  expect(exportBox).not.toBeNull();
  expect(clearBox).not.toBeNull();
  expect(toolbarBox).not.toBeNull();
  expect(Math.abs((exportBox.x + exportBox.width) - clearBox.x)).toBeLessThan(20);
  expect(exportBox.x).toBeGreaterThan(toolbarBox.x + toolbarBox.width / 2);

  // Custom range export: a wide window downloads a CSV with a header and rows.
  await page.locator('#export-activity').click();
  await expect(page.locator('#export-dialog')).toBeVisible();
  await expect(page.locator('#export-from')).toBeVisible();
  await expect(page.locator('#export-range-error')).toHaveText('');
  await page.locator('#export-from').fill('2020-01-01');
  await page.locator('#export-to').fill('2099-01-01');
  const download = page.waitForEvent('download');
  await page.locator('#export-custom').click();
  const file = await download;
  expect(file.suggestedFilename()).toContain('activity');
  await expect(page.locator('#export-dialog')).toBeHidden();

  // Reversed range is rejected client-side without closing the dialog.
  await page.locator('#export-activity').click();
  await page.locator('#export-from').fill('2099-01-01');
  await page.locator('#export-to').fill('2020-01-01');
  await page.locator('#export-custom').click();
  await expect(page.locator('#export-range-error')).not.toHaveText('');
  await expect(page.locator('#export-dialog')).toBeVisible();
  await page.locator('#close-export').click();
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();

  // Global activity card has its own Export CSV button that downloads the
  // account-wide CSV.
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await page.locator('[data-activity-tab="history"]').click();
  await expect(page.locator('#export-global-activity')).toBeVisible();
  await page.locator('#export-global-activity').click();
  await expect(page.locator('#export-dialog')).toBeVisible();
  const globalDownload = page.waitForEvent('download');
  await page.locator('[data-export-period="all"]').click();
  const globalFile = await globalDownload;
  expect(globalFile.suggestedFilename()).toContain('activity');
});

test('activity shows metadata-only reasoning degradation on desktop and mobile', async ({ page }) => {
  await openAdmin(page);
  await page.route('**/api/admin/activity?*', route => route.fulfill({ json: { data: [{
    id: 'reasoning-warning', requested_model: 'test/model', protocol: 'chat', streaming: false,
    http_status: 200, latency_ms: 1, attempt_count: 1, attempt_rows: 0,
    resolved_provider: 'test', resolved_model: 'model', reasoning_stripped: true,
    client_name: 'test client', created_at: '2026-01-01T00:00:00Z',
  }], limit: 20, offset: 0 } }));
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await page.locator('[data-activity-tab="history"]').click();
  await expect(page.locator('#global-activity-body .activity-reasoning-warning')).toHaveText('Reasoning history omitted');
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.locator('.history-card:visible .activity-reasoning-warning')).toHaveText('Reasoning history omitted');
});

// Every failure renders as the red error box (model name + status, one-line
// summary, click-through for details) — never as bare red text. Covers the
// three request shapes: an ordered-fallback request (resolved target plus a
// failed first attempt), a request-level failure with no attempt rows to
// render, and the mobile card. All rows are stubbed so the lanes stay fast.
test('activity renders every failure as a clickable error box, not bare text', async ({ page }) => {
  await openAdmin(page);
  const fallbackRow = {
    id: 'errbox-fallback', requested_model: 'fixture-provider/mock-model-b', protocol: 'chat', streaming: false,
    http_status: 200, latency_ms: 120, attempt_count: 2, attempt_rows: 2,
    resolved_provider: 'fixture-provider', resolved_model: 'mock-model-b',
    fallback_used: true, fallback_reason: 'http_500', error_text: 'upstream_error',
    error_message: 'The upstream provider returned an error', client_name: 'errbox client',
    client_request_id: 'errbox-fallback', created_at: '2026-01-01T00:00:00Z',
  };
  const preAttemptRow = {
    id: 'errbox-pre', requested_model: 'missing-model', protocol: 'chat', streaming: false,
    http_status: 404, latency_ms: 2, attempt_count: 0, attempt_rows: 0,
    error_text: 'model_not_found', error_message: 'The requested model was not found or is not configured',
    client_name: 'errbox client', client_request_id: 'errbox-pre', created_at: '2026-01-01T00:00:00Z',
  };
  await page.route('**/api/admin/activity?*', route => route.fulfill({ json: { data: [fallbackRow, preAttemptRow], limit: 20, offset: 0 } }));
  await page.route('**/api/admin/activity/*/attempts', route => route.fulfill({ json: { data: [
    { attempt_number: 1, provider: 'fixture-provider', model: 'mock-model-a', result: 'failed', http_status: 500, failure_class: 'http_500', latency_ms: 90, created_at: '2026-01-01T00:00:00Z' },
    { attempt_number: 2, provider: 'fixture-provider', model: 'mock-model-b', result: 'success', http_status: 200, latency_ms: 30, created_at: '2026-01-01T00:00:10Z' },
  ] } }));
  await page.locator('#nav-links').getByRole('link', { name: 'Activity' }).click();
  await page.locator('[data-activity-tab="history"]').click();
  await expect(page.locator('#activity-panel-history')).toBeVisible();

  // No failure may fall back to the old bare red text span.
  await expect(page.locator('#global-activity-body .error-text')).toHaveCount(0);

  // Ordered fallback: the attempt chain renders *and* the request-level failure
  // keeps its own red box (previously it was silently dropped). Rows are keyed
  // by request-id, not row text — both stub rows carry the same client name.
  const fallbackCells = page.locator('#global-activity-body tr').filter({ has: page.locator('[data-activity-resolved="errbox-fallback"]') });
  await expect(fallbackCells).toHaveCount(1);
  // The two attempt rows plus the request-level error box (also an .activity-attempt).
  await expect(fallbackCells.locator('.activity-attempt')).toHaveCount(3);
  const fallbackBox = fallbackCells.locator('.activity-error-box');
  await expect(fallbackBox).toHaveCount(1);
  await expect(fallbackBox.locator('.model-id')).toHaveText('fixture-provider/mock-model-b');
  await expect(fallbackBox.locator('.attempt-error-summary')).toHaveText('Upstream returned an error');
  await expect(fallbackBox.locator('.attempt-status')).toHaveText('failed');

  // Pre-attempt failure: no target resolved, so the box names the requested
  // model and carries the human summary for the class.
  const preCells = page.locator('#global-activity-body tr').filter({ has: page.locator('[data-activity-resolved="errbox-pre"]') });
  await expect(preCells).toHaveCount(1);
  await expect(preCells.locator('.error-text')).toHaveCount(0);
  const preBox = preCells.locator('.activity-error-box');
  await expect(preBox).toHaveCount(1);
  await expect(preBox.locator('.model-id')).toHaveText('missing-model');
  await expect(preBox.locator('.attempt-error-summary')).toHaveText('Model not found');

  // Clicking the box opens the detail dialog with the full error class text.
  await preBox.click();
  await expect(page.locator('#error-dialog')).toBeVisible();
  await expect(page.locator('#error-title')).toContainText('missing-model');
  await expect(page.locator('#error-body')).toContainText('model_not_found');
  await expect(page.locator('#error-body')).toContainText('HTTP status: 404');
  await page.locator('#done-error').click();
  await expect(page.locator('#error-dialog')).toBeHidden();

  // The mobile card shows the same box, not a bare text span.
  await page.setViewportSize({ width: 390, height: 844 });
  // Global history renders its mobile cards into #global-activity-cards.
  const card = page.locator('#global-activity-cards .history-card').filter({ has: page.locator('[data-activity-resolved="errbox-pre"]') });
  await expect(card).toBeVisible();
  await expect(card.locator('.history-error .activity-error-box')).toHaveCount(1);
  await expect(card.locator('.error-text')).toHaveCount(0);
  await expect(card.locator('.history-error .attempt-error-summary')).toHaveText('Model not found');
});
