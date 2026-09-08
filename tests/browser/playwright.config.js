const { defineConfig } = require('@playwright/test');
const { STORAGE_PATH } = require('./global-setup');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30_000,
  // NB: retries intentionally 0. A retry re-runs on the same worker router
  // where the first attempt may already have created fixed-name fixtures.
  // Isolation between shards comes from a fresh router per shard (see run.sh).
  retries: 0,
  workers: Number(process.env.PLAYWRIGHT_WORKERS || 1),
  fullyParallel: true,
  // Authenticate once per process via globalSetup, then reuse the cookie state
  // for normal tests. loginFresh() tests bypass this with an unauthenticated
  // context.
  globalSetup: require.resolve('./global-setup.js'),
  use: {
    baseURL: process.env.TILLER_BROWSER_BASE_URL || 'http://127.0.0.1:18080',
    storageState: STORAGE_PATH,
    trace: 'retain-on-failure',
    // Grant clipboard so the secret-copy test can both write and read the OS
    // clipboard. Without `clipboard-read`, navigator.clipboard.readText()
    // rejects and the "did the key actually land on the clipboard?" assertion
    // cannot run.
    permissions: ['clipboard-read', 'clipboard-write']
  },
  reporter: [['list', { printSteps: false }]]
});
