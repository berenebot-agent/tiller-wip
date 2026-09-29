const { test, expect } = require('@playwright/test');

// Analytics is hosted-only, but the browser harness runs a local-mode router.
// These specs therefore mock `/api/runtime` (and the hosted endpoints) to drive
// the same client code paths, the same way oauth-redirect.spec.js mocks the
// hosted redirect flow.
const SCRIPT_URL = 'https://analytics.example.com/script.js';
const SITE_ID = 'site-1';

async function mockHostedAnalytics(page, { enabled = true } = {}) {
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/auth/options', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/analytics/options', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify(enabled ? { enabled: true, provider: 'umami', script_url: SCRIPT_URL, site_id: SITE_ID } : { enabled: false }),
  }));
  // The local-mode CSP blocks the third-party origin, so the script element is
  // created but never fetches. Fulfil it anyway so no request dangles.
  await page.route('https://analytics.example.com/**', route => route.fulfill({ status: 200, contentType: 'application/javascript', body: '/* analytics */' }));
}

test('analytics script loads only after consent is accepted', async ({ page }) => {
  await mockHostedAnalytics(page, { enabled: true });
  await page.goto('/');

  const banner = page.locator('#analytics-consent');
  await expect(banner).toBeVisible();
  await expect(page.locator('script[data-tiller-analytics]')).toHaveCount(0);

  await page.locator('#analytics-accept').click();
  await expect(banner).toBeHidden();
  const script = page.locator('script[data-tiller-analytics]');
  await expect(script).toHaveCount(1);
  await expect(script).toHaveAttribute('src', SCRIPT_URL);
  await expect(script).toHaveAttribute('data-website-id', SITE_ID);
});

test('declining is remembered and no analytics script is loaded', async ({ page }) => {
  await mockHostedAnalytics(page, { enabled: true });
  await page.goto('/');

  const banner = page.locator('#analytics-consent');
  await expect(banner).toBeVisible();
  await page.locator('#analytics-decline').click();
  await expect(banner).toBeHidden();
  await expect(page.locator('script[data-tiller-analytics]')).toHaveCount(0);

  // The decision survives a reload: no banner, still no script.
  await page.reload();
  await expect(page.locator('#analytics-consent')).toBeHidden();
  await expect(page.locator('script[data-tiller-analytics]')).toHaveCount(0);
});

test('a stored acceptance loads analytics without prompting', async ({ page }) => {
  await page.addInitScript(() => { try { localStorage.setItem('tiller_analytics_consent', 'accepted'); } catch {} });
  await mockHostedAnalytics(page, { enabled: true });
  await page.goto('/');

  await expect(page.locator('#analytics-consent')).toBeHidden();
  await expect(page.locator('script[data-tiller-analytics]')).toHaveCount(1);
});

test('no banner or script when analytics is disabled', async ({ page }) => {
  await mockHostedAnalytics(page, { enabled: false });
  await page.goto('/');

  await expect(page.locator('#analytics-consent')).toBeHidden();
  await expect(page.locator('script[data-tiller-analytics]')).toHaveCount(0);
});

test('platform admin loads and saves analytics settings', async ({ page }) => {
  let saved = null;
  const settings = {
    hosted_signup_enabled: true,
    audit_retention_days: 30,
    analytics: { enabled: true, provider: 'umami', script_url: SCRIPT_URL, site_id: SITE_ID },
  };
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/analytics/options', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ enabled: false }) }));
  await page.route('**/api/platform/session', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ authenticated: true, csrf_token: 'csrf-test', expires_at: '2099-01-01T00:00:00Z' }) }));
  await page.route('**/api/platform/stats', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ usage_available: false, accounts: {} }) }));
  await page.route('**/api/platform/settings', route => {
    if (route.request().method() === 'PUT') {
      saved = route.request().postDataJSON();
      return route.fulfill({ status: 204, body: '' });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(settings) });
  });

  await page.goto('/platform#settings');
  const form = page.locator('#platform-analytics-form');
  await expect(form).toBeVisible();
  await expect(form.locator('[name="analytics_enabled"]')).toBeChecked();
  await expect(form.locator('[name="analytics_provider"]')).toHaveValue('umami');
  await expect(form.locator('[name="analytics_script_url"]')).toHaveValue(SCRIPT_URL);
  await expect(form.locator('[name="analytics_site_id"]')).toHaveValue(SITE_ID);

  await form.locator('[name="analytics_provider"]').selectOption('plausible');
  await form.locator('[name="analytics_script_url"]').fill('https://plausible.example.com/js/script.js');
  await form.locator('[name="analytics_site_id"]').fill('tiller.example.com');
  await form.getByRole('button', { name: 'Save analytics settings' }).click();

  await expect.poll(() => saved).not.toBeNull();
  expect(saved).toMatchObject({
    analytics_enabled: true,
    analytics_provider: 'plausible',
    analytics_script_url: 'https://plausible.example.com/js/script.js',
    analytics_site_id: 'tiller.example.com',
  });
});
