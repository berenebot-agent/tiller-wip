const { test, expect } = require('@playwright/test');
const {
  ADMIN_USER, ADMIN_PASS, MOCK_BASE, MOCK_CONTROL_BASE,
  login, adminCsrf, createProvider, createClient,
  mockAddModel, mockRemoveModel, refreshProviderApi
} = require('./helpers');

test('admin login, responsive navigation, one-time secret, and system view', async ({ page }) => {
  await page.setViewportSize({ width: 780, height: 700 });
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Tiller Router' })).toBeVisible();
  await expect(page.locator('link[rel="icon"]')).toHaveAttribute('href', '/media/tiller-favicon.svg');
  await expect(page.locator('.login-mark')).toBeVisible();
  await page.getByLabel('Administrator').fill(process.env.TILLER_BROWSER_ADMIN_USERNAME || 'admin');
  await page.getByLabel('Password').fill(process.env.TILLER_BROWSER_ADMIN_PASSWORD || 'browser-test-password');
  await page.getByRole('button', { name: 'Enter control panel' }).click();
  await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();
  await expect(page.locator('.brand-mark')).toBeVisible();

  await page.getByRole('button', { name: '+ Add client' }).click();
  await page.getByLabel('Client name').fill('Container browser client');
  await page.getByLabel('Description').fill('Disposable Playwright workflow');
  await page.locator('select[name="type"]').selectOption('catalogue');
  await page.getByRole('button', { name: 'Create & show key' }).click();

  const secret = page.locator('#secret-value');
  await expect(secret).toHaveText(/^sk-tr-[A-Za-z0-9_-]{12}\.[A-Za-z0-9_-]{43}$/);
  const expectedSecret = (await secret.textContent()).trim();
  // 127.0.0.1 is a secure-context loopback origin, so the native Copy button
  // must be visible (not hidden) and must actually land the secret on the
  // OS clipboard — not just claim it.
  await expect(page.locator('#copy-secret')).toBeVisible();
  await page.getByRole('button', { name: 'Copy' }).click();
  await expect(page.locator('#copy-state')).toHaveText('Copied to clipboard.');
  await expect.poll(
    () => page.evaluate(() => navigator.clipboard.readText()),
    { timeout: 3000 }
  ).toBe(expectedSecret);
  await page.getByRole('button', { name: 'I have stored the key' }).click();
  await expect(secret).toHaveText('');
  await expect(page.locator('#clients-body tr.group-row')).toHaveCount(1);
  await expect(page.locator('#clients-body tr.group-toggle')).toHaveCount(1);

  await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await expect(page.locator('#top-status')).toHaveText('READY');
  await expect(page.locator('.backup-warning')).toContainText('recoverable provider API credentials');
  await expect(page.locator('#fallback-form input[name="fallback_timeout_seconds"]')).toHaveValue('60');
  await expect(page.locator('#fallback-form')).toContainText('at least 60 seconds');

  await page.setViewportSize({ width: 1440, height: 900 });
  await expect(page.getByRole('button', { name: 'Toggle navigation' })).toBeHidden();
  expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
});

test('insecure origin (plain HTTP): one-time-secret hides the Copy button and selects the key', async ({ page }) => {
  // Over plain HTTP on a LAN IP the browser will not honour a silent clipboard
  // write, so the UI must NOT claim "Copied". Instead it must hide the Copy
  // button, highlight the key for the user's own Ctrl/Cmd+C gesture, and say
  // so. Model that insecure context here by faking isSecureContext=false and
  // removing the clipboard API before any page script runs.
  await page.addInitScript(() => {
    Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true });
    try { Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true }); } catch {}
  });
  await page.setViewportSize({ width: 1280, height: 800 });
  await login(page);
  await page.getByRole('button', { name: '+ Add client' }).click();
  await page.getByLabel('Client name').fill('Insecure copy client');
  await page.getByLabel('Description').fill('Confirms the Copy button is hidden on plain HTTP');
  await page.locator('select[name="type"]').selectOption('catalogue');
  await page.getByRole('button', { name: 'Create & show key' }).click();

  const secret = page.locator('#secret-value');
  await expect(secret).toHaveText(/^sk-tr-[A-Za-z0-9_-]{12}\.[A-Za-z0-9_-]{43}$/);
  // Copy button must be hidden (it cannot work on plain HTTP)…
  await expect(page.locator('#copy-secret')).toBeHidden();
  // …and the state must instruct a manual Ctrl/Cmd+C rather than claim a copy.
  await expect(page.locator('#copy-state')).toHaveText('Key selected — press Ctrl/Cmd+C to copy it.');
  await page.getByRole('button', { name: 'I have stored the key' }).click();
});

test('mobile: Client Keys renders as cards with expandable detail and working actions', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'mobile-cards';
  const clientName = 'mobile-cards-client';
  await createProvider(page, csrf, providerName);
  await createClient(page, csrf, clientName);
  // Reload so the freshly created client appears in the (default) Client Keys view.
  await page.reload();

  // Lands on Client Keys by default.
  await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();
  // Card list is shown; the wide table is hidden on mobile.
  await expect(page.locator('#clients-cards')).toBeVisible();
  await expect(page.locator('.clients-table-shell')).toBeHidden();

  const card = page.locator('.client-card', { hasText: clientName });
  await expect(card).toBeVisible();
  await expect(card.locator('.client-card-name')).toHaveText(clientName);
  await expect(card.locator('.client-card-detail')).toBeHidden();

  // Tap the card to expand the in-place detail.
  await card.locator('.client-card-head').click();
  const detail = card.locator('.client-card-detail');
  await expect(detail).toBeVisible();
  // Vertical fields are present.
  for (const label of ['Description', 'Fingerprint', 'Created', 'Type', 'Route', 'Usage']) {
    await expect(detail.getByText(label)).toBeVisible();
  }
  // Actions are present.
  for (const action of ['Activity', 'Rotate', 'Settings', 'Delete']) {
    await expect(detail.getByRole('button', { name: action })).toBeVisible();
  }

  // Tapping an action opens the dialog.
  await detail.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();

  // A Catalogue key's card opens catalogue permissions; the permissions dialog
  // renders with its filter cleared.
  await detail.getByRole('button', { name: 'Catalogue permissions' }).click();
  await expect(page.locator('#permissions-dialog')).toBeVisible();
  await expect(page.locator('#permission-search')).toHaveValue('');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
});

