const { test, expect } = require('@playwright/test');
const { openAdmin } = require('./helpers');

// A subscription provider shows compact quota bars only — no cost line, no plan
// name. (Pay-per-token cost rendering is covered by the Go cost/aggregation
// tests; the browser harness has no costed Activity to drive it here.)
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
  // The plan name and any cost line are gone for a subscription provider.
  await expect(card).not.toContainText('Pro');
  await expect(card).not.toContainText('est.');
  await expect(card.locator('.provider-cost')).toHaveCount(0);
  await expect(page.locator('#account-quota-card')).toHaveCount(0);
});

// An Ollama Cloud card shows the two subscription windows plus a purchased
// "credits" window that carries a remaining USD amount and no percentage, so it
// renders "$ left" rather than a bar or "Unlimited".
test('ollama cloud card shows percentage windows and a credits balance', async ({ page }) => {
  await page.route(/\/api\/admin\/providers\?/, route => route.fulfill({ json: { data: [{ id: 'ollama', name: 'ollama', type: 'ollama-cloud', enabled: true, protocols: ['chat'], available_model_count: 1, model_count: 1 }] } }));
  await page.route('**/api/admin/models?all=1', route => route.fulfill({ json: { data: [{ id: 'model', provider_id: 'ollama', provider_name: 'ollama', canonical_model_id: 'ollama/model', upstream_model_id: 'model', available: true }] } }));
  await page.route('**/api/admin/quota', route => route.fulfill({ json: { providers: { ollama: { available: true, windows: [
    { label: 'session', used_percent: 27.66, resets_at: '2026-10-10T11:00:00Z' },
    { label: 'weekly', used_percent: 81.45 },
    { label: 'credits', remaining: 12.5 },
  ] } } } }));
  await openAdmin(page);
  await page.locator('a[data-view="providers"]').first().click();
  const card = page.locator('[data-provider-id="ollama"]');
  await expect(card).toContainText('28% used');
  await expect(card).toContainText('81% used');
  await expect(card).toContainText('$12.50 left');
  await expect(card).not.toContainText('Unlimited');
});
