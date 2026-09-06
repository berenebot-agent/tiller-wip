const { test, expect } = require('@playwright/test');
const { login } = require('./helpers');

const response = (data) => ({ data, limit: 200, offset: 0 });

function mockCatalogue(page, { models = [], virtualModels = [], groups = [], providers = [] }) {
  return page.route('**/api/admin/**', async route => {
    const url = new URL(route.request().url());
    const body = url.pathname === '/api/admin/models' ? response(models)
      : url.pathname === '/api/admin/virtual-models' ? response(virtualModels)
        : url.pathname === '/api/admin/virtual-groups' ? response(groups)
          : url.pathname === '/api/admin/providers' ? response(providers)
            : url.pathname === '/api/admin/usage' ? { target_last_outcome: {}, target_health: {} }
              : null;
    if (body) return route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    return route.continue();
  });
}

const baseModel = (overrides = {}) => ({
  id: 'real-capability', provider_id: 'provider-capability', provider_name: 'forge',
  upstream_model_id: 'reasoner-v1', canonical_model_id: 'forge/reasoner-v1',
  context_length: 131072, max_output_tokens: 16384, native_protocol: 'chat',
  supports_tools: true, supports_vision: false, supports_reasoning: true,
  supports_structured_output: true, input_modalities: ['text', 'image'], output_modalities: ['text'],
  available: true, first_seen_at: '2026-01-01T00:00:00Z',
  ...overrides
});

test('real capabilities dialog renders normalized reasoning metadata and tri-state states', async ({ page }) => {
  const dialog = await openRealFixture(page, baseModel({ reasoning_capabilities: {
    options: [
      { type: 'effort', values: [] },
      { type: 'toggle' },
      { type: 'budget_tokens', min: 256, max: 8192 }
    ],
    thinking_modes: ['adaptive', 'enabled'], default_effort: 'medium',
    mandatory: true, default_enabled: false, parameters: ['reasoning_effort', 'thinking']
  } }));
  await expect(dialog).toHaveAttribute('aria-labelledby', 'capabilities-title');
  await expect(dialog).toContainText('Effort values');
  await expect(dialog).toContainText('Any value');
  await expect(dialog).toContainText('Supported');
  await expect(dialog).toContainText('Toggle support');
  await expect(dialog).toContainText('Minimum 256');
  await expect(dialog).toContainText('Maximum 8,192');
  await expect(dialog).toContainText('adaptive');
  await expect(dialog).toContainText('Default effort');
  await expect(dialog).toContainText('medium');
  await expect(dialog).toContainText('Mandatory');
  await expect(dialog).toContainText('Default enabled');
  await expect(dialog).toContainText('Accepted parameter names');
  await expect(dialog).toContainText('reasoning_effort');
});

async function openRealFixture(page, model) {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.addInitScript(() => { window.EventSource = class { addEventListener() {} close() {} }; });
  await login(page);
  await mockCatalogue(page, { providers: [{ id: 'provider-capability', name: 'forge', enabled: true }], models: [model] });
  await page.getByRole('link', { name: 'Real Models' }).click();
  const row = page.locator(`#models-body tr[data-model-id="${model.id}"]`);
  await expect(row.locator(`[data-model-capabilities="${model.id}"]`)).toBeVisible();
  await row.locator(`[data-model-capabilities="${model.id}"]`).click();
  await expect(page.locator('#capabilities-title')).toHaveText(`${model.canonical_model_id} capabilities`);
  return page.locator('#capabilities-dialog');
}

test('real capabilities distinguishes unknown reasoning metadata', async ({ page }) => {
  const dialog = await openRealFixture(page, baseModel({ id: 'real-unknown', upstream_model_id: 'unknown-v1', canonical_model_id: 'forge/unknown-v1', reasoning_capabilities: undefined }));
  await expect(dialog.locator('[data-reasoning-state="unknown"]')).toContainText('Not reported');
});

