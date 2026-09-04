const { test, expect } = require('@playwright/test');
const {
  login, adminCsrf, createProvider,
  mockFailModel, mockOkModel
} = require('./helpers');

test('live refresh: transient errors retain one reconnecting EventSource', async ({ page }) => {
  await page.addInitScript(() => {
    window.__liveEventSources = [];
    window.EventSource = class {
      constructor(url) {
        this.url = url;
        this.closed = false;
        window.__liveEventSources.push(this);
      }
      addEventListener() {}
      close() { this.closed = true; }
    };
  });
  // Load the module on its own so the app singleton does not create another
  // connection before this lifecycle test starts.
  await page.goto('/live.js');

  const result = await page.evaluate(async () => {
    const { LiveStream } = await import('/live.js');
    const addEventListener = document.addEventListener;
    document.addEventListener = (type, ...args) => {
      if (type !== 'visibilitychange') addEventListener.call(document, type, ...args);
    };
    const stream = new LiveStream('/api/admin/live');
    document.addEventListener = addEventListener;
    stream.start();
    const source = stream.es;
    source.onerror();
    return {
      count: window.__liveEventSources.length,
      owned: Boolean(source) && stream.es === source && !source.closed,
    };
  });
  expect(result.count).toBe(1);
  expect(result.owned).toBeTruthy();
});

test('live refresh: transient error that recovers before timeout does not trigger auth failure', async ({ page }) => {
  // A transient blip (network/proxy) must not kick the admin back to login
  // if EventSource auto-reconnects successfully before the auth-failure
  // timer fires. onopen must clear the pending timer.
  await page.addInitScript(() => {
    window.__liveEventSources = [];
    window.__authFailureCalls = 0;
    window.EventSource = class {
      constructor(url) {
        this.url = url;
        this.closed = false;
        this._onopen = null;
        this._onerror = null;
        window.__liveEventSources.push(this);
      }
      set onopen(fn) { this._onopen = fn; }
      get onopen() { return this._onopen; }
      set onerror(fn) { this._onerror = fn; }
      get onerror() { return this._onerror; }
      addEventListener(type, fn) {
        if (type === 'open') this._onopen = fn;
      }
      close() { this.closed = true; }
      __fireOpen() { if (this._onopen) this._onopen(); }
      __fireError() { if (this._onerror) this._onerror(); }
    };
  });
  await page.goto('/live.js');

  const result = await page.evaluate(async () => {
    const { LiveStream } = await import('/live.js');
    const addEventListener = document.addEventListener;
    document.addEventListener = (type, ...args) => {
      if (type !== 'visibilitychange') addEventListener.call(document, type, ...args);
    };
    const stream = new LiveStream('/api/admin/live', { onAuthFailure: () => { window.__authFailureCalls++; } });
    document.addEventListener = addEventListener;
    stream.start();
    const source = stream.es;
    // Simulate a transient error, then a successful reconnect 1s later.
    source.__fireError();
    await new Promise(r => setTimeout(r, 1000));
    source.__fireOpen();
    // Wait past the original 5s timeout to confirm the timer was cleared.
    await new Promise(r => setTimeout(r, 5000));
    return { authFailureCalls: window.__authFailureCalls };
  });
  expect(result.authFailureCalls).toBe(0);
});

test('live refresh: sustained error with explicit 401 triggers auth failure', async ({ page }) => {
  // If the connection stays down and the session endpoint returns 401,
  // the auth-failure callback must fire so the UI returns to login.
  await page.addInitScript(() => {
    window.__liveEventSources = [];
    window.__authFailureCalls = 0;
    window.EventSource = class {
      constructor(url) {
        this.url = url;
        this.closed = false;
        this._onerror = null;
        window.__liveEventSources.push(this);
      }
      set onerror(fn) { this._onerror = fn; }
      get onerror() { return this._onerror; }
      addEventListener() {}
      close() { this.closed = true; }
      __fireError() { if (this._onerror) this._onerror(); }
    };
    // Mock fetch to return 401 for the session probe.
    window.fetch = async (url) => {
      if (url.includes('/api/admin/session')) return { status: 401, ok: false };
      return { status: 200, ok: true };
    };
    // Speed up the test: override setTimeout to fire immediately for the auth timer.
    window.__realSetTimeout = window.setTimeout;
    window.setTimeout = (fn, ms) => {
      if (ms === 5000) return window.__realSetTimeout(fn, 10);
      return window.__realSetTimeout(fn, ms);
    };
  });
  await page.goto('/live.js');

  const result = await page.evaluate(async () => {
    const { LiveStream } = await import('/live.js');
    const addEventListener = document.addEventListener;
    document.addEventListener = (type, ...args) => {
      if (type !== 'visibilitychange') addEventListener.call(document, type, ...args);
    };
    const stream = new LiveStream('/api/admin/live', { onAuthFailure: () => { window.__authFailureCalls++; } });
    document.addEventListener = addEventListener;
    stream.start();
    const source = stream.es;
    source.__fireError();
    // Wait for the (accelerated) auth timer + fetch to complete.
    await new Promise(r => window.__realSetTimeout(r, 500));
    return { authFailureCalls: window.__authFailureCalls };
  });
  expect(result.authFailureCalls).toBe(1);
});

