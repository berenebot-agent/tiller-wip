const { test, expect } = require('@playwright/test');

test('admin login, responsive navigation, one-time secret, and system view', async ({ page }) => {
  await page.setViewportSize({ width: 780, height: 700 });
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Tiller Router' })).toBeVisible();
  await page.getByLabel('Administrator').fill(process.env.TILLER_BROWSER_ADMIN_USERNAME || 'admin');
  await page.getByLabel('Password').fill(process.env.TILLER_BROWSER_ADMIN_PASSWORD || 'browser-test-password');
  await page.getByRole('button', { name: 'Enter control panel' }).click();
  await expect(page.getByRole('heading', { name: 'Providers', exact: true })).toBeVisible();

  await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page.getByRole('button', { name: 'Client Keys' }).click();
  await page.getByRole('button', { name: '+ Create client key' }).click();
  await page.getByLabel('Client name').fill('Container browser client');
  await page.getByLabel('Description').fill('Disposable Playwright workflow');
  await page.getByRole('button', { name: 'Create & show key' }).click();

  const secret = page.locator('#secret-value');
  await expect(secret).toHaveText(/^sk-tr-[A-Za-z0-9_-]{12}\.[A-Za-z0-9_-]{43}$/);
  await page.getByRole('button', { name: 'I have stored the key' }).click();
  await expect(secret).toHaveText('');
  await expect(page.locator('#clients-body tr')).toHaveCount(1);

  await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page.getByRole('button', { name: 'Backup / System' }).click();
  await expect(page.locator('#health-state')).toHaveText('READY');
  await expect(page.locator('.danger-note')).toContainText('recoverable provider API credentials');

  await page.setViewportSize({ width: 1440, height: 900 });
  await expect(page.getByRole('button', { name: 'Toggle navigation' })).toBeHidden();
  expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
});
