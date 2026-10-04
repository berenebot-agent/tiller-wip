const { test, expect } = require('@playwright/test');

// Passkeys are hosted-only, but the browser harness runs a local-mode router.
// Like google-link.spec.js and analytics.spec.js, these specs mock
// `/api/runtime` and the hosted auth endpoints, then replace
// navigator.credentials with a stub that returns a synthetic credential. The
// real WebAuthn ceremony (key generation, signatures, backup flags) is proven
// by the Go tests with a software authenticator; this suite proves the browser
// glue: CSRF headers, the begin/finish round trip, UI visibility, and the
// passkey-only re-authentication wiring for sensitive account operations.

const CHALLENGE = Buffer.from('test-challenge-bytes-123456').toString('base64url');
const USER_ID = Buffer.from('user-handle-bytes').toString('base64url');

const PROFILE = {
  user_id: 'user-1',
  email: 'owner@example.com',
  account_id: 'acct-1',
  plan: 'free',
  user_status: 'active',
  account_status: 'active',
  verified: true,
  password_enabled: true,
  has_password: true,
  google_linked: false,
  passkeys_enabled: true,
  passkeys: [{ id: 'pk-1', name: 'Laptop', created_at: '2026-10-01T10:00:00Z', last_used_at: '2026-10-03T09:00:00Z' }],
};

// installCredentialStub makes window.PublicKeyCredential present and intercepts
// navigator.credentials.create/get, recording each call so the spec can assert
// the site actually invoked the ceremony.
async function installCredentialStub(page) {
  await page.addInitScript(() => {
    window.__credentialCalls = [];
    const buffer = bytes => new Uint8Array(bytes).buffer;
    const fakeCredential = () => ({
      id: 'fake-credential-id',
      rawId: buffer([1, 2, 3, 4]),
      type: 'public-key',
      response: {
        clientDataJSON: buffer([5, 6, 7]),
        attestationObject: buffer([8, 9, 10]),
        authenticatorData: buffer([11, 12, 13]),
        signature: buffer([14, 15, 16]),
        userHandle: buffer([17, 18]),
      },
      getClientExtensionResults: () => ({}),
    });
    Object.defineProperty(window, 'PublicKeyCredential', { value: function PublicKeyCredential() {}, configurable: true });
    navigator.credentials.create = async options => { window.__credentialCalls.push({ kind: 'create', options }); return fakeCredential(); };
    navigator.credentials.get = async options => { window.__credentialCalls.push({ kind: 'get', options }); return fakeCredential(); };
  });
}

// mockHostedPasskeyPage drives the hosted account page with a mocked profile.
// finishHandler and reauthHandler let each test observe request bodies and
// headers and choose responses.
async function mockHostedPasskeyPage(page, { profile = PROFILE, onRegisterFinish, onReauthFinish } = {}) {
  let current = { ...profile };
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/auth/options', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ google_enabled: false, signup_enabled: true, turnstile_enabled: false, passkeys_enabled: true }),
  }));
  await page.route('**/api/auth/session', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ authenticated: true, email: 'owner@example.com', username: 'owner@example.com', csrf_token: 'csrf-token', account_id: 'acct-1' }),
  }));
  await page.route('**/api/auth/account', async route => {
    if (route.request().method() === 'GET') {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(current) });
      return;
    }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true }) });
  });
  await page.route('**/api/auth/account/plan', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/admin/usage', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/admin/providers**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/client-keys**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/virtual-models**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/live', route => route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }));
  await page.route('**/api/auth/onboarding', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ needs_onboarding: false }) }));
  await page.route('**/api/auth/account/passkeys/register/begin', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ challenge_token: 'reg-token', options: { publicKey: { challenge: CHALLENGE, user: { id: USER_ID }, excludeCredentials: [] } } }),
  }));
  await page.route('**/api/auth/account/passkeys/register/finish*', async route => {
    if (onRegisterFinish) onRegisterFinish(route.request());
    const payload = { ok: true, password_only: false };
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(payload) });
    current = { ...current, passkeys: [...current.passkeys, { id: 'pk-2', name: 'Phone', created_at: '2026-10-04T10:00:00Z' }] };
  });
  await page.route('**/api/auth/account/passkeys/reauth/begin', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ challenge_token: 'reauth-token', options: { publicKey: { challenge: CHALLENGE, allowCredentials: [{ id: USER_ID, type: 'public-key' }] } } }),
  }));
  await page.route('**/api/auth/account/passkeys/reauth/finish', async route => {
    if (onReauthFinish) onReauthFinish(route.request());
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ confirmed: true }) });
  });
  await page.route('**/api/auth/account/passkeys/rename', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ renamed: true }) }));
  await page.route('**/api/auth/account/passkeys/delete', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ removed: true }) }));
  await page.route('**/api/auth/account/passkeys/password-signin', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ enabled: true }) }));
  return { setProfile: next => { current = next; } };
}