test('mobile: Single-key card route summary and Settings entry point', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'mobile-single';
  const clientName = 'mobile-single-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  const real = (await modelsResponse.json()).data.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'real', single_target_id: real.id }
  });
  expect(createRes.status()).toBe(201);
  const secret = (await createRes.json()).secret;

  // Create the alternative virtual target BEFORE opening the quick picker: the
  // combobox options are built from state when the dialog mounts. The model
  // name is deliberately unique so it can't match other tests' searches.
  const models = (await modelsResponse.json()).data;
  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'zzz-msf-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'msf-alt', target_provider_id: provider.id, target_model_id: models.find(model => model.upstream_model_id === 'mock-model')?.id || models[0].id } });
  expect(virtualResponse.status()).toBe(201);

  await page.reload();
  const card = page.locator('.client-card', { hasText: clientName });
  await expect(card).toBeVisible();

  // The collapsed card and expanded detail show the current route target.
  await card.locator('.client-card-head').click();
  const detail = card.locator('.client-card-detail');
  await expect(detail).toBeVisible();
  await expect(detail.locator('.client-card-route')).toContainText(`${providerName}/mock-model`);
  // The route is available, so no broken-target badge.
  await expect(detail.locator('.client-route-broken')).toHaveCount(0);

  // "Change route" opens the target-only quick picker — not the full client
  // Settings dialog. On mobile it renders as a bottom sheet.
  await detail.getByRole('button', { name: 'Change route' }).click();
  const dialog = page.locator('#form-dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog).toHaveClass(/route-picker-dialog/);
  await expect(page.locator('#dialog-title')).toHaveText(`Route · ${clientName}`);
  // Only the target field: no client-name, model-name, or type fields.
  await expect(dialog.locator('[name="name"]')).toHaveCount(0);
  await expect(dialog.locator('[name="single_model_name"]')).toHaveCount(0);
  await expect(dialog.locator('[name="type"]')).toHaveCount(0);
  const routeInput = dialog.locator('[data-single-target] input[type="text"]');
  // Starts empty and focused so the user can type ahead from the first
  // keystroke, like the desktop inline route box.
  await expect(routeInput).toHaveValue('');
  await expect(routeInput).toBeFocused();

  // Click opens the full option list; pick the virtual target created earlier.
  await routeInput.click();
  await page.getByRole('option', { name: 'Virtual · zzz-msf-vg/msf-alt' }).click();
  await expect(routeInput).toHaveValue('Virtual · zzz-msf-vg/msf-alt');
  await dialog.getByRole('button', { name: 'Apply route' }).click();
  await expect(dialog).toBeHidden();

  // The card re-renders with the new target; the server binding actually changed.
  const refreshedCard = page.locator('.client-card', { hasText: clientName });
  // The accordion restarts collapsed after the re-render, so expand it again.
  await refreshedCard.locator('.client-card-head').click();
  const refreshedDetail = refreshedCard.locator('.client-card-detail');
  await expect(refreshedDetail).toBeVisible();
  await expect(refreshedDetail.locator('.client-card-route')).toContainText('zzz-msf-vg/msf-alt');
  const catalogueResponse = await page.request.get('/v1/models', { headers: { Authorization: `Bearer ${secret}` } });
  expect(catalogueResponse.status()).toBe(200);
  expect((await catalogueResponse.json()).data.map(model => model.id)).toEqual(['main']);

  // "Settings" in the actions row still opens the full client Settings dialog.
  await refreshedDetail.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.locator('#form-dialog').getByLabel('Client-facing model name')).toHaveValue('main');
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page.locator('#form-dialog')).toBeHidden();
});

test('Real Models expands large provider groups in cancellable batches', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'progressive-models';
  const totalRows = 95;
  await createProvider(page, csrf, providerName);

  await page.getByRole('link', { name: 'Real Models' }).click();
  const header = page.locator(`[data-group-toggle="models"][data-group-key="${providerName}"]`);
  await expect(header).toBeVisible();

  // Build a large group in the rendered catalogue without adding dozens of
  // upstream fixtures. The cloned rows exercise the real collapse handler.
  await page.evaluate(({ providerName, totalRows }) => {
    const groupHeader = document.querySelector(`[data-group-toggle="models"][data-group-key="${CSS.escape(providerName)}"]`);
    const source = groupHeader.nextElementSibling;
    let insertionPoint = source.nextElementSibling;
    while (insertionPoint && !insertionPoint.classList.contains('group-toggle')) insertionPoint = insertionPoint.nextElementSibling;
    for (let index = 1; index < totalRows; index += 1) {
      const clone = source.cloneNode(true);
      clone.querySelector('.model-id').textContent = `${providerName}/synthetic-${index}`;
      groupHeader.parentElement.insertBefore(clone, insertionPoint);
    }
  }, { providerName, totalRows });

  const groupState = () => page.evaluate(providerName => {
    const groupHeader = document.querySelector(`[data-group-toggle="models"][data-group-key="${CSS.escape(providerName)}"]`);
    const rows = [];
    let row = groupHeader.nextElementSibling;
    while (row && !row.classList.contains('group-toggle')) { rows.push(row); row = row.nextElementSibling; }
    return {
      expanded: groupHeader.getAttribute('aria-expanded'),
      visible: rows.filter(item => !item.classList.contains('group-row-hidden')).length,
      hidden: rows.filter(item => item.classList.contains('group-row-hidden')).length
    };
  }, providerName);

  expect(await page.evaluate(() => getComputedStyle(document.querySelector('.model-table')).tableLayout)).toBe('auto');

  // Collapse is immediate, and expanding reveals only the first 20 rows in
  // the click turn before scheduling the remaining frames.
  expect(await page.evaluate(providerName => {
    document.querySelector(`[data-group-toggle="models"][data-group-key="${CSS.escape(providerName)}"]`).click();
    return true;
  }, providerName)).toBe(true);
  await expect.poll(groupState).toEqual({ expanded: 'false', visible: 0, hidden: totalRows });

  const immediateExpansion = await page.evaluate(providerName => {
    const groupHeader = document.querySelector(`[data-group-toggle="models"][data-group-key="${CSS.escape(providerName)}"]`);
    groupHeader.click();
    const rows = [];
    let row = groupHeader.nextElementSibling;
    while (row && !row.classList.contains('group-toggle')) { rows.push(row); row = row.nextElementSibling; }
    return {
      expanded: groupHeader.getAttribute('aria-expanded'),
      visible: rows.filter(item => !item.classList.contains('group-row-hidden')).length
    };
  }, providerName);
  expect(immediateExpansion).toEqual({ expanded: 'true', visible: 20 });
  await expect.poll(groupState).toEqual({ expanded: 'true', visible: totalRows, hidden: 0 });

  // A second click during expansion must cancel the queued frames rather than
  // allowing a stale callback to reopen part of the group.
  await page.evaluate(providerName => {
    const groupHeader = document.querySelector(`[data-group-toggle="models"][data-group-key="${CSS.escape(providerName)}"]`);
    groupHeader.click(); // collapse the fully expanded group
    groupHeader.click(); // start progressive expansion
    groupHeader.click(); // immediately collapse and cancel it
  }, providerName);
  await page.waitForTimeout(100);
  await expect.poll(groupState).toEqual({ expanded: 'false', visible: 0, hidden: totalRows });

  // A catalogue rerender keeps the provider collapsed and discards old work.
  await page.locator('#show-retired').uncheck();
  await expect(header).toHaveAttribute('aria-expanded', 'false');
  await expect(header.locator('xpath=following-sibling::tr[1]')).toHaveClass(/group-row-hidden/);
});

