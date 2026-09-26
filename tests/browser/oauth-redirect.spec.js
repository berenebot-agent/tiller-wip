const { test, expect } = require('@playwright/test');
const { openAdmin } = require('./helpers');

// The hosted redirect-callback dialog is driven entirely by the
// `callback_mode: "redirect"` field in the oauth/start response, so the local
// browser harness can exercise it by mocking the OAuth endpoints. The SPA must
// wait on status polling and keep the paste fallback available.

const PROVIDER_ID = 'provider-redirect';

async function setupRedirectOAuth(page, { statusRef }) {
  await page.addInitScript(() => {
    window.open = () => null;
  });
  await page.route('**/api/admin/provider-types', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ data: [{ type: 'codex-subscription', label: 'Codex Subscription', auth_mode: 'oauth', credential_needed: false, protocols: ['responses'] }] }),
  }));
  await page.route(/\/api\/admin\/providers\?/, route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ data: [{ id: PROVIDER_ID, name: 'codex-redirect', type: 'codex-subscription', base_url: 'https://chatgpt.com/backend-api/codex', enabled: true, protocols: ['responses'], credential_configured: true, auth_state: 'connected', available_model_count: 1, model_count: 1 }] }),
  }));
  await page.route(`**/api/admin/providers/${PROVIDER_ID}/oauth/start`, route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ authorization_url: 'https://auth.openai.com/oauth/authorize?x=1', redirect_uri: 'https://tiller.example.com/auth/callback', callback_mode: 'redirect', expires_in: 600 }),
  }));
  await page.route(`**/api/admin/providers/${PROVIDER_ID}/oauth/status`, route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ status: statusRef.value }),
  }));
}

async function openRedirectDialog(page) {
  await openAdmin(page);
  await page.locator('a[data-view="providers"]').first().click();
  await page.locator(`[data-provider-edit="${PROVIDER_ID}"]`).click();
  await page.locator('[data-provider-reconnect-btn]').click();
}

test('hosted redirect OAuth waits for the callback and closes on completion', async ({ page }) => {
  const statusRef = { value: 'pending' };
  await setupRedirectOAuth(page, { statusRef });

  await openRedirectDialog(page);
  const dialog = page.locator('#form-dialog');
  await expect(dialog.getByRole('heading', { name: 'Finish sign-in' })).toBeVisible();
  await expect(dialog.getByText('Waiting for sign-in...')).toBeVisible();
  await expect(dialog.getByText('Trouble signing in? Paste the redirected URL instead.')).toBeVisible();

  // The server-side callback completes; the next status poll must close the
  // dialog rather than showing the stale pre-existing token as connected.
  statusRef.value = 'connected';
  await expect(page.locator('#flash')).toContainText('Codex Subscription connected.', { timeout: 10000 });
  await expect(dialog).toBeHidden();
});

test('hosted redirect OAuth paste fallback submits the redirected URL', async ({ page }) => {
  const statusRef = { value: 'pending' };
  let callbackBody = null;
  await setupRedirectOAuth(page, { statusRef });
  await page.route(`**/api/admin/providers/${PROVIDER_ID}/oauth/callback`, route => {
    callbackBody = route.request().postDataJSON();
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'connected' }) });
  });

  await openRedirectDialog(page);
  const dialog = page.locator('#form-dialog');
  await dialog.locator('summary').click();
  await dialog.getByLabel('Redirected URL').fill('https://tiller.example.com/auth/callback?code=abc&state=xyz');
  await page.locator('#dialog-submit').click();

  await expect(page.locator('#flash')).toContainText('Codex Subscription connected.');
  expect(callbackBody).toEqual({ redirected_url: 'https://tiller.example.com/auth/callback?code=abc&state=xyz' });
});