// openAccountTab navigates to the Account settings tab after boot. The Settings
// nav link must be clicked first: the settings sub-tabs only render inside the
// Settings view.
async function openAccountTab(page) {
  await page.goto('/');
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await page.locator('[data-settings-tab="account"]').click();
  await expect(page.locator('#account-passkeys-card')).toBeVisible();
}

test('passkey sign-in button is hidden on a browser without WebAuthn support', async ({ page }) => {
  // Hide the API before the app module runs: the button must not render.
  await page.addInitScript(() => {
    Object.defineProperty(window, 'PublicKeyCredential', { value: undefined, configurable: true });
  });
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/auth/options', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ passkeys_enabled: true, google_enabled: false, signup_enabled: true, turnstile_enabled: false }),
  }));
  await page.route('**/api/auth/session', route => route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unauthorized', message: 'Sign in required.' } }) }));
  await page.goto('/');
  await expect(page.locator('#passkey-signin')).toBeHidden();
});

test('passkey sign-in performs the ceremony and enters the app', async ({ page }) => {
  await installCredentialStub(page);
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/auth/options', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ passkeys_enabled: true, google_enabled: false, signup_enabled: true, turnstile_enabled: false }),
  }));
  await page.route('**/api/auth/session', route => route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unauthorized', message: 'Sign in required.' } }) }));
  await page.route('**/api/auth/passkey/begin', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ challenge_token: 'login-token', options: { publicKey: { challenge: CHALLENGE, allowCredentials: [] } } }),
  }));
  let finishRequest = null;
  await page.route('**/api/auth/passkey/finish', async route => {
    finishRequest = route.request();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ authenticated: true, email: 'owner@example.com', username: 'owner@example.com', csrf_token: 'csrf-token', account_id: 'acct-1' }),
    });
  });
  await page.route('**/api/auth/onboarding', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ needs_onboarding: false }) }));
  await page.route('**/api/auth/account/plan', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/admin/usage', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({}) }));
  await page.route('**/api/admin/providers**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/client-keys**', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [] }) }));
  await page.route('**/api/admin/live', route => route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }));

  await page.goto('/');
  await expect(page.locator('#passkey-signin')).toBeVisible();
  await page.click('#passkey-signin');
  await expect(page.locator('#app')).toBeVisible();
  await expect.poll(() => finishRequest).not.toBeNull();
  expect(finishRequest.headers()['x-webauthn-challenge']).toBe('login-token');
  expect(JSON.parse(finishRequest.postData())).toHaveProperty('rawId');
});

test('adding a passkey sends the challenge token and the CSRF header', async ({ page }) => {
  // Regression guard: register/finish is session+CSRF protected. This failed
  // before the X-CSRF-Token header was added to the ceremony helper.
  await installCredentialStub(page);
  let finishRequest = null;
  await mockHostedPasskeyPage(page, { onRegisterFinish: req => { finishRequest = req; } });
  await page.on('dialog', dialog => dialog.accept('Phone'));
  await openAccountTab(page);

  await expect(page.locator('#account-passkeys-list')).toContainText('Laptop');
  await page.click('#account-passkey-add');

  await expect.poll(() => finishRequest).not.toBeNull();
  expect(finishRequest.headers()['x-webauthn-challenge']).toBe('reg-token');
  expect(finishRequest.headers()['x-csrf-token']).toBe('csrf-token');
  expect(finishRequest.url()).toContain('name=Phone');
  await expect(page.locator('#account-passkeys-list')).toContainText('Phone');
});

test('passkey management renders created and last-used metadata', async ({ page }) => {
  await installCredentialStub(page);
  await mockHostedPasskeyPage(page);
  await openAccountTab(page);
  const row = page.locator('[data-passkey="pk-1"]');
  await expect(row).toContainText('Laptop');
  await expect(row).toContainText('Added');
  await expect(row).toContainText('Last used');
});