test('permission edits survive filtering, and cancel/save semantics hold', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'browser-perm';
  const modelId = `${providerName}/mock-model`;
  const clientName = 'browser-perm-client';
  await createProvider(page, csrf, providerName);
  await createClient(page, csrf, clientName);

  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();

  const openPermissions = async () => {
    await clientRow.getByRole('button', { name: `Manage models for ${clientName}` }).click();
    await expect(page.locator('#permissions-dialog')).toBeVisible();
  };
  const modelCheckbox = page.getByLabel(`Enable ${modelId}`);
  const feederCheckbox = page.locator('.permission-group', { hasText: providerName }).locator('input[data-group-id]');

  // 1. Open, toggle a model and the group feeder.
  await openPermissions();
  await expect(modelCheckbox).toBeVisible();
  await modelCheckbox.check();
  await feederCheckbox.check();
  await expect(modelCheckbox).toBeChecked();
  await expect(feederCheckbox).toBeChecked();

  // 2. Filter so the changed model disappears.
  await page.locator('#permission-search').fill('zzz-no-match');
  await expect(modelCheckbox).toBeHidden();

  // 3. Clear the filter; both unsaved changes must remain.
  await page.locator('#permission-search').fill('');
  await expect(modelCheckbox).toBeVisible();
  await expect(modelCheckbox).toBeChecked();
  await expect(feederCheckbox).toBeChecked();

  // 4. Cancel discards; reopening reloads from the API (both back to OFF).
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await openPermissions();
  await expect(modelCheckbox).not.toBeChecked();
  await expect(feederCheckbox).not.toBeChecked();

  // 5. Repeat, save, reopen, and verify persistence.
  await modelCheckbox.check();
  await feederCheckbox.check();
  await page.getByRole('button', { name: 'Save catalogue' }).click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await openPermissions();
  await expect(modelCheckbox).toBeChecked();
  await expect(feederCheckbox).toBeChecked();
});

test('Single key creation, response identity, rename warning, and inline route switching', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'single-ui';
  const clientName = 'single-ui-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'single-ui-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);

  await page.getByRole('link', { name: 'Clients' }).click();
  await page.getByRole('button', { name: '+ Add client' }).click();
  await page.getByLabel('Client name').fill(clientName);
  await page.locator('#form-dialog select[name="type"]').selectOption('single');
  await expect(page.locator('#form-dialog').getByLabel('Client-facing model name')).toHaveValue('main');
  const createTarget = page.locator('[data-single-target] input[type="text"]');
  await createTarget.click();
  await page.getByRole('option', { name: `Real · ${providerName}/mock-model` }).click();
  await expect(page.locator('[name="single_target"]')).toHaveValue(/^real:/);
  await page.getByRole('button', { name: 'Create & show key' }).click();
  await page.waitForTimeout(300);
  expect(await page.locator('#dialog-error').textContent()).toBe('');
  const secretValue = page.locator('#secret-value');
  await expect(secretValue).toHaveText(/^sk-tr-/);
  const secret = await secretValue.textContent();
  await page.getByRole('button', { name: 'I have stored the key' }).click();

  const row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row.locator('[data-inline-route] input[type="text"]')).toHaveValue(`Real · ${providerName}/mock-model`);
  const response = await page.request.post('/v1/chat/completions', { headers: { Authorization: `Bearer ${secret}` }, data: { model: 'ignored-typo', messages: [] } });
  expect(response.status()).toBe(200);
  expect((await response.json()).model).toBe('main');

  await row.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.locator('#form-dialog').getByLabel('Client-facing model name').fill('coding');
  await expect(page.locator('[data-single-confirm]')).toBeVisible();
  await page.getByRole('button', { name: 'Save client' }).click();
  await expect(page.locator('#dialog-error')).toContainText('Confirm the breaking change');
  await page.locator('[name="confirm_model_name_change"]').check();
  await page.getByRole('button', { name: 'Save client' }).click();
  await expect(page.locator('#form-dialog')).toBeHidden();

  const refreshedRow = page.locator('#clients-body tr', { hasText: clientName });
  const inlineRoute = refreshedRow.locator('[data-inline-route] input[type="text"]');
  await inlineRoute.click();
  await page.getByRole('option', { name: 'Virtual · single-ui-vg/coding' }).click();
  await expect(inlineRoute).toHaveValue('Virtual · single-ui-vg/coding');
  await refreshedRow.getByRole('button', { name: 'Apply new route' }).click();
  await expect(refreshedRow.locator('[data-inline-route] input[type="text"]')).toHaveValue('Virtual · single-ui-vg/coding');
  const catalogueResponse = await page.request.get('/v1/models', { headers: { Authorization: `Bearer ${secret}` } });
  expect(catalogueResponse.status()).toBe(200);
  expect((await catalogueResponse.json()).data.map(model => model.id)).toEqual(['coding']);

  // Cleanup: remove this client's logged request so later global-activity
  // pagination tests see a clean baseline.
  const listRes = await page.request.get(`/api/admin/client-keys?search=${clientName}`);
  const found = (await listRes.json()).data.find(c => c.name === clientName);
  if (found) await page.request.delete(`/api/admin/client-keys/${found.id}/activity`, { headers: { 'X-CSRF-Token': csrf } });
});

test('single-route inline picker: typeahead selects and tick applies', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'enter-save';
  const clientName = 'enter-save-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'enter-save-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);

  // Create a single client bound to the real model.
  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'real', single_target_id: real.id }
  });
  expect(createRes.status()).toBe(201);

  await page.getByRole('link', { name: 'Clients' }).click();
  const row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row).toBeVisible();

  // The inline route field shows the current real target.
  const inlineRoute = row.locator('[data-inline-route] input[type="text"]');
  await expect(inlineRoute).toHaveValue(`Real · ${providerName}/mock-model`);

  // Clicking focuses and blanks the pre-filled label so the first keystroke filters.
  await inlineRoute.click();
  await expect(inlineRoute).toHaveValue('');

  // Type to filter from the first character, press Enter to select the virtual model.
  await inlineRoute.pressSequentially('coding');
  await inlineRoute.press('Enter');
  await expect(inlineRoute).toHaveValue('Virtual · enter-save-vg/coding');

  // Apply via the green tick; the row re-renders to the new target.
  await row.getByRole('button', { name: 'Apply new route' }).click();
  await expect(row.locator('[data-inline-route] input[type="text"]')).toHaveValue('Virtual · enter-save-vg/coding');
});