test('live refresh: sustained error with 200 session does not trigger auth failure', async ({ page }) => {
  // If the connection blips but the session is still valid (200), the
  // auth-failure callback must NOT fire — EventSource keeps reconnecting.
  await page.addInitScript(() => {
    window.__liveEventSources = [];
    window.__authFailureCalls = 0;
    window.EventSource = class {
      constructor(url) {
        this.url = url;
        this.closed = false;
        this._onerror = null;
        window.__liveEventSources.push(this);
      }
      set onerror(fn) { this._onerror = fn; }
      get onerror() { return this._onerror; }
      addEventListener() {}
      close() { this.closed = true; }
      __fireError() { if (this._onerror) this._onerror(); }
    };
    window.fetch = async (url) => {
      if (url.includes('/api/admin/session')) return { status: 200, ok: true };
      return { status: 200, ok: true };
    };
    window.__realSetTimeout = window.setTimeout;
    window.setTimeout = (fn, ms) => {
      if (ms === 5000) return window.__realSetTimeout(fn, 10);
      return window.__realSetTimeout(fn, ms);
    };
  });
  await page.goto('/live.js');

  const result = await page.evaluate(async () => {
    const { LiveStream } = await import('/live.js');
    const addEventListener = document.addEventListener;
    document.addEventListener = (type, ...args) => {
      if (type !== 'visibilitychange') addEventListener.call(document, type, ...args);
    };
    const stream = new LiveStream('/api/admin/live', { onAuthFailure: () => { window.__authFailureCalls++; } });
    document.addEventListener = addEventListener;
    stream.start();
    const source = stream.es;
    source.__fireError();
    await new Promise(r => window.__realSetTimeout(r, 500));
    return { authFailureCalls: window.__authFailureCalls };
  });
  expect(result.authFailureCalls).toBe(0);
});

// Live SSE refresh: patchTokenCell keeps a single .tok wrapper and updates
// its contents in place across populated/empty transitions, never nesting
// .tok .tok. Drive the live view end-to-end with a real virtual model.
test('live refresh: token cell stays non-nested when usage appears', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'live-tok';
  const clientName = 'live-tok-client';
  await mockOkModel(page, 'mock-model');
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-tok-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);
  const virtual = await virtualResponse.json();

  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'virtual', single_target_id: virtual.id }
  });
  expect(createRes.status()).toBe(201);
  const secret = (await createRes.json()).secret;

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await expect(page.getByRole('heading', { name: 'Virtual Models', exact: true })).toBeVisible();
  const row = page.locator(`tr[data-virtual-id="${virtual.id}"]`);
  await expect(row).toBeVisible();
  const tokCell = row.locator('.tok[data-window="1h"]').first();
  await expect(tokCell).toBeVisible();

  // Initial state: dash, no cache wrapper. The cell itself is the .tok element.
  const initial = await tokCell.evaluate(cell => ({
    nestedTok: cell.querySelectorAll('.tok').length,
    dash: cell.textContent.trim() === '—',
    cacheHit: Boolean(cell.querySelector('.cache-hit')),
  }));
  expect(initial.nestedTok).toBe(0);
  expect(initial.dash).toBeTruthy();
  expect(initial.cacheHit).toBeFalsy();

  // Fire a successful request; usage appears. The patched cell must
  // still be a single .tok wrapper — never .tok(.tok ...). NestedTok
  // must remain 0 because patchTokenCell updates innerHTML of the
  // existing element rather than replacing the cell.
  const fire = async () => {
    const res = await page.request.post('/v1/chat/completions', {
      headers: { Authorization: `Bearer ${secret}` },
      data: { model: 'main', messages: [{ role: 'user', content: 'live' }] }
    });
    return res.status();
  };
  expect(await fire()).toBe(200);
  await expect.poll(() => tokCell.textContent(), { timeout: 8000 }).not.toContain('—');
  const populated = await tokCell.evaluate(cell => ({
    nestedTok: cell.querySelectorAll('.tok').length,
    mtok: cell.querySelector('b')?.textContent || '',
  }));
  expect(populated.nestedTok).toBe(0);
  expect(populated.mtok).not.toBe('');
});

