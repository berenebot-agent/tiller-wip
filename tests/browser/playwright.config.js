const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30_000,
  // NB: retries intentionally 0. A retry re-runs on the same worker router
  // where the first attempt may already have created fixed-name fixtures.
  // Isolation between shards comes from a fresh router per shard (see run.sh).
  retries: 0,
  workers: Number(process.env.PLAYWRIGHT_WORKERS || 1),
  fullyParallel: true,
  use: {
    baseURL: process.env.TILLER_BROWSER_BASE_URL || 'http://127.0.0.1:18080',
    trace: 'retain-on-failure',
    // Grant clipboard so the secret-copy test can both write and read the OS
    // clipboard. Without `clipboard-read`, navigator.clipboard.readText()
    // rejects and the "did the key actually land on the clipboard?" assertion
    // cannot run.
    permissions: ['clipboard-read', 'clipboard-write']
  },
  reporter: [['list', { printSteps: false }]]
});