test('single-route inline picker: red X cancels without saving', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'cancel-route';
  const clientName = 'cancel-route-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'cancel-route-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);

  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'real', single_target_id: real.id }
  });
  expect(createRes.status()).toBe(201);
  const secret = (await createRes.json()).secret;

  await page.getByRole('link', { name: 'Clients' }).click();
  const row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row).toBeVisible();
  const inlineRoute = row.locator('[data-inline-route] input[type="text"]');
  await expect(inlineRoute).toHaveValue(`Real · ${providerName}/mock-model`);

  // Select a new target, then cancel.
  await inlineRoute.click();
  await page.getByRole('option', { name: 'Virtual · cancel-route-vg/coding' }).click();
  await expect(inlineRoute).toHaveValue('Virtual · cancel-route-vg/coding');
  await row.getByRole('button', { name: 'Cancel route change' }).click();
  await expect(inlineRoute).toHaveValue(`Real · ${providerName}/mock-model`);

  // Verify the route was NOT changed via the API.
  const modelsRes = await page.request.get('/v1/models', { headers: { Authorization: `Bearer ${secret}` } });
  expect(modelsRes.status()).toBe(200);
  expect((await modelsRes.json()).data.map(m => m.id)).toEqual(['main']);
});

test('single-route inline picker: clicking away without selecting reverts to the saved route', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'blur-revert';
  const clientName = 'blur-revert-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsRes = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsRes.ok()).toBeTruthy();
  const real = (await modelsRes.json()).data.find(m => m.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'real', single_target_id: real.id }
  });
  expect(createRes.status()).toBe(201);

  await page.getByRole('link', { name: 'Clients' }).click();
  const row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row).toBeVisible();
  const inlineRoute = row.locator('[data-inline-route] input[type="text"]');
  await expect(inlineRoute).toHaveValue(`Real · ${providerName}/mock-model`);

  // Clicking focuses and blanks the field.
  await inlineRoute.click();
  await expect(inlineRoute).toHaveValue('');

  // Clicking away without selecting a model reverts to the saved route.
  await page.locator('#client-search').click();
  await expect(inlineRoute).toHaveValue(`Real · ${providerName}/mock-model`);
});

test('Settings dialog switches a client between catalogue and single', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'settings-switch';
  const clientName = 'settings-switch-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  // Start with a catalogue client.
  const catRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'catalogue' }
  });
  expect(catRes.status()).toBe(201);

  await page.getByRole('link', { name: 'Clients' }).click();
  let row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row).toBeVisible();
  await expect(row.getByRole('button', { name: `Manage models for ${clientName}` })).toBeVisible();

  // Switch catalogue -> single via Settings and pick a target.
  await row.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.locator('#form-dialog')).toBeVisible();
  await page.locator('#form-dialog select[name="type"]').selectOption('single');
  await page.locator('[data-single-target] input[type="text"]').click();
  await page.getByRole('option', { name: `Real · ${providerName}/mock-model` }).click();
  await page.getByRole('button', { name: 'Save client' }).click();
  await expect(page.locator('#form-dialog')).toBeHidden();

  // The row now shows the inline Single route picker.
  row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row.locator('[data-inline-route] input[type="text"]')).toHaveValue(`Real · ${providerName}/mock-model`);

  // Switch single -> catalogue via Settings.
  await row.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.locator('#form-dialog')).toBeVisible();
  await page.locator('#form-dialog select[name="type"]').selectOption('catalogue');
  await page.getByRole('button', { name: 'Save client' }).click();
  await expect(page.locator('#form-dialog')).toBeHidden();

  // The row returns to the Manage models (catalogue) button.
  row = page.locator('#clients-body tr', { hasText: clientName });
  await expect(row.getByRole('button', { name: `Manage models for ${clientName}` })).toBeVisible();
  await expect(row.locator('[data-inline-route]')).toHaveCount(0);
});

test('permission bulk enable/disable applies only to current available models', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'bulk-perm';
  const clientName = 'bulk-perm-client';
  const canonical = id => `${providerName}/${id}`;

  const provider = await createProvider(page, csrf, providerName);
  await createClient(page, csrf, clientName);

  // Grow the mock upstream catalogue, then retire one model, so the provider
  // ends up with: mock-model (available), bulk-extra (available),
  // bulk-retired (retired/unavailable).
  await mockAddModel(page, 'bulk-extra');
  await mockAddModel(page, 'bulk-retired');
  await refreshProviderApi(page, csrf, provider.id);
  await mockRemoveModel(page, 'bulk-retired');
  await refreshProviderApi(page, csrf, provider.id);

  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();

  const openPermissions = async () => {
    await clientRow.getByRole('button', { name: `Manage models for ${clientName}` }).click();
    await expect(page.locator('#permissions-dialog')).toBeVisible();
  };
  const model = id => page.getByLabel(`Enable ${canonical(id)}`);
  const feederCheckbox = page.locator('.permission-group', { hasText: providerName }).locator('input[data-group-id]');
  const enableAll = page.locator('#enable-all-permissions');
  const disableAll = page.locator('#disable-all-permissions');
  const search = page.locator('#permission-search');
  const cancel = page.getByRole('button', { name: 'Cancel', exact: true });
  const save = page.getByRole('button', { name: 'Save catalogue' });

  // 1. Enable all AVAILABLE models with no filter; retired and feeder untouched.
  await openPermissions();
  await expect(model('mock-model')).toBeVisible();
  await expect(model('bulk-extra')).toBeVisible();
  await expect(model('bulk-retired')).toBeVisible();
  await expect(feederCheckbox).not.toBeChecked();
  await enableAll.click();
  await expect(model('mock-model')).toBeChecked();
  await expect(model('bulk-extra')).toBeChecked();
  await expect(model('bulk-retired')).not.toBeChecked();
  await expect(feederCheckbox).not.toBeChecked();

  // 2. Per-group "Disable all" acts on the whole group regardless of the filter,
  //    disabling every AVAILABLE model while retired stays untouched.
  await search.fill('bulk-extra');
  await page.locator('.permission-group', { hasText: providerName }).getByRole('button', { name: 'Disable all' }).click();
  await expect(model('bulk-extra')).not.toBeChecked();
  await expect(model('mock-model')).not.toBeChecked();   // available but not matching filter
  await expect(model('bulk-retired')).not.toBeChecked(); // retired and not matching
  await search.fill('');
  await expect(model('bulk-extra')).not.toBeChecked();
  await expect(model('mock-model')).not.toBeChecked();

  // 2b. Per-group "Enable all" re-enables every AVAILABLE model in the group.
  await page.locator('.permission-group', { hasText: providerName }).getByRole('button', { name: 'Enable all' }).click();
  await expect(model('mock-model')).toBeChecked();
  await expect(model('bulk-extra')).toBeChecked();
  await expect(model('bulk-retired')).not.toBeChecked();

  // 3+5. Cancel discards bulk changes; reopening refetches (all back OFF).
  await cancel.click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await openPermissions();
  await expect(model('mock-model')).not.toBeChecked();
  await expect(model('bulk-extra')).not.toBeChecked();
  await expect(model('bulk-retired')).not.toBeChecked();

  // 6. Save persists bulk changes; retired models still untouched.
  await enableAll.click();
  await save.click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await openPermissions();
  await expect(model('mock-model')).toBeChecked();
  await expect(model('bulk-extra')).toBeChecked();
  await expect(model('bulk-retired')).not.toBeChecked();

  // 7. A newly discovered model follows the feeder, not any prior bulk action.
  await feederCheckbox.check();
  await save.click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();
  await mockAddModel(page, 'bulk-new');
  await refreshProviderApi(page, csrf, provider.id);
  await openPermissions();
  await expect(model('bulk-new')).toBeChecked();   // feeder applied at discovery
  await expect(feederCheckbox).toBeChecked();      // feeder persisted

  // Cleanup: remove mock extras so unrelated later providers stay isolated.
  await cancel.click();
  await mockRemoveModel(page, 'bulk-extra');
  await mockRemoveModel(page, 'bulk-new');
});

