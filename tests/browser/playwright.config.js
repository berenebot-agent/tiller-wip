const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30_000,
  // NB: retries intentionally 0. These tests create providers/clients under
  // FIXED names against ONE shared router for the whole run. A retry re-runs
  // on the same DB where the first (failed) attempt already created those
  // names, so it fails deterministically with a 409 name_conflict and never
  // succeeds. Isolation comes from a fresh router per run (see run.sh), not
  // from per-test retries.
  retries: 0,
  workers: 1,
  use: {
    baseURL: process.env.TILLER_BROWSER_BASE_URL || 'http://127.0.0.1:18080',
    trace: 'retain-on-failure'
  },
  reporter: [['list', { printSteps: false }]]
});