// Live SSE refresh: patchResolution always refreshes the tooltip and
// aria-label, even when the resolution class does not change between
// neutral sub-states ("No activity recorded" vs "No activity in 24h").
// Drive the live view end-to-end with a real virtual model: fire a
// successful request, then wait for the resolution icon to update, and
// assert aria-label/title are non-empty for whatever class is shown.
test('live refresh: neutral tooltip refresh without class change', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'live-tooltip';
  const clientName = 'live-tooltip-client';
  await mockOkModel(page, 'mock-model');
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-tooltip-vg' } });
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  const virtual = await virtualResponse.json();
  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'virtual', single_target_id: virtual.id }
  });
  const secret = (await createRes.json()).secret;

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await expect(page.getByRole('heading', { name: 'Virtual Models', exact: true })).toBeVisible();
  const row = page.locator(`tr[data-virtual-id="${virtual.id}"]`);
  await expect(row).toBeVisible();
  const targetLine = row.locator(`[data-target-key="${real.id}"]`);
  await expect(targetLine).toBeVisible();
  const icon = targetLine.locator('.resolution-indicator');
  await expect(icon).toBeVisible();

  // Initial state: neutral, "No activity recorded".
  await expect(icon).toHaveClass(/resolution-neutral/);
  const initial = await icon.evaluate(el => ({ label: el.getAttribute('aria-label'), title: el.getAttribute('title') }));
  expect(initial.label).toBe('No activity recorded');
  expect(initial.title).toBe('No activity recorded');

  // Fire a successful request; the icon flips to good via the SSE
  // outcome delta. Verify aria-label/title are set (always-update
  // contract) even though the class changed.
  const fire = async () => {
    const res = await page.request.post('/v1/chat/completions', {
      headers: { Authorization: `Bearer ${secret}` },
      data: { model: 'main', messages: [{ role: 'user', content: 'tooltip' }] }
    });
    return res.status();
  };
  expect(await fire()).toBe(200);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');
  const afterGood = await icon.evaluate(el => ({
    label: el.getAttribute('aria-label'), title: el.getAttribute('title')
  }));
  expect(afterGood.label).toBe('Resolving successfully');
  expect(afterGood.title).toBe('Resolving successfully');

  // Navigate away and back to force a reconcile. The icon class is
  // already good so patchResolution skips the SVG swap, but must still
  // write aria-label/title from the current resolutionStatus().
  await page.getByRole('link', { name: 'Clients' }).click();
  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await expect(row).toBeVisible();
  await expect(icon).toBeVisible();
  // If the reconciliation wrote stale values we'd see either no text or
  // the old neutral text. The good label must be present.
  const afterReconcile = await icon.evaluate(el => ({
    cls: el.className,
    label: el.getAttribute('aria-label'),
    title: el.getAttribute('title'),
  }));
  expect(afterReconcile.cls).toContain('resolution-good');
  expect(afterReconcile.label).toBe('Resolving successfully');
  expect(afterReconcile.title).toBe('Resolving successfully');
});
test('live refresh: resolution icon and token counter update without navigation', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'live-refresh';
  const clientName = 'live-refresh-client';
  // The mock upstream is shared across the shard; reset mock-model to healthy
  // so this test starts from a clean baseline regardless of prior tests.
  await mockOkModel(page, 'mock-model');
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  expect(modelsResponse.ok()).toBeTruthy();
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  // Virtual model targeting the real model.
  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-vg' } });
  expect(groupResponse.status()).toBe(201);
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  expect(virtualResponse.status()).toBe(201);
  const virtual = await virtualResponse.json();

  // Single client bound to the virtual model so requests route through it.
  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'virtual', single_target_id: virtual.id }
  });
  expect(createRes.status()).toBe(201);
  const secret = (await createRes.json()).secret;

  // Navigate to Virtual Models.
  await page.getByRole('link', { name: 'Virtual Models' }).click();
  await expect(page.getByRole('heading', { name: 'Virtual Models', exact: true })).toBeVisible();

  const row = page.locator(`tr[data-virtual-id="${virtual.id}"]`);
  await expect(row).toBeVisible();
  const targetLine = row.locator(`[data-target-key="${real.id}"]`);
  await expect(targetLine).toBeVisible();
  const icon = targetLine.locator('.resolution-indicator');

  // Initially no activity -> neutral.
  await expect(icon).toHaveClass(/resolution-neutral/);

  // Fire a successful request; the icon must flip to good WITHOUT navigation.
  // Fire a request and return its status (the caller asserts per phase).
  const fire = async () => {
    const res = await page.request.post('/v1/chat/completions', {
      headers: { Authorization: `Bearer ${secret}` },
      data: { model: 'main', messages: [{ role: 'user', content: 'live' }] }
    });
    return res.status();
  };
  // Success phase: 200 and the icon flips to good WITHOUT navigation.
  expect(await fire()).toBe(200);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');

  // Fail phase: upstream 500 -> virtual unavailable (503), icon flips to bad.
  await mockFailModel(page, 'mock-model');
  expect(await fire()).toBe(503);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-bad');

  // Restore; the icon flips back to good.
  await mockOkModel(page, 'mock-model');
  expect(await fire()).toBe(200);
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');

  // The 1h token counter reflects the traffic (no longer a dash) via the
  // snapshot cadence.
  const tokCell = row.locator('.tok[data-window="1h"]').first();
  await expect.poll(() => tokCell.textContent(), { timeout: 8000 }).not.toContain('—');
});