test('reopening permissions clears the stale filter so bulk actions scope to all models', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'reopen-perm';
  const clientName = 'reopen-perm-client';
  const canonical = id => `${providerName}/${id}`;

  const provider = await createProvider(page, csrf, providerName);
  await createClient(page, csrf, clientName);

  // Two AVAILABLE models so a filter can hide a subset: mock-model and reopen-extra.
  await mockAddModel(page, 'reopen-extra');
  await refreshProviderApi(page, csrf, provider.id);

  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();

  const openPermissions = async () => {
    await clientRow.getByRole('button', { name: `Manage models for ${clientName}` }).click();
    await expect(page.locator('#permissions-dialog')).toBeVisible();
  };
  const model = id => page.getByLabel(`Enable ${canonical(id)}`);
  const search = page.locator('#permission-search');
  const cancel = page.getByRole('button', { name: 'Cancel', exact: true });
  const enableAll = page.locator('#enable-all-permissions');

  // 1. Open, then filter so only one of the two available models shows.
  await openPermissions();
  await expect(model('mock-model')).toBeVisible();
  await expect(model('reopen-extra')).toBeVisible();
  await search.fill('reopen-extra');
  await expect(model('reopen-extra')).toBeVisible();
  await expect(model('mock-model')).toBeHidden();

  // 2. Close without saving, leaving the stale filter in the input.
  await cancel.click();
  await expect(page.locator('#permissions-dialog')).toBeHidden();

  // 3. Reopen: the search field must be cleared and ALL models rendered.
  await openPermissions();
  await expect(search).toHaveValue('');
  await expect(model('mock-model')).toBeVisible();
  await expect(model('reopen-extra')).toBeVisible();

  // 4. "Enable all" must now scope to the whole available set, including
  //    the model that the old filter had hidden.
  await enableAll.click();
  await expect(model('mock-model')).toBeChecked();
  await expect(model('reopen-extra')).toBeChecked();

  // Cleanup: remove the extra mock model so unrelated later providers stay isolated.
  await cancel.click();
  await mockRemoveModel(page, 'reopen-extra');
});

test('Manage models collapse: Real/Virtual sections and provider groups', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'collapse-perm';
  const clientName = 'collapse-perm-client';
  const canonical = id => `${providerName}/${id}`;

  const provider = await createProvider(page, csrf, providerName);
  await createClient(page, csrf, clientName);

  // Create a virtual group + model so both the Real and Virtual sections render.
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();
  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'collapse-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);

  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();

  const openPermissions = async () => {
    await clientRow.getByRole('button', { name: `Manage models for ${clientName}` }).click();
    await expect(page.locator('#permissions-dialog')).toBeVisible();
  };
  const realArrow = page.locator('[data-permission-section="real"]');
  const virtualArrow = page.locator('[data-permission-section="virtual"]');
  const realModel = page.getByLabel(`Enable ${canonical('mock-model')}`);
  const virtualModel = page.getByLabel('Enable collapse-vg/coding');
  const realGroupArrow = page.locator('.permission-group', { hasText: providerName }).locator('.permission-collapse');

  // 1. Both sections render and start expanded.
  await openPermissions();
  await expect(realArrow).toBeVisible();
  await expect(virtualArrow).toBeVisible();
  await expect(realArrow).toHaveAttribute('aria-expanded', 'true');
  await expect(virtualArrow).toHaveAttribute('aria-expanded', 'true');
  await expect(realModel).toBeVisible();
  await expect(virtualModel).toBeVisible();

  // 2. Collapse the Real section: the real model hides, virtual stays.
  await realArrow.click();
  await expect(realArrow).toHaveAttribute('aria-expanded', 'false');
  await expect(realModel).toBeHidden();
  await expect(page.locator('.permission-section-body.permission-section-hidden')).toHaveCount(1);
  await expect(virtualModel).toBeVisible();

  // 3. Expand Real again.
  await realArrow.click();
  await expect(realArrow).toHaveAttribute('aria-expanded', 'true');
  await expect(realModel).toBeVisible();

  // 4. Group collapse arrow hides the group's model list (CSS fix).
  await realGroupArrow.click();
  await expect(realModel).toBeHidden();
  await expect(page.locator('.permission-list.permission-list-hidden')).toHaveCount(1);
  await realGroupArrow.click();
  await expect(realModel).toBeVisible();

  // 5. Collapse the Virtual section.
  await virtualArrow.click();
  await expect(virtualArrow).toHaveAttribute('aria-expanded', 'false');
  await expect(virtualModel).toBeHidden();
});


test('activity loads clear a previously shown error on success', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'browser-errclear';
  const clientName = 'browser-errclear-client';
  await createProvider(page, csrf, providerName);
  await createClient(page, csrf, clientName);

  // 1. Per-client dialog: plant a sentinel error, then a successful re-load
  //    (debounced search) must clear it so no stale error lingers.
  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();
  await clientRow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();

  await page.evaluate(() => { document.querySelector('#activity-error').textContent = 'sentinel-error'; });
  await expect(page.locator('#activity-error')).toHaveText('sentinel-error');

  await page.locator('#activity-search').fill(clientName);
  await expect(page.locator('#activity-error')).toHaveText('');
  await expect(page.locator('#activity-dialog')).toBeVisible();
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();

  // 2. Global activity section: same sentinel-then-success pattern must clear the error.
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await expect(page.locator('#view-settings')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Global activity' })).toBeVisible();

  await page.evaluate(() => { document.querySelector('#global-activity-error').textContent = 'sentinel-error'; });
  await page.locator('#global-activity-search').fill(clientName);
  await expect(page.locator('#global-activity-error')).toHaveText('');
  await page.locator('#global-activity-search').fill('');
});

