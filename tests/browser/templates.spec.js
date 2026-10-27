const { test, expect } = require('@playwright/test');
const { openAdmin, adminCsrf, createProvider } = require('./helpers');

// Catalogue templates: create one, pick it for a Catalogue client on the
// catalogue screen, see the models read-only, switch back to Custom via the
// Settings link, then delete the template.
test('catalogue template links a client, shows it read-only, and detaches to Custom', async ({ page }) => {
  const suffix = Date.now().toString(36);
  const providerName = `tpl-prov-${suffix}`, templateName = `Template ${suffix}`, clientName = `tpl-client-${suffix}`;
  await openAdmin(page);
  const csrf = await adminCsrf(page);
  await createProvider(page, csrf, providerName);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();

  // Create an empty template; its model list opens straight after.
  await page.locator('#open-templates').click();
  await expect(page.locator('#templates-dialog')).toBeVisible();
  await page.locator('#add-template').click();
  await page.locator('#form-dialog [name="name"]').fill(templateName);
  await page.locator('#form-dialog [name="start_from"]').selectOption('empty');
  await page.locator('#dialog-submit').click();
  await expect(page.locator('#permissions-title')).toHaveText(`Manage models · ${templateName}`);
  const firstModel = page.locator(`#permission-groups .permission-row[data-canonical^="${providerName}/"] input`).first();
  await firstModel.check();
  await page.locator('#save-permissions').click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  const templateRow = page.locator('#template-list .template-row').filter({ hasText: templateName });
  await expect(templateRow).toContainText('Used by 0 clients');
  await page.locator('#done-templates').click();

  // Create a Catalogue client; it starts Custom.
  await page.locator('#add-client').click();
  await page.locator('#form-dialog [name="name"]').fill(clientName);
  await page.locator('#form-dialog [name="type"]').selectOption('catalogue');
  await page.locator('#dialog-submit').click();
  await expect(page.locator('#secret-value')).toBeVisible();
  await page.locator('#close-secret').click();
  const clientRow = page.locator('#clients-body tr').filter({ hasText: clientName });
  const routeButton = clientRow.locator('[data-client-models]');
  await expect(routeButton).toContainText('Custom');

  // Pick the template on the catalogue screen: its models show read-only.
  const modelSwitch = () => page.locator(`#permission-groups .permission-row[data-canonical^="${providerName}/"] input`).first();
  await routeButton.click();
  await expect(page.locator('#permission-template')).toHaveValue('');
  await expect(modelSwitch()).not.toBeChecked();
  await page.locator('#permission-template').selectOption({ label: `Template · ${templateName}` });
  await expect(modelSwitch()).toBeChecked();
  await expect(modelSwitch()).toBeDisabled();
  await expect(page.locator('#save-permissions')).toHaveText(`Use ${templateName}`);
  await page.locator('#save-permissions').click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await expect(routeButton).toContainText(`Template · ${templateName}`);

  // Reopened, a linked client is read-only with nothing to save.
  await routeButton.click();
  await expect(page.locator('#permission-template')).toHaveValue(/.+/);
  await expect(page.locator('#save-permissions')).toBeHidden();
  await expect(modelSwitch()).toBeDisabled();
  await page.locator('#cancel-permissions').click();

  // Settings links to the catalogue screen; switching to Custom there keeps
  // the models and makes them editable.
  await clientRow.locator('[data-client-edit]').click();
  const catalogueLink = page.locator('#form-dialog [data-open-catalogue]');
  await expect(catalogueLink).toContainText(`Template · ${templateName}`);
  await catalogueLink.click();
  await expect(page.locator('#form-dialog')).toBeHidden();
  await page.locator('#permission-template').selectOption('');
  await expect(modelSwitch()).toBeChecked();
  await expect(modelSwitch()).toBeEnabled();
  await page.locator('#save-permissions').click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await expect(routeButton).toContainText('Custom');

  // Delete the template.
  await page.locator('#open-templates').click();
  await templateRow.locator('[data-template-delete]').click();
  await expect(page.locator('#confirm-copy')).toHaveText('No clients use this template.');
  await page.locator('#confirm-action').click();
  await expect(templateRow).toHaveCount(0);
});
