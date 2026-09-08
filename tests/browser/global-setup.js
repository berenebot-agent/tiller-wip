// global-setup.js — authenticate once per Playwright process and save the
// resulting cookie state to a fixed path. Each shard runs in its own container
// (see run.sh), so /tmp/tiller-admin-storage.json is isolated per shard.
const { request } = require('@playwright/test');

const STORAGE_PATH = '/tmp/tiller-admin-storage.json';

module.exports = async function globalSetup() {
  const baseUrl = process.env.TILLER_BROWSER_BASE_URL || 'http://127.0.0.1:18080';
  const username = process.env.TILLER_BROWSER_ADMIN_USERNAME || 'admin';
  const password = process.env.TILLER_BROWSER_ADMIN_PASSWORD || 'browser-test-password';

  const ctx = await request.newContext({ baseURL: baseUrl });
  try {
    const res = await ctx.post('/api/admin/session', {
      data: { username, password },
    });
    if (!res.ok()) {
      const body = await res.text().catch(() => '');
      throw new Error(`login failed: HTTP ${res.status()} ${body}`);
    }
    await ctx.storageState({ path: STORAGE_PATH });
  } finally {
    await ctx.dispose();
  }
};

module.exports.STORAGE_PATH = STORAGE_PATH;
