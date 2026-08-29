const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30_000,
  retries: 0,
  workers: 1,
  use: {
    baseURL: process.env.TILLER_BROWSER_BASE_URL || 'http://127.0.0.1:18080',
    trace: 'retain-on-failure'
  },
  reporter: [['line']]
});
