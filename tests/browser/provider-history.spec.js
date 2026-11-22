const { test, expect } = require('@playwright/test');
const { ADMIN_USER, ADMIN_PASS, openAdmin, seedProvider } = require('./helpers');

test('provider detail follows the URL through reload and browser navigation', async ({ page }) => {
  const provider = seedProvider('provider-history');
  await openAdmin(page);
  await page.getByRole('link', { name: 'Providers' }).click();
  await page.getByRole('button', { name: 'Browse provider-history models' }).click();
  const detailURL = `/#providers/${encodeURIComponent(provider.id)}`;
  await expect(page).toHaveURL(new RegExp(`#providers/${provider.id}$`));
  await expect(page.locator('#provider-drawer-title')).toHaveText(provider.name);
  await page.reload();
  await expect(page.locator('#provider-drawer-title')).toHaveText(provider.name);
  await page.goBack();
  await expect(page).toHaveURL(/#providers$/);
  await expect(page.locator('#provider-drawer')).toBeHidden();
  await page.goForward();
  await expect(page.locator('#provider-drawer')).toBeVisible();
  await page.locator('#provider-drawer-close').click();
  await expect(page).toHaveURL(/#providers$/);
  await page.goto(detailURL);
  await expect(page.locator('#provider-drawer')).toBeVisible();
  await page.locator('#provider-drawer-close').click();
  await expect(page.locator('#provider-drawer')).toBeHidden();
  await page.goto('/#providers/missing-provider');
  await expect(page).toHaveURL(/#providers$/);
  await expect(page.getByText('Provider not found.')).toBeVisible();
});

test('provider deep link survives password sign-in', async ({ browser }) => {
  const provider = seedProvider('provider-history-login');
  const context = await browser.newContext({ baseURL: process.env.TILLER_BROWSER_BASE_URL, storageState: undefined });
  try {
    const page = await context.newPage();
    await page.goto(`/#providers/${encodeURIComponent(provider.id)}`);
    await page.locator('#login-form').getByLabel('Tiller username').fill(ADMIN_USER);
    await page.locator('#login-form').getByLabel('Password').fill(ADMIN_PASS);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.locator('#provider-drawer-title')).toHaveText(provider.name);
  } finally {
    await context.close();
  }
});
