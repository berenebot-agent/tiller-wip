const { expect } = require('@playwright/test');

const ADMIN_USER = process.env.TILLER_BROWSER_ADMIN_USERNAME || 'admin';
const ADMIN_PASS = process.env.TILLER_BROWSER_ADMIN_PASSWORD || 'browser-test-password';
const MOCK_BASE = process.env.TILLER_BROWSER_MOCK_BASE_URL || 'http://127.0.0.1:18081/v1';
const MOCK_CONTROL_BASE = MOCK_BASE.replace(/\/v1$/, '');

async function login(page) {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Tiller Router' })).toBeVisible();
  await page.getByLabel('Administrator').fill(ADMIN_USER);
  await page.getByLabel('Password').fill(ADMIN_PASS);
  await page.getByRole('button', { name: 'Enter control panel' }).click();
  await expect(page.getByRole('heading', { name: 'Clients', exact: true })).toBeVisible();
}
async function adminCsrf(page) { const res = await page.request.get('/api/admin/session'); expect(res.ok()).toBeTruthy(); return (await res.json()).csrf_token; }
async function createProvider(page, csrf, name) { const res = await page.request.post('/api/admin/providers', { headers: { 'X-CSRF-Token': csrf }, data: { name, type: 'generic-openai', base_url: MOCK_BASE } }); expect(res.status()).toBe(201); const body = await res.json(); expect(body.refresh_error).toBeFalsy(); return body; }
async function createClient(page, csrf, name) { const res = await page.request.post('/api/admin/client-keys', { headers: { 'X-CSRF-Token': csrf }, data: { name, description: 'browser permission test client' } }); expect(res.status()).toBe(201); return res.json(); }
async function mockAddModel(page, id) { const res = await page.request.post(`${MOCK_CONTROL_BASE}/__/models/add/${id}`); expect(res.ok()).toBeTruthy(); }
async function mockRemoveModel(page, id) { const res = await page.request.post(`${MOCK_CONTROL_BASE}/__/models/remove/${id}`); expect(res.ok()).toBeTruthy(); }
async function refreshProviderApi(page, csrf, providerId) { const res = await page.request.post(`/api/admin/providers/${providerId}/refresh`, { headers: { 'X-CSRF-Token': csrf } }); expect(res.ok()).toBeTruthy(); }
async function clearActivity(page, csrf) { const res = await page.request.get('/api/admin/client-keys?limit=200'); expect(res.ok()).toBeTruthy(); for (const client of (await res.json()).data) { const clear = await page.request.delete(`/api/admin/client-keys/${client.id}/activity`, { headers: { 'X-CSRF-Token': csrf } }); expect(clear.ok()).toBeTruthy(); } }

module.exports = { ADMIN_USER, ADMIN_PASS, MOCK_BASE, MOCK_CONTROL_BASE, login, adminCsrf, createProvider, createClient, mockAddModel, mockRemoveModel, refreshProviderApi, clearActivity };