test('real capabilities distinguishes known empty selectors', async ({ page }) => {
  const dialog = await openRealFixture(page, baseModel({ id: 'real-empty', upstream_model_id: 'empty-v1', canonical_model_id: 'forge/empty-v1', reasoning_capabilities: { options: [] } }));
  await expect(dialog.locator('[data-reasoning-state="known"]')).toContainText('No configurable selectors');
  await expect(dialog.locator('[data-reasoning-state="known"]')).toContainText('Toggle support');
  await expect(dialog.locator('[data-reasoning-state="known"]')).toContainText('Not supported');
});

test('thinking modes remain configurable when selector options are empty', async ({ page }) => {
  const dialog = await openRealFixture(page, baseModel({ id: 'real-thinking', upstream_model_id: 'thinking-v1', canonical_model_id: 'forge/thinking-v1', reasoning_capabilities: { options: [], thinking_modes: ['adaptive'] } }));
  await expect(dialog.locator('[data-reasoning-state="known"]')).toContainText('adaptive');
  await expect(dialog.locator('[data-reasoning-state="known"]')).not.toContainText('No configurable selectors');
});

test('virtual capabilities dialog leads with aggregate, preserves target metadata, and wraps on mobile', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  await mockCatalogue(page, {
    providers: [{ id: 'provider-capability', name: 'forge', enabled: true }],
    models: [],
    groups: [{ id: 'group-capability', name: 'routing' }],
    virtualModels: [{
      id: 'virtual-capability', group_id: 'group-capability', group_name: 'routing', name: 'reasoning',
      canonical_model_id: 'routing/reasoning', routing_mode: 'ordered_fallback', available: true,
      context_length: 64000, max_output_tokens: 8192, supports_tools: true, supports_vision: false,
      supports_reasoning: true, supports_structured_output: true,
      reasoning_capabilities: {
        options: [{ type: 'effort', values: ['low', 'high'] }, { type: 'budget_tokens', min: 512, max: 4096 }],
        thinking_modes: ['adaptive'], default_effort: 'low', mandatory: false, default_enabled: true,
        parameters: ['reasoning_effort']
      },
      targets: [
        {
          id: 'target-one', provider_model_id: 'model-one', provider_id: 'provider-capability', provider_name: 'forge', upstream_model_id: 'reasoner-v1',
          native_protocol: 'responses', position: 0, enabled: true, available: true, context_length: 64000, max_output_tokens: 8192,
          supports_tools: true, supports_vision: false, supports_reasoning: true, supports_structured_output: true,
          reasoning_capabilities: { options: [{ type: 'effort', values: ['low', 'medium', 'high'] }], default_effort: 'medium', parameters: ['reasoning_effort'] }
        },
        {
          id: 'target-two', provider_model_id: 'model-two', provider_id: 'provider-capability', provider_name: 'forge', upstream_model_id: 'backup-v1',
          native_protocol: 'chat', position: 1, enabled: false, available: true, warning: 'Target is disabled', context_length: 128000, max_output_tokens: 4096,
          supports_tools: true, supports_vision: true, supports_reasoning: null, supports_structured_output: true, reasoning_capabilities: null
        }
      ]
    }]
  });

  await page.getByRole('button', { name: 'Toggle navigation' }).click();
  await page.getByRole('link', { name: 'Virtual Models' }).click();
  const row = page.locator('tr[data-virtual-id="virtual-capability"]');
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: 'Capabilities' }).click();
  const dialog = page.locator('#capabilities-dialog');
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText('AGGREGATE / ADVERTISED TO HERMES + V1');
  await expect(dialog).toContainText('An individual fallback target may use its provider default when it does not support a selector from the aggregate');
  await expect(dialog).toContainText('EXACT FALLBACK TARGETS');
  await expect(dialog).toContainText('01 · forge/reasoner-v1');
  await expect(dialog).toContainText('02 · forge/backup-v1');
  await expect(dialog).toContainText('eligible');
  await expect(dialog).toContainText('Target is disabled');
  await expect(dialog).toContainText('Not reported');
  const dialogWidth = await dialog.evaluate(element => element.getBoundingClientRect().width);
  const viewportWidth = await page.evaluate(() => window.innerWidth);
  expect(dialogWidth).toBeLessThanOrEqual(viewportWidth);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy();
  await dialog.getByRole('button', { name: 'Done' }).click();
});