test('client key group: listing badge, create/edit dialog, and filter', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const defaultClient = 'browser-group-default';
  const groupedClient = 'browser-group-lab';
  const groupName = 'testing';

  // Create one key with an explicit group and one with the default.
  const create = async (name, group) => {
    const res = await page.request.post('/api/admin/client-keys', {
      headers: { 'X-CSRF-Token': csrf },
      data: group ? { name, group } : { name }
    });
    expect(res.status()).toBe(201);
  };
  await create(defaultClient, null);
  await create(groupedClient, groupName);

  await page.locator('#nav-links').getByRole('link', { name: 'Clients' }).click();
  await expect(page.locator('#view-clients')).toBeVisible();

  // Listing groups keys under collapsible group headings (like Real Models).
  const groupedHeader = page.locator('#clients-body tr.group-toggle .group-label', { hasText: groupName });
  await expect(groupedHeader).toHaveText(groupName);
  const defaultHeader = page.locator('#clients-body tr.group-toggle .group-label', { hasText: 'default' });
  await expect(defaultHeader).toHaveText('default');

  // A group heading toggles its rows open/closed.
  const groupedRow = page.locator('#clients-body tr.group-row', { hasText: groupedClient });
  await expect(groupedRow).toBeVisible();
  await groupedHeader.click();
  await expect(groupedRow).toBeHidden();
  await groupedHeader.click();
  await expect(groupedRow).toBeVisible();

  // Filter by group narrows the visible group headings.
  await page.locator('#client-group-filter').selectOption(groupName);
  await expect(groupedHeader).toBeVisible();
  await expect(defaultHeader).toBeHidden();
  await page.locator('#client-group-filter').selectOption('');
  await expect(defaultHeader).toBeVisible();

  // Edit dialog prefills the group.
  await groupedRow.getByRole('button', { name: 'Settings' }).click();
  await expect(page.locator('#form-dialog')).toBeVisible();
  await expect(page.locator('#form-dialog input[name="group"]')).toHaveValue(groupName);
  await page.locator('#form-dialog').getByRole('button', { name: 'Cancel' }).click();
  await expect(page.locator('#form-dialog')).toBeHidden();

  // Create dialog defaults group to "default".
  await page.getByRole('button', { name: '+ Add client' }).click();
  await expect(page.locator('#form-dialog')).toBeVisible();
  await expect(page.locator('#form-dialog input[name="group"]')).toHaveValue('default');
});

test('activity request ID: click-to-copy on secure origin lands the full ID on the clipboard', async ({ page }) => {
  // Mirrors the secure-context assumption the UI itself makes: 127.0.0.1 is a
  // secure-context loopback origin, so the request-ID cell should render with
  // a click affordance and writing the clipboard should succeed.
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'browser-copy-reqid';
  const clientName = 'browser-copy-reqid-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsRes = await page.request.get('/api/admin/models');
  const realModel = (await modelsRes.json()).data.find(m => m.provider_id === provider.id && m.upstream_model_id === 'mock-model');
  expect(realModel).toBeTruthy();
  const modelId = `${providerName}/mock-model`;
  const client = await createClient(page, csrf, clientName);

  // Grant the client access to the provider's model and fire one request so
  // the activity table has at least one row with a server-assigned
  // client_request_id.
  const grantRes = await page.request.put(`/api/admin/client-keys/${client.id}/permissions`, {
    headers: { 'X-CSRF-Token': csrf },
    data: { defaults: [], permissions: [{ kind: 'real', model_id: realModel.id, enabled: true }] }
  });
  expect(grantRes.status()).toBe(204);
  const completion = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${client.secret}` },
    data: { model: modelId, messages: [{ role: 'user', content: 'copy-reqid' }] }
  });
  expect(completion.status()).toBe(200);

  // Open the per-client Activity dialog.
  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();
  await clientRow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();

  // On a secure context the cell must carry the click affordance and the
  // full request ID in its dataset (the visible text is only the short
  // 8-char prefix).
  const requestCell = page.locator('#activity-body tr .activity-request-id').first();
  await expect(requestCell).toBeVisible();
  const fullId = await requestCell.getAttribute('data-copy-request-id');
  expect(fullId).toBeTruthy();
  expect(fullId.length).toBeGreaterThan(8);
  expect(await requestCell.getAttribute('role')).toBe('button');
  expect(await requestCell.getAttribute('tabindex')).toBe('0');

  // Clear any prior clipboard contents so the read assertion is unambiguous.
  await page.evaluate(() => navigator.clipboard.writeText(''));
  await requestCell.click();
  await expect(requestCell).toHaveText('Copied');
  await expect(requestCell).toHaveClass(/copied/);
  await expect.poll(
    () => page.evaluate(() => navigator.clipboard.readText()),
    { timeout: 3000 }
  ).toBe(fullId);
  // The cell reverts to the short prefix once the "Copied" indicator clears.
  await expect(requestCell).not.toHaveText('Copied', { timeout: 3000 });

  // Keyboard activation must also copy.
  await page.evaluate(() => navigator.clipboard.writeText(''));
  await requestCell.focus();
  await page.keyboard.press('Enter');
  await expect.poll(
    () => page.evaluate(() => navigator.clipboard.readText()),
    { timeout: 3000 }
  ).toBe(fullId);

  // The same affordance must exist in the Global activity table.
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#activity-dialog')).toBeHidden();
  await page.locator('#nav-links').getByRole('link', { name: 'Settings' }).click();
  await expect(page.locator('#view-settings')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Global activity' })).toBeVisible();
  const globalCell = page.locator('#global-activity-body tr .activity-request-id').first();
  await expect(globalCell).toBeVisible();
  const globalId = await globalCell.getAttribute('data-copy-request-id');
  expect(globalId).toBeTruthy();
  expect(globalId.length).toBeGreaterThan(8);
});

test('activity request ID: insecure origin renders a plain tooltip, no click affordance', async ({ page }) => {
  // On a plain HTTP origin navigator.clipboard.writeText is unavailable, so
  // the UI must not pretend the cell is clickable. The hover tooltip still
  // shows the full ID.
  await page.addInitScript(() => {
    Object.defineProperty(window, 'isSecureContext', { value: false, configurable: true });
    try { Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true }); } catch {}
  });
  await page.setViewportSize({ width: 1280, height: 800 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'browser-copy-reqid-insecure';
  const clientName = 'browser-copy-reqid-insecure-client';
  const provider = await createProvider(page, csrf, providerName);
  const modelsRes = await page.request.get('/api/admin/models');
  const realModel = (await modelsRes.json()).data.find(m => m.provider_id === provider.id && m.upstream_model_id === 'mock-model');
  expect(realModel).toBeTruthy();
  const modelId = `${providerName}/mock-model`;
  const client = await createClient(page, csrf, clientName);
  const grantRes = await page.request.put(`/api/admin/client-keys/${client.id}/permissions`, {
    headers: { 'X-CSRF-Token': csrf },
    data: { defaults: [], permissions: [{ kind: 'real', model_id: realModel.id, enabled: true }] }
  });
  expect(grantRes.status()).toBe(204);
  const completion = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${client.secret}` },
    data: { model: modelId, messages: [{ role: 'user', content: 'copy-reqid-insecure' }] }
  });
  expect(completion.status()).toBe(200);

  await page.getByRole('link', { name: 'Clients' }).click();
  const clientRow = page.locator('#clients-body tr', { hasText: clientName });
  await expect(clientRow).toBeVisible();
  await clientRow.getByRole('button', { name: 'Activity' }).click();
  await expect(page.locator('#activity-dialog')).toBeVisible();

  const requestCell = page.locator('#activity-body tr .activity-request-id').first();
  await expect(requestCell).toBeVisible();
  // No click affordance: no data-copy-request-id, no role/tabindex.
  expect(await requestCell.getAttribute('data-copy-request-id')).toBeNull();
  expect(await requestCell.getAttribute('role')).toBeNull();
  expect(await requestCell.getAttribute('tabindex')).toBeNull();
  // The full ID is still in the title for hover-to-reveal.
  const title = await requestCell.getAttribute('title');
  expect(title?.length || 0).toBeGreaterThan(8);
});

