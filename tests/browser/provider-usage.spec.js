const { test, expect } = require('@playwright/test');
const { openAdmin } = require('./helpers');

// A subscription provider shows quota bars only — no cost line, no plan name.
test('subscription provider cards show compact quota bars without cost', async ({ page }) => {
  await page.route(/\/api\/admin\/providers\?/, route => route.fulfill({ json: { data: [{ id: 'sub', name: 'subscription', type: 'codex-subscription', enabled: true, protocols: ['responses'], available_model_count: 1, model_count: 1 }] } }));
  await page.route('**/api/admin/models?all=1', route => route.fulfill({ json: { data: [{ id: 'model', provider_id: 'sub', provider_name: 'subscription', canonical_model_id: 'subscription/model', upstream_model_id: 'model', available: true }] } }));
  await page.route('**/api/admin/quota', route => route.fulfill({ json: { providers: { sub: { available: true, plan: 'Pro', fetched_at: '2026-09-30T12:00:00Z', windows: [{ label: '5h', used_percent: 35 }, { label: 'weekly', used_percent: 55 }] } } } }));
  await openAdmin(page);
  await page.locator('a[data-view="providers"]').first().click();
  const card = page.locator('[data-provider-id="sub"]');
  await expect(card.locator('progress')).toHaveCount(2);
  await expect(card.locator('progress').first()).toHaveAttribute('value', '35');
  await expect(card).toContainText('55% used');
  // The plan name and any cost line are gone.
  await expect(card).not.toContainText('Pro');
  await expect(card).not.toContainText('est.');
  await expect(card.locator('.provider-cost')).toHaveCount(0);
  await expect(page.locator('#account-quota-card')).toHaveCount(0);
});

// A pay-per-token provider shows a cost line with 1h/24h/7d and no quota bars.
test('pay-per-token provider cards show router cost across 1h/24h/7d', async ({ page }) => {
  await page.route(/\/api\/admin\/providers\?/, route => route.fulfill({ json: { data: [{ id: 'api', name: 'openai-main', type: 'openai', enabled: true, protocols: ['chat'], available_model_count: 1, model_count: 1 }] } }));
  await page.route('**/api/admin/models?all=1', route => route.fulfill({ json: { data: [{ id: 'model', provider_id: 'api', provider_name: 'openai-main', canonical_model_id: 'openai-main/gpt', upstream_model_id: 'gpt', available: true }] } }));
  await page.route('**/api/admin/quota', route => route.fulfill({ json: { providers: {} } }));
  await page.route(/\/api\/admin\/usage/, route => route.fulfill({ json: { real_cost: { 'openai-main/gpt': { '1h': 100000, '24h': 2500000, '7d': 9000000, estimated: { '1h': true, '24h': true, '7d': false } } } } }));
  await openAdmin(page);
  await page.locator('a[data-view="providers"]').first().click();
  const card = page.locator('[data-provider-id="api"]');
  await expect(card.locator('progress')).toHaveCount(0);
  await expect(card.locator('.provider-cost')).toContainText('1h');
  await expect(card.locator('.provider-cost')).toContainText('24h');
  await expect(card.locator('.provider-cost')).toContainText('7d');
  await expect(card.locator('.provider-cost')).toContainText('est.');
});
