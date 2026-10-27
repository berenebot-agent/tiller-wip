const { test, expect } = require('@playwright/test');
const { openAdmin, adminCsrf, createProvider } = require('./helpers');

// Catalogue templates: create one, link a Catalogue client to it, see the
// client's model list read-only, detach it to Custom, then delete the template.
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

  // Create a Catalogue client linked to the template.
  await page.locator('#add-client').click();
  await page.locator('#form-dialog [name="name"]').fill(clientName);
  await page.locator('#form-dialog [name="type"]').selectOption('catalogue');
  await page.locator('#form-dialog [name="template_id"]').selectOption({ label: templateName });
  await page.locator('#dialog-submit').click();
  await expect(page.locator('#secret-value')).toBeVisible();
  await page.locator('#close-secret').click();
  const clientRow = page.locator('#clients-body tr').filter({ hasText: clientName });
  const routeButton = clientRow.locator('[data-client-models]');
  await expect(routeButton).toContainText(`Template · ${templateName}`);

  // Linked clients show the template's models read-only.
  await routeButton.click();
  await expect(page.locator('#permission-linked')).toBeVisible();
  await expect(page.locator('#permission-linked-copy')).toContainText(templateName);
  await expect(page.locator('#save-permissions')).toBeHidden();
  const linkedModel = page.locator(`#permission-groups .permission-row[data-canonical^="${providerName}/"] input`).first();
  await expect(linkedModel).toBeChecked();
  await expect(linkedModel).toBeDisabled();
  await page.locator('#cancel-permissions').click();

  // Switching to Custom keeps the models and makes them editable.
  await clientRow.locator('[data-client-edit]').click();
  await page.locator('#form-dialog [name="template_id"]').selectOption('');
  await page.locator('#dialog-submit').click();
  await expect(page.locator('#form-dialog')).toBeHidden();
  await expect(routeButton).toContainText('Custom');
  await routeButton.click();
  await expect(page.locator('#permission-linked')).toBeHidden();
  await expect(page.locator('#save-permissions')).toBeVisible();
  const customModel = page.locator(`#permission-groups .permission-row[data-canonical^="${providerName}/"] input`).first();
  await expect(customModel).toBeChecked();
  await expect(customModel).toBeEnabled();
  await page.locator('#cancel-permissions').click();

  // Delete the template.
  await page.locator('#open-templates').click();
  await templateRow.locator('[data-template-delete]').click();
  await expect(page.locator('#confirm-copy')).toHaveText('No clients use this template.');
  await page.locator('#confirm-action').click();
  await expect(templateRow).toHaveCount(0);
});
