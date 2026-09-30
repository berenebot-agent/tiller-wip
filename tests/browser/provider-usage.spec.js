const { test, expect } = require('@playwright/test');
const { openAdmin } = require('./helpers');

test('provider cards show quota bars and attributed router costs', async ({ page }) => {
  await page.route(/\/api\/admin\/providers\?/, route => route.fulfill({ json: { data: [{ id: 'sub', name: 'subscription', type: 'codex-subscription', enabled: true, protocols: ['responses'], available_model_count: 1, model_count: 1 }] } }));
  await page.route('**/api/admin/models?all=1', route => route.fulfill({ json: { data: [{ id: 'model', provider_id: 'sub', provider_name: 'subscription', canonical_model_id: 'subscription/model', upstream_model_id: 'model', available: true }] } }));
  await page.route('**/api/admin/quota', route => route.fulfill({ json: { providers: { sub: { available: true, plan: 'Pro', fetched_at: '2026-09-30T12:00:00Z', windows: [{ label: '5h', used_percent: 35 }, { label: 'weekly', used_percent: 55 }] } } } }));
  await openAdmin(page);
  await page.locator('a[data-view="providers"]').first().click();
  const card = page.locator('[data-provider-id="sub"]');
  await expect(card.locator('progress')).toHaveCount(2);
  await expect(card.locator('progress').first()).toHaveAttribute('value', '35');
  await expect(card).toContainText('55% used');
  await expect(card).toContainText('API-equivalent router usage');
  await expect(page.locator('#account-quota-card')).toHaveCount(0);
});
