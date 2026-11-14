const { test, expect } = require('@playwright/test');

// Turnstile gates are hosted-only, but the browser harness runs a local-mode
// router. Like google-link.spec.js, passkey.spec.js and analytics.spec.js, these
// specs mock `/api/runtime` and `/api/auth/options`, then install a stub
// `window.turnstile` that records rendered actions and lets the test decide
// when (and whether) the challenge reports success.
//
// The behaviour under test is the client-side courtesy gate: while a challenge
// is mounted and unsolved, the submit (and the "back" control) on the signup and
// password-recovery pages are disabled so a visitor cannot spend a click on a
// request that is guaranteed to be rejected. The server check is unchanged and
// remains the authority.
async function installTurnstileStub(page) {
  await page.addInitScript(() => {
    window.__turnstile = { rendered: [], removed: [], resets: 0, solve: null };
    window.turnstile = {
      render: (_selector, options) => {
        const widget = `widget-${window.__turnstile.rendered.length}`;
        window.__turnstile.rendered.push({ action: options.action, widget });
        window.__turnstile.solve = () => options.callback('captcha-proof');
        window.__turnstile.expire = () => options['expired-callback']();
        return widget;
      },
      reset: () => { window.__turnstile.resets += 1; },
      remove: widget => { window.__turnstile.removed.push(widget); },
    };
  });
}

async function mockHostedAuth(page, { turnstile = true } = {}) {
  await page.route('**/api/runtime', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ mode: 'hosted' }) }));
  await page.route('**/api/auth/options', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      google_enabled: false,
      signup_enabled: true,
      passkeys_enabled: false,
      turnstile_enabled: turnstile,
      turnstile_site_key: turnstile ? 'site-key' : '',
    }),
  }));
  await page.route('**/api/auth/session', route => route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unauthorized', message: 'Sign in required.' } }) }));
}

const solveTurnstile = page => page.evaluate(() => window.__turnstile.solve());
const renderedActions = page => page.evaluate(() => window.__turnstile.rendered.map(entry => entry.action));

test('signup submit stays disabled until the security check is solved', async ({ page }) => {
  await installTurnstileStub(page);
  await mockHostedAuth(page);
  let signups = 0;
  await page.route('**/api/auth/signup', route => { signups += 1; return route.fulfill({ status: 202, json: { message: 'Check your inbox.' } }); });

  await page.goto('/login');
  await page.click('#show-signup');
  const submit = page.locator('#signup-form button[type=submit]');
  await expect(page.locator('#signup-form')).toBeVisible();
  await expect(page.locator('#auth-captcha-wrap')).toBeVisible();
  await expect.poll(() => renderedActions(page)).toEqual(['signup']);

  // Fields stay usable; only the actions are held back.
  await page.locator('#signup-form input[name=email]').fill('new@example.com');
  await page.locator('#signup-form input[name=accept_terms]').check();
  await expect(page.locator('#signup-form input[name=email]')).toBeEnabled();
  await expect(submit).toBeDisabled();
  await expect(page.locator('#show-login-from-signup')).toBeDisabled();
  await expect(page.locator('#auth-captcha-wrap .meta-line')).toHaveText('Complete the security check to continue.');

  // A locked click is inert: no request reaches the signup endpoint.
  await submit.click({ force: true });
  expect(signups).toBe(0);

  await solveTurnstile(page);
  await expect(submit).toBeEnabled();
  await expect(page.locator('#show-login-from-signup')).toBeEnabled();

  await submit.click();
  await expect.poll(() => signups).toBe(1);
  await expect(page.locator('#signup-done')).toBeVisible();
});

test('an expired security check re-locks the signup submit', async ({ page }) => {
  await installTurnstileStub(page);
  await mockHostedAuth(page);
  await page.goto('/login');
  await page.click('#show-signup');
  const submit = page.locator('#signup-form button[type=submit]');
  await expect(submit).toBeDisabled();
  await solveTurnstile(page);
  await expect(submit).toBeEnabled();
  await page.evaluate(() => window.__turnstile.expire());
  await expect(submit).toBeDisabled();
});

test('password recovery submit stays disabled until the security check is solved', async ({ page }) => {
  await installTurnstileStub(page);
  await mockHostedAuth(page);
  let recoveryRequests = 0;
  await page.route('**/api/auth/password-reset/request', route => { recoveryRequests += 1; return route.fulfill({ status: 202, json: { message: 'If the address belongs to an account, a reset message will arrive shortly.' } }); });

  await page.goto('/login');
  await page.click('#show-forgot-password');
  const submit = page.locator('#forgot-form button[type=submit]');
  await expect(page.locator('#forgot-form')).toBeVisible();
  await expect.poll(() => renderedActions(page)).toEqual(['recovery']);
  await page.locator('#forgot-form input[name=email]').fill('owner@example.com');
  await expect(submit).toBeDisabled();
  await expect(page.locator('#show-login-from-forgot')).toBeDisabled();

  await submit.click({ force: true });
  expect(recoveryRequests).toBe(0);

  await solveTurnstile(page);
  await expect(submit).toBeEnabled();
  await submit.click();
  await expect.poll(() => recoveryRequests).toBe(1);
  await expect(page.locator('#forgot-done')).toBeVisible();
});

test('switching views re-locks the new page and releases the old one', async ({ page }) => {
  await installTurnstileStub(page);
  await mockHostedAuth(page);
  await page.goto('/login');
  await page.click('#show-signup');
  await solveTurnstile(page);
  await expect(page.locator('#signup-form button[type=submit]')).toBeEnabled();

  // Recovery needs its own action scope, so the widget is re-rendered and the
  // recovery page starts locked again — including its own "back" control.
  await page.click('#show-login-from-signup');
  await expect(page.locator('#login-form')).toBeVisible();
  await expect(page.locator('#login-form button[type=submit]')).toBeEnabled();
  await page.click('#show-forgot-password');
  await expect.poll(() => renderedActions(page)).toEqual(['signup', 'recovery']);
  await expect(page.locator('#forgot-form button[type=submit]')).toBeDisabled();
  await expect(page.locator('#show-login-from-forgot')).toBeDisabled();
  await solveTurnstile(page);
  await expect(page.locator('#show-login-from-forgot')).toBeEnabled();

  // Back on sign-in nothing is gated at all.
  await page.click('#show-login-from-forgot');
  await expect(page.locator('#login-form')).toBeVisible();
  await expect(page.locator('#login-form button[type=submit]')).toBeEnabled();
});

test('without Turnstile the signup and recovery submits are untouched', async ({ page }) => {
  await installTurnstileStub(page);
  await mockHostedAuth(page, { turnstile: false });
  await page.goto('/login');
  await page.click('#show-signup');
  await expect(page.locator('#signup-form button[type=submit]')).toBeEnabled();
  await expect(page.locator('#auth-captcha-wrap')).toBeHidden();
  await page.click('#show-login-from-signup');
  await page.click('#show-forgot-password');
  await expect(page.locator('#forgot-form button[type=submit]')).toBeEnabled();
});