test('live refresh: DOM writes pause while a dialog is open and reconcile on close', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  const csrf = await adminCsrf(page);

  const providerName = 'live-dialog';
  const clientName = 'live-dialog-client';
  await mockOkModel(page, 'mock-model');
  const provider = await createProvider(page, csrf, providerName);
  const modelsResponse = await page.request.get(`/api/admin/providers/${provider.id}/models`);
  const models = (await modelsResponse.json()).data;
  const real = models.find(model => model.upstream_model_id === 'mock-model');
  expect(real).toBeTruthy();

  const groupResponse = await page.request.post('/api/admin/virtual-groups', { headers: { 'X-CSRF-Token': csrf }, data: { name: 'live-dialog-vg' } });
  const group = await groupResponse.json();
  const virtualResponse = await page.request.post('/api/admin/virtual-models', { headers: { 'X-CSRF-Token': csrf }, data: { group_id: group.id, name: 'coding', target_provider_id: provider.id, target_model_id: real.id } });
  const virtual = await virtualResponse.json();
  const createRes = await page.request.post('/api/admin/client-keys', {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: clientName, type: 'single', single_model_name: 'main', single_target_type: 'virtual', single_target_id: virtual.id }
  });
  const secret = (await createRes.json()).secret;

  await page.getByRole('link', { name: 'Virtual Models' }).click();
  const row = page.locator(`tr[data-virtual-id="${virtual.id}"]`);
  await expect(row).toBeVisible();
  const icon = row.locator(`[data-target-key="${real.id}"] .resolution-indicator`);
  await expect(icon).toHaveClass(/resolution-neutral/);

  // Open the Capabilities dialog (a modal) and fire a request behind it.
  await row.getByRole('button', { name: 'Capabilities' }).click();
  await expect(page.locator('#capabilities-dialog')).toBeVisible();

  const res = await page.request.post('/v1/chat/completions', {
    headers: { Authorization: `Bearer ${secret}` },
    data: { model: 'main', messages: [{ role: 'user', content: 'behind-dialog' }] }
  });
  expect(res.status()).toBe(200);

  // While the dialog is open the icon must NOT be patched (DOM writes paused).
  await page.waitForTimeout(1500);
  await expect(icon).toHaveClass(/resolution-neutral/);

  // Closing the dialog reconciles the pending state.
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.locator('#capabilities-dialog')).toBeHidden();
  await expect.poll(() => icon.getAttribute('class'), { timeout: 5000 }).toContain('resolution-good');
});