test('passkey-only account confirms a password change with a passkey assertion', async ({ page }) => {
  await installCredentialStub(page);
  const passkeyOnly = { ...PROFILE, password_enabled: false, has_password: true };
  let reauthSeen = null;
  let passwordBody = null;
  await mockHostedPasskeyPage(page, {
    profile: passkeyOnly,
    onReauthFinish: req => { reauthSeen = req; },
  });
  await page.route('**/api/auth/account/password', async route => {
    passwordBody = route.request().postDataJSON();
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ changed: true }) });
  });
  await openAccountTab(page);

  // The password row is replaced by a passkey-confirm row.
  await expect(page.locator('#account-current-password-row')).toBeHidden();
  await expect(page.locator('#account-passkey-reauth-row')).toBeVisible();
  // The re-enable toggle is offered for a passkey-only account.
  await expect(page.locator('#account-password-signin-toggle')).toBeVisible();

  await page.fill('#account-new-password', 'a replacement passphrase');
  await page.click('#account-password-form button[type="submit"]');

  await expect.poll(() => passwordBody).not.toBeNull();
  expect(passwordBody.current_password).toBe('');
  await expect.poll(() => reauthSeen).not.toBeNull();
  expect(reauthSeen.headers()['x-webauthn-challenge']).toBe('reauth-token');
  expect(reauthSeen.headers()['x-csrf-token']).toBe('csrf-token');
});

test('a failed passkey assertion blocks the sensitive operation and shows the error', async ({ page }) => {
  await installCredentialStub(page);
  const passkeyOnly = { ...PROFILE, password_enabled: false, has_password: true };
  let passwordCalled = false;
  await mockHostedPasskeyPage(page, { profile: passkeyOnly });
  await page.route('**/api/auth/account/passkeys/reauth/finish', route => route.fulfill({
    status: 401,
    contentType: 'application/json',
    body: JSON.stringify({ error: { code: 'invalid_credentials', message: 'Passkey confirmation failed.' } }),
  }));
  await page.route('**/api/auth/account/password', async route => {
    passwordCalled = true;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ changed: true }) });
  });
  await openAccountTab(page);

  await page.fill('#account-new-password', 'a replacement passphrase');
  await page.click('#account-password-form button[type="submit"]');

  await expect(page.locator('#account-password-error')).toContainText('Passkey confirmation failed.');
  expect(passwordCalled).toBe(false);
});

test('a passkey-only account can delete itself with a passkey confirmation', async ({ page }) => {
  await installCredentialStub(page);
  const passkeyOnly = { ...PROFILE, password_enabled: false, has_password: true };
  await mockHostedPasskeyPage(page, { profile: passkeyOnly });
  let reauthCalled = false;
  await page.route('**/api/auth/account/passkeys/reauth/finish', async route => {
    reauthCalled = true;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ confirmed: true }) });
  });
  let deleteBody = null;
  await page.route('**/api/auth/account', async route => {
    if (route.request().method() === 'DELETE') {
      deleteBody = route.request().postDataJSON();
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ message: 'Your account and its data have been deleted.' }) });
      return;
    }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(passkeyOnly) });
  });
  await openAccountTab(page);

  await expect(page.locator('#account-delete-password-row')).toBeHidden();
  await expect(page.locator('#account-delete-passkey-row')).toBeVisible();
  await page.click('#account-delete-form button[type="submit"]');
  // The confirmation shell asks for the email typed exactly.
  await page.fill('#account-delete-confirm-email', 'owner@example.com');
  await page.click('#account-delete-confirm-form button[type="submit"]');

  await expect.poll(() => deleteBody).not.toBeNull();
  expect(deleteBody.password).toBe('');
  expect(reauthCalled).toBe(true);
});

test('Google-linked accounts see no password re-enable toggle', async ({ page }) => {
  await installCredentialStub(page);
  const googleLinked = { ...PROFILE, password_enabled: false, google_linked: true, has_password: false, passkeys: [] };
  await mockHostedPasskeyPage(page, { profile: googleLinked });
  await openAccountTab(page);
  await expect(page.locator('#account-password-signin-toggle')).toBeHidden();
});

test('adding a passkey can make it the only sign-in method', async ({ page }) => {
  await installCredentialStub(page);
  let finishURL = null;
  await mockHostedPasskeyPage(page, { onRegisterFinish: req => { finishURL = req.url(); } });
  // Accept the name prompt and the "only method" confirmation.
  await page.on('dialog', dialog => dialog.accept('Laptop'));
  await openAccountTab(page);
  await page.click('#account-passkey-add');
  await expect.poll(() => finishURL).not.toBeNull();
  expect(finishURL).toContain('only=1');
});
