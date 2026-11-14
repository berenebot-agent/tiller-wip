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
  await openProviderSettings(page);
  await page.locator('[data-provider-reconnect-btn]').click();
}

async function openProviderSettings(page) {
  await openAdmin(page);
  await page.locator('a[data-view="providers"]').first().click();
  await page.getByRole('button', { name: 'Browse codex-redirect models' }).click();
  await page.locator('#drawer-edit').click();
  await expect(page.locator('#form-dialog')).toBeVisible();
}

test('subscription provider settings never show a base URL field', async ({ page }) => {
  await setupRedirectOAuth(page, { statusRef: { value: 'pending' } });
  await openProviderSettings(page);

  // The OAuth provider fixes the upstream, so the field is noise in the form.
  const dialog = page.locator('#form-dialog');
  await expect(dialog.locator('[data-provider-reconnect-btn]')).toBeVisible();
  await expect(dialog.locator('[name="base_url"]')).toHaveCount(0);
  await expect(dialog.getByLabel('Base URL')).toHaveCount(0);
});

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

// Device authorization providers (Codex subscription, GitHub Copilot) show a
// user code and must give the operator a way to reach the provider's approval
// page. The dialog regressed once by dropping that link, leaving only Copy, so
// both controls are asserted here.
test('device authorization dialog offers copy and the provider approval page', async ({ page }) => {
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
    body: JSON.stringify({ flow: 'device_code', verification_uri: 'https://auth.openai.com/codex/device', user_code: '48HT-E45RX', expires_in: 900, interval: 5 }),
  }));
  await page.route(`**/api/admin/providers/${PROVIDER_ID}/oauth/status`, route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ status: 'pending' }),
  }));

  await openProviderSettings(page);
  await page.locator('[data-provider-reconnect-btn]').click();

  const dialog = page.locator('#form-dialog');
  await expect(dialog.getByText('48HT-E45RX')).toBeVisible();
  await expect(dialog.locator('[data-copy-device]')).toBeVisible();

  const login = dialog.getByRole('link', { name: 'Login to Codex' });
  await expect(login).toBeVisible();
  await expect(login).toHaveAttribute('href', 'https://auth.openai.com/codex/device');
  await expect(login).toHaveAttribute('target', '_blank');
});
