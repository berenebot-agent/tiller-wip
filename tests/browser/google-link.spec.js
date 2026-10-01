const { test, expect } = require('@playwright/test');

// The Google link interstitial is hosted-only, but the browser harness runs a
// local-mode router. These specs mock `/api/runtime` and the hosted auth
// endpoints to drive the real client code paths, the same way
// analytics.spec.js and oauth-redirect.spec.js exercise other hosted-only UI.
//
// The local-mode CSP is `script-src 'self'`, so the real Google Identity
// Services script must never be allowed to load; GSI-driven cases therefore
// inject a minimal `window.google` stub and fire its credential callback, which
// is the exact seam app.js wires to `handleGoogleCredential`.

const ACCOUNT_JSON = JSON.stringify({
  email: 'owner@example.com', account_id: 'acct-1', plan: 'free', account_status: 'active',
  google_linked: true, password_enabled: false, has_password: true, verified: true,
});

async function mockHostedGoogleSignIn(page, { pendingLink = true } = {}) {
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/auth/options', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ google_enabled: true, google_client_id: 'test-client.apps.googleusercontent.com', signup_enabled: true, turnstile_enabled: false }),
  }));
  // Not signed in yet, so boot lands on the login card.
  await page.route('**/api/auth/session', route => route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unauthorized', message: 'Sign in required.' } }) }));
  await page.route('**/api/auth/login', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      authenticated: true, email: 'owner@example.com', username: 'owner@example.com',
      csrf_token: 'csrf-token', account_id: 'acct-1', ...(pendingLink ? { pending_google_link: true } : {}),
    }),
  }));
  // Anything the app fetches once it is inside the shell.
  await page.route('**/api/auth/onboarding', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ needs_onboarding: false }) }));
  await page.route('**/api/auth/account', route => route.fulfill({ status: 200, contentType: 'application/json', body: ACCOUNT_JSON }));
  await page.route('**/api/auth/account/plan', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/admin/usage', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/admin/providers**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/client-keys**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/virtual-models**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/live', route => route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }));
}

// installGsiStub replaces the Google Identity Services global with a stub that
// captures the credential callback app.js registers, and exposes a way to fire
// it. It must run before the app module executes.
async function installGsiStub(page) {
  await page.addInitScript(() => {
    window.__gsiCallback = null;
    window.__credentialCallback = null;
    window.google = {
      accounts: {
        id: {
          initialize(config) { window.__credentialCallback = config.callback; },
          renderButton() {},
          prompt() {},
          disableAutoSelect() {},
        },
      },
    };
  });
}

async function loginWithPendingGoogleLink(page) {
  await page.goto('/');
  await page.fill('#login-form [name="username"]', 'owner@example.com');
  await page.fill('#login-form [name="password"]', 'correct horse battery staple');
  await page.click('#login-submit');
}

test('password login with a pending Google link offers the interstitial', async ({ page }) => {
  await mockHostedGoogleSignIn(page);
  await loginWithPendingGoogleLink(page);

  await expect(page.locator('#account-google-link-shell')).toBeVisible();
  await expect(page.locator('#account-google-link-title')).toHaveText('Link Google to this account?');
  // The app must stay behind the prompt until the choice is made.
  await expect(page.locator('#app')).toBeHidden();
});

test('declining the link prompt enters the app signed in', async ({ page }) => {
  await mockHostedGoogleSignIn(page);
  await loginWithPendingGoogleLink(page);
  await expect(page.locator('#account-google-link-shell')).toBeVisible();

  await page.click('#account-google-link-skip');

  await expect(page.locator('#account-google-link-shell')).toBeHidden();
  await expect(page.locator('#app')).toBeVisible();
});

test('confirming the link prompt starts the authenticated Google link flow', async ({ page }) => {
  await mockHostedGoogleSignIn(page);
  let linkStartBody = null;
  await page.route('**/api/auth/google/link/start', async route => {
    linkStartBody = route.request().postDataJSON();
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ redirect_url: '/mock-google-authorize' }) });
  });
  // The confirm path calls location.assign; give the target a quiet response so
  // navigation does not race the assertion.
  await page.route('**/mock-google-authorize', route => route.fulfill({ status: 204, body: '' }));

  await loginWithPendingGoogleLink(page);
  await page.click('#account-google-link-confirm');

  await expect.poll(() => linkStartBody).not.toBeNull();
  // The interstitial path must not demand the password a second time.
  expect(linkStartBody).toEqual({ current_password: '' });
});

test('ordinary password login without a pending link goes straight to the app', async ({ page }) => {
  await mockHostedGoogleSignIn(page, { pendingLink: false });
  await loginWithPendingGoogleLink(page);

  await expect(page.locator('#account-google-link-shell')).toBeHidden();
  await expect(page.locator('#app')).toBeVisible();
});

test('a third-party GSI match sends the user to password sign-in with an explanation', async ({ page }) => {
  await mockHostedGoogleSignIn(page, { pendingLink: false });
  await installGsiStub(page);
  await page.route('**/api/auth/google/gsi', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ link_challenge_required: true, email: 'owner@example.com' }),
  }));
  await page.goto('/');
  await page.waitForFunction(() => typeof window.__credentialCallback === 'function');
  await page.evaluate(() => window.__credentialCallback({ credential: 'fake-credential' }));

  await expect(page.locator('#login-form')).toBeVisible();
  await expect(page.locator('#login-error')).toContainText('Sign in with your password to continue');
});

test('an authoritative GSI match still offers the one-click link', async ({ page }) => {
  await mockHostedGoogleSignIn(page, { pendingLink: false });
  await installGsiStub(page);
  await page.route('**/api/auth/google/gsi', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ link_required: true, email: 'owner@example.com' }),
  }));
  await page.goto('/');
  await page.waitForFunction(() => typeof window.__credentialCallback === 'function');
  await page.evaluate(() => window.__credentialCallback({ credential: 'fake-credential' }));

  // This form was previously never un-hidden by authView, making the shortcut
  // unreachable; this is the regression guard for that repair.
  await expect(page.locator('#google-link-confirm-form')).toBeVisible();
  await expect(page.locator('#google-link-confirm-message')).toContainText('owner@example.com');
});