test('virtual-model target combobox: unique upstream_model_id match auto-accepts without an explicit pick', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'vm-exact-match';
  const provider = await createProvider(page, csrf, providerName);

  // Add a second upstream model so we can prove exact-match resolves to it,
  // not whatever the combobox pre-selects. Name it so it sorts after
  // 'mock-model' (the mock's only default) — the router's models endpoint
  // orders by upstream_model_id, so 'mock-model' becomes option[0] and the
  // typed match can only land on this second one via the auto-accept.
  const extraModelID = 'zoo-extra';
  await mockAddModel(page, extraModelID);
  await refreshProviderApi(page, csrf, provider.id);

  const modelsRes = await page.request.get('/api/admin/models?all=1');
  const allModels = (await modelsRes.json()).data;
  const extra = allModels.find(m => m.provider_name === providerName && m.upstream_model_id === extraModelID);
  expect(extra).toBeTruthy();
  const mock = allModels.find(m => m.provider_name === providerName && m.upstream_model_id === 'mock-model');
  expect(mock).toBeTruthy();

  // Seed a virtual group so the create dialog has a group to pick.
  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'vm-exact-match-vg' } });
  expect(groupRes.status()).toBe(201);

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await page.getByRole('button', { name: '+ Virtual model' }).click();
  await expect(page.getByRole('heading', { name: 'Create virtual model' })).toBeVisible();

  const fixedInput = page.locator('[data-fixed-target] input[type="text"]');
  const fixedHidden = page.locator('[data-fixed-target] input[type="hidden"]');
  await expect(fixedInput).toBeVisible();

  // The dialog pre-selects the first option in the list. Capture it so we
  // can later prove the exact-match resolution landed on a different one.
  const preSelectedId = await fixedHidden.inputValue();
  expect(preSelectedId).toBeTruthy();
  expect(preSelectedId).not.toBe(extra.id);

  // Clear the visible field, then type the exact upstream_model_id of the
  // second model. The hidden provider_model_id should auto-resolve to it
  // without an explicit dropdown pick — there is only one match.
  await fixedInput.click();
  await fixedInput.press('Control+A');
  await fixedInput.press('Delete');
  await expect(fixedHidden).toHaveValue('');
  await fixedInput.fill(extraModelID);
  await expect(fixedHidden).toHaveValue(extra.id);
  await expect(fixedHidden).not.toHaveValue(preSelectedId);

  // Submitting the create form should succeed; the new virtual model routes
  // to the second model, not the pre-selected first option.
  await page.locator('#entity-form').getByLabel('Virtual model name').fill('exact-match-route');
  await page.locator('#entity-form').getByLabel('Virtual group').selectOption({ label: 'vm-exact-match-vg' });
  // Sanity: the hidden target_model field must still be populated right up
  // until we click Create; the exact-match reconciliation set it on input.
  await expect(fixedHidden).toHaveValue(extra.id);
  await page.getByRole('button', { name: 'Create route' }).click();
  // If the dialog stays open, surface the server error so the failure is
  // self-explanatory in the log.
  await page.locator('#form-dialog').waitFor({ state: 'hidden', timeout: 8000 }).catch(async () => {
    const err = (await page.locator('#dialog-error').textContent()) || '<no error>';
    const hidden = await fixedHidden.inputValue();
    throw new Error(`Create dialog did not close; server error: ${err}; hidden=${hidden}`);
  });

  const listRes = await page.request.get('/api/admin/virtual-models?search=exact-match-route');
  expect(listRes.ok()).toBeTruthy();
  const found = (await listRes.json()).data.find(m => m.canonical_model_id === 'vm-exact-match-vg/exact-match-route');
  expect(found).toBeTruthy();
  expect(found.targets[0].provider_model_id).toBe(extra.id);

  // Cleanup: remove the virtual model and group, plus the extra upstream model.
  await page.request.delete(`/api/admin/virtual-models/${found.id}`, { headers: { 'X-CSRF-Token': csrf } });
  const groupList = await (await page.request.get('/api/admin/virtual-groups?search=vm-exact-match-vg')).json();
  const group = groupList.data.find(g => g.name === 'vm-exact-match-vg');
  if (group) await page.request.delete(`/api/admin/virtual-groups/${group.id}`, { headers: { 'X-CSRF-Token': csrf } });
  await mockRemoveModel(page, extraModelID);
  await refreshProviderApi(page, csrf, provider.id);
});

test('virtual-model target combobox: ambiguous upstream_model_id match does not auto-accept', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  // Create two providers that both expose the same upstream_model_id. This
  // is the common case (e.g. openai/gpt-x and openrouter/gpt-x). Typing
  // the shared ID must NOT silently pick one — the operator must choose.
  const sharedModelID = 'gpt-shared';
  const providerA = await createProvider(page, csrf, 'vm-ambiguous-a');
  const providerB = await createProvider(page, csrf, 'vm-ambiguous-b');
  await mockAddModel(page, sharedModelID);
  await refreshProviderApi(page, csrf, providerA.id);
  await refreshProviderApi(page, csrf, providerB.id);

  const modelsRes = await page.request.get('/api/admin/models?all=1');
  const allModels = (await modelsRes.json()).data;
  const matches = allModels.filter(m => m.upstream_model_id === sharedModelID);
  expect(matches.length).toBe(2);

  // Seed a virtual group so the create dialog has a group to pick.
  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'vm-ambiguous-vg' } });
  expect(groupRes.status()).toBe(201);

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await page.getByRole('button', { name: '+ Virtual model' }).click();
  await expect(page.getByRole('heading', { name: 'Create virtual model' })).toBeVisible();

  const fixedInput = page.locator('[data-fixed-target] input[type="text"]');
  const fixedHidden = page.locator('[data-fixed-target] input[type="hidden"]');
  await expect(fixedInput).toBeVisible();

  // Clear the field and type the shared upstream_model_id. The hidden value
  // must stay empty because there are two matches — auto-accept only fires
  // for a unique match.
  await fixedInput.click();
  await fixedInput.press('Control+A');
  await fixedInput.press('Delete');
  await expect(fixedHidden).toHaveValue('');
  await fixedInput.fill(sharedModelID);
  await expect(fixedHidden).toHaveValue('');

  // The dropdown should show both provider choices so the operator can pick.
  const listItems = page.locator('[data-fixed-target] .combobox-list li');
  await expect(listItems).toHaveCount(2);
  await expect(listItems.nth(0)).toContainText('vm-ambiguous-a');
  await expect(listItems.nth(1)).toContainText('vm-ambiguous-b');

  // Cleanup: remove the group and extra model.
  await page.request.delete(`/api/admin/providers/${providerA.id}`, { headers: { 'X-CSRF-Token': csrf } });
  await page.request.delete(`/api/admin/providers/${providerB.id}`, { headers: { 'X-CSRF-Token': csrf } });
  const groupList = await (await page.request.get('/api/admin/virtual-groups?search=vm-ambiguous-vg')).json();
  const group = groupList.data.find(g => g.name === 'vm-ambiguous-vg');
  if (group) await page.request.delete(`/api/admin/virtual-groups/${group.id}`, { headers: { 'X-CSRF-Token': csrf } });
  await mockRemoveModel(page, sharedModelID);
});

test('virtual-model target combobox: non-matching text still blocks submission', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'vm-nomatch';
  const provider = await createProvider(page, csrf, providerName);

  // Seed a virtual group so the create dialog has a group to pick.
  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'vm-nomatch-vg' } });
  expect(groupRes.status()).toBe(201);

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await page.getByRole('button', { name: '+ Virtual model' }).click();
  await expect(page.getByRole('heading', { name: 'Create virtual model' })).toBeVisible();

  const fixedInput = page.locator('[data-fixed-target] input[type="text"]');
  const fixedHidden = page.locator('[data-fixed-target] input[type="hidden"]');
  await expect(fixedInput).toBeVisible();

  // Clear the field, then type a string that doesn't match any upstream
  // model. Hidden value must stay empty and submit must error.
  await fixedInput.click();
  await fixedInput.press('Control+A');
  await fixedInput.press('Delete');
  await expect(fixedHidden).toHaveValue('');
  await fixedInput.fill('not-a-real-model-xyz');
  await expect(fixedHidden).toHaveValue('');

  await page.locator('#entity-form').getByLabel('Virtual model name').fill('no-match-route');
  await page.getByRole('button', { name: 'Create route' }).click();
  await expect(page.locator('#dialog-error')).toContainText('Choose a target model.');
  await expect(page.locator('#form-dialog')).toBeVisible();

  // Cancel and clean up the group so we leave the suite tidy.
  await page.getByRole('button', { name: 'Cancel' }).click();
  const groupList = await (await page.request.get('/api/admin/virtual-groups?search=vm-nomatch-vg')).json();
  const group = groupList.data.find(g => g.name === 'vm-nomatch-vg');
  if (group) await page.request.delete(`/api/admin/virtual-groups/${group.id}`, { headers: { 'X-CSRF-Token': csrf } });
});

test('mobile: ordered-fallback target dropdown spans the dialog width', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'vm-mobile-list';
  await createProvider(page, csrf, providerName);
  const modelsRes = await page.request.get('/api/admin/models?all=1');
  const real = (await modelsRes.json()).data.find(m => m.provider_name === providerName && m.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'vm-mobile-list-vg' } });
  expect(groupRes.status()).toBe(201);
  const group = await groupRes.json();
  const virtualRes = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'mobile-fb', routing_mode: 'ordered_fallback', targets: [{ provider_model_id: real.id, enabled: true }] } });
  expect(virtualRes.status()).toBe(201);
  const virtual = await virtualRes.json();

  await page.goto('/#virtual');
  await expect(page.locator('#view-virtual')).toBeVisible();
  const row = page.locator('#virtual-body tr', { hasText: 'vm-mobile-list-vg/mobile-fb' });
  await row.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Edit vm-mobile-list-vg/mobile-fb' })).toBeVisible();

  const input = page.locator('[data-fallback-targets] .target-row .combobox input[type="text"]');
  await input.click();
  const list = page.locator('[data-fallback-targets] .combobox-list');
  await expect(list).toBeVisible();

  const m = await page.evaluate(() => {
    const l = document.querySelector('[data-fallback-targets] .combobox-list').getBoundingClientRect();
    const i = document.querySelector('[data-fallback-targets] .combobox input[type="text"]').getBoundingClientRect();
    const d = document.querySelector('#form-dialog').getBoundingClientRect();
    return { listW: l.width, listRight: l.right, inputW: i.width, dialogW: d.width, vw: window.innerWidth };
  });
  expect(m.dialogW).toBeGreaterThan(m.inputW + 40);
  expect(m.listW).toBeGreaterThanOrEqual(m.dialogW - 2);
  expect(m.listW).toBeGreaterThan(m.inputW + 40);
  expect(m.listRight).toBeLessThanOrEqual(m.vw);

  await input.click();
  await expect(list).toBeHidden();
  await page.getByRole('button', { name: 'Cancel' }).click();
  await page.request.delete(`/api/admin/virtual-models/${virtual.id}`, { headers: { 'X-CSRF-Token': csrf } });
  await page.request.delete(`/api/admin/virtual-groups/${group.id}`, { headers: { 'X-CSRF-Token': csrf } });
});

test('virtual models table: group header colspan matches data row columns', async ({ page }) => {
  // The State column was removed, leaving 7 columns. The group header
  // colspan must match the data row cell count so the table does not get
  // an extra synthetic column.
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);
  const providerName = 'vm-colspan';
  await createProvider(page, csrf, providerName);

  const groupRes = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'vm-colspan-vg' } });
  expect(groupRes.status()).toBe(201);
  const group = await groupRes.json();
  const modelsRes = await page.request.get('/api/admin/models?all=1');
  const real = (await modelsRes.json()).data.find(m => m.provider_name === providerName && m.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();
  const virtualRes = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'colspan-route', routing_mode: 'fixed', targets: [{ provider_model_id: real.id, enabled: true }] } });
  expect(virtualRes.status()).toBe(201);
  const virtual = await virtualRes.json();

  await page.goto('/#virtual');
  await expect(page.locator('#view-virtual')).toBeVisible();

  // The group header colspan must equal the data row cell count (7).
  const headerColspan = await page.locator('#virtual-body tr.group-toggle td').first().getAttribute('colspan');
  expect(headerColspan).toBe('7');

  // And the data row must have exactly 7 cells.
  const dataRow = page.locator('#virtual-body tr[data-virtual-id]', { hasText: 'vm-colspan-vg/colspan-route' });
  await expect(dataRow).toBeVisible();
  const cellCount = await dataRow.locator('td').count();
  expect(cellCount).toBe(7);

  // Cleanup.
  await page.request.delete(`/api/admin/virtual-models/${virtual.id}`, { headers: { 'X-CSRF-Token': csrf } });
  await page.request.delete(`/api/admin/virtual-groups/${group.id}`, { headers: { 'X-CSRF-Token': csrf } });
});
