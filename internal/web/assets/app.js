import { LiveStream } from './live.js';
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const state = { csrf: '', view: 'clients', providers: [], models: [], groups: [], virtualModels: [], clients: [], permissionData: null, providerTypes: [], usage: null, usageAt: 0, usageReady: false, liveRequests: {}, liveRoutes: {}, liveLegs: {}, mobileActivity: [], loadToken: 0, platformUsersOffset: 0, platformUsersSearch: '', platformUsersLoadToken: 0, platformPlans: [], platformPlanData: [] };
let runtimeMode = 'local';
let hostedAuthOptions = {};
let signupEmail = '';
let captchaWidgetID = null;
let captchaAction = '';
let captchaToken = '';
let captchaGeneration = 0;
let captchaScriptPromise = null;
const mobileVirtualDrafts = new Map();
const mobileVirtualExpanded = new Set();
// routeActivity derives a virtual route's spinner state from the per
// (client, route) tickets ("any ticket with this route"). OR-folds active +
// streaming so two clients — or one client with parallel requests on this
// route — keep the spinner lit until all drain, and the streaming label
// survives if any stream is in flight. Returns null when quiet so the existing
// activity?.active optional-chaining keeps working. Deliberately unmemoized:
// O(rows × tickets) is trivial at admin-UI scale, and a cache here would risk
// stale spinners.
const routeActivity = routeID => {
  let out = null;
  for (const req of Object.values(state.liveRoutes)) {
    if (req.routeID !== routeID || req.active <= 0) continue;
    out = { active: 1, streaming: Math.max(out?.streaming || 0, req.streaming || 0) };
  }
  return out;
};
// routeTicketKey joins the live-ticket map key the same way the backend does
// (client id + NUL + route id). The composite key keeps concurrent requests
// from one client key on different routes distinct.
const routeTicketKey = (clientID, routeID) => `${clientID}\u0000${routeID || ''}`;
const sortState = { column: '1h', direction: 'desc' };
const SORT_DEFAULTS = { canonical: 'asc', provider: 'asc', '1h': 'desc', '24h': 'desc', '7d': 'desc' };
// MODEL_USAGE_SORTS names the model-table sort columns whose ordering depends on
// the usage envelope. The catalogue renders before usage arrives, so a usage
// sort is only meaningful once usage lands — at which point the rows must be
// re-sorted (see modelsResortPending / reorderModelRows).
const MODEL_USAGE_SORTS = new Set(['1h', '24h', '7d']);
// modelsResortPending records that usage first became available while a
// usage-sorted model table may still be in catalogue order. Set by
// markUsageReady, consumed once by reconcileLive (which respects an open dialog
// and the user's current sortState). This deliberately re-applies the *current*
// sort — it never resets the user's chosen column or direction.
let modelsResortPending = false;
const collapsedModels = new Set(); const collapsedVirtual = new Set(); const collapsedClients = new Set(); const collapsedPermissionGroups = new Set(); const collapsedPermissionSections = new Set();
const GROUP_ARROW = { up: '▼', down: '▶' };
const MODEL_EXPAND_BATCH_SIZE = 20;
const groupRevealFrames = new WeakMap();
const h = value => String(value ?? '').replace(/[&<>'"]/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' })[char]);
const date = value => value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Never';
// tokLoadingInner is the placeholder shown in a token cell before the usage /
// health envelope has arrived. It is deliberately distinct from the "—" empty
// state: "—" means "no traffic recorded" and only appears once usage is known,
// so a slow first load reads as "still coming" rather than "no data". The
// spinner is transform-only (compositor-friendly) and aria-hidden, because the
// loading state can briefly cover thousands of cells at once; the wrapper
// carries aria-busy instead of each cell being a live region. It is transient:
// the first usage snapshot replaces it.
const tokLoadingInner = '<span class="tok-loading" aria-hidden="true"><span class="tok-loading-spin"></span></span>';
// renderTokInner returns the inner markup of a .tok cell (no <span class="tok">
// wrapper). Both initial render (tok) and live patching (patchTokenCell) build
// their DOM from this single source so the .tok element is never re-wrapped and
// transitions between loading, populated, and empty states keep consistent
// structure.
const renderTokInner = (tokens, pct, loading = false) => {
  if (loading) return tokLoadingInner;
  if (!tokens && pct == null) return '—';
  const num = tokens ? `<b>${(tokens / 1e6).toFixed(2)}</b><small>Mtok</small>` : '';
  const cache = (pct != null && !isNaN(pct))
    ? `<span class="cache-hit"><b>${Math.round(pct)}%</b><small>Cache</small></span>`
    : `<span class="cache-hit na"><small>n.a. Cache</small></span>`;
  return `${num}${cache}`;
};
// A cell is "loading" only while the usage envelope is unknown AND this cell has
// no value yet. Once any snapshot/fetch has landed (usageReady), absent data is
// a genuine empty state ("—").
const tokLoading = (tokens, pct) => !state.usageReady && !tokens && pct == null;
const tok = (tokens, pct, window) => {
  const loading = tokLoading(tokens, pct);
  return `<span class="tok" data-window="${window}"${loading ? ' aria-busy="true"' : ''}>${renderTokInner(tokens, pct, loading)}</span>`;
};
const rowCache = (row) => {
  const inp = row.input_tokens;
  const output = row.output_tokens;
  const cache = row.cache_read_input_tokens;
  const line = (cache != null && inp > 0)
    ? `<span class="cache-hit"><b>${Math.round(cache / inp * 100)}%</b><small>Cache</small></span>`
    : `<span class="cache-hit na"><small>n.a. Cache</small></span>`;
  return `<span class="activity-tokens"><b>${inp ?? '—'} / ${output ?? '—'}</b>${line}</span>`;
};
const VIEWS = ['providers', 'models', 'virtual', 'clients', 'activity', 'settings'];
const viewFromHash = () => { const raw = (location.hash.replace(/^#\/?/, '') || 'clients'); const v = raw.split('/')[0]; return VIEWS.includes(v) ? v : 'clients'; };
const settingsTabFromHash = () => { const parts = location.hash.replace(/^#\/?/, '').split('/'); return parts[0] === 'settings' && parts[1] ? parts[1] : ''; };

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body && !(options.body instanceof FormData)) headers.set('Content-Type', 'application/json');
  if (state.csrf && options.method && !['GET', 'HEAD'].includes(options.method)) headers.set('X-CSRF-Token', state.csrf);
  const response = await fetch(path, { credentials: 'same-origin', ...options, headers });
  const type = response.headers.get('content-type') || '';
  const payload = type.includes('json') ? await response.json().catch(() => ({})) : null;
  if (!response.ok) {
    if (response.status === 401 && !path.endsWith('/session')) showLogin();
    const error = new Error(payload?.error?.message || `Request failed (${response.status})`);
    error.code = payload?.error?.code;
    error.status = response.status;
    error.data = payload;
    throw error;
  }
  return payload;
}

// loadUsage returns the usage/health envelope, reusing a recently received one
// (from a prior fetch or an SSE snapshot) inside USAGE_REUSE_MS. On a first
// page load the SSE baseline snapshot and the view's parallel fetches would
// otherwise each request /api/admin/usage back-to-back; this collapses them.
// Concurrent callers within a tick also share a single in-flight request.
const USAGE_REUSE_MS = 2000;
let usageInFlight = null;
// markUsageReady flips token cells from the loading spinner to real values (or
// the "—" empty state) on the first usage arrival, and flags a one-time re-sort
// of the model table if it was rendered under a usage sort before usage was
// known. Idempotent: later snapshots do not re-sort, matching the pre-existing
// "sort at render, patch values live" behaviour.
function markUsageReady() {
  if (state.usageReady) return;
  state.usageReady = true;
  modelsResortPending = true;
}
async function loadUsage() {
  if (state.usage && Date.now() - state.usageAt < USAGE_REUSE_MS) return state.usage;
  if (usageInFlight) return usageInFlight;
  usageInFlight = api('/api/admin/usage').then(usage => {
    state.usage = usage; state.usageAt = Date.now(); markUsageReady();
    return usage;
  }).finally(() => { usageInFlight = null; });
  return usageInFlight;
}

// deferUsage keeps usage off the view's critical render path. Catalogue views
// paint from their own (fast) fetches immediately; the usage/health envelope
// then arrives either via the SSE baseline snapshot or this fallback fetch, and
// reconcileLive patches the token/health cells in place. Errors are ignored:
// the SSE snapshot is the primary source, this is the degraded-mode fallback.
function deferUsage() {
  loadUsage().then(() => reconcileLive()).catch(() => {});
}
function authView(name) {
  ['login-form','signup-form','signup-done','forgot-form','verify-panel','reset-form','platform-login-form','google-consent-form'].forEach(id => { const el = $('#' + id); if (el) el.hidden = id !== name; });
  const loginCard = $('.login-card'); if (loginCard) loginCard.classList.toggle('is-platform', name === 'platform-login-form');
  const hosted = runtimeMode === 'hosted';
  $('#hosted-auth-links').hidden = !hosted || name !== 'login-form';
  $('#show-signup').hidden = !hosted || !hostedAuthOptions.signup_enabled;
  $('#google-signin').hidden = !hosted || name !== 'login-form' || !hostedAuthOptions.google_enabled;
  $('#google-signin-notice').hidden = !hosted || name !== 'login-form' || !hostedAuthOptions.google_enabled;
  ['resend-login-verification', 'resend-signup-verification', 'verify-email-wrap', 'resend-verification', 'reset-login'].forEach(id => { const el = $('#' + id); if (el) el.hidden = true; });
  const action = name === 'signup-form' ? 'signup' : name === 'forgot-form' ? 'recovery' : name === 'login-form' && hostedAuthOptions.google_enabled ? 'google_signin' : '';
  showAuthCaptcha(action);
}
function showLogin() { $('#app').hidden = true; $('#platform-shell').hidden = true; $('#legal-shell').hidden = true; $('#login-shell').hidden = false; state.csrf = ''; const platform = runtimeMode === 'hosted' && location.pathname.startsWith('/platform'); authView(platform ? 'platform-login-form' : 'login-form'); history.replaceState(null, '', platform ? '/platform' : (runtimeMode === 'hosted' ? '/login' : '/')); liveStop(); }
function showApp(session) { state.csrf = session.csrf_token; $('#admin-name').textContent = session.username || session.email; $('#login-shell').hidden = true; $('#platform-shell').hidden = true; $('#legal-shell').hidden = true; $('#app').hidden = false; $('#app-footer').hidden = runtimeMode !== 'hosted'; liveStart(); navigate(state.view); if (runtimeMode === 'hosted') { loadFooterVersion(); refreshWizardButton(true); } }
function flash(message, kind = 'success') { const box = $('#flash'); box.textContent = message; box.className = `flash flash-${kind}`; box.hidden = false; clearTimeout(flash.timer); flash.timer = setTimeout(() => box.hidden = true, 5000); }
function errorMessage(error, fallback = 'The operation could not be completed.') { return error?.message || fallback; }

function loadTurnstileScript() {
  if (window.turnstile) return Promise.resolve();
  if (!captchaScriptPromise) {
    captchaScriptPromise = new Promise((resolve, reject) => {
      const script = document.createElement('script');
      script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit';
      script.async = true; script.defer = true;
      script.onload = () => window.turnstile ? resolve() : reject(new Error('Security check failed to load.'));
      script.onerror = () => reject(new Error('Security check failed to load.'));
      document.head.appendChild(script);
    });
  }
  return captchaScriptPromise;
}

async function showAuthCaptcha(action) {
  const wrap = $('#auth-captcha-wrap');
  if (!wrap) return;
  if (!hostedAuthOptions.turnstile_enabled || !action) {
    captchaGeneration++; captchaToken = ''; captchaAction = '';
    if (captchaWidgetID !== null && window.turnstile) window.turnstile.remove(captchaWidgetID);
    captchaWidgetID = null; $('#auth-captcha').replaceChildren(); wrap.hidden = true;
    return;
  }
  if (captchaAction === action && captchaWidgetID !== null) { wrap.hidden = false; return; }
  const generation = ++captchaGeneration;
  captchaAction = action; captchaToken = ''; captchaWidgetID = null;
  if (window.turnstile && $('#auth-captcha').dataset.widget) window.turnstile.remove($('#auth-captcha').dataset.widget);
  $('#auth-captcha').replaceChildren(); delete $('#auth-captcha').dataset.widget;
  wrap.hidden = false;
  try {
    await loadTurnstileScript();
    if (generation !== captchaGeneration) return;
    captchaWidgetID = window.turnstile.render('#auth-captcha', {
      sitekey: hostedAuthOptions.turnstile_site_key, action,
      callback: token => { captchaToken = token; captchaAction = action; },
      'expired-callback': () => { captchaToken = ''; },
      'error-callback': () => { captchaToken = ''; },
    });
    $('#auth-captcha').dataset.widget = captchaWidgetID;
  } catch (error) {
    if (generation === captchaGeneration) $('#auth-captcha-wrap').querySelector('.meta-line').textContent = errorMessage(error, 'Security check failed to load.');
  }
}

function authCaptchaToken(action) {
  if (!hostedAuthOptions.turnstile_enabled) return '';
  if (captchaAction !== action || !captchaToken) {
    showAuthCaptcha(action);
    throw new Error('Complete the security check, then try again.');
  }
  return captchaToken;
}

function resetAuthCaptcha(action) {
  if (!hostedAuthOptions.turnstile_enabled || captchaAction !== action) return;
  captchaToken = '';
  if (captchaWidgetID !== null && window.turnstile) window.turnstile.reset(captchaWidgetID);
}

$('#login-form').addEventListener('submit', async event => {
  event.preventDefault(); $('#login-error').textContent = '';
  const formElement = event.currentTarget; const form = new FormData(formElement); const button = $('button[type="submit"]', formElement); button.disabled = true;
  try { const path = runtimeMode === 'hosted' ? '/api/auth/login' : '/api/admin/session'; const body = runtimeMode === 'hosted' ? { email: form.get('username'), password: form.get('password') } : { username: form.get('username'), password: form.get('password') }; const session = await api(path, { method: 'POST', body: JSON.stringify(body) }); formElement.reset(); showApp(session); }
  catch (error) { $('#login-error').textContent = errorMessage(error, 'Login failed.'); if (error.code === 'email_not_verified') exposeResend('#resend-login-verification', '#login-form [name="username"]', '#login-error'); }
  finally { button.disabled = false; }
});
$('#logout').addEventListener('click', async () => { try { await api(runtimeMode === 'hosted' ? '/api/auth/session' : '/api/admin/session', { method: 'DELETE' }); } finally { showLogin(); } });

function showAuthError(id, error, fallback) { const el = $('#' + id); if (el) el.textContent = errorMessage(error, fallback); }
async function resendVerification(email, target) {
  const captcha_token = authCaptchaToken('recovery');
  let result;
  try { result = await api('/api/auth/verification/resend', { method: 'POST', body: JSON.stringify({ email, captcha_token }) }); }
  finally { resetAuthCaptcha('recovery'); }
  $(target).textContent = result.message || 'If the address can receive mail, a verification message will arrive shortly.';
  $(target).style.color = 'var(--green)';
}
function exposeResend(target, emailInput, messageTarget) {
  const button = $(target); if (!button) return;
  button.hidden = false; showAuthCaptcha(hostedAuthOptions.turnstile_enabled ? 'recovery' : ''); button.onclick = async () => { button.disabled = true; try { await resendVerification($(emailInput).value, messageTarget); } catch (error) { showAuthError(messageTarget.replace('#', ''), error, 'Could not resend verification email.'); } finally { button.disabled = false; } };
}
$('#show-signup').onclick = () => authView('signup-form');
$('#show-forgot-password').onclick = () => authView('forgot-form');
$('#show-login-from-signup').onclick = () => authView('login-form');
$('#signup-done-login').onclick = () => authView('login-form');
$('#show-login-from-forgot').onclick = () => authView('login-form');
// exposeSignupResend wires the resend button on the post-signup inbox screen.
// The address is captured at submit time (signupEmail) because the done panel
// has no email input of its own.
function exposeSignupResend() {
  const button = $('#resend-signup-verification'); if (!button) return;
  button.hidden = false;
  showAuthCaptcha(hostedAuthOptions.turnstile_enabled ? 'recovery' : '');
  button.onclick = async () => { button.disabled = true; try { await resendVerification(signupEmail, '#signup-done-error'); } catch (error) { showAuthError('signup-done-error', error, 'Could not resend verification email.'); } finally { button.disabled = false; } };
}
$('#signup-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget);
  if (form.get('accept_terms') !== 'on') { showAuthError('signup-error', { message: 'Please agree to the Terms of Service to continue.' }, 'Signup failed.'); return; }
  try {
    const captcha_token = authCaptchaToken('signup');
    await api('/api/auth/signup', { method: 'POST', body: JSON.stringify({ email: form.get('email'), password: form.get('password'), accept_terms: true, captcha_token }) });
    signupEmail = String(form.get('email') || '');
    event.currentTarget.reset();
    $('#signup-done-message').textContent = `If an account can be created for ${signupEmail}, a verification link is on its way. Click the link in the email to activate your account.`;
    authView('signup-done');
    exposeSignupResend();
  } catch (error) { showAuthError('signup-error', error, 'Signup failed.'); } finally { resetAuthCaptcha('signup'); }
});
$('#forgot-form').addEventListener('submit', async event => { event.preventDefault(); const form = new FormData(event.currentTarget); try { const captcha_token = authCaptchaToken('recovery'); const result = await api('/api/auth/password-reset/request', { method: 'POST', body: JSON.stringify({ email: form.get('email'), captcha_token }) }); $('#forgot-error').textContent = result.message || 'Check your email.'; } catch (error) { showAuthError('forgot-error', error, 'Recovery failed.'); } finally { resetAuthCaptcha('recovery'); } });
$('#google-signin').addEventListener('click', async () => {
  $('#login-error').textContent = '';
  try {
    const captcha_token = authCaptchaToken('google_signin');
    const result = await api('/api/auth/google/start', { method: 'POST', body: JSON.stringify({ captcha_token }) });
    location.assign(result.redirect_url);
  } catch (error) { showAuthError('login-error', error, 'Google sign-in could not be started.'); }
  finally { resetAuthCaptcha('google_signin'); }
});
$('#google-consent-cancel').onclick = () => { history.replaceState(null, '', '/login'); authView('login-form'); };
$('#google-consent-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget);
  if (form.get('accept_terms') !== 'on') { showAuthError('google-consent-error', { message: 'Please agree to the Terms of Service and Privacy Policy.' }, 'Account creation failed.'); return; }
  try {
    const session = await api('/api/auth/google/signup/complete', { method: 'POST', body: JSON.stringify({ accept_terms: true }) });
    history.replaceState(null, '', '/#clients'); showApp(session);
  } catch (error) { showAuthError('google-consent-error', error, 'Account creation failed.'); }
});
$('#verify-login').onclick = () => authView('login-form');
$('#reset-login').onclick = () => authView('login-form');
$('#reset-form').addEventListener('submit', async event => { event.preventDefault(); const token = new URLSearchParams(location.search).get('token') || ''; const form = new FormData(event.currentTarget); try { await api('/api/auth/password-reset/confirm', { method: 'POST', body: JSON.stringify({ token, password: form.get('password') }) }); $('#reset-error').textContent = 'Password changed. You can sign in now.'; $('#reset-error').style.color = 'var(--green)'; $('#reset-login').hidden = false; } catch (error) { showAuthError('reset-error', error, 'Reset failed.'); } });
$('#platform-login-form').addEventListener('submit', async event => { event.preventDefault(); const form = new FormData(event.currentTarget); try { const session = await api('/api/platform/session', { method: 'POST', body: JSON.stringify({ username: form.get('username'), password: form.get('password') }) }); state.csrf = session.csrf_token; $('#login-shell').hidden = true; $('#platform-shell').hidden = false; await loadPlatformDashboard(); } catch (error) { showAuthError('platform-login-error', error, 'Platform login failed.'); } });
$('#platform-logout').onclick = async () => { try { await api('/api/platform/session', { method: 'DELETE' }); } finally { history.replaceState(null, '', '/platform'); showLogin(); } };

async function navigate(view) {
  state.view = view;
  // Preserve the settings sub-tab in the hash; only the base view is rewritten.
  const desiredHash = view === 'settings' ? ('#settings' + (settingsTabFromHash() && settingsTabFromHash() !== 'routing' ? '/' + settingsTabFromHash() : '')) : '#' + view;
  if (location.hash !== desiredHash) history.pushState(null, '', desiredHash);
  $$('.view').forEach(panel => panel.classList.toggle('active', panel.id === `view-${view}`)); $$('[data-view]').forEach(button => button.classList.toggle('active', button.dataset.view === view));
  try { if (view === 'providers') await loadProviders(); if (view === 'models') await loadModels(); if (view === 'virtual') await loadVirtual(); if (view === 'clients') await loadClients(); if (view === 'activity') await loadActivityView(); if (view === 'settings') { showSettingsTab(settingsTabFromHash() || settingsTab); await loadSettings(); if (settingsTab === 'account') await loadAccount(); } }
  catch (error) { flash(errorMessage(error), 'error'); }
  if (view !== 'activity') destroyActivityView();
}
$$('[data-view]').forEach(link => link.addEventListener('click', event => { if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return; event.preventDefault(); navigate(link.dataset.view); }));
window.addEventListener('popstate', () => navigate(viewFromHash()));
$$('[data-refresh-view]').forEach(button => button.addEventListener('click', () => navigate(button.dataset.refreshView)));
$$('[data-filter-toggle]').forEach(button => button.addEventListener('click', () => {
  const bar = button.closest('[data-filter-bar]');
  const open = bar.classList.toggle('filter-open');
  button.setAttribute('aria-expanded', String(open));
}));
$('#add-client-mobile').onclick = () => openClient();
$('#add-provider-mobile').onclick = () => openProvider();
$('#add-real-model-mobile').onclick = openManualModel;
$('#add-virtual-group-mobile').onclick = () => openVirtualGroup();
$('#add-virtual-model-mobile').onclick = () => openVirtualModel();

let filterTimers = new Map();
function filterInput(selector, callback) { $(selector).addEventListener('input', event => { clearTimeout(filterTimers.get(selector)); filterTimers.set(selector, setTimeout(() => callback(event.target.value), 180)); }); }
filterInput('#provider-search', loadProviders); filterInput('#model-search', loadModels); filterInput('#virtual-search', loadVirtual); filterInput('#client-search', loadClients);
$('#show-retired').addEventListener('change', renderModels);
$('#client-group-filter').addEventListener('change', loadClients);

async function loadProviders(search = $('#provider-search').value) {
  const token = ++state.loadToken;
  const [result, types] = await Promise.all([api(`/api/admin/providers?limit=200&search=${encodeURIComponent(search || '')}`), state.providerTypes.length ? Promise.resolve({ data: state.providerTypes }) : api('/api/admin/provider-types')]);
  if (token !== state.loadToken) return;
  state.providers = result.data; state.providerTypes = types.data; renderProviders();
}
function renderProviders() {
  const body = $('#providers-body'); $('#providers-empty').hidden = state.providers.length > 0; body.innerHTML = state.providers.map(provider => `<tr>
    <td class="primary-cell"><strong>${h(provider.name)}</strong><small>${h(provider.base_url)}</small>${provider.last_refresh_error ? `<span class="error-text">${h(provider.last_refresh_error)}</span>` : ''}</td>
    <td><strong>${h(typeLabel(provider.type))}</strong><div class="protocols">${provider.protocols.map(p => `<span class="protocol">${h(p)}</span>`).join('')}</div></td>
    <td><strong>${provider.available_model_count}</strong> available${provider.model_count !== provider.available_model_count ? `<span class="meta-line"> · ${provider.model_count - provider.available_model_count} retired</span>` : ''}</td>
    <td><span class="meta-line">${date(provider.last_refresh_at)}</span></td>
    <td>${badge(provider.enabled && !provider.last_refresh_error, provider.enabled ? (provider.last_refresh_error ? 'Refresh error' : 'Enabled') : 'Disabled', provider.enabled ? (provider.last_refresh_error ? 'warn' : 'good') : 'neutral')}<div class="meta-line">Credential: ${provider.auth_state ? provider.auth_state : (provider.credential_configured ? 'configured' : 'none')}</div></td>
   <td><div class="actions"><button class="btn btn-small btn-secondary" data-provider-refresh="${h(provider.id)}">Refresh</button><button class="btn btn-small btn-secondary" data-provider-edit="${h(provider.id)}">Edit</button><button class="btn btn-small btn-danger" data-provider-delete="${h(provider.id)}">Delete</button></div></td></tr>`).join('');
  $('#providers-empty-mobile').hidden = state.providers.length > 0;
  $('#providers-cards').innerHTML = state.providers.map(providerCard).join('');
  const available = state.providers.reduce((sum, item) => sum + item.available_model_count, 0), retired = state.providers.reduce((sum, item) => sum + item.model_count - item.available_model_count, 0), errors = state.providers.filter(item => item.last_refresh_error).length;
  $('#provider-metrics').innerHTML = metric(state.providers.length, 'Provider instances') + metric(available, 'Available models') + metric(retired, 'Retired models') + metric(errors, 'Refresh errors');
  $$('[data-provider-refresh]').forEach(button => button.onclick = () => refreshProvider(button.dataset.providerRefresh));
  $$('[data-provider-edit]').forEach(button => button.onclick = () => openProvider(state.providers.find(p => p.id === button.dataset.providerEdit)));
  $$('[data-provider-delete]').forEach(button => button.onclick = () => deleteProvider(button.dataset.providerDelete));
  $$('[data-mobile-provider-refresh]').forEach(button => button.onclick = () => refreshProvider(button.dataset.mobileProviderRefresh));
  $$('[data-mobile-provider-edit]').forEach(button => button.onclick = () => openProvider(state.providers.find(p => p.id === button.dataset.mobileProviderEdit)));
  $$('[data-mobile-provider-delete]').forEach(button => button.onclick = () => deleteProvider(button.dataset.mobileProviderDelete));
}
function providerCard(provider) {
  const healthy = provider.enabled && !provider.last_refresh_error;
  const stateLabel = provider.enabled ? (provider.last_refresh_error ? 'Refresh error' : 'Enabled') : 'Disabled';
  return `<article class="mobile-card provider-card" data-provider-id="${h(provider.id)}">
    <div class="mobile-card-head"><div class="mobile-card-title"><span class="status-roundel${healthy ? '' : ' status-roundel-broken'}" role="img" aria-label="${h(stateLabel)}"></span><strong>${h(provider.name)}</strong></div>${badge(healthy, stateLabel, healthy ? 'good' : provider.enabled ? 'warn' : 'neutral')}</div>
    <div class="mobile-card-subtitle">${h(typeLabel(provider.type))} · ${h((provider.protocols || []).join(' · ') || 'provider default')}</div>
    <div class="mobile-card-meta"><span><b>${h(provider.available_model_count)}</b> available</span><span><b>${h(provider.model_count - provider.available_model_count)}</b> retired</span><span>${h(date(provider.last_refresh_at))}</span></div>
    ${provider.last_refresh_error ? `<p class="mobile-card-alert">${h(provider.last_refresh_error)}</p>` : ''}
    <div class="mobile-card-actions"><button class="btn btn-small btn-secondary" data-mobile-provider-refresh="${h(provider.id)}">Refresh</button><button class="btn btn-small btn-secondary" data-mobile-provider-edit="${h(provider.id)}">Edit</button><button class="btn btn-small btn-danger" data-mobile-provider-delete="${h(provider.id)}">Delete</button></div>
  </article>`;
}
const metric = (value, label) => `<div class="metric"><strong>${h(value)}</strong><span>${h(label)}</span></div>`;
const badge = (active, label, kind = active ? 'good' : 'bad') => `<span class="badge badge-${kind}">${h(label)}</span>`;
const typeLabel = type => state.providerTypes.find(item => item.type === type)?.label || type;

$('#add-provider').onclick = () => openProvider();
function providerFields(provider) {
  const options = state.providerTypes.map(item => `<option value="${h(item.type)}" ${provider?.type === item.type ? 'selected' : ''}>${h(item.label)}</option>`).join('');
  const selectedProtocols = provider?.protocols || ['chat'];
  const isOAuthEdit = provider && ['codex-subscription','claude-subscription','github-copilot'].includes(provider.type);
  return `<label>Provider name <input name="name" value="${h(provider?.name || '')}" placeholder="openai-main" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"><small>Lowercase namespace used in client-facing model IDs. Leave blank to use provider type (e.g. DeepSeek → deepseek).</small></label>
    <label>Provider type <select name="type" ${provider ? 'disabled' : ''} required>${options}</select></label>
    ${isOAuthEdit ? `<label>Base URL <input name="base_url" type="url" value="${h(provider?.base_url || '')}" disabled><small>Fixed by the OAuth provider. Reconnect to change the upstream.</small></label>` : `<label>Base URL <input name="base_url" type="url" value="${h(provider?.base_url || '')}" placeholder="https://api.example.com/v1" required></label>`}
      ${provider ? '' : '<div data-credential-create><label data-api-credential>API credential <input name="credential" type="password" autocomplete="new-password"><small>Write-only. Leave empty only when the provider permits unauthenticated access.</small></label></div>'}
    ${provider && !isOAuthEdit ? `<label data-credential-replace>Replacement credential <input name="credential" type="password" autocomplete="new-password" placeholder="•••••••• (set)"><small>A credential is configured. Type a new value to replace it.</small></label>` : ''}
    ${isOAuthEdit ? '<div data-provider-reconnect><label>OAuth connection <button type="button" class="btn btn-secondary" data-provider-reconnect-btn>Reconnect</button><small>Start a fresh OAuth sign-in. Replaces any saved token for this provider.</small></label></div><div data-provider-disconnect><button type="button" class="btn btn-danger" data-provider-disconnect-btn>Disconnect</button><small>Remove the OAuth connection. Provider configuration, models, and routing are preserved.</small></div>' : ''}
    <fieldset class="protocol-select" data-protocol-config><legend>Declared native protocols</legend>${['chat','responses','messages'].map(protocol => `<label><input type="checkbox" name="protocol" value="${protocol}" ${selectedProtocols.includes(protocol) ? 'checked' : ''}> ${protocol}</label>`).join('')}<small>Generic providers default to Chat Completions. Declare only surfaces the upstream implements natively.</small></fieldset>
    <label class="toggle-label"><input class="switch" name="enabled" type="checkbox" ${provider?.enabled !== false ? 'checked' : ''}> Provider enabled</label>
    ${provider ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm_breaking_change" type="checkbox"> <span>Confirm if the provider name changes; every direct model ID will change.</span></label>' : ''}`;
}
 function openProvider(provider = null, onSaved = null) {
   openEntity({ eyebrow: provider ? 'EDIT UPSTREAM' : 'REGISTER UPSTREAM', title: provider ? `Edit ${provider.name}` : 'Add provider', fields: providerFields(provider), submit: provider ? 'Save provider' : 'Add & discover', onMount: form => { const select = $('[name="type"]', form); const protocolConfig = $('[data-protocol-config]', form); const showProtocols = () => protocolConfig.hidden = !['generic-openai','vllm'].includes(provider?.type || select.value); if (!provider) { const base = $('[name="base_url"]', form); const nameInput = $('[name="name"]', form); const credential = $('[name="credential"]', form); const createWrap = $('[data-credential-create]', form); const apiCredential = $('[data-api-credential]', form); const apply = () => { const item = state.providerTypes.find(t => t.type === select.value); if (!base.value || base.dataset.auto === 'true') { base.value = item?.default_base_url || ''; base.dataset.auto = 'true'; } if (nameInput) nameInput.placeholder = select.value || 'openai-main'; const isKeyless = item?.type === 'opencode-free'; const isOAuth = item?.auth_mode === 'oauth'; credential.required = Boolean(item?.credential_needed) && !isOAuth; if (createWrap) createWrap.hidden = isKeyless; if (apiCredential) apiCredential.hidden = isOAuth; if (isOAuth) { credential.value = ''; $('#dialog-submit').textContent = 'Connect with ' + item.label; } else $('#dialog-submit').textContent = 'Add & discover'; showProtocols(); }; base.addEventListener('input', () => base.dataset.auto = 'false'); select.addEventListener('change', apply); apply(); } else { const replaceWrap = $('[data-credential-replace]', form); if (replaceWrap) replaceWrap.hidden = provider.type === 'opencode-free' || (state.providerTypes.find(t => t.type === provider.type)?.auth_mode === 'oauth'); showProtocols(); const reconnectBtn = $('[data-provider-reconnect-btn]', form); if (reconnectBtn) reconnectBtn.onclick = () => { $('#form-dialog').close(); connectProviderOAuth(provider.id); }; const disconnectBtn = $('[data-provider-disconnect-btn]', form); if (disconnectBtn) disconnectBtn.onclick = () => { $('#form-dialog').close(); disconnectProviderOAuth(provider.id); }; const credentialInput = $('[name="credential"]', form); if (credentialInput) { const savedPlaceholder = credentialInput.placeholder; credentialInput.addEventListener('focus', () => { credentialInput.placeholder = ''; }); credentialInput.addEventListener('blur', () => { if (!credentialInput.value) credentialInput.placeholder = savedPlaceholder; }); } const nameInput = $('[name="name"]', form), confirmWrap = $('[data-confirm-wrap]', form); const syncConfirm = () => { confirmWrap.hidden = nameInput.value === provider.name; if (confirmWrap.hidden) { const cb = $('[name="confirm_breaking_change"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', syncConfirm); syncConfirm(); } }, onSubmit: async form => {
     const values = new FormData(form); const rawName = String(values.get('name') || '').trim(); const payload = { name: rawName || String(values.get('type') || '').trim(), base_url: values.get('base_url'), enabled: values.get('enabled') === 'on', protocols: values.getAll('protocol') };
    if (provider) { payload.confirm_breaking_change = values.get('confirm_breaking_change') === 'on'; await api(`/api/admin/providers/${provider.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); if (values.get('credential')) await api(`/api/admin/providers/${provider.id}/credential`, { method: 'PUT', body: JSON.stringify({ credential: values.get('credential') }) }); flash('Provider configuration updated.'); }
     else { payload.type = values.get('type'); payload.credential = values.get('credential'); const result = await api('/api/admin/providers', { method: 'POST', body: JSON.stringify(payload) }); if (['codex-subscription','claude-subscription','github-copilot'].includes(payload.type)) { $('#form-dialog').close(); await loadProviders(); connectProviderOAuth(result.id, onSaved); return; } flash(result.refresh_error || 'Provider saved and catalogue discovered.', result.refresh_error ? 'info' : 'success'); }
    await loadProviders(); await loadClients(); if (onSaved) await onSaved();
  }});
}
async function refreshProvider(id) { const button = $(`[data-provider-refresh="${CSS.escape(id)}"]`); button.disabled = true; try { await api(`/api/admin/providers/${id}/refresh`, { method: 'POST' }); flash('Catalogue refresh completed.'); await loadProviders(); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); await loadProviders(); await loadClients(); } finally { button.disabled = false; } }
 async function connectProviderOAuth(id, onSaved = null) { const provider = state.providers.find(item => item.id === id); const type = provider?.type; try { const result = await api(`/api/admin/providers/${id}/oauth/start`, { method: 'POST' }); if (result.flow === 'device_code') { showGitHubDeviceDialog(id, result, onSaved); return; } window.open(result.authorization_url, 'tiller-oauth-auth', 'popup,width=520,height=720,resizable=yes,scrollbars=yes'); if (result.callback_mode === 'redirect') { showOAuthRedirectDialog(id, result.authorization_url, type, result.redirect_uri, onSaved); return; } showOAuthCallbackDialog(id, result.authorization_url, type, result.redirect_uri, onSaved); } catch (error) { flash(errorMessage(error), 'error'); } }
 function showGitHubDeviceDialog(id, result, onSaved = null) { openEntity({ eyebrow: 'GITHUB COPILOT', title: 'Connect GitHub Copilot', submit: 'Done', fields: `<p>1. Open GitHub device sign-in.<br>2. Enter this code:<br><strong class="device-code">${h(result.user_code)}</strong><br>3. Approve access, then leave this dialog open.</p><p><a class="btn btn-secondary" href="${h(result.verification_uri)}" target="_blank" rel="noopener">Open GitHub</a> <button type="button" class="btn btn-secondary" data-copy-device>Copy code</button></p><p data-oauth-status>Waiting for GitHub authorization...</p>`, onMount: form => { $('[data-copy-device]', form).onclick = () => navigator.clipboard?.writeText(result.user_code); const poll = setInterval(async () => { try { const status = await api(`/api/admin/providers/${id}/oauth/status`); const label = $('[data-oauth-status]', form); if (label) label.textContent = status.status === 'pending' ? 'Waiting for GitHub authorization...' : status.status === 'connected' ? 'GitHub connected.' : (status.error || 'GitHub connection failed.'); if (status.status !== 'pending') { clearInterval(poll); if (status.status === 'connected') { $('#form-dialog').close(); flash('GitHub Copilot connected.'); await loadProviders(); if (onSaved) await onSaved(); } } } catch { /* dialog remains available for transient polling errors */ } }, 2000); form.addEventListener('close', () => clearInterval(poll), { once: true }); }, onSubmit: async () => { await loadProviders(); }}); }
function showOAuthRedirectDialog(id, authorizationURL, type, redirectURI, onSaved = null) { const label = typeLabel(type); openEntity({ eyebrow: label, title: 'Finish sign-in', submit: 'Connect', fields: `<p>1. Finish signing in in the small sign-in window.<br>2. It returns to <code>${h(redirectURI)}</code> automatically. Leave this dialog open.</p><p data-oauth-status>Waiting for sign-in...</p><label>Authorization URL <textarea readonly rows="4">${h(authorizationURL)}</textarea></label><details><summary>Trouble signing in? Paste the redirected URL instead.</summary><label>Redirected URL <textarea name="redirected_url" rows="3" placeholder="${h(redirectURI)}?code=...&state=..."></textarea></label><small>Copy the complete URL from the sign-in window's address bar and paste it above, then choose Connect.</small></details>`, onMount: form => { const poll = setInterval(async () => { try { const status = await api(`/api/admin/providers/${id}/oauth/status`); const node = $('[data-oauth-status]', form); if (status.status === 'connected') { if (node) node.textContent = label + ' connected.'; clearInterval(poll); $('#form-dialog').close(); flash(label + ' connected.'); await loadProviders(); if (onSaved) await onSaved(); } } catch { /* dialog remains available for transient polling errors */ } }, 2000); form.addEventListener('close', () => clearInterval(poll), { once: true }); }, onSubmit: async form => { const value = String(new FormData(form).get('redirected_url') || '').trim(); if (!value) { throw new Error('Finish sign-in in the popup, or paste the complete redirected URL.'); } await api(`/api/admin/providers/${id}/oauth/callback`, { method: 'POST', body: JSON.stringify({ redirected_url: value }) }); flash(label + ' connected.'); await loadProviders(); if (onSaved) await onSaved(); }}); }
 function showOAuthCallbackDialog(id, authorizationURL, type, redirectURI, onSaved = null) { const label = typeLabel(type); openEntity({ eyebrow: label, title: 'Finish sign-in', submit: 'Connect', fields: `<p>1. Finish signing in in the small sign-in window.<br>2. When it redirects to <code>${h(redirectURI)}</code>, copy the complete URL from your browser address bar.<br>3. Paste that URL below. The page may not load; that is expected.</p><label>Authorization URL <textarea readonly rows="4">${h(authorizationURL)}</textarea></label><label>Redirected URL <textarea name="redirected_url" rows="3" required placeholder="${h(redirectURI)}?code=...&state=..."></textarea></label>`, onSubmit: async form => { const value = new FormData(form).get('redirected_url'); await api(`/api/admin/providers/${id}/oauth/callback`, { method: 'POST', body: JSON.stringify({ redirected_url: value }) }); flash(label + ' connected.'); await loadProviders(); if (onSaved) await onSaved(); }}); }
async function refreshModels(id) { const button = $(`[data-refresh-models="${CSS.escape(id)}"]`); button.disabled = true; try { await api(`/api/admin/providers/${id}/refresh`, { method: 'POST' }); flash('Catalogue refresh completed.'); } catch (error) { flash(errorMessage(error), 'error'); } finally { await loadModels(); await loadProviders(); await loadClients(); button.disabled = false; } }
async function deleteProvider(id) {
  const provider = state.providers.find(item => item.id === id);
  const doDelete = async () => {
    try {
      await api(`/api/admin/providers/${id}`, { method: 'DELETE' });
      flash('Provider deleted.');
      await loadProviders();
      await loadClients();
    } catch (error) {
      if (error.status === 409 && error.data?.error?.data) {
        const data = error.data.error.data;
        if (Array.isArray(data.blocked) && data.blocked.length > 0) {
          flash(`Cannot delete ${provider.name}: last target in ${data.blocked.length} chain(s): ${data.blocked.join(', ')}. Repoint those first.`, 'error');
          return;
        }
      }
      flash(errorMessage(error), 'error');
    }
  };
  if (!await confirmAction({ title: `Delete ${provider.name}?`, copy: 'All discovered models and their client permissions will be removed. Non-terminal references in virtual model fallback chains are removed automatically; deletion is blocked while this provider is the last target in any chain.', action: 'Delete provider', typeMatch: provider.name, typeLabel: 'provider name' })) return;
  await doDelete();
}

async function disconnectProviderOAuth(id) { const provider = state.providers.find(item => item.id === id); if (!await confirmAction({ title: `Disconnect ${provider?.name || 'provider'}?`, copy: 'This removes the OAuth connection. Provider configuration, models, and routing are preserved.', action: 'Disconnect', typeMatch: null, typeLabel: '' })) return; try { await api(`/api/admin/providers/${id}/oauth`, { method: 'DELETE' }); flash('Provider disconnected.'); await loadProviders(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function deleteManualModel(id) { const model = state.models.find(item => item.id === id); if (!model || !await confirmAction({ title: `Delete ${model.canonical_model_id}?`, copy: 'This manually-added model will be removed from the provider catalogue.', action: 'Delete model', typeMatch: null, typeLabel: '' })) return; try { await api(`/api/admin/models/${id}`, { method: 'DELETE' }); flash('Manual model deleted.'); await loadModels(); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }
function manualModelFields() {
  const providers = state.providers.filter(item => item.enabled);
  const protocols = ['', 'chat', 'responses', 'messages'];
  return `<label>Provider <select name="provider_id" required>${providers.map(provider => `<option value="${h(provider.id)}">${h(provider.name)}</option>`).join('')}</select></label>
    <label>Provider-native model ID <input name="upstream_model_id" required maxlength="255" placeholder="model-name"><small>Enter the exact model ID accepted by the provider.</small></label>
    <div class="detect-row"><button type="button" class="btn btn-small btn-secondary" data-detect-model>Detect metadata</button><span class="meta-line">Fills from the provider, then models.dev. Blank fields are detected on save.</span></div>
    <label>Display name <input name="display_name" placeholder="Optional"></label>
    <label>Context length <input name="context_length" type="number" min="1" placeholder="Optional"></label>
    <label>Max output tokens <input name="max_output_tokens" type="number" min="1" placeholder="Optional"></label>
    <label>Native protocol <select name="native_protocol">${protocols.map(protocol => `<option value="${protocol}">${protocol || 'Provider default'}</option>`).join('')}</select><small>Leave as provider default unless the upstream surface is known.</small></label>`;
}
function openManualModel() {
  if (!state.providers.some(item => item.enabled)) { flash('Add an enabled provider before adding a model.', 'error'); return; }
  openEntity({
    eyebrow: 'REAL MODEL', title: 'Add manual model', fields: manualModelFields(), submit: 'Add model',
    onMount: form => {
      const button = $('[data-detect-model]', form);
      if (!button) return;
      button.onclick = async () => {
        const providerID = $('[name="provider_id"]', form).value;
        const upstreamID = $('[name="upstream_model_id"]', form).value.trim();
        $('#dialog-error').textContent = '';
        if (!upstreamID) { $('#dialog-error').textContent = 'Enter the provider-native model ID first.'; return; }
        button.disabled = true; const label = button.textContent; button.textContent = 'Detecting…';
        try {
          const result = await api(`/api/admin/providers/${providerID}/models/lookup?upstream_model_id=${encodeURIComponent(upstreamID)}`);
          const set = (name, value) => { if (value) $('[name="' + name + '"]', form).value = value; };
          set('display_name', result.display_name);
          set('context_length', result.context_length);
          set('max_output_tokens', result.max_output_tokens);
          set('native_protocol', result.native_protocol);
          if (!result.display_name && !result.context_length && !result.max_output_tokens && !result.native_protocol) $('#dialog-error').textContent = 'No metadata found; enter values manually or save to retry detection.';
        } catch (error) { $('#dialog-error').textContent = errorMessage(error, 'Could not detect metadata.'); }
        finally { button.disabled = false; button.textContent = label; }
      };
    },
    onSubmit: async form => {
      const values = new FormData(form);
      const number = name => values.get(name) ? Number(values.get(name)) : null;
      await api(`/api/admin/providers/${values.get('provider_id')}/models`, { method: 'POST', body: JSON.stringify({ upstream_model_id: values.get('upstream_model_id'), display_name: values.get('display_name'), context_length: number('context_length'), max_output_tokens: number('max_output_tokens'), native_protocol: values.get('native_protocol') }) });
      flash('Manual model added.'); await loadModels(); await loadClients();
    }
  });
}
$('#add-real-model').onclick = openManualModel;
async function loadModels(search = $('#model-search').value) { const token = ++state.loadToken; const [result, providersResult] = await Promise.all([api(`/api/admin/models?all=1&search=${encodeURIComponent(search || '')}`), api('/api/admin/providers?limit=200')]); if (token !== state.loadToken) return; state.models = result.data; state.providers = providersResult.data; renderModels(); deferUsage(); }
function groupBanner(kind, key, label, note, count, actions = '') { const collapsed = (kind === 'models' ? collapsedModels : kind === 'clients' ? collapsedClients : collapsedVirtual).has(key); const columns = kind === 'virtual' ? 7 : kind === 'clients' ? 7 : 6; const noteMarkup = kind === 'virtual' ? '' : `<span class="meta-line">${h(note)}</span>`; return `<tr class="group-toggle" data-group-toggle="${kind}" data-group-key="${h(key)}" data-expanded="${collapsed ? 'false' : 'true'}" aria-expanded="${collapsed ? 'false' : 'true'}"><td colspan="${columns}"><span class="group-arrow">${collapsed ? GROUP_ARROW.down : GROUP_ARROW.up}</span><span class="group-label">${h(label)}</span><span class="count-badge">${h(count)}</span>${noteMarkup}${actions ? `<span class="banner-actions">${actions}</span>` : ''}</td></tr>`; }
function toggleGroup(event) {
  const header = event.currentTarget;
  const pendingFrame = groupRevealFrames.get(header);
  if (pendingFrame !== undefined) cancelAnimationFrame(pendingFrame);
  groupRevealFrames.delete(header);

  const rows = [];
  let next = header.nextElementSibling;
  while (next && !next.classList.contains('group-toggle')) { rows.push(next); next = next.nextElementSibling; }

  const nowExpanded = header.dataset.expanded !== 'true';
  header.dataset.expanded = String(nowExpanded);
  header.setAttribute('aria-expanded', String(nowExpanded));
  $('.group-arrow', header).textContent = nowExpanded ? GROUP_ARROW.up : GROUP_ARROW.down;
  const store = header.dataset.groupToggle === 'models' ? collapsedModels : header.dataset.groupToggle === 'clients' ? collapsedClients : collapsedVirtual;
  const key = header.dataset.groupKey;
  if (nowExpanded) store.delete(key); else store.add(key);

  if (!nowExpanded) {
    rows.forEach(row => row.classList.add('group-row-hidden'));
    return;
  }
  if (header.dataset.groupToggle !== 'models' || rows.length <= MODEL_EXPAND_BATCH_SIZE) {
    rows.forEach(row => row.classList.remove('group-row-hidden'));
    return;
  }

  let index = 0;
  const revealBatch = () => {
    if (!header.isConnected || header.dataset.expanded !== 'true') {
      groupRevealFrames.delete(header);
      return;
    }
    const end = Math.min(index + MODEL_EXPAND_BATCH_SIZE, rows.length);
    for (; index < end; index += 1) rows[index].classList.remove('group-row-hidden');
    if (index < rows.length) groupRevealFrames.set(header, requestAnimationFrame(revealBatch));
    else groupRevealFrames.delete(header);
  };
  revealBatch();
}
const groupRows = (rows, collapsed) => `${rows.map(row => `<tr class="group-row${collapsed ? ' group-row-hidden' : ''}"${row.attr || ''}>${row.html}</tr>`).join('')}`;
// MODEL_SORT_WINDOWS defines the usage-window cascade for each sortable usage
// column: the selected window leads, then each longer window breaks ties, so a
// 1h sort ranks most-used-in-the-last-hour first and falls back to 24h then 7d
// before any non-usage tiebreak. A 7d sort has no shorter-window tiebreak.
const MODEL_SORT_WINDOWS = { '1h': ['1h', '24h', '7d'], '24h': ['24h', '7d'], '7d': ['7d'] };
// compareModelUsage walks the window cascade in order and returns the first
// non-zero comparison (scaled by direction), or 0 when the rows tie on every
// listed window. direction is +1 for ascending, -1 for descending, so a
// descending usage sort uses descending tiebreaks as well.
function compareModelUsage(a, b, windows, direction) {
  for (const window of windows) {
    const delta = ((a.usage?.[window]) || 0) - ((b.usage?.[window]) || 0);
    if (delta !== 0) return direction * delta;
  }
  return 0;
}
function applyModelSort(models) {
  const rows = models.map(model => ({ model, canonical: model.canonical_model_id || '', provider: model.provider_name || '', usage: state.usage?.real_models?.[model.canonical_model_id] || {} }));
  const direction = sortState.direction === 'asc' ? 1 : -1;
  // canonicalAsc is direction-independent: it is the deterministic final
  // tiebreak so exact ties (including all-zero usage) never inherit the API's
  // catalogue order, which shifts as providers refresh.
  const canonicalAsc = (a, b) => a.canonical.localeCompare(b.canonical);
  return rows.sort((a, b) => {
    switch (sortState.column) {
      case 'canonical': return direction * canonicalAsc(a, b);
      case 'provider': return direction * a.provider.localeCompare(b.provider) || canonicalAsc(a, b);
      case '1h':
      case '24h':
      case '7d': return compareModelUsage(a, b, MODEL_SORT_WINDOWS[sortState.column], direction) || canonicalAsc(a, b);
      default: return canonicalAsc(a, b);
    }
  }).map(row => row.model);
}
function cycleModelSort(column) {
  if (sortState.column === column) {
    sortState.direction = sortState.direction === 'asc' ? 'desc' : 'asc';
  } else {
    sortState.column = column;
    sortState.direction = SORT_DEFAULTS[column] || 'asc';
  }
  renderModels();
}
// shownModels is the set the Models table renders: available models (unless
// "show retired" is checked) owned by an enabled provider.
function shownModels() {
  const disabledProviders = new Set(state.providers.filter(item => !item.enabled).map(item => item.id));
  return state.models.filter(item => !disabledProviders.has(item.provider_id) && ($('#show-retired').checked || item.available));
}
// reorderModelRows re-applies the current sort in place, moving the existing
// <tr> nodes rather than rebuilding the tbody. It exists for the one-time
// correction after usage first arrives: a models table rendered before usage
// was known sorts every row as zero and keeps catalogue order, while the header
// still claims the default "1h ↓". applyModelSort reads the live sortState, so
// this honours whatever column/direction the user has selected — it never
// resets the sort. Event handlers and transient DOM state are preserved because
// the nodes are moved, not replaced.
function reorderModelRows() {
  const body = $('#models-body');
  if (!body) return;
  const rowsByID = new Map();
  $$('tr[data-model-id]', body).forEach(row => rowsByID.set(row.dataset.modelId, row));
  const fragment = document.createDocumentFragment();
  applyModelSort(shownModels()).forEach(model => {
    const row = rowsByID.get(model.id);
    if (row) fragment.appendChild(row);
  });
  body.appendChild(fragment);
}
 function renderModels() {
   const shown = shownModels();
   $('#models-empty').hidden = shown.length > 0;
   $('#models-empty-mobile').hidden = shown.length > 0;
   const rows = applyModelSort(shown);
   const mobile = window.matchMedia('(max-width: 720px)').matches;
   $('#models-body').innerHTML = mobile ? '' : rows.map(model => `<tr data-model-id="${h(model.id)}"><td><code class="model-id">${h(model.canonical_model_id)}</code></td><td><code class="model-provider">${h(model.provider_name)}</code></td><td><code class="model-id">${h(model.upstream_model_id)}</code></td><td>${tok(state.usage?.real_models?.[model.canonical_model_id]?.['1h'], state.usage?.real_cache?.[model.canonical_model_id]?.['1h'], '1h')}</td><td>${tok(state.usage?.real_models?.[model.canonical_model_id]?.['24h'], state.usage?.real_cache?.[model.canonical_model_id]?.['24h'], '24h')}</td><td>${tok(state.usage?.real_models?.[model.canonical_model_id]?.['7d'], state.usage?.real_cache?.[model.canonical_model_id]?.['7d'], '7d')}</td><td><div class="actions">${model.origin === 'manual' ? `<button class="btn btn-small btn-danger" data-model-delete="${h(model.id)}">Delete</button>` : ''}<button class="btn btn-small btn-secondary" data-model-activity="${h(model.canonical_model_id)}">Activity</button><button class="btn btn-small btn-secondary" data-model-capabilities="${h(model.id)}">Capabilities</button></div></td></tr>`).join('');
   $('#models-cards').innerHTML = mobile ? rows.map(modelCard).join('') : '';
  const head = $('#models-body').parentElement.querySelector('thead');
  if (head) {
    $$('th', head).forEach(th => {
      if (th.dataset && th.dataset.sort) {
        th.classList.toggle('sort-active', th.dataset.sort === sortState.column);
        th.onclick = () => cycleModelSort(th.dataset.sort);
      }
    });
  }
   if (!mobile) {
     $$('[data-model-activity]', $('#models-body')).forEach(button => button.onclick = () => openModelActivity(state.models.find(item => item.canonical_model_id === button.dataset.modelActivity), 'real'));
     $$('[data-model-capabilities]', $('#models-body')).forEach(button => button.onclick = event => { event.stopPropagation(); openRealModelCapabilities(state.models.find(item => item.id === button.dataset.modelCapabilities)); });
     $$('[data-model-delete]', $('#models-body')).forEach(button => button.onclick = event => { event.stopPropagation(); deleteManualModel(button.dataset.modelDelete); });
   } else {
     $$('[data-mobile-model-toggle]').forEach(button => button.onclick = () => toggleMobileCard(button));
     $$('[data-mobile-model-activity]').forEach(button => button.onclick = () => openModelActivity(state.models.find(item => item.id === button.dataset.mobileModelActivity), 'real'));
     $$('[data-mobile-model-capabilities]').forEach(button => button.onclick = () => openRealModelCapabilities(state.models.find(item => item.id === button.dataset.mobileModelCapabilities)));
     $$('[data-mobile-model-delete]').forEach(button => button.onclick = () => deleteManualModel(button.dataset.mobileModelDelete));
   }
 }
function mobileUsage(model, kind = 'real') {
  const key = model.canonical_model_id;
  const usage = kind === 'virtual' ? state.usage?.virtual_models?.[key] : state.usage?.real_models?.[key];
  const cache = kind === 'virtual' ? state.usage?.virtual_cache?.[key] : state.usage?.real_cache?.[key];
  return ['1h', '24h', '7d'].map(window => `<span><small>${window}</small>${tok(usage?.[window], cache?.[window], window)}</span>`).join('');
}
function modelCard(model) {
  const available = model.available;
  return `<article class="mobile-card model-card" data-mobile-card="${h(model.id)}">
    <button class="mobile-card-head mobile-card-toggle" data-mobile-model-toggle="${h(model.id)}" aria-expanded="false" aria-controls="mobile-model-detail-${h(model.id)}"><span class="mobile-card-heading"><strong>${h(model.canonical_model_id)}</strong><small>${h(model.provider_name)} · ${h(model.upstream_model_id)}</small></span><span class="mobile-card-chevron" aria-hidden="true">▾</span></button>
    <div class="mobile-card-usage">${mobileUsage(model)}</div>
    <div class="mobile-card-detail" id="mobile-model-detail-${h(model.id)}" hidden><dl class="mobile-detail-grid"><div><dt>Provider</dt><dd>${h(model.provider_name)}</dd></div><div><dt>Native ID</dt><dd><code>${h(model.upstream_model_id)}</code></dd></div><div><dt>Context</dt><dd>${h(capabilityNumber(model.context_length))}</dd></div><div><dt>Max output</dt><dd>${h(capabilityNumber(model.max_output_tokens))}</dd></div></dl><div class="mobile-card-actions"><button class="btn btn-small btn-secondary" data-mobile-model-activity="${h(model.id)}">Activity</button><button class="btn btn-small btn-secondary" data-mobile-model-capabilities="${h(model.id)}">Capabilities</button>${model.origin === 'manual' ? `<button class="btn btn-small btn-danger" data-mobile-model-delete="${h(model.id)}">Delete</button>` : ''}</div></div>
  </article>`;
}
function toggleMobileCard(button) {
  const card = button.closest('[data-mobile-card]');
  const detail = card?.querySelector('.mobile-card-detail');
  if (!detail) return;
  const expanded = detail.hidden;
  const virtualID = button.dataset.mobileVirtualToggle;
  if (virtualID) {
    if (expanded) mobileVirtualExpanded.add(virtualID);
    else mobileVirtualExpanded.delete(virtualID);
  }
  $$('.mobile-card-detail', button.closest('.mobile-card-list') || document).forEach(item => { item.hidden = true; });
  $$('[data-mobile-model-toggle], [data-mobile-virtual-toggle]', button.closest('.mobile-card-list') || document).forEach(item => item.setAttribute('aria-expanded', 'false'));
  detail.hidden = !expanded;
  button.setAttribute('aria-expanded', String(expanded));
}

async function loadVirtual(search = $('#virtual-search').value) {
  const token = ++state.loadToken;
  const [groups, virtualModels, providersResult, modelsResult] = await Promise.all([
    api('/api/admin/virtual-groups?limit=200'), api(`/api/admin/virtual-models?limit=200&search=${encodeURIComponent(search || '')}`), api('/api/admin/providers?limit=200'), api('/api/admin/models?all=1')
  ]);
  if (token !== state.loadToken) return;
  state.groups = groups.data; state.virtualModels = virtualModels.data; state.providers = providersResult.data; state.models = modelsResult.data; renderVirtual(); deferUsage();
}
const RESOLUTION_STALE_MS = 24 * 3600 * 1000;
const RESOLUTION_ICONS = {
  good: '<svg viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 6.5l2.5 2.5 4.5-5.5"/></svg>',
  bad: '<svg viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M3.4 3.4l5.2 5.2M8.6 3.4l-5.2 5.2"/></svg>',
  neutral: '<svg viewBox="0 0 12 12" aria-hidden="true"><circle cx="6" cy="6" r="2.4" fill="currentColor"/></svg>'
};
function resolutionStatus(target) {
  const key = target.provider_model_id || target.target_model_id;
  const legacyKey = `${target.provider_name}/${target.upstream_model_id}`;
  const cooling = state.usage?.target_cooldown?.[key] || state.usage?.target_cooldown?.[legacyKey];
  if (cooling) {
    const label = cooling.origin_error_class
      ? `${cooling.provider}/${cooling.model} failed: ${cooling.origin_error_class}${cooling.origin_error_message ? ` — ${cooling.origin_error_message}` : ''} (in cooldown)`
      : `${cooling.provider}/${cooling.model} failed (in cooldown)`;
    return ['bad', label];
  }
  const health = state.usage?.target_health?.[legacyKey];
  const last = state.usage?.target_last_outcome?.[key]
            || state.usage?.target_last_outcome?.[legacyKey];
  const lastFresh = last?.at && (Date.now() - new Date(last.at).getTime()) <= RESOLUTION_STALE_MS;
  // A recent success always wins. The main page must agree with the green
  // Activity log: one failed fallback attempt must not paint a target
  // unhealthy when the logical request still resolved.
  if (lastFresh && last.is_success) return ['good', 'Resolving successfully'];
  if (health?.success_1h) return ['good', 'Resolved successfully in the last hour'];
  if (health?.failure_1h) return ['bad', 'Failed in the last hour'];
  if (lastFresh) return ['bad', 'Last request failed'];
  if (health?.success_24h) return ['neutral', 'No successful activity in the last hour'];
  return ['neutral', 'No activity recorded'];
}
function resolutionIndicator(target) {
  const status = resolutionStatus(target);
  return `<span class="resolution-indicator resolution-${status[0]}" role="img" aria-label="${status[1]}" title="${status[1]}">${RESOLUTION_ICONS[status[0]]}<span class="resolution-indicator-spin" aria-hidden="true"></span></span>`;
}
function targetActivityKey(virtualID, targetID) {
  return `${virtualID}\u0000${targetID}`;
}
function renderVirtual() {
  const searching = ($('#virtual-search').value || '').trim().length > 0;
  const byGroup = new Map(); state.virtualModels.forEach(model => { const key = model.group_name || '—'; if (!byGroup.has(key)) byGroup.set(key, []); byGroup.get(key).push(model); });
  const groupNames = searching ? [...byGroup.keys()] : [...new Set([...state.groups.map(g => g.name), ...byGroup.keys()])];
  $('#virtual-empty').hidden = state.virtualModels.length > 0 || (!searching && state.groups.length > 0);
  const html = groupNames.sort((a, b) => a.localeCompare(b)).map(name => {
    const models = byGroup.get(name) || [];
    const grp = state.groups.find(g => g.name === name);
    const collapsed = collapsedVirtual.has(name);
    const broken = models.filter(m => !m.available).length;
    const note = broken ? `${broken} broken target` : (models.length ? 'group' : 'empty group');
    const actions = grp ? `<button class="btn btn-small btn-secondary" data-group-edit="${h(grp.id)}">Edit</button><button class="btn btn-small btn-danger" data-group-delete="${h(grp.id)}">Delete</button>` : '';
    return groupBanner('virtual', name, name, note, `${models.length} model${models.length === 1 ? '' : 's'}`, actions) + groupRows(models.map(model => { const targets = model.targets || []; const summary = targets.length ? `<div class="target-summary">${targets.map((target, index) => `<span class="meta-line" data-target-key="${h(target.provider_model_id || `${target.provider_name}/${target.upstream_model_id}`)}">${index + 1}. ${resolutionIndicator(target)}${h(target.provider_name)}/${h(target.upstream_model_id)}${target.enabled ? '' : ' (disabled)'}</span>`).join('')}</div>` : `<span class="meta-line" data-target-key="${h(model.target_provider_name || '')}/${h(model.target_upstream_model_id || '')}">${resolutionIndicator({provider_name:model.target_provider_name,upstream_model_id:model.target_upstream_model_id})}</span><code class="model-id">${h(model.target_provider_name || '')}/${h(model.target_upstream_model_id || '')}</code>`; return { attr: ` data-virtual-id="${h(model.id)}"`, html: `<td><div class="client-name-line"><span class="status-roundel${model.available ? '' : ' status-roundel-broken'}" role="img" aria-label="${h(model.available ? 'Routable' : 'Broken target')}" title="${h(model.available ? 'Routable' : 'Broken target')}"><span class="status-roundel-spin" aria-hidden="true"></span></span><strong>${h(model.canonical_model_id)}</strong></div><span class="meta-line">${h(model.routing_mode === 'ordered_fallback' ? 'Ordered fallback' : 'Fixed')}</span></td><td></td><td>${summary}</td><td>${tok(state.usage?.virtual_models?.[model.canonical_model_id]?.['1h'], state.usage?.virtual_cache?.[model.canonical_model_id]?.['1h'], '1h')}</td><td>${tok(state.usage?.virtual_models?.[model.canonical_model_id]?.['24h'], state.usage?.virtual_cache?.[model.canonical_model_id]?.['24h'], '24h')}</td><td>${tok(state.usage?.virtual_models?.[model.canonical_model_id]?.['7d'], state.usage?.virtual_cache?.[model.canonical_model_id]?.['7d'], '7d')}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-model-activity="${h(model.canonical_model_id)}">Activity</button><button class="btn btn-small btn-secondary" data-virtual-capabilities="${h(model.id)}">Capabilities</button><button class="btn btn-small btn-secondary" data-virtual-edit="${h(model.id)}">Settings</button><button class="btn btn-small btn-danger" data-virtual-delete="${h(model.id)}">Delete</button></div></td>` }; }), collapsed);
  }).join('');
   $('#virtual-body').innerHTML = html;
   $('#virtual-empty-mobile').hidden = state.virtualModels.length > 0 || (!searching && state.groups.length > 0);
   $('#virtual-cards').innerHTML = groupNames.sort((a, b) => a.localeCompare(b)).map(name => {
     const models = byGroup.get(name) || [];
     const group = state.groups.find(item => item.name === name);
     return `<section class="mobile-model-group"><div class="mobile-model-group-head"><span>${h(name)}</span><small>${models.length} model${models.length === 1 ? '' : 's'}</small>${group ? `<span class="mobile-model-group-actions"><button class="btn-link" data-mobile-group-edit="${h(group.id)}">Edit</button><button class="btn-link danger-link" data-mobile-group-delete="${h(group.id)}">Delete</button></span>` : ''}</div>${models.map(virtualModelCard).join('')}${models.length ? '' : '<p class="mobile-card-empty">No models in this group.</p>'}</section>`;
   }).join('');
   patchVirtualActivityRows();
  $$('.group-toggle', $('#virtual-body')).forEach(header => header.onclick = toggleGroup);
  $$('[data-model-activity]', $('#virtual-body')).forEach(button => button.onclick = event => { event.stopPropagation(); openModelActivity(state.virtualModels.find(item => item.canonical_model_id === button.dataset.modelActivity), 'virtual'); });
  $$('[data-virtual-edit]').forEach(button => button.onclick = () => openVirtualModel(state.virtualModels.find(item => item.id === button.dataset.virtualEdit)));
  $$('[data-virtual-capabilities]').forEach(button => button.onclick = event => { event.stopPropagation(); openCapabilities(state.virtualModels.find(item => item.id === button.dataset.virtualCapabilities)); });
  $$('[data-virtual-delete]').forEach(button => button.onclick = () => deleteVirtualModel(button.dataset.virtualDelete));
  $$('[data-group-edit]').forEach(button => button.onclick = event => { event.stopPropagation(); openVirtualGroup(state.groups.find(item => item.id === button.dataset.groupEdit)); });
   $$('[data-group-delete]').forEach(button => button.onclick = event => { event.stopPropagation(); deleteVirtualGroup(button.dataset.groupDelete); });
   $$('[data-mobile-virtual-toggle]').forEach(button => button.onclick = () => toggleMobileCard(button));
    $$('[data-mobile-virtual-capabilities]').forEach(button => button.onclick = () => openCapabilities(state.virtualModels.find(item => item.id === button.dataset.mobileVirtualCapabilities)));
    $$('[data-mobile-virtual-edit]').forEach(button => button.onclick = () => openVirtualModel(state.virtualModels.find(item => item.id === button.dataset.mobileVirtualEdit)));
    $$('[data-mobile-virtual-delete]').forEach(button => button.onclick = () => deleteVirtualModel(button.dataset.mobileVirtualDelete));
    $$('[data-mobile-target-up]').forEach(button => button.onclick = () => moveMobileVirtualTarget(button.dataset.mobileTargetUp, Number(button.dataset.mobileTargetIndex), -1));
    $$('[data-mobile-target-down]').forEach(button => button.onclick = () => moveMobileVirtualTarget(button.dataset.mobileTargetDown, Number(button.dataset.mobileTargetIndex), 1));
    $$('[data-mobile-target-remove]').forEach(button => button.onclick = () => removeMobileVirtualTarget(button.dataset.mobileTargetRemove, Number(button.dataset.mobileTargetIndex)));
    $$('[data-mobile-target-toggle]').forEach(input => input.onchange = () => toggleMobileVirtualTarget(input.dataset.mobileTargetToggle, Number(input.dataset.mobileTargetIndex), input.checked));
    $$('[data-mobile-target-apply]').forEach(button => button.onclick = () => applyMobileVirtualTargets(button.dataset.mobileTargetApply));
    $$('[data-mobile-target-discard]').forEach(button => button.onclick = () => discardMobileVirtualTargets(button.dataset.mobileTargetDiscard));
    $$('[data-mobile-target-add]').forEach(box => mountMobileVirtualAddPicker(box, state.virtualModels.find(item => item.id === box.dataset.mobileTargetAdd)));
   $$('[data-mobile-group-edit]').forEach(button => button.onclick = () => openVirtualGroup(state.groups.find(item => item.id === button.dataset.mobileGroupEdit)));
   $$('[data-mobile-group-delete]').forEach(button => button.onclick = () => deleteVirtualGroup(button.dataset.mobileGroupDelete));
}

function virtualModelCard(model) {
  const targets = mobileVirtualTargets(model);
  const statusLabel = model.available ? 'Routable' : 'Broken target';
  const routable = targets.filter(target => target.enabled && target.available).length;
  const ordered = model.routing_mode === 'ordered_fallback';
  const draft = mobileVirtualDraftState(model.id);
  const expanded = mobileVirtualExpanded.has(model.id);
  const targetRows = targets.length ? targets.map((target, index) => `<li class="mobile-target-row"><span class="target-index">${String(index + 1).padStart(2, '0')}</span>${resolutionIndicator(target)}<span class="mobile-target-name">${h(target.provider_name)}/${h(target.upstream_model_id)}</span>${ordered ? `<label class="mobile-target-toggle"><span>Use</span><input type="checkbox" class="switch" data-mobile-target-toggle="${h(model.id)}" data-mobile-target-index="${index}" ${target.enabled ? 'checked' : ''} aria-label="Use target ${index + 1}"></label><span class="mobile-target-actions"><button type="button" data-mobile-target-up="${h(model.id)}" data-mobile-target-index="${index}" ${index === 0 ? 'disabled' : ''} aria-label="Move target ${index + 1} up">↑</button><button type="button" data-mobile-target-down="${h(model.id)}" data-mobile-target-index="${index}" ${index === targets.length - 1 ? 'disabled' : ''} aria-label="Move target ${index + 1} down">↓</button><button type="button" data-mobile-target-remove="${h(model.id)}" data-mobile-target-index="${index}" ${targets.length <= 1 ? 'disabled' : ''} aria-label="Remove target ${index + 1}">×</button></span>` : ''}${target.enabled ? '' : '<small>disabled</small>'}</li>`).join('') : `<li><span class="target-index">01</span><span>No target configured</span></li>`;
  const addModelControl = ordered ? `<label class="mobile-target-add-wrap"><span>Add model</span><div class="combobox mobile-target-add-combobox" data-mobile-target-add="${h(model.id)}"><input type="text" placeholder="Search available model…" aria-label="Add model to fallback queue"><input type="hidden"></div></label>` : '';
  return `<article class="mobile-card virtual-model-card" data-mobile-card="${h(model.id)}">
    <button class="mobile-card-head mobile-card-toggle" data-mobile-virtual-toggle="${h(model.id)}" aria-expanded="${expanded}" aria-controls="mobile-virtual-detail-${h(model.id)}"><span class="status-roundel${model.available ? '' : ' status-roundel-broken'}" role="img" aria-label="${h(statusLabel)}"></span><span class="mobile-card-heading"><strong>${h(model.canonical_model_id)}</strong><small>${h(model.routing_mode === 'ordered_fallback' ? 'Ordered fallback' : 'Fixed route')}</small></span><span class="virtual-routable-count">${routable} routable</span><span class="mobile-card-chevron" aria-hidden="true">▾</span></button>
    <div class="mobile-card-detail" id="mobile-virtual-detail-${h(model.id)}"${expanded ? '' : ' hidden'}><div class="mobile-target-list"><div class="mobile-target-list-head"><p class="mobile-card-label">${ordered ? 'Fallback order' : 'Target'}</p>${ordered ? '<small>Move, disable, or remove targets here. Changes stay pending until Apply.</small>' : ''}</div><ol>${targetRows}</ol>${addModelControl}${ordered ? '<div class="mobile-target-pending-actions"><button class="btn btn-small btn-primary" type="button" data-mobile-target-apply="' + h(model.id) + '" ' + (draft?.dirty ? '' : 'disabled') + '>Apply changes</button><button class="btn btn-small btn-secondary" type="button" data-mobile-target-discard="' + h(model.id) + '" ' + (draft?.dirty ? '' : 'disabled') + '>Discard</button></div>' : ''}</div><dl class="mobile-detail-grid"><div><dt>Context</dt><dd>${h(capabilityNumber(model.context_length))}</dd></div><div><dt>Max output</dt><dd>${h(capabilityNumber(model.max_output_tokens))}</dd></div></dl><div class="mobile-card-actions"><button class="btn btn-small btn-secondary" data-mobile-virtual-capabilities="${h(model.id)}">Capabilities</button><button class="btn btn-small btn-secondary" data-mobile-virtual-edit="${h(model.id)}">Edit settings</button><button class="btn btn-small btn-danger" data-mobile-virtual-delete="${h(model.id)}">Delete</button></div></div>
  </article>`;
}

function mobileVirtualTargets(model) {
  const draft = mobileVirtualDrafts.get(model.id);
  if (draft) return draft.targets;
  const targets = (model.targets || []).map(target => ({ ...target }));
  mobileVirtualDrafts.set(model.id, { targets, dirty: false });
  return targets;
}

function mobileVirtualDraftState(modelID) {
  return mobileVirtualDrafts.get(modelID);
}

function mobileVirtualAddOptions(model, targets) {
  const providerEnabled = new Map(state.providers.map(provider => [provider.id, provider.enabled]));
  const used = new Set(targets.map(target => target.provider_model_id));
  return state.models
    .filter(item => item.available && providerEnabled.get(item.provider_id) !== false && !used.has(item.id))
    .sort((a, b) => `${a.provider_name}/${a.upstream_model_id}`.localeCompare(`${b.provider_name}/${b.upstream_model_id}`))
    .map(item => ({ id: item.id, label: `${item.provider_name} / ${item.upstream_model_id}` }));
}

function mountMobileVirtualAddPicker(root, model) {
  if (!model) return;
  const input = $('input[type="text"]', root);
  const hidden = $('input[type="hidden"]', root);
  const options = mobileVirtualAddOptions(model, mobileVirtualTargets(model)).map(option => ({ value: option.id, label: option.label, match: option.label }));
  combobox({ input, hidden, options, placeholder: 'Search available model…', onSelect: option => addMobileVirtualTarget(model.id, option.value), minWidth: 260 });
}

function markMobileVirtualDraftDirty(modelID) {
  const draft = mobileVirtualDrafts.get(modelID);
  if (!draft) return;
  draft.dirty = true;
  renderVirtual();
}

function addMobileVirtualTarget(modelID, providerModelID) {
  const model = state.virtualModels.find(item => item.id === modelID);
  const targetModel = state.models.find(item => item.id === providerModelID);
  if (!model || model.routing_mode !== 'ordered_fallback' || !targetModel) return;
  const targets = mobileVirtualTargets(model);
  if (targets.length >= 16) {
    flash('The admin UI supports up to 16 targets.', 'info');
    renderVirtual();
    return;
  }
  if (targets.some(target => target.provider_model_id === providerModelID)) {
    flash('That model is already in the fallback chain.', 'info');
    renderVirtual();
    return;
  }
  targets.push({
    provider_model_id: targetModel.id,
    provider_name: targetModel.provider_name,
    upstream_model_id: targetModel.upstream_model_id,
    enabled: true,
    available: targetModel.available,
  });
  mobileVirtualExpanded.add(modelID);
  markMobileVirtualDraftDirty(modelID);
}

async function persistMobileVirtualTargets(modelID, targets) {
  const model = state.virtualModels.find(item => item.id === modelID);
  if (!model) return;
  const buttons = $$(`[data-mobile-card="${CSS.escape(modelID)}"] button, [data-mobile-card="${CSS.escape(modelID)}"] input`);
  buttons.forEach(button => { button.disabled = true; });
  try {
    await api(`/api/admin/virtual-models/${modelID}`, {
      method: 'PATCH',
      body: JSON.stringify({ targets: targets.map(target => ({ provider_model_id: target.provider_model_id, enabled: target.enabled !== false })) })
    });
    flash('Fallback targets updated. New requests use the new order immediately.');
    mobileVirtualDrafts.delete(modelID);
    await loadVirtual();
  } catch (error) {
    flash(errorMessage(error), 'error');
    renderVirtual();
  }
}

function moveMobileVirtualTarget(modelID, index, direction) {
  const model = state.virtualModels.find(item => item.id === modelID);
  if (!model || model.routing_mode !== 'ordered_fallback') return;
  const targets = mobileVirtualTargets(model);
  const next = index + direction;
  if (index < 0 || next < 0 || next >= targets.length) return;
  [targets[index], targets[next]] = [targets[next], targets[index]];
  mobileVirtualExpanded.add(modelID);
  markMobileVirtualDraftDirty(modelID);
}

function toggleMobileVirtualTarget(modelID, index, enabled) {
  const model = state.virtualModels.find(item => item.id === modelID);
  if (!model || model.routing_mode !== 'ordered_fallback') return;
  const targets = mobileVirtualTargets(model);
  if (!targets[index]) return;
  targets[index].enabled = enabled;
  if (!targets.some(target => target.enabled)) {
    targets[index].enabled = true;
    flash('Keep at least one fallback target enabled.', 'info');
    renderVirtual();
    return;
  }
  mobileVirtualExpanded.add(modelID);
  markMobileVirtualDraftDirty(modelID);
}

function removeMobileVirtualTarget(modelID, index) {
  const model = state.virtualModels.find(item => item.id === modelID);
  if (!model || model.routing_mode !== 'ordered_fallback') return;
  const targets = mobileVirtualTargets(model);
  if (targets.length <= 1) return;
  targets.splice(index, 1);
  mobileVirtualExpanded.add(modelID);
  markMobileVirtualDraftDirty(modelID);
}

async function applyMobileVirtualTargets(modelID) {
  const draft = mobileVirtualDraftState(modelID);
  if (!draft?.dirty) return;
  await persistMobileVirtualTargets(modelID, draft.targets);
}

function discardMobileVirtualTargets(modelID) {
  mobileVirtualDrafts.delete(modelID);
  renderVirtual();
}

function patchVirtualActivityRows() {
  $$('tr[data-virtual-id]', $('#virtual-body')).forEach(row => {
    const model = state.virtualModels.find(item => item.id === row.dataset.virtualId);
    if (!model) return;
    const roundel = $('.status-roundel', row);
    if (!roundel) return;
    roundel.classList.toggle('status-roundel-broken', !model.available);
    patchVirtualSpinner(row, routeActivity(model.id));
  });
}

function patchVirtualSpinner(row, activity) {
  const roundel = $('.status-roundel', row);
  if (!roundel) return;
  const model = state.virtualModels.find(item => item.id === row.dataset.virtualId);
  if (!model) return;
  if (!model.available) {
    roundel.classList.remove('status-roundel-active');
    roundel.setAttribute('aria-label', 'Broken target');
    roundel.title = 'Broken target';
    return;
  }
  const active = activity?.active > 0;
  const streaming = active && activity?.streaming > 0;
  roundel.classList.toggle('status-roundel-active', active);
  const label = streaming ? 'Streaming response' : active ? 'Waiting for upstream response' : 'Routable';
  roundel.setAttribute('aria-label', label);
  roundel.title = label;
}

function clientRoundelLabel(client, activity) {
  if (!client.enabled) return 'Disabled';
  const active = activity?.active > 0;
  const streaming = active && activity?.streaming > 0;
  return streaming ? 'Streaming response' : active ? 'Waiting for upstream response' : 'Enabled';
}

function applyClientRoundel(roundel, client, activity) {
  if (!roundel) return;
  if (!client.enabled) {
    roundel.classList.remove('status-roundel-active');
    roundel.classList.add('status-roundel-broken');
  } else {
    roundel.classList.remove('status-roundel-broken');
    roundel.classList.toggle('status-roundel-active', activity?.active > 0);
  }
  const label = clientRoundelLabel(client, activity);
  roundel.setAttribute('aria-label', label);
  roundel.title = label;
}

function patchClientActivityRows() {
  $$('tr[data-client-id]', $('#clients-body')).forEach(row => {
    const client = state.clients.find(item => item.id === row.dataset.clientId);
    if (!client) return;
    const roundel = $('.status-roundel', row);
    if (!roundel) return;
    roundel.classList.toggle('status-roundel-broken', !client.enabled);
    applyClientRoundel(roundel, client, state.liveRequests[client.id]);
  });
  $$('.client-card[data-client-id]', $('#clients-cards')).forEach(card => {
    const client = state.clients.find(item => item.id === card.dataset.clientId);
    if (!client) return;
    const roundel = $('.status-roundel', card);
    if (!roundel) return;
    roundel.classList.toggle('status-roundel-broken', !client.enabled);
    applyClientRoundel(roundel, client, state.liveRequests[client.id]);
  });
}

function patchClientRoundelRow(row) {
  const client = state.clients.find(item => item.id === row.dataset.clientId);
  if (!client) return;
  applyClientRoundel($('.status-roundel', row), client, state.liveRequests[client.id]);
}

const capabilityNumber = value => value ? new Intl.NumberFormat().format(value) : 'Not reported';
const capFlag = value => value === true ? '✓' : value === false ? '✗' : '—';
const capFlags = c => `<span class="capability-flag" title="Tool calling">T ${capFlag(c.supports_tools)}</span><span class="capability-flag" title="Vision (image input)">V ${capFlag(c.supports_vision)}</span><span class="capability-flag" title="Reasoning">R ${capFlag(c.supports_reasoning)}</span><span class="capability-flag" title="Structured output">S ${capFlag(c.supports_structured_output)}</span>`;
const capabilityValue = value => value == null || value === '' ? 'Not reported' : h(value);
const capabilityList = (values, empty = 'Not reported') => Array.isArray(values) && values.length ? values.map(value => `<span class="capability-chip">${h(value)}</span>`).join('') : `<span class="capability-muted">${empty}</span>`;
const capabilityState = value => value == null ? 'Not reported' : value ? 'Yes' : 'No';
const capabilityBound = value => value == null ? 'No limit reported' : new Intl.NumberFormat().format(value);
function reasoningCapabilities(caps) {
  if (!caps) return `<section class="reasoning-capabilities" data-reasoning-state="unknown" aria-label="Reasoning capabilities"><div class="reasoning-heading"><p class="eyebrow">REASONING CONTROLS</p><strong>Not reported</strong></div><p class="meta-line">The upstream did not report configurable reasoning metadata.</p></section>`;
  const options = Array.isArray(caps.options) ? caps.options : [];
  const thinkingModes = Array.isArray(caps.thinking_modes) ? caps.thinking_modes : [];
  const hasToggle = options.some(option => option.type === 'toggle');
  const selectors = options.length ? options.map(option => {
    if (option.type === 'effort') {
      const aliases = caps.effort_aliases || {};
      const values = (option.values || []).map(value => aliases[value] ? `${value} → ${aliases[value]}` : value);
      return `<div class="reasoning-detail"><small>Effort values</small><div class="capability-chips">${capabilityList(values, 'Any value')}</div></div>`;
    }
    if (option.type === 'toggle') return '';
    if (option.type === 'budget_tokens') return `<div class="reasoning-detail"><small>Token budget</small><div class="reasoning-bounds"><span>Minimum <b>${capabilityBound(option.min)}</b></span><span>Maximum <b>${capabilityBound(option.max)}</b></span></div></div>`;
    return `<div class="reasoning-detail"><small>${h(option.type || 'Selector')}</small><strong>Supported</strong></div>`;
  }).filter(Boolean).join('') : '';
  const hasSelectors = options.length > 0 || thinkingModes.length > 0;
  return `<section class="reasoning-capabilities" data-reasoning-state="known" aria-label="Reasoning capabilities"><div class="reasoning-heading"><p class="eyebrow">REASONING CONTROLS</p><strong>${hasSelectors ? 'Configurable selectors' : 'No configurable selectors'}</strong></div><div class="reasoning-selector-grid">${hasSelectors ? (selectors || `<div class="reasoning-detail"><small>Selector summary</small><strong>Thinking modes reported</strong></div>`) : `<div class="reasoning-empty"><strong>No configurable selectors</strong><span class="meta-line">The source reported reasoning metadata, but no selectable controls.</span></div>`}</div><div class="reasoning-meta-grid"><div class="reasoning-detail"><small>Toggle support</small><strong>${hasToggle ? 'Supported' : 'Not supported'}</strong></div><div class="reasoning-detail"><small>Thinking modes</small><div class="capability-chips">${capabilityList(thinkingModes)}</div></div><div class="reasoning-detail"><small>Default effort</small><strong>${capabilityValue(caps.default_effort)}</strong></div><div class="reasoning-detail"><small>Mandatory</small><strong>${capabilityState(caps.mandatory)}</strong></div><div class="reasoning-detail"><small>Default enabled</small><strong>${capabilityState(caps.default_enabled)}</strong></div><div class="reasoning-detail reasoning-detail-wide"><small>Accepted parameter names</small><div class="capability-chips">${capabilityList(caps.parameters)}</div></div></div></section>`;
}
async function refreshProviderCatalogues(providerIDs) {
  for (const providerID of [...new Set(providerIDs.filter(Boolean))]) await api(`/api/admin/providers/${providerID}/refresh`, { method: 'POST' });
}
function openCapabilities(model) {
  const targets = model.targets || [];
  const usable = targets.filter(target => target.enabled && target.available);
  const contexts = usable.map(target => target.context_length).filter(value => value > 0);
  const outputs = usable.map(target => target.max_output_tokens).filter(value => value > 0);
  const effectiveContext = contexts.length ? Math.min(...contexts) : null;
  const effectiveOutput = outputs.length ? Math.min(...outputs) : null;
  $('#capabilities-title').textContent = `${model.canonical_model_id} capabilities`;
  $('#refresh-capabilities').dataset.kind = 'virtual';
  $('#refresh-capabilities').dataset.modelId = model.id;
  $('#capabilities-content').innerHTML = `<section class="capability-effective"><p class="eyebrow">AGGREGATE / ADVERTISED TO HERMES + V1</p><div class="capability-grid"><div><small>Context window</small><strong>${capabilityNumber(effectiveContext)}</strong></div><div><small>Max output</small><strong>${capabilityNumber(effectiveOutput)}</strong></div></div><div class="capability-flags">${capFlags(model)}</div>${reasoningCapabilities(model.reasoning_capabilities)}<p class="capability-note">An individual fallback target may use its provider default when it does not support a selector from the aggregate.</p></section><section class="capability-targets"><p class="eyebrow">EXACT FALLBACK TARGETS</p>${targets.length ? targets.map((target, index) => `<article class="capability-target"><div class="capability-target-head"><div><strong>${String(index + 1).padStart(2, '0')} · ${h(target.provider_name)}/${h(target.upstream_model_id)}</strong><span class="meta-line">${target.native_protocol ? h(target.native_protocol) : 'Provider default'} · ${target.enabled && target.available ? 'eligible' : h(target.warning || 'not eligible')}</span></div><div class="capability-values"><span><small>Context</small><b>${capabilityNumber(target.context_length)}</b></span><span><small>Output</small><b>${capabilityNumber(target.max_output_tokens)}</b></span></div></div><div class="capability-flags">${capFlags(target)}</div>${reasoningCapabilities(target.reasoning_capabilities)}</article>`).join('') : '<p class="meta-line">No targets configured.</p>'}</section><p class="form-error" id="capabilities-refresh-error" role="alert"></p>`;
  $('#capabilities-dialog').showModal();
}
function openRealModelCapabilities(model) {
  if (!model) return;
  $('#capabilities-title').textContent = `${model.canonical_model_id} capabilities`;
  $('#refresh-capabilities').dataset.kind = 'real';
  $('#refresh-capabilities').dataset.modelId = model.id;
  const modalities = (list) => list && list.length ? h(list.join(', ')) : 'Not reported';
  $('#capabilities-content').innerHTML = `<section class="capability-effective"><p class="eyebrow">ADVERTISED TO HERMES + V1</p><div class="capability-grid"><div><small>Context window</small><strong>${capabilityNumber(model.context_length)}</strong></div><div><small>Max output</small><strong>${capabilityNumber(model.max_output_tokens)}</strong></div></div><div class="capability-flags">${capFlags(model)}</div>${reasoningCapabilities(model.reasoning_capabilities)}</section><section class="capability-targets"><p class="eyebrow">EXACT UPSTREAM</p><article class="capability-target"><div class="capability-target-head"><div><strong>${h(model.provider_name)}/${h(model.upstream_model_id)}</strong><span class="meta-line">${model.native_protocol ? h(model.native_protocol) : 'Provider default'} · ${model.available ? 'available' : 'retired'} · first seen ${date(model.first_seen_at)}</span></div><div class="capability-values"><span><small>Input</small><b>${modalities(model.input_modalities)}</b></span><span><small>Output</small><b>${modalities(model.output_modalities)}</b></span></div></div></article></section><p class="form-error" id="capabilities-refresh-error" role="alert"></p>`;
  $('#capabilities-dialog').showModal();
}
$('#refresh-capabilities').onclick = async () => {
  const button = $('#refresh-capabilities');
  const modelId = button.dataset.modelId;
  button.disabled = true; $('#capabilities-refresh-error').textContent = '';
  try {
    if (button.dataset.kind === 'real') {
      const model = state.models.find(item => item.id === modelId);
      if (!model) return;
      await refreshProviderCatalogues([model.provider_id]);
      await loadModels();
      $('#capabilities-dialog').close();
      openRealModelCapabilities(state.models.find(item => item.id === modelId) || model);
    } else {
      const model = state.virtualModels.find(item => item.id === modelId);
      if (!model) return;
      await refreshProviderCatalogues((model.targets || []).map(target => target.provider_id));
      await loadVirtual();
      $('#capabilities-dialog').close();
      openCapabilities(state.virtualModels.find(item => item.id === model.id) || model);
    }
  } catch (error) { $('#capabilities-refresh-error').textContent = errorMessage(error, 'Could not refresh target capabilities.'); }
  finally { button.disabled = false; }
};
$('#close-capabilities').onclick = $('#done-capabilities').onclick = () => $('#capabilities-dialog').close();
$('#add-virtual-group').onclick = () => openVirtualGroup(); $('#add-virtual-model').onclick = () => openVirtualModel();
function openVirtualGroup(group = null) { openEntity({ eyebrow: 'VIRTUAL NAMESPACE', title: group ? `Rename ${group.name}` : 'Create virtual group', fields: `<label>Group name <input name="name" value="${h(group?.name || '')}" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" placeholder="virtual" required><small>Lowercase slug. Shares the provider namespace.</small></label>${group ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm" type="checkbox"> <span>I understand that every model ID in this group will change.</span></label>' : ''}`, submit: group ? 'Rename group' : 'Create group', onMount: group ? form => { const nameInput = $('[name="name"]', form), wrap = $('[data-confirm-wrap]', form); const sync = () => { wrap.hidden = nameInput.value === group.name; if (wrap.hidden) { const cb = $('[name="confirm"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', sync); sync(); } : null, onSubmit: async form => { const values = new FormData(form); if (group) await api(`/api/admin/virtual-groups/${group.id}`, { method: 'PATCH', body: JSON.stringify({ name: values.get('name'), confirm_breaking_change: values.get('confirm') === 'on' }) }); else await api('/api/admin/virtual-groups', { method: 'POST', body: JSON.stringify({ name: values.get('name') }) }); flash(group ? 'Virtual group renamed.' : 'Virtual group created.'); await loadVirtual(); } }); }
async function deleteVirtualGroup(id) { const group = state.groups.find(item => item.id === id); if (!await confirmAction({ title: `Delete group ${group.name}?`, copy: 'Only empty virtual groups can be deleted.', action: 'Delete group', typeMatch: group.name, typeLabel: 'group name' })) return; try { await api(`/api/admin/virtual-groups/${id}`, { method: 'DELETE' }); flash('Virtual group deleted.'); await loadVirtual(); } catch (error) { flash(errorMessage(error), 'error'); } }
// Position a combobox list within the visual viewport, opening upward when there
// isn't enough room below (e.g. the mobile virtual keyboard is open). The list is
// position:fixed, so coordinates are viewport-relative.
function positionComboboxList(list, input, minWidth) {
  const box = input.getBoundingClientRect();
  const vv = window.visualViewport;
  const vvTop = vv ? vv.offsetTop : 0;
  const vvBottom = vv ? vvTop + vv.height : window.innerHeight;
  const vvWidth = vv ? vv.width : window.innerWidth;
  let width = Math.min(Math.max(box.width, minWidth || 0), vvWidth - 16);
  let left = Math.min(box.left, Math.max(8, vvWidth - width - 8));
  // On phones, widen the fixed list to span its dialog so long
  // provider/model labels aren't truncated to the input's grid column.
  if (vvWidth <= 720) {
    const host = input.closest('dialog');
    if (host) {
      const hb = host.getBoundingClientRect();
      width = Math.min(Math.max(width, hb.width), vvWidth - 16);
      left = Math.max(8, Math.min(hb.left, vvWidth - width - 8));
    }
  }
  const spaceBelow = vvBottom - box.bottom - 8;
  const spaceAbove = box.top - vvTop - 8;
  const openUp = spaceBelow < 200 && spaceAbove > spaceBelow;
  const avail = openUp ? spaceAbove : spaceBelow;
  const maxH = Math.max(80, Math.min(360, avail));
  list.style.maxHeight = `${maxH}px`;
  list.style.left = `${left}px`;
  list.style.width = `${width}px`;
  if (openUp) {
    // Anchor the list's bottom just above the input; it grows upward, capped to
    // the space above (so it stays within the visual viewport).
    list.style.top = 'auto';
    list.style.bottom = `${window.innerHeight - box.top + 3}px`;
  } else {
    list.style.top = `${box.bottom + 3}px`;
    list.style.bottom = 'auto';
  }
}
function combobox({ input, hidden, options, placeholder, onSelect, onEnter, minWidth }) {
  const list = document.createElement('ul'); list.className = 'combobox-list'; list.setAttribute('role', 'listbox'); list.hidden = true;
  input.setAttribute('role', 'combobox'); input.setAttribute('aria-autocomplete', 'list'); input.setAttribute('aria-expanded', 'false'); input.setAttribute('autocomplete', 'off'); input.setAttribute('spellcheck', 'false'); input.placeholder = placeholder || 'Type to filter…';
  input.parentNode.appendChild(list);
  let items = [], active = -1, open = false;
  const close = () => { open = false; list.hidden = true; input.setAttribute('aria-expanded', 'false'); active = -1; };
  const render = (showAll = false) => {
    const term = showAll ? '' : input.value.trim().toLowerCase();
    items = options.filter(opt => !term || opt.label.toLowerCase().includes(term));
    list.innerHTML = items.map((opt, i) => `<li role="option" data-i="${i}" ${i === active ? 'aria-selected="true"' : ''} ${opt.disabled ? 'aria-disabled="true"' : ''}>${h(opt.label)}${opt.muted ? '<small> — retired</small>' : ''}${opt.disabled ? '<small> — unavailable</small>' : ''}</li>`).join('');
    positionComboboxList(list, input, minWidth);
    list.hidden = !items.length; open = !list.hidden; input.setAttribute('aria-expanded', String(open));
    if (active >= items.length) active = items.length - 1;
  };
  const select = i => { const opt = items[i]; if (!opt || opt.disabled) return false; hidden.value = opt.value; input.value = opt.label; onSelect?.(opt); close(); return true; };
  input.addEventListener('input', () => {
    if (hidden.value && !options.some(o => o.value === hidden.value && o.label === input.value)) hidden.value = '';
    if (!hidden.value) {
      const typed = input.value;
      const matches = options.filter(o => !o.disabled && o.match && o.match === typed);
      if (matches.length === 1) {
        const matchIndex = options.indexOf(matches[0]);
        hidden.value = options[matchIndex].value;
        active = matchIndex;
        render();
        return;
      }
    }
    active = -1; render();
  });
  input.addEventListener('click', () => { if (open) close(); else render(true); });
  input.addEventListener('keydown', event => {
    if (!open && (event.key === 'ArrowDown' || event.key === 'ArrowUp')) { event.preventDefault(); render(true); return; }
    if (event.key === 'ArrowDown') { event.preventDefault(); active = Math.min(active + 1, items.length - 1); render(); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); active = Math.max(active - 1, 0); render(); }
    else if (event.key === 'Enter') { event.preventDefault(); const target = active >= 0 ? active : (items.length ? 0 : -1); if (target >= 0 && select(target)) onEnter?.(); }
    else if (event.key === 'Escape') { close(); }
  });
  list.addEventListener('mousedown', event => { event.preventDefault(); const li = event.target.closest('li[data-i]'); if (li) select(Number(li.dataset.i)); });
  input.addEventListener('blur', () => setTimeout(close, 120));
  return { setOptions: next => { options = next; items = options; if (hidden.value && !options.some(o => o.value === hidden.value)) { hidden.value = ''; input.value = ''; } active = -1; close(); }, select };
}
function virtualModelFields(model) {
  const groupOptions = state.groups.map(group => `<option value="${h(group.id)}" ${model?.group_id === group.id ? 'selected' : ''}>${h(group.name)}</option>`).join('');
  const groupField = state.groups.length ? `<label>Virtual group <select name="group_id" ${model ? 'disabled' : ''} required>${groupOptions}</select></label>` : `<label>New virtual group <input name="group_name" value="${h(model?.group_name || 'virtual')}" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" placeholder="virtual" required><small>No group exists yet; this creates one.</small></label>`;
  return `<div class="row">${groupField}<label>Virtual model name <input name="name" value="${h(model?.name || '')}" placeholder="coding" required><small>Stable client-facing identity.</small></label></div><label>Routing mode <select name="routing_mode"><option value="fixed" ${model?.routing_mode !== 'ordered_fallback' ? 'selected' : ''}>Fixed</option><option value="ordered_fallback" ${model?.routing_mode === 'ordered_fallback' ? 'selected' : ''}>Ordered fallback</option></select></label><small class="fallback-hint" data-fallback-hint hidden>Targets run from top to bottom. Turn a target off to skip it, or use the arrows to change its priority.</small><div class="routing-targets" data-fixed-target></div><div class="routing-targets" data-fallback-targets hidden></div><button class="btn btn-small btn-secondary target-add" type="button" data-target-add hidden>+ Add target</button>${model ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm" type="checkbox"> <span>Confirm if changing the virtual model name; this is a breaking client-facing rename.</span></label>' : ''}`;
}
function openVirtualModel(model = null, onSaved = null) { if (!state.models.length) { flash('Discover at least one real model before creating a virtual route.', 'info'); return; } let availableOptions = []; const dialog = $('#form-dialog'); if (model) { dialog.classList.add('virtual-settings-dialog'); dialog.addEventListener('close', () => dialog.classList.remove('virtual-settings-dialog'), { once: true }); } openEntity({ eyebrow: model ? 'ROUTING POLICY' : 'NEW STABLE IDENTITY', title: model ? `Edit ${model.canonical_model_id}` : 'Create virtual model', fields: virtualModelFields(model), submit: model ? 'Apply' : 'Create route', onMount: form => {
  const fixed = $('[data-fixed-target]', form), fallback = $('[data-fallback-targets]', form), mode = $('[name="routing_mode"]', form), addButton = $('[data-target-add]', form), hint = $('[data-fallback-hint]', form);
  const providerEnabled = new Map(state.providers.map(item => [item.id, item.enabled]));
  const options = state.models.filter(item => item.available && providerEnabled.get(item.provider_id) !== false).map(item => ({ value:item.id, label:`${item.provider_name} / ${item.upstream_model_id}`, match:item.upstream_model_id })); availableOptions = options;
  const targets = model?.targets?.length ? model.targets : [{provider_model_id:model?.target_model_id,enabled:true}];
  const makePicker = (target, row = null) => { const box = document.createElement('div'); box.className='combobox'; box.innerHTML='<input type="text" placeholder="Type a provider or model name…"><input type="hidden" name="target_model" required>'; const input=$('input[type="text"]',box), hidden=$('input[type="hidden"]',box); const pickerOptions=[...options]; const stale=target?.provider_model_id && !options.some(item=>item.value===target.provider_model_id); if(stale){ const label=target?.provider_name&&target?.upstream_model_id?`${target.provider_name} / ${target.upstream_model_id}`:target.provider_model_id; pickerOptions.unshift({value:target.provider_model_id,label:`Unavailable · ${label}`,disabled:true}); } const picker=combobox({ input, hidden, options:pickerOptions, placeholder:'Type a provider or model name…' }); picker.setOptions(pickerOptions); const found=options.find(item=>item.value===target?.provider_model_id); if(found) picker.select(options.indexOf(found)); else if(stale){ hidden.value=target.provider_model_id; input.value=pickerOptions[0].label; } else if(!target?.provider_model_id && options.length){ picker.select(0); } const original=target?.provider_model_id||null; input.addEventListener('focus',()=>{ if(input.value||hidden.value){ input.value=''; hidden.value=''; } }); input.addEventListener('blur',()=>{ if(hidden.value) return; if(original){ const idx=options.findIndex(item=>item.value===original); if(idx>=0) picker.select(idx); else { hidden.value=original; input.value=pickerOptions[0].label; } } }); const wrap=document.createElement('div'); wrap.className='combobox-wrap'; wrap.append(box); if(stale){ const err=document.createElement('small'); err.className='target-error'; err.textContent='Remove or replace unavailable model'; wrap.append(err); } return wrap; };
  const updateControls = () => { const rows=$$('.target-row',fallback); rows.forEach((row,index)=>{ $('.target-index',row).textContent=String(index+1).padStart(2,'0'); $('[data-target-up]',row).disabled=index===0; $('[data-target-down]',row).disabled=index===rows.length-1; }); };
   const addFallback = (target = {}) => { const row=document.createElement('div'); row.className='target-row'; const enableLabel=document.createElement('label'); enableLabel.className='target-enable'; enableLabel.title='Enable this target during fallback'; enableLabel.innerHTML='<span class="target-toggle-copy">Use</span>'; const enable=document.createElement('input'); enable.type='checkbox'; enable.className='switch'; enable.checked=target.enabled!==false; enable.setAttribute('aria-label','Enable target'); enableLabel.append(enable); row.append(Object.assign(document.createElement('span'),{className:'target-index'}),makePicker(target),enableLabel); const actions=document.createElement('div'); actions.className='target-actions'; actions.innerHTML='<button type="button" data-target-up title="Move target up" aria-label="Move target up"><span class="target-action-glyph">↑</span><span class="target-action-text">Up</span></button><button type="button" data-target-down title="Move target down" aria-label="Move target down"><span class="target-action-glyph">↓</span><span class="target-action-text">Down</span></button><button type="button" data-target-remove title="Remove target" aria-label="Remove target"><span class="target-action-glyph">×</span><span class="target-action-text">Remove</span></button>'; $('[data-target-up]',actions).onclick=()=>{ const previous=row.previousElementSibling; if(previous) { fallback.insertBefore(row,previous); updateControls(); } }; $('[data-target-down]',actions).onclick=()=>{ const next=row.nextElementSibling; if(next) { fallback.insertBefore(next,row); updateControls(); } }; $('[data-target-remove]',actions).onclick=()=>{ if($$('.target-row',fallback).length>1) { row.remove(); updateControls(); } }; row.append(actions); fallback.append(row); updateControls(); };
   fixed.append(makePicker(targets[0])); targets.forEach(addFallback); const syncMode=()=>{ const ordered=mode.value==='ordered_fallback'; fixed.hidden=ordered; fallback.hidden=!ordered; addButton.hidden=!ordered; hint.hidden=!ordered; }; mode.onchange=syncMode; syncMode(); addButton.onclick=()=>{ if($$('.target-row',fallback).length<16) addFallback(); else flash('The admin UI supports up to 16 targets.', 'info'); };
  const nameInput = $('[name="name"]', form); if (model) { const wrap = $('[data-confirm-wrap]', form); const sync = () => { wrap.hidden = nameInput.value === model.name; if (wrap.hidden) { const cb = $('[name="confirm"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', sync); sync(); }
  }, onSubmit: async form => { const values = new FormData(form); const ordered=values.get('routing_mode')==='ordered_fallback'; const rows=ordered ? $$('.target-row',form) : [ $('[data-fixed-target]',form) ];   const targets=rows.map(row=>({provider_model_id:$('[name="target_model"]',row).value,enabled:ordered ? !!row.querySelector('.target-enable input')?.checked : true})); if(targets.some(target=>!target.provider_model_id)) throw new Error('Choose a target model.'); if(targets.some(target=>target.provider_model_id && !availableOptions.some(o=>o.value===target.provider_model_id))) throw new Error('Replace the unavailable target model before saving.'); const payload = { name: values.get('name'), routing_mode: values.get('routing_mode'), targets }; if (model) { if(!ordered) payload.fixed_target_id=targets[0].provider_model_id; payload.confirm_breaking_change = values.get('confirm') === 'on'; await api(`/api/admin/virtual-models/${model.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); $('#form-dialog').close(); flash('Virtual routing updated. New requests use the new target immediately.'); } else { const groupID = values.get('group_id'); if (groupID) payload.group_id = groupID; else payload.group_name = values.get('group_name'); await api('/api/admin/virtual-models', { method: 'POST', body: JSON.stringify(payload) }); $('#form-dialog').close(); flash('Virtual route created.'); } await loadVirtual(); await loadClients(); if (onSaved) await onSaved(payload.name); } }); }
async function deleteVirtualModel(id) { const model = state.virtualModels.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${model.canonical_model_id}?`, copy: 'Clients using this stable identity will receive model-not-found after deletion.', action: 'Delete virtual model' })) return; try { await api(`/api/admin/virtual-models/${id}`, { method: 'DELETE' }); flash('Virtual model deleted.'); await loadVirtual(); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadClients() {
  const token = ++state.loadToken;
  const search = $('#client-search').value, group = $('#client-group-filter').value;
  const [result, models, virtual, providers] = await Promise.all([api(`/api/admin/client-keys?limit=200&search=${encodeURIComponent(search || '')}&group=${encodeURIComponent(group || '')}`), api('/api/admin/models?all=1'), api('/api/admin/virtual-models?limit=200'), api('/api/admin/providers?limit=200')]);
  if (token !== state.loadToken) return;
  state.clients = result.data; state.models = models.data; state.virtualModels = virtual.data; state.providers = providers.data;
  renderClientGroupFilter();
  renderClients();
  deferUsage();
}
function renderClientGroupFilter() {
  const select = $('#client-group-filter');
  const current = select.value;
  const groups = [...new Set(state.clients.map(c => c.group).filter(Boolean))].sort();
  select.innerHTML = '<option value="">All groups</option>' + groups.map(g => `<option value="${h(g)}">${h(g)}</option>`).join('');
  select.value = groups.includes(current) ? current : '';
}
function singleTargetOptions(client = null) {
  const providerEnabled = new Map(state.providers.map(item => [item.id, item.enabled]));
  const options = [
    ...state.virtualModels.filter(item => item.available).map(item => ({ value:`virtual:${item.id}`, kind:'virtual', id:item.id, label:`Virtual · ${item.canonical_model_id}`, canonical:item.canonical_model_id })),
    ...state.models.filter(item => item.available && providerEnabled.get(item.provider_id) !== false).map(item => ({ value:`real:${item.id}`, kind:'real', id:item.id, label:`Real · ${item.canonical_model_id}`, canonical:item.canonical_model_id }))
  ];
  if (client?.single_target_id && !options.some(item => item.kind === client.single_target_type && item.id === client.single_target_id)) options.unshift({ value:`${client.single_target_type}:${client.single_target_id}`, kind:client.single_target_type, id:client.single_target_id, label:`${client.single_target_type === 'virtual' ? 'Virtual' : 'Real'} · ${client.single_target_canonical || 'Unavailable target'}`, canonical:client.single_target_canonical || 'Unavailable target', disabled:true });
  return options.sort((a,b) => a.label.localeCompare(b.label));
}
function mountSingleTargetPicker(root, client, onChange = null, onEnter = null, prefill = true) {
  const input = $('input[type="text"]', root), hidden = $('input[type="hidden"]', root), options = singleTargetOptions(client);
  const currentValue = client?.single_target_id ? `${client.single_target_type}:${client.single_target_id}` : '';
  const picker = combobox({ input, hidden, options, placeholder:'Search real or virtual models…', onSelect:onChange, onEnter });
  if (prefill) {
    const current = options.find(item => item.value === currentValue);
    if (current) { hidden.value = current.value; input.value = current.label; }
  }
  return picker;
}
function mountInlineRoutePicker(root, client) {
  const box = $('[data-inline-route]', root);
  const input = $('input[type="text"]', box), hidden = $('input[type="hidden"]', box);
  const confirm = $('[data-route-confirm]', root), tick = $('[data-route-tick]', root), cancel = $('[data-route-cancel]', root);
  const options = singleTargetOptions(client);
  const currentValue = client?.single_target_id ? `${client.single_target_type}:${client.single_target_id}` : '';
  const original = options.find(item => item.value === currentValue);
  let pending = null;
  combobox({ input, hidden, options, placeholder:'Search real or virtual models…', minWidth:420, onSelect: opt => { pending = opt.value; confirm.hidden = false; } });
  if (original) { hidden.value = original.value; input.value = original.label; }
  // Focusing a pre-filled route field blanks it so search filters from the
  // first keystroke; the prior target stays recoverable via the cancel button.
  input.addEventListener('focus', () => { if (input.value || hidden.value) { input.value = ''; hidden.value = ''; } });
  // Clicking out without selecting a model reverts the field to the saved route.
  input.addEventListener('blur', () => {
    if (pending) return;
    if (original) { hidden.value = original.value; input.value = original.label; }
    else { hidden.value = ''; input.value = ''; }
  });
  tick.addEventListener('click', async () => {
    if (!pending) return;
    const split = pending.indexOf(':');
    if (split < 1) return;
    tick.disabled = true;
    try {
      await api(`/api/admin/client-keys/${client.id}`, { method: 'PATCH', body: JSON.stringify({ single_target_type: pending.slice(0, split), single_target_id: pending.slice(split + 1) }) });
      flash('Route updated. New requests use the new target immediately.');
      await loadClients();
    } catch (error) { flash(errorMessage(error), 'error'); tick.disabled = false; }
  });
  cancel.addEventListener('click', () => {
    pending = null; confirm.hidden = true;
    if (original) { hidden.value = original.value; input.value = original.label; }
    else { hidden.value = ''; input.value = ''; }
  });
}
// openRoutePicker is the mobile card's quick route change: a target-only
// dialog with the current target pre-selected, instead of the full client
// Settings dialog. It PATCHes just the binding — name and other settings are
// untouched — and new requests use the new target immediately.
function openRoutePicker(client) {
  const dialog = $('#form-dialog');
  // Mobile: render as a bottom sheet that stays above the virtual keyboard.
  dialog.classList.add('route-picker-dialog');
  dialog.addEventListener('close', () => dialog.classList.remove('route-picker-dialog'), { once: true });
  openEntity({
    eyebrow: 'CHANGE ROUTE',
    title: `Route · ${client.name}`,
    fields: `<div class="route-picker-intro"><span class="client-card-label">Current route</span><strong>${h(client.single_target_canonical || 'Unavailable target')}</strong><small>Choose an available real or virtual target. New requests use the new target immediately.</small></div><label>New target <div class="combobox" data-single-target><input type="text"><input type="hidden" name="single_target" required></div><small>Search by provider, model, or canonical model ID.</small></label>`,
    submit: 'Apply route',
    onMount: form => {
      // Start empty and focused so the user can type ahead from the first
      // keystroke, like the desktop inline route box.
      mountSingleTargetPicker($('[data-single-target]', form), client, null, null, false);
    },
    onSubmit: async form => {
      const selected = String(new FormData(form).get('single_target') || ''), split = selected.indexOf(':');
      if (split < 1) throw new Error('Choose an available real or virtual target.');
      await api(`/api/admin/client-keys/${client.id}`, { method: 'PATCH', body: JSON.stringify({ single_target_type: selected.slice(0, split), single_target_id: selected.slice(split + 1) }) });
      flash('Route updated. New requests use the new target immediately.');
      await loadClients();
    }
  });
}
// Keep the mobile route picker (a bottom sheet) above the virtual keyboard.
// The Visual Viewport API reports the keyboard as a shrink of the visual
// viewport; we lift the dialog so it stays above the keyboard. The combobox
// dropdown itself is repositioned by positionComboboxList (see below).
function positionRoutePickerAboveKeyboard() {
  const dialogs = [$('#form-dialog'), $('#permissions-dialog')].filter(Boolean).filter(dialog => dialog.classList.contains('route-picker-dialog') || dialog.classList.contains('permissions-dialog'));
  if (!dialogs.length) return;
  const vv = window.visualViewport;
  if (!vv) return;
  const viewportTop = vv.offsetTop || 0;
  const viewportBottom = viewportTop + vv.height;
  const keyboardOpen = window.innerHeight - viewportBottom > 80 || window.innerHeight - vv.height > 80;
  dialogs.forEach(dialog => {
    if (keyboardOpen) {
      // Android may resize either the visual viewport or the layout viewport.
      // Anchoring both edges to the visual viewport handles both variants and
      // keeps the dialog footer above the IME instead of underneath it.
      dialog.style.top = `${viewportTop + 8}px`;
      dialog.style.bottom = `${Math.max(8, window.innerHeight - viewportBottom + 8)}px`;
      dialog.style.height = `${Math.max(220, vv.height - 16)}px`;
      dialog.style.maxHeight = `${Math.max(220, vv.height - 16)}px`;
      dialog.style.margin = '0';
    } else {
      dialog.style.top = '';
      dialog.style.bottom = '';
      dialog.style.height = '';
      dialog.style.maxHeight = '';
      dialog.style.margin = '';
    }
  });
}
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', positionRoutePickerAboveKeyboard);
  window.visualViewport.addEventListener('scroll', positionRoutePickerAboveKeyboard);
  window.addEventListener('resize', positionRoutePickerAboveKeyboard);
  // Reposition any open combobox list when the keyboard opens/closes so it stays
  // within the visual viewport (e.g. the mobile route picker's dropdown).
  const repositionOpenLists = () => {
    document.querySelectorAll('.combobox-list:not([hidden])').forEach(list => {
      const input = list.parentElement?.querySelector('input[type="text"]');
      if (input) positionComboboxList(list, input);
    });
  };
  window.visualViewport.addEventListener('resize', repositionOpenLists);
  window.visualViewport.addEventListener('scroll', repositionOpenLists);
}
function clientCard(client) {
  const statusDot = `<span class="status-roundel${client.enabled ? '' : ' status-roundel-broken'}" role="img" aria-label="${client.enabled ? 'Enabled' : 'Disabled'}" title="${client.enabled ? 'Enabled' : 'Disabled'}"><span class="status-roundel-spin" aria-hidden="true"></span></span>`;
  const routeSummary = client.type === 'single'
    ? `<code class="model-id ${client.single_target_available === false ? 'client-route-unavailable' : ''}">${h(client.single_target_canonical || 'Unavailable target')}</code>${client.single_target_available === false ? '<span class="client-route-broken">Unavailable</span>' : ''}`
    : `<span class="meta-line">Catalogue — model permissions</span>`;
  const quickAction = client.type === 'single'
    ? `<button class="client-route-button" data-client-route="${h(client.id)}" aria-label="Change route for ${h(client.name)}"><span>Current model</span><strong class="${client.single_target_available === false ? 'client-route-unavailable' : ''}">${h(client.single_target_canonical || 'Unavailable target')}</strong><i aria-hidden="true">›</i></button>`
    : `<button class="client-permission-button" data-client-models="${h(client.id)}" aria-label="Manage permissions for ${h(client.name)}"><span>Catalogue</span><strong>Manage permissions</strong><i aria-hidden="true">›</i></button>`;
  return `<article class="client-card" data-client-id="${h(client.id)}">
    <button class="client-card-head" data-card-toggle="${h(client.id)}" aria-expanded="false" aria-controls="client-detail-${h(client.id)}">
      ${statusDot}
      <span class="client-card-heading"><span class="client-card-name">${h(client.name)}</span></span>
      ${client.group ? `<span class="group-badge">${h(client.group)}</span>` : ''}
      <span class="client-card-chevron" aria-hidden="true">▾</span>
    </button>
    <div class="client-card-quick-actions">${quickAction}</div>
    <div class="client-card-detail" id="client-detail-${h(client.id)}" hidden>
      <div class="client-card-route-panel"><div class="client-card-field-head"><span class="client-card-label">Route</span><span class="client-card-kind">${client.type === 'single' ? 'Single target' : 'Catalogue access'}</span></div><div class="client-card-route">${routeSummary}</div></div>
      <div class="client-card-actions">
        <button class="btn btn-small btn-secondary" data-client-activity="${h(client.id)}">Activity</button>
        <button class="btn btn-small btn-secondary" data-client-rotate="${h(client.id)}">Rotate</button>
        <button class="btn btn-small btn-secondary" data-client-edit="${h(client.id)}">Settings</button>
        <button class="btn btn-small btn-danger" data-client-delete="${h(client.id)}">Delete</button>
      </div>
      <div class="client-card-secondary">
        <div class="client-card-field"><span class="client-card-label">Description</span><span>${h(client.description || 'No description')}</span></div>
        <div class="client-card-field"><span class="client-card-label">Fingerprint</span><code class="secret-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</code></div>
        <div class="client-card-field"><span class="client-card-label">Created</span><span>${date(client.created_at)}</span></div>
        ${client.rotated_at ? `<div class="client-card-field"><span class="client-card-label">Rotated</span><span>${date(client.rotated_at)}</span></div>` : ''}
        <div class="client-card-field"><span class="client-card-label">Type</span><span>${client.type === 'single' ? 'Single' : 'Catalogue'}</span></div>
        ${client.group ? `<div class="client-card-field"><span class="client-card-label">Group</span><span>${h(client.group)}</span></div>` : ''}
      </div>
    </div>
  </article>`;
}
function clientRow(client) {
  const routeCell = client.type === 'single'
    ? `<div class="client-route-picker ${client.single_target_available === false ? 'route-picker-error' : ''}" data-client-id="${h(client.id)}"><div class="combobox" data-inline-route><input type="text" aria-label="Route for ${h(client.name)}"><input type="hidden"></div><div class="route-confirm" data-route-confirm hidden><button class="route-confirm-tick" data-route-tick type="button" title="Apply new route" aria-label="Apply new route">✓</button><button class="route-confirm-cancel" data-route-cancel type="button" title="Cancel" aria-label="Cancel route change">✕</button></div></div>`
    : `<button class="route-button" data-client-models="${h(client.id)}" aria-label="Manage models for ${h(client.name)}"><span>Catalogue</span><strong>Catalogue permissions</strong><i aria-hidden="true">›</i></button>`;
  return { attr: ` data-client-id="${h(client.id)}"`, html: `<td class="primary-cell"><div class="client-name-line"><span class="status-roundel${client.enabled ? '' : ' status-roundel-broken'}" role="img" aria-label="${client.enabled ? 'Enabled' : 'Disabled'}" title="${client.enabled ? 'Enabled' : 'Disabled'}"><span class="status-roundel-spin" aria-hidden="true"></span></span><strong>${h(client.name)}</strong></div><small>${h(client.description || 'No description')}</small></td><td>${routeCell}</td><td>${tok(state.usage?.client_keys?.[client.id]?.['1h'], state.usage?.client_cache?.[client.id]?.['1h'], '1h')}</td><td>${tok(state.usage?.client_keys?.[client.id]?.['24h'], state.usage?.client_cache?.[client.id]?.['24h'], '24h')}</td><td>${tok(state.usage?.client_keys?.[client.id]?.['7d'], state.usage?.client_cache?.[client.id]?.['7d'], '7d')}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-client-activity="${h(client.id)}">Activity</button><button class="btn btn-small btn-secondary" data-client-rotate="${h(client.id)}">Rotate</button><button class="btn btn-small btn-secondary" data-client-edit="${h(client.id)}">Settings</button><button class="btn btn-small btn-danger" data-client-delete="${h(client.id)}">Delete</button></div></td>` };
}
function renderClients() {
  $('#clients-empty').hidden = state.clients.length > 0;
  const mobileByGroup = new Map();
  state.clients.forEach(client => {
    const group = client.group || 'default';
    if (!mobileByGroup.has(group)) mobileByGroup.set(group, []);
    mobileByGroup.get(group).push(client);
  });
  $('#clients-cards').innerHTML = [...mobileByGroup.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([group, clients]) => `<section class="client-group-section"><div class="client-group-section-head"><span>${h(group)}</span><small>${clients.length} client${clients.length === 1 ? '' : 's'}</small></div>${clients.map(clientCard).join('')}</section>`).join('');
  const byGroup = new Map();
  state.clients.forEach(client => { const g = client.group || 'default'; if (!byGroup.has(g)) byGroup.set(g, []); byGroup.get(g).push(client); });
  $('#clients-body').innerHTML = [...byGroup.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([group, clients]) => {
    const singles = clients.filter(c => c.type === 'single').length;
    const catalogues = clients.length - singles;
    const note = singles && catalogues ? `${catalogues} catalogue · ${singles} single` : singles ? `${singles} single` : `${catalogues} catalogue`;
    const collapsed = collapsedClients.has(group);
    return groupBanner('clients', group, group, note, `${clients.length}`, '') + groupRows(clients.map(clientRow), collapsed);
  }).join('');
  patchClientActivityRows();
  $$('.group-toggle', $('#clients-body')).forEach(header => header.onclick = toggleGroup);
  $$('[data-inline-route]').forEach(box => mountInlineRoutePicker(box.closest('.client-route-picker'), state.clients.find(item => item.id === box.closest('.client-route-picker').dataset.clientId)));
  $$('[data-client-models]').forEach(button => button.onclick = () => openPermissions(state.clients.find(item => item.id === button.dataset.clientModels)));
  $$('[data-client-route]').forEach(button => button.onclick = () => openRoutePicker(state.clients.find(item => item.id === button.dataset.clientRoute)));
  $$('[data-client-activity]').forEach(button => button.onclick = () => openActivity(state.clients.find(item => item.id === button.dataset.clientActivity))); $$('[data-client-rotate]').forEach(button => button.onclick = () => rotateClient(button.dataset.clientRotate)); $$('[data-client-edit]').forEach(button => button.onclick = () => openClient(state.clients.find(item => item.id === button.dataset.clientEdit))); $$('[data-client-delete]').forEach(button => button.onclick = () => deleteClient(button.dataset.clientDelete));
}
// Mobile card accordion: tapping a card head expands its detail in place,
// closing any other open card (one open at a time). Action buttons inside the
// detail are not inside the head, so their clicks bubble past this handler.
$('#clients-cards').addEventListener('click', event => {
  const head = event.target.closest('[data-card-toggle]');
  if (!head) return;
  const card = head.closest('.client-card');
  const detail = card.querySelector('.client-card-detail');
  const wasOpen = !detail.hidden;
  $$('.client-card-detail', $('#clients-cards')).forEach(d => { d.hidden = true; });
  $$('[data-card-toggle]', $('#clients-cards')).forEach(b => b.setAttribute('aria-expanded', 'false'));
  if (!wasOpen) { detail.hidden = false; head.setAttribute('aria-expanded', 'true'); }
});
$('#add-client').onclick = () => openClient();
function openClient(client = null, onSaved = null, defaultType = null) {
  const dialog = $('#form-dialog');
  dialog.classList.add('client-form-dialog');
  dialog.addEventListener('close', () => dialog.classList.remove('client-form-dialog'), { once: true });
  const selectedType = client?.type || defaultType;
  const singleFields = `<section data-single-fields ${client?.type === 'single' ? '' : 'hidden'}><label>Client-facing model name <input name="single_model_name" value="${h(client?.single_model_name || 'main')}" pattern="[A-Za-z0-9._~-](?:[A-Za-z0-9._~/-]{0,253}[A-Za-z0-9._~-])?" required><small>This is the only model identity exposed to the client.</small></label><label>Target <div class="combobox" data-single-target><input type="text"><input type="hidden" name="single_target" required></div><small>Search and select an available real or virtual model.</small></label>${client ? '<label class="confirm-check" data-single-confirm hidden><input name="confirm_model_name_change" type="checkbox"> <span>I understand changing this client-facing name may require client reconfiguration.</span></label>' : ''}</section>`;
  const typeField = `<label>Type <select name="type"><option value="catalogue" ${selectedType === 'catalogue' ? 'selected' : ''}>Catalogue — Choose which real and virtual models the client can access</option><option value="single" ${selectedType !== 'catalogue' ? 'selected' : ''}>Single — Expose one model to the client and route all requests to that single model</option></select><small>Single — Expose one model to the client and route all requests to that single model.</small><small>Catalogue — Choose which real and virtual models the client can access.</small></label>`;
  const operationalFields = client ? `<label class="toggle-label"><input class="switch" name="enabled" type="checkbox" ${client.enabled ? 'checked' : ''}> Client key enabled</label><label class="toggle-label"><input class="switch" name="logging_enabled" type="checkbox" ${client.logging_enabled ? 'checked' : ''}> Log requests for this client</label><label>Retention (days) <input name="retention_days" type="number" min="1" step="1" value="${h(client.retention_days)}" required><small>Request logs older than this are pruned.</small></label>` : '';
  openEntity({
    eyebrow: client ? 'CLIENT SETTINGS' : 'ISSUE CREDENTIAL',
    title: client ? `Settings · ${client.name}` : 'Create client',
    fields: `<label>Client name <input name="name" value="${h(client?.name || '')}" placeholder="Hermes Server 3" required></label><label>Description <textarea name="description" rows="3" placeholder="Workload, owner, or deployment note">${h(client?.description || '')}</textarea></label>${typeField}${singleFields}${operationalFields}<label>Group <input name="group" value="${h(client?.group || 'default')}" maxlength="63" placeholder="default"><small>Optional group to organise client keys visually.</small></label>`,
    submit: client ? 'Save client' : 'Create & show key',
    onMount: form => {
      const typeSelect = $('[name="type"]', form), fields = $('[data-single-fields]', form), name = $('[name="single_model_name"]', form);
      mountSingleTargetPicker($('[data-single-target]', form), client);
      const syncType = () => { fields.hidden = typeSelect.value !== 'single'; name.required = !fields.hidden; };
      typeSelect.onchange = syncType;
      syncType();
      if (client?.type === 'single') {
        const wrap = $('[data-single-confirm]', form);
        const syncName = () => { wrap.hidden = name.value === client.single_model_name; if (wrap.hidden) { const cb = $('[name="confirm_model_name_change"]', form); if (cb) cb.checked = false; } };
        name.addEventListener('input', syncName); syncName();
      }
    },
    onSubmit: async form => {
      const values = new FormData(form);
      const payload = { name: values.get('name'), description: values.get('description'), group: values.get('group') };
      if (client) {
        payload.enabled = values.get('enabled') === 'on';
        payload.logging_enabled = values.get('logging_enabled') === 'on';
        payload.retention_days = Number(values.get('retention_days'));
        payload.type = values.get('type');
        if (payload.type === 'single') {
          const selected = String(values.get('single_target') || ''), split = selected.indexOf(':');
          if (split < 1) throw new Error('Choose an available real or virtual target.');
          payload.single_model_name = values.get('single_model_name');
          payload.single_target_type = selected.slice(0, split);
          payload.single_target_id = selected.slice(split + 1);
          payload.confirm_model_name_change = values.get('confirm_model_name_change') === 'on';
        }
        await api(`/api/admin/client-keys/${client.id}`, { method: 'PATCH', body: JSON.stringify(payload) });
        flash('Client settings updated.');
      } else {
        const selected = String(values.get('single_target') || ''), split = selected.indexOf(':');
        payload.type = values.get('type');
        if (payload.type === 'single') {
          if (split < 1) throw new Error('Choose an available real or virtual target.');
          payload.single_model_name = values.get('single_model_name');
          payload.single_target_type = selected.slice(0, split);
          payload.single_target_id = selected.slice(split + 1);
        }
        const result = await api('/api/admin/client-keys', { method: 'POST', body: JSON.stringify(payload) });
        showSecret(result.secret);
        if (onSaved) await onSaved(result, payload);
      }
      await loadClients();
    }
  });
}
async function rotateClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Rotate ${client.name}?`, copy: 'The current secret will stop authenticating immediately. Permissions and metadata are preserved.', action: 'Rotate now' })) return; try { const result = await api(`/api/admin/client-keys/${id}/rotate`, { method: 'POST' }); showSecret(result.secret); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }
async function deleteClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${client.name}?`, copy: 'The client secret will be invalidated immediately and all permissions will be removed.', action: 'Delete client key' })) return; try { await api(`/api/admin/client-keys/${id}`, { method: 'DELETE' }); flash('Client key deleted and invalidated.'); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }

let permissionsSavedHook = null;
async function openPermissions(client, onSaved = null) {
  permissionsSavedHook = onSaved;
  try {
    state.permissionData = await api(`/api/admin/client-keys/${client.id}/permissions`);
    state.modelClient = client;
    $('#permissions-title').textContent = `Manage models · ${client.name}`;
    $('#permission-search').value = '';
    renderPermissions();
    $('#permissions-dialog').showModal();
  }
  catch (error) { flash(errorMessage(error), 'error'); }
}
// enableAllClientModels gives a freshly-created catalogue key access to every
// currently-available model and turns on the per-group "new models default" so
// future models in those groups are enabled too. Retired models keep whatever
// state they already had, matching the permissions dialog's Enable all action.
async function enableAllClientModels(client) {
  const data = await api(`/api/admin/client-keys/${client.id}/permissions`);
  const defaults = (data.groups || []).map(group => ({ kind: group.kind, group_id: group.id, enabled: true }));
  const permissions = (data.groups || []).flatMap(group => group.models.map(model => ({ kind: model.kind, model_id: model.id, enabled: model.available ? true : model.enabled })));
  await api(`/api/admin/client-keys/${client.id}/permissions`, { method: 'PUT', body: JSON.stringify({ defaults, permissions }) });
}
function renderPermissions() {
  const renderGroup = group => {
    const models = group.models.map(model => `<label class="permission-row ${model.available ? '' : 'retired'}" data-canonical="${h(model.canonical_model_id)}"><code>${h(model.canonical_model_id)}</code><input class="switch" type="checkbox" data-permission-kind="${h(model.kind)}" data-model-id="${h(model.id)}" ${model.enabled ? 'checked' : ''} aria-label="Enable ${h(model.canonical_model_id)}"></label>`).join('');
    const groupActions = group.models.length ? `<div class="permission-group-actions"><label class="toggle-label">New models default <input class="switch" type="checkbox" data-default-kind="${h(group.kind)}" data-group-id="${h(group.id)}" ${group.new_models_enabled ? 'checked' : ''}></label><div class="permission-bulk"><button class="btn btn-small btn-secondary" data-group-enable="${h(group.kind)}:${h(group.id)}" type="button">Enable all</button><button class="btn btn-small btn-secondary" data-group-disable="${h(group.kind)}:${h(group.id)}" type="button">Disable all</button></div></div>` : `<label class="toggle-label">New models default <input class="switch" type="checkbox" data-default-kind="${h(group.kind)}" data-group-id="${h(group.id)}" ${group.new_models_enabled ? 'checked' : ''}></label>`;
    const groupKey = `${group.kind}:${group.id}`;
    const collapsed = collapsedPermissionGroups.has(groupKey);
    const title = group.models.length ? `<div class="permission-group-title"><button class="permission-collapse" data-permission-collapse="${h(groupKey)}" aria-expanded="${collapsed ? 'false' : 'true'}" aria-label="Collapse ${h(group.name)}">${collapsed ? GROUP_ARROW.down : GROUP_ARROW.up}</button><h3>${h(group.name)} <span class="protocol">${h(group.kind)}</span></h3></div>` : `<h3>${h(group.name)} <span class="protocol">${h(group.kind)}</span></h3>`;
    return `<section class="permission-group"><header class="permission-group-head">${title}${groupActions}</header><div class="permission-list${collapsed ? ' permission-list-hidden' : ''}">${models || '<p class="meta-line">No models in this group.</p>'}</div></section>`;
  };
  const renderSection = kind => {
    const groups = state.permissionData.groups.filter(g => g.kind === kind);
    if (!groups.length) return '';
    const collapsed = collapsedPermissionSections.has(kind);
    const label = kind === 'real' ? 'REAL MODELS' : 'VIRTUAL MODELS';
    return `<section class="permission-section"><header class="permission-section-head"><button class="permission-collapse" data-permission-section="${h(kind)}" aria-expanded="${collapsed ? 'false' : 'true'}" aria-label="Collapse ${h(label)}">${collapsed ? GROUP_ARROW.down : GROUP_ARROW.up}</button><h3>${h(label)}</h3></header><div class="permission-section-body${collapsed ? ' permission-section-hidden' : ''}">${groups.map(renderGroup).join('')}</div></section>`;
  };
  $('#permission-groups').innerHTML = ['real', 'virtual'].map(renderSection).join('');
  // The search term controls visibility only; it must never mutate permissions.
  // These handlers update the in-memory state before any re-render so unsaved
  // toggles survive filtering. state.permissionData is the single source of truth.
  $$('[data-model-id]', $('#permission-groups')).forEach(input => input.addEventListener('change', () => {
    const model = state.permissionData.groups.flatMap(g => g.models).find(m => m.kind === input.dataset.permissionKind && m.id === input.dataset.modelId);
    if (model) model.enabled = input.checked;
  }));
  $$('[data-group-id]', $('#permission-groups')).forEach(input => input.addEventListener('change', () => {
    const group = state.permissionData.groups.find(g => g.kind === input.dataset.defaultKind && g.id === input.dataset.groupId);
    if (group) group.new_models_enabled = input.checked;
  }));
  $$('[data-group-enable]', $('#permission-groups')).forEach(btn => btn.addEventListener('click', () => bulkSetGroupPermissions(btn.dataset.groupEnable, true)));
  $$('[data-group-disable]', $('#permission-groups')).forEach(btn => btn.addEventListener('click', () => bulkSetGroupPermissions(btn.dataset.groupDisable, false)));
  $$('[data-permission-collapse]', $('#permission-groups')).forEach(btn => btn.addEventListener('click', () => togglePermissionGroup(btn.dataset.permissionCollapse)));
  $$('[data-permission-section]', $('#permission-groups')).forEach(btn => btn.addEventListener('click', () => togglePermissionSection(btn.dataset.permissionSection)));
  // Clicking anywhere on a section or group header toggles collapse, except
  // on the interactive controls inside (arrow, switch, Enable/Disable buttons).
  $$('.permission-section-head', $('#permission-groups')).forEach(head => head.addEventListener('click', (event) => {
    if (event.target.closest('button, input, label.toggle-label')) return;
    togglePermissionSection(head.querySelector('[data-permission-section]').dataset.permissionSection);
  }));
  $$('.permission-group-head', $('#permission-groups')).forEach(head => head.addEventListener('click', (event) => {
    if (event.target.closest('button, input, label.toggle-label')) return;
    const btn = head.querySelector('[data-permission-collapse]');
    if (btn) togglePermissionGroup(btn.dataset.permissionCollapse);
  }));
}
function togglePermissionSection(key) {
  if (collapsedPermissionSections.has(key)) collapsedPermissionSections.delete(key);
  else collapsedPermissionSections.add(key);
  const arrow = document.querySelector(`[data-permission-section="${key}"]`);
  const body = arrow.closest('.permission-section').querySelector('.permission-section-body');
  const collapsed = collapsedPermissionSections.has(key);
  body.classList.toggle('permission-section-hidden', collapsed);
  arrow.textContent = collapsed ? GROUP_ARROW.down : GROUP_ARROW.up;
  arrow.setAttribute('aria-expanded', String(!collapsed));
}
function togglePermissionGroup(key) {
  if (collapsedPermissionGroups.has(key)) collapsedPermissionGroups.delete(key);
  else collapsedPermissionGroups.add(key);
  const arrow = document.querySelector(`[data-permission-collapse="${key}"]`);
  const list = arrow.closest('.permission-group').querySelector('.permission-list');
  const collapsed = collapsedPermissionGroups.has(key);
  list.classList.toggle('permission-list-hidden', collapsed);
  arrow.textContent = collapsed ? GROUP_ARROW.down : GROUP_ARROW.up;
  arrow.setAttribute('aria-expanded', String(!collapsed));
}
function applyPermissionFilter(term) {
  const t = term.toLowerCase();
  $$('.permission-row', $('#permission-groups')).forEach(row => {
    row.hidden = t && !row.dataset.canonical.toLowerCase().includes(t);
  });
  // Hide a provider (group) row when the search matches none of its models,
  // mirroring the Models tab. Search only affects visibility, never permissions.
  $$('.permission-group', $('#permission-groups')).forEach(group => {
    const rows = $$('.permission-row', group);
    group.hidden = t && rows.every(row => row.hidden);
  });
  // Hide a section when the search matches none of its groups.
  $$('.permission-section', $('#permission-groups')).forEach(section => {
    const groups = $$('.permission-group', section);
    section.hidden = t && groups.every(group => group.hidden);
  });
}
$('#permission-search').addEventListener('input', event => applyPermissionFilter(event.target.value));
function bulkSetGroupPermissions(groupKey, enabled) {
  // A per-group bulk action targets only AVAILABLE models in that group
  // (retired/unavailable are preserved), ignoring the active search filter.
  // It mutates only the in-memory checkboxes; nothing touches new_models_enabled.
  const [kind, id] = groupKey.split(':');
  const group = state.permissionData.groups.find(g => g.kind === kind && g.id === id);
  if (!group) return;
  group.models.forEach(model => { if (model.available) model.enabled = enabled; });
  group.models.forEach(model => {
    const cb = document.querySelector(`[data-permission-kind="${model.kind}"][data-model-id="${model.id}"]`);
    if (cb) cb.checked = model.enabled;
  });
}
function bulkSetAllPermissions(enabled) {
  // A global bulk action targets only AVAILABLE models across every group
  // (retired/unavailable are preserved), regardless of the search filter. It
  // mutates only the in-memory checkboxes; nothing touches new_models_enabled.
  state.permissionData.groups.forEach(group => group.models.forEach(model => {
    if (model.available) model.enabled = enabled;
  }));
  state.permissionData.groups.forEach(group => group.models.forEach(model => {
    const cb = document.querySelector(`[data-permission-kind="${model.kind}"][data-model-id="${model.id}"]`);
    if (cb) cb.checked = model.enabled;
  }));
}
$('#enable-all-permissions').onclick = () => bulkSetAllPermissions(true);
$('#disable-all-permissions').onclick = () => bulkSetAllPermissions(false);
$('#close-permissions').onclick = $('#cancel-permissions').onclick = () => { permissionsSavedHook = null; $('#permissions-dialog').close(); };
$('#save-permissions').onclick = async () => {
  const button = $('#save-permissions'), client = state.modelClient;
  button.disabled = true;
  $('#permissions-error').textContent = '';
  try {
    const defaults = state.permissionData.groups.map(group => ({ kind: group.kind, group_id: group.id, enabled: group.new_models_enabled }));
    const permissions = state.permissionData.groups.flatMap(group => group.models.map(model => ({ kind: model.kind, model_id: model.id, enabled: model.enabled })));
    await api(`/api/admin/client-keys/${client.id}/permissions`, { method: 'PUT', body: JSON.stringify({ defaults, permissions }) });
    flash('Client catalogue permissions saved.');
    const hook = permissionsSavedHook; permissionsSavedHook = null;
    $('#permissions-dialog').close();
    await loadClients();
    if (hook) await hook();
  } catch (error) { $('#permissions-error').textContent = errorMessage(error); }
  finally { button.disabled = false; }
};

const ACTIVITY_PAGE_SIZE = 20;
const ATTEMPT_HYDRATE_CONCURRENCY = 4;

function activityNeedsAttempts(row) { return !row.attempts && !row.attemptsError && (row.attempt_rows > 1 || row.error_text || row.fallback_used); }

// activityDetailHTML is the shared "Resolved" cell / card-detail body. The full
// attempt list is rendered here once row.attempts has been backfilled, so both
// the table and the mobile card surface stay identical.
function activityDetailHTML(row, kind) {
  const fixedAttempt = row.resolved_provider ? { provider: row.resolved_provider, model: row.resolved_model || '', failure_class: row.error_text, latency_ms: row.latency_ms, http_status: row.http_status, error_message: row.error_message, error_body: row.error_body, error_body_truncated: row.error_body_truncated, request_body: row.request_body, request_body_truncated: row.request_body_truncated, result: row.http_status >= 200 && row.http_status < 300 ? 'success' : 'failed' } : null;
  const resolved = row.fallback_used ? '' : fixedAttempt ? (row.attempt_rows > 1 ? '' : `<div class="attempt-sequence">${activityAttemptDetails(fixedAttempt, 0)}</div>`) : '';
  const sequence = (row.fallback_used || !fixedAttempt || row.attempt_rows > 1) ? attemptSequence(row) : '';
  const errorHover = [row.error_message, row.latency_ms ? `Latency: ${row.latency_ms} ms` : ''].filter(Boolean).join(' · ');
  const error = sequence ? '' : (row.error_text ? `<span class="error-text"${errorHover ? ` title="${h(errorHover)}"` : ''}>${h(row.error_text)}</span>` : '');
  const loadError = row.attemptsError ? `<span class="error-text activity-attempts-error">${h(row.attemptsError)}</span><button type="button" class="activity-retry" data-activity-retry="${h(row.id)}">Retry</button>` : '';
  const pending = activityNeedsAttempts(row) ? '<span class="activity-detail-pending" aria-hidden="true">…</span>' : '';
  // The mobile card already shows error_text in its footer, so its detail body
  // only carries the hydrated attempt sequence (plus load errors / retry).
  const parts = kind === 'card' ? [sequence] : [resolved, sequence, error];
  return `${parts.join('')}${loadError}${pending}`;
}
function resolvedActivity(row) { return activityDetailHTML(row, 'table'); }
function activityCardDetail(row) { return activityDetailHTML(row, 'card'); }

// createActivityFeed renders a window of rows immediately, backfills each row's
// attempt details as it scrolls into view (bounded concurrency), and loads
// older pages when the scroll container nears its end. No detail fetch blocks
// the render.
function createActivityFeed(options) {
  const state = options.state;
  state.rows = state.rows || [];
  state.offset = state.offset || 0;
  state.limit = state.limit || ACTIVITY_PAGE_SIZE;
  state.search = state.search || '';
  if (state.hasMore === undefined) state.hasMore = true;
  state.loading = false;
  state.appending = false;
  state.controller = null;
  state.hydrating = new Set();
  state.queued = new Set();
  state.queue = [];
  state.active = 0;
  let rowsIO = null;
  const scrollHosts = new Set();
  const feed = { state, reload: () => load(true), setSearch(value) { state.search = value; state.offset = 0; return load(true); }, loadMore, hydrate };
  const containers = () => (options.containers ? options.containers() : []);
  function observeRows() {
    if (rowsIO) rowsIO.disconnect();
    rowsIO = new IntersectionObserver(entries => {
      entries.forEach(entry => {
        if (!entry.isIntersecting) return;
        rowsIO.unobserve(entry.target);
        const row = state.rows.find(item => item.id === entry.target.dataset.activityRow);
        if (row) hydrate(row);
      });
    }, { rootMargin: '120px' });
    containers().forEach(container => { if (container) $$('[data-activity-row]', container).forEach(el => rowsIO.observe(el)); });
  }
  function onScroll(event) {
    const el = event.currentTarget === window ? document.scrollingElement : event.currentTarget;
    if (!el) return;
    if (el.scrollHeight - el.scrollTop - el.clientHeight > 160) return;
    if (options.scrollGuard && !options.scrollGuard(event.currentTarget)) return;
    loadMore();
  }
  function attachScrollLoading() {
    (options.scrollHosts ? options.scrollHosts() : []).forEach(host => {
      if (!host || scrollHosts.has(host)) return;
      scrollHosts.add(host);
      host.addEventListener('scroll', onScroll, { passive: true });
    });
  }
  function patch(row) {
    containers().forEach(container => {
      if (!container) return;
      $$('[data-activity-resolved]', container).forEach(el => {
        if (el.dataset.activityResolved !== row.id) return;
        el.innerHTML = el.dataset.activityDetailKind === 'card' ? activityCardDetail(row) : resolvedActivity(row);
      });
    });
  }
  function hydrate(row) {
    if (!activityNeedsAttempts(row) || state.queued.has(row.id) || state.hydrating.has(row.id)) return;
    state.queued.add(row.id);
    state.queue.push(row);
    pump();
  }
  function pump() {
    while (state.active < ATTEMPT_HYDRATE_CONCURRENCY && state.queue.length) {
      const row = state.queue.shift();
      state.queued.delete(row.id);
      if (row.attempts || state.hydrating.has(row.id)) continue;
      state.hydrating.add(row.id);
      state.active += 1;
      const controller = state.controller;
      api(`/api/admin/activity/${encodeURIComponent(row.id)}/attempts`, { signal: controller?.signal })
        .then(result => { row.attempts = result.data || []; delete row.attemptsError; })
        .catch(error => { if (error.name !== 'AbortError') row.attemptsError = errorMessage(error, 'Could not load attempt details.'); })
        .finally(() => { state.hydrating.delete(row.id); state.active -= 1; if (controller === state.controller) patch(row); pump(); });
    }
  }
  function loadMore() { if (state.loading || state.appending || !state.hasMore) return; state.appending = true; load(false); }
  async function load(reset) {
    if (reset) {
      state.controller?.abort();
      state.controller = new AbortController();
      state.queue.length = 0;
      state.queued.clear();
      state.loading = true;
      state.appending = false;
      state.offset = 0;
      state.rows = [];
      options.render(state);
      observeRows();
      options.resetScroll();
    }
    const signal = state.controller?.signal;
    try {
      const result = await options.fetch(state.offset, state.limit, state.search, signal);
      const fetched = result.data || [];
      state.hasMore = fetched.length > state.limit;
      const page = fetched.slice(0, state.limit);
      state.rows = reset ? page : state.rows.concat(page);
      state.offset = state.rows.length;
      state.loading = false;
      state.appending = false;
      options.showError('');
      if (reset) options.render(state); else options.append(state, page);
      observeRows();
      attachScrollLoading();
    } catch (error) {
      if (error.name === 'AbortError') return;
      state.loading = false;
      state.appending = false;
      if (reset) { state.rows = []; options.render(state); observeRows(); }
      options.showError(errorMessage(error));
    }
  }
  containers().forEach(container => {
    if (!container) return;
    container.addEventListener('click', event => {
      const button = event.target.closest('[data-activity-retry]');
      if (!button) return;
      const row = state.rows.find(item => item.id === button.dataset.activityRetry);
      if (!row) return;
      delete row.attemptsError;
      patch(row);
      hydrate(row);
    });
  });
  return feed;
}

function updateActivityCount(state, selector) { $(selector).textContent = state.loading ? 'Loading…' : (state.rows.length ? `1–${state.rows.length}` : '0 results'); }
function activityTableRow(row, showClient) { return `<tr data-activity-row="${h(row.id)}"><td><span class="meta-line">${date(row.created_at)}</span></td>${showClient ? `<td><strong class="client-name">${h(row.client_name || '')}</strong></td>` : ''}<td>${requestIdentity(row)}</td><td data-activity-resolved="${h(row.id)}" data-activity-detail-kind="table">${resolvedActivity(row)}</td><td><span class="protocol">${h(row.protocol)}</span>${row.streaming ? '<span class="protocol">stream</span>' : ''}</td><td><span class="meta-line">${row.latency_ms} ms</span></td><td>${rowCache(row)}</td><td>${activityRequestID(row)}</td></tr>`; }

const activityState = { kind: '', client: null, modelID: '', modelName: '', rows: [], offset: 0, limit: ACTIVITY_PAGE_SIZE, search: '', hasMore: true, loading: false };
function resetActivityScroll() { $('.activity-table-shell', $('#activity-dialog')).scrollTop = 0; }
function activityFeedURL(kind, client, modelID, offset, limit, search) {
  const base = kind === 'client' ? `/api/admin/client-keys/${client.id}/activity` : kind === 'real' ? `/api/admin/models/${modelID}/activity` : `/api/admin/virtual-models/${modelID}/activity`;
  return `${base}?limit=${limit + 1}&offset=${offset}&search=${encodeURIComponent(search || '')}`;
}
function renderActivity(state) { errorDetails.clear(); errorDetailSeq = 0; const showClient = !state.client; $('#activity-client-head').hidden = !showClient; $('#activity-empty').hidden = state.loading || state.rows.length > 0; $('#activity-body').innerHTML = state.loading ? '<tr><td colspan="8"><span class="meta-line">Loading activity…</span></td></tr>' : state.rows.map(row => activityTableRow(row, showClient)).join(''); $('#activity-mobile-body').innerHTML = state.loading ? '<p class="meta-line">Loading activity…</p>' : state.rows.map(row => activityHistoryCard(row, showClient)).join(''); updateActivityCount(state, '#activity-count'); }
function appendActivity(state, page) { const showClient = !state.client; const table = $('#activity-body'); const cards = $('#activity-mobile-body'); page.forEach(row => { table.insertAdjacentHTML('beforeend', activityTableRow(row, showClient)); cards.insertAdjacentHTML('beforeend', activityHistoryCard(row, showClient)); }); $('#activity-empty').hidden = state.rows.length > 0; updateActivityCount(state, '#activity-count'); }
const activityFeed = createActivityFeed({
  state: activityState,
  containers: () => [$('#activity-body'), $('#activity-mobile-body')],
  scrollHosts: () => [$('#activity-dialog .activity-table-shell'), $('#activity-mobile-body')],
  resetScroll: resetActivityScroll,
  showError: message => { $('#activity-error').textContent = message; },
  fetch: (offset, limit, search, signal) => api(activityFeedURL(activityState.kind, activityState.client, activityState.modelID, offset, limit, search), { signal }),
  render: renderActivity,
  append: appendActivity,
});
function loadActivity() { if (!activityState.kind) return Promise.resolve(); return activityFeed.reload(); }
async function openActivity(client) {
  activityState.kind = 'client'; activityState.client = client; activityState.modelID = ''; activityState.modelName = ''; activityState.offset = 0; activityState.search = '';
  $('#activity-search').value = '';
  $('#activity-title').textContent = `${client.name} activity`;
  $('#clear-activity').hidden = false;
  const toolbar = $('.activity-toolbar', $('#activity-dialog'));
  let info = $('#activity-client-info');
  if (!info) {
    info = document.createElement('div');
    info.id = 'activity-client-info';
    info.className = 'activity-client-info';
    toolbar.before(info);
  }
  info.innerHTML = `<span class="activity-client-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</span>  <span>Created ${date(client.created_at)}</span>${client.rotated_at ? `  <span>Rotated ${date(client.rotated_at)}</span>` : ''}`;
  $('#activity-dialog').showModal();
  resetActivityScroll();
  await loadActivity();
}
async function openModelActivity(model, kind) { activityState.kind = kind; activityState.client = null; activityState.modelID = model.id; activityState.modelName = model.canonical_model_id; activityState.offset = 0; activityState.search = ''; $('#activity-search').value = ''; $('#activity-title').textContent = `${model.canonical_model_id} activity`; $('#clear-activity').hidden = true; document.getElementById('activity-client-info')?.remove(); $('#activity-dialog').showModal(); resetActivityScroll(); await loadActivity(); }
function activityAttemptDetails(attempt, index) {
  const route = `${attempt.provider}/${attempt.model}`;
  const isCooldown = attempt.failure_class === 'cooldown';
  const isSkipped = attempt.result === 'skipped';
  const status = attempt.http_status ? `HTTP ${attempt.http_status}` : (isCooldown ? 'cooldown' : h(attempt.result));
  let cls;
  if (attempt.result === 'success') cls = 'attempt-success';
  else if (attempt.result === 'failed') cls = 'attempt-failed';
  else if (isCooldown) cls = 'attempt-cooldown';
  else cls = 'attempt-neutral';
  let clickAttr = '';
  let hoverAttr = '';
  if (attempt.result === 'failed') {
    const key = ++errorDetailSeq;
    const parts = [];
    if (attempt.error_message) parts.push(attempt.error_message);
    if (attempt.failure_class) parts.push(`Resolver: ${attempt.failure_class}`);
    if (attempt.error_body) parts.push(`Provider error body:\n${attempt.error_body}${attempt.error_body_truncated ? '\n\n[truncated]' : ''}`);
    if (attempt.request_body) parts.push(`Client request body:\n${attempt.request_body}${attempt.request_body_truncated ? '\n\n[truncated]' : ''}`);
    if (attempt.latency_ms) parts.push(`Latency: ${attempt.latency_ms} ms`);
    errorDetails.set(key, { title: `${route} · ${status}`, body: parts.join('\n\n') || '(no error details)' });
    clickAttr = ` data-error-key="${key}"`;
    const hover = [attempt.error_message, attempt.latency_ms ? `Latency: ${attempt.latency_ms} ms` : ''].filter(Boolean).join(' · ');
    if (hover) hoverAttr = ` title="${h(hover)}"`;
  } else if (isCooldown) {
    const key = ++errorDetailSeq;
    errorDetails.set(key, { cooldown: { provider: attempt.provider, model: attempt.model, created_at: attempt.created_at }, title: `${route} · cooldown` });
    clickAttr = ` data-error-key="${key}"`;
    hoverAttr = ` title="${h('Cooldown — click for details')}"`;
  } else if (isSkipped) {
    const key = ++errorDetailSeq;
    const parts = [];
    if (attempt.error_message) parts.push(attempt.error_message);
    if (attempt.failure_class) parts.push(`Resolver: ${attempt.failure_class}`);
    if (attempt.latency_ms) parts.push(`Latency: ${attempt.latency_ms} ms`);
    errorDetails.set(key, { title: `${route} · skipped`, body: parts.join('\n\n') || 'The target was skipped before an upstream request was made.' });
    clickAttr = ` data-error-key="${key}"`;
    hoverAttr = ` title="${h('Skipped — click for details')}"`;
  }
  return `<div class="activity-attempt ${cls}"${clickAttr}${hoverAttr}><span class="attempt-number">${String(index + 1).padStart(2, '0')}</span><span class="attempt-route"><code title="${h(route)}">${h(route)}</code></span><span class="attempt-status">${status}</span></div>`;
}

// Full error text for failed attempts. The complete string lives here keyed by
// the data-error-key rendered above, and is shown in #error-dialog on click. The
// store is rebuilt on every activity render so keys never go stale.
let errorDetailSeq = 0;
const errorDetails = new Map();
let cooldownTickId = null;
function clearCooldownTick() { if (cooldownTickId !== null) { clearInterval(cooldownTickId); cooldownTickId = null; } }
function fmtDuration(totalSeconds) { if (totalSeconds <= 0) return '0s'; const s = Math.floor(totalSeconds); const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60; if (h > 0) return `${h}h ${m}m ${sec}s`; if (m > 0) return `${m}m ${sec}s`; return `${sec}s`; }
function openErrorDetail(key) { const detail = errorDetails.get(Number(key)); if (!detail) return; clearCooldownTick(); if (detail.cooldown) { const route = `${detail.cooldown.provider}/${detail.cooldown.model}`; $('#error-title').textContent = detail.title; $('#copy-error').hidden = !(window.isSecureContext && navigator.clipboard?.writeText); $('#clear-cooldown').hidden = false; $('#error-dialog').showModal();
  const bodyEl = $('#error-body'); $('#error-copy-state').textContent = '';
  bodyEl.textContent = 'Checking cooldown state…';
  const setBody = state => bodyEl.textContent = state;
  const renderCooling = cool => {
    const ago = cool.ago_seconds + ((Date.now() - cool.fetchedAt) / 1000);
    const rest = cool.restored_in_seconds - ((Date.now() - cool.fetchedAt) / 1000);
    if (rest <= 0) { clearCooldownTick(); renderExpired(); return; }
    const lines = [];
    if (cool.origin_error_class) lines.push(`Original error: ${cool.origin_error_class}${cool.origin_error_message ? ` — ${cool.origin_error_message}` : ''}`);
    if (cool.origin_request_id) lines.push(`Origin request: ${cool.origin_request_id}`);
    lines.push(`Started ${fmtDuration(ago)} ago (${date(cool.started_at)})`);
    lines.push(`Restores in ${fmtDuration(rest)}`);
    setBody(lines.join('\n\n'));
  };
  const renderExpired = () => {
    const lines = [];
    if (detail.cooldown.created_at) lines.push(`Skipped due to cooldown at ${date(detail.cooldown.created_at)}.`);
    lines.push('The cooldown window has since expired.');
    setBody(lines.join('\n\n'));
  };
  const loadStatus = async () => {
    try { const res = await api(`/api/admin/cooldown?provider=${encodeURIComponent(detail.cooldown.provider)}&model=${encodeURIComponent(detail.cooldown.model)}`); const cool = { ...res, fetchedAt: Date.now() }; if (cool.cooling) { renderCooling(cool); clearCooldownTick(); cooldownTickId = setInterval(() => renderCooling(cool), 1000); } else { renderExpired(); } } catch (error) { setBody('Could not load cooldown state: ' + errorMessage(error)); }
  };
  $('#clear-cooldown').onclick = async () => { const button = $('#clear-cooldown'); button.disabled = true; try { await api(`/api/admin/cooldown?provider=${encodeURIComponent(detail.cooldown.provider)}&model=${encodeURIComponent(detail.cooldown.model)}`, { method: 'DELETE' }); clearCooldownTick(); renderExpired(); } catch (error) { setBody('Could not clear cooldown: ' + errorMessage(error)); } finally { button.disabled = false; } };
  loadStatus();
  return; }
  $('#error-title').textContent = detail.title; $('#error-body').textContent = detail.body; $('#error-copy-state').textContent = ''; $('#copy-error').hidden = !(window.isSecureContext && navigator.clipboard?.writeText); $('#clear-cooldown').hidden = true; $('#error-dialog').showModal(); }
document.addEventListener('click', event => { const target = event.target.closest('[data-error-key]'); if (target) openErrorDetail(target.dataset.errorKey); });
$('#copy-error').onclick = async () => { const text = $('#error-body').textContent; const state = $('#error-copy-state'); if (!(window.isSecureContext && navigator.clipboard?.writeText)) return; try { await navigator.clipboard.writeText(text); state.textContent = 'Copied to clipboard.'; } catch { state.textContent = 'Clipboard copy was denied — select the text and press Ctrl/Cmd+C.'; } };
$('#close-error').onclick = $('#done-error').onclick = () => { clearCooldownTick(); $('#error-dialog').close(); };
  function attemptSequence(row) { if (!row.attempts?.length) return ''; return `<div class="attempt-sequence">${row.attempts.map((attempt, index) => activityAttemptDetails(index === 0 ? { ...attempt, request_body: row.request_body, request_body_truncated: row.request_body_truncated } : attempt, index)).join('')}</div>`; }
 function requestIdentity(row) { const requestedModel = h(row.requested_model); const requestedTitle = h(row.requested_model); if (row.exposed_model && row.exposed_model !== row.requested_model) { return `<code class="model-id activity-requested-model" title="${requestedTitle}">${requestedModel}</code><br><span class="meta-line activity-exposed-model" title="${h(row.exposed_model)}">map &gt; ${h(row.exposed_model)}</span>`; } return `<code class="model-id activity-requested-model" title="${requestedTitle}">${requestedModel}</code>`; }
function activityRequestID(row) { const id = row.client_request_id || ''; const short = id.length > 8 ? `${id.slice(0, 8)}…` : id; const copyable = id && (window.isSecureContext && navigator.clipboard?.writeText); const attrs = copyable ? ` data-copy-request-id="${h(id)}" role="button" tabindex="0" aria-label="Copy request ID ${h(id)}"` : ''; return `<code class="model-id activity-request-id"${attrs} title="${h(id)}">${h(short)}</code>`; }
async function copyRequestID(button) { const id = button.dataset.copyRequestId; if (!id) return; if (!(window.isSecureContext && navigator.clipboard?.writeText)) return; try { await navigator.clipboard.writeText(id); const original = button.textContent; button.classList.add('copied'); button.textContent = 'Copied'; setTimeout(() => { button.classList.remove('copied'); button.textContent = original; }, 1200); } catch { const range = document.createRange(); range.selectNodeContents(button); const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range); button.title = 'Press Ctrl/Cmd+C to copy'; } }
document.addEventListener('click', event => { const target = event.target.closest('[data-copy-request-id]'); if (target) copyRequestID(target); });
document.addEventListener('keydown', event => { if (event.key !== 'Enter' && event.key !== ' ') return; const target = event.target.closest('[data-copy-request-id]'); if (!target) return; event.preventDefault(); copyRequestID(target); });
 function activityHistoryCard(row, showClient = true) {
  const status = row.http_status >= 200 && row.http_status < 300 ? 'Succeeded' : `HTTP ${row.http_status || 'error'}`;
  const statusClass = row.http_status >= 200 && row.http_status < 300 ? 'history-success' : 'history-failure';
  const resolved = row.resolved_provider && row.resolved_model ? `${row.resolved_provider}/${row.resolved_model}` : 'No resolved target';
   return `<article class="history-card" data-activity-row="${h(row.id)}"><div class="history-card-head"><span class="history-status ${statusClass}">${h(status)}</span><time>${h(date(row.created_at))}</time></div>${showClient ? `<strong class="history-client">${h(row.client_name || '')}</strong>` : ''}<div class="history-route"><code>${h(row.requested_model)}</code>${row.exposed_model && row.exposed_model !== row.requested_model ? `<small>map → ${h(row.exposed_model)}</small>` : ''}</div><div class="history-resolution"><span>Resolved</span><strong>${h(resolved)}</strong></div><div class="history-meta"><span>${h(row.protocol)}${row.streaming ? ' · stream' : ''}</span><span>${h(row.latency_ms)} ms</span><span>${row.fallback_used ? 'Fallback' : 'Direct'}</span></div><div data-activity-resolved="${h(row.id)}" data-activity-detail-kind="card">${activityCardDetail(row)}</div><div class="history-footer"><span>${activityRequestID(row)}</span>${row.error_text ? `<span class="error-text">${h(row.error_text)}</span>` : ''}</div></article>`;
}
 filterInput('#activity-search', value => { activityFeed.setSearch(value); });
$('#close-activity').onclick = $('#done-activity').onclick = () => $('#activity-dialog').close();
$('#export-activity').onclick = () => $('#export-dialog').showModal();
$('#clear-activity').onclick = async () => {
  if (activityState.kind !== 'client' || !activityState.client) return;
  if (!await confirmAction({ title: `Clear ${activityState.client.name} activity?`, copy: 'All recorded request metadata for this client will be permanently deleted.', action: 'Clear activity' })) return;
  const button = $('#clear-activity'); button.disabled = true; $('#activity-error').textContent = '';
  try {
    await api(`/api/admin/client-keys/${activityState.client.id}/activity`, { method: 'DELETE' });
    activityState.offset = 0;
    flash('Client activity cleared.');
    await loadActivity();
  } catch (error) { $('#activity-error').textContent = errorMessage(error); }
  finally { button.disabled = false; }
};
$('#close-export').onclick = () => $('#export-dialog').close();
$$('[data-export-period]', $('#export-dialog')).forEach(button => button.onclick = () => { const period = button.dataset.exportPeriod; const base = activityState.kind === 'client' ? `/api/admin/client-keys/${activityState.client.id}/activity/export` : activityState.kind === 'real' ? `/api/admin/models/${activityState.modelID}/activity/export` : `/api/admin/virtual-models/${activityState.modelID}/activity/export`; const url = `${base}?period=${period}&search=${encodeURIComponent(activityState.search || '')}`; const a = document.createElement('a'); a.href = url; a.download = ''; document.body.appendChild(a); a.click(); a.remove(); $('#export-dialog').close(); });

// Global activity is a read-only section in the Settings view, distinct from
// the per-client Activity dialog. It shows metadata across all client keys and
// renders rows through the same helpers as the dialog (activityTableRow(),
// resolvedActivity(), requestIdentity(), rowCache(), background attempt
// hydration) so fallbacks appear identically — the extra Client column is the
// only difference.
const globalActivityState = { rows: [], offset: 0, limit: ACTIVITY_PAGE_SIZE, search: '', hasMore: true, loading: false };
function renderGlobalActivity(state) { errorDetails.clear(); errorDetailSeq = 0; $('#global-activity-empty').hidden = state.loading || state.rows.length > 0; $('#global-activity-empty-mobile').hidden = state.loading || state.rows.length > 0; $('#global-activity-body').innerHTML = state.loading ? '<tr><td colspan="8"><span class="meta-line">Loading activity…</span></td></tr>' : state.rows.map(row => activityTableRow(row, true)).join(''); $('#global-activity-cards').innerHTML = state.loading ? '<p class="meta-line">Loading activity…</p>' : state.rows.map(row => activityHistoryCard(row, true)).join(''); updateActivityCount(state, '#global-activity-count'); }
function appendGlobalActivity(state, page) { const table = $('#global-activity-body'); const cards = $('#global-activity-cards'); page.forEach(row => { table.insertAdjacentHTML('beforeend', activityTableRow(row, true)); cards.insertAdjacentHTML('beforeend', activityHistoryCard(row, true)); }); $('#global-activity-empty').hidden = state.rows.length > 0; $('#global-activity-empty-mobile').hidden = state.rows.length > 0; updateActivityCount(state, '#global-activity-count'); }
const globalActivityFeed = createActivityFeed({
  state: globalActivityState,
  containers: () => [$('#global-activity-body'), $('#global-activity-cards')],
  scrollHosts: () => [$('#global-activity-body').closest('.activity-table-shell'), window],
  scrollGuard: host => {
    const shell = $('#global-activity-body').closest('.activity-table-shell');
    const shellVisible = !!shell && shell.clientHeight > 0;
    return host === window ? !shellVisible : shellVisible;
  },
  resetScroll: () => { const shell = $('#global-activity-body').closest('.activity-table-shell'); if (shell) shell.scrollTop = 0; },
  showError: message => { $('#global-activity-error').textContent = message; },
  fetch: (offset, limit, search, signal) => api(`/api/admin/activity?limit=${limit + 1}&offset=${offset}&search=${encodeURIComponent(search || '')}`, { signal }),
  render: renderGlobalActivity,
  append: appendGlobalActivity,
});
function loadGlobalActivity() { return globalActivityFeed.reload(); }
filterInput('#global-activity-search', value => { globalActivityFeed.setSearch(value); });

const mobileHistoryState = { rows: [], offset: 0, limit: ACTIVITY_PAGE_SIZE, search: '', hasMore: true, loading: false };
function renderMobileHistory(state) {
  $('#mobile-history-body').innerHTML = state.loading ? '<p class="meta-line">Loading activity…</p>' : state.rows.map(row => activityHistoryCard(row, true)).join('');
  $('#mobile-history-empty').hidden = state.loading || state.rows.length > 0;
  updateActivityCount(state, '#mobile-history-count');
}
function appendMobileHistory(state, page) {
  const list = $('#mobile-history-body');
  page.forEach(row => list.insertAdjacentHTML('beforeend', activityHistoryCard(row, true)));
  $('#mobile-history-empty').hidden = state.rows.length > 0;
  updateActivityCount(state, '#mobile-history-count');
}
const mobileHistoryFeed = createActivityFeed({
  state: mobileHistoryState,
  containers: () => [$('#mobile-history-body')],
  scrollHosts: () => [$('#mobile-history-body')],
  resetScroll: () => { $('#mobile-history-body').scrollTop = 0; },
  showError: message => { $('#mobile-history-error').textContent = message; },
  fetch: (offset, limit, search, signal) => api(`/api/admin/activity?limit=${limit + 1}&offset=${offset}&search=${encodeURIComponent(search || '')}`, { signal }),
  render: renderMobileHistory,
  append: appendMobileHistory,
});
function loadMobileHistory() { return mobileHistoryFeed.reload(); }
$('#open-mobile-history').onclick = () => { mobileHistoryState.search = ''; $('#mobile-history-search').value = ''; loadMobileHistory(); $('#mobile-history-dialog').showModal(); };
$('#close-mobile-history').onclick = $('#done-mobile-history').onclick = () => $('#mobile-history-dialog').close();
filterInput('#mobile-history-search', value => { mobileHistoryFeed.setSearch(value); });

let authHeaderDirty = false;
let authHeaderClear = false;
async function loadSettings() {
  $('#backup-card').hidden = runtimeMode === 'hosted';
  const token = ++state.loadToken;
  const [health, settings] = await Promise.all([api('/api/admin/health'), api('/api/admin/settings')]);
  if (token !== state.loadToken) return;
  $('#top-status').textContent = health.status.toUpperCase(); $('[name="default_logging_enabled"]', $('#settings-form')).checked = settings.default_logging_enabled; $('[name="log_error_bodies"]', $('#settings-form')).checked = settings.log_error_bodies; $('[name="default_retention_days"]', $('#settings-form')).value = settings.default_retention_days; $('[name="fallback_timeout_seconds"]', $('#fallback-form')).value = settings.fallback_timeout_seconds; $('[name="fallback_cooldown_seconds"]', $('#fallback-form')).value = settings.fallback_cooldown_seconds; const nf = $('#notifications-form'); $('[name="notifications_enabled"]', nf).checked = settings.notifications_enabled; $('[name="notifications_webhook_url"]', nf).value = settings.notifications_webhook_url || ''; $('[name="notifications_event_fallback"]', nf).checked = settings.notifications_event_fallback; $('[name="notifications_event_all_failed"]', nf).checked = settings.notifications_event_all_failed; $('[name="notifications_event_client_key_created"]', nf).checked = settings.notifications_event_client_key_created; $('[name="notifications_event_client_key_deleted"]', nf).checked = settings.notifications_event_client_key_deleted; $('[name="notifications_event_admin_login"]', nf).checked = settings.notifications_event_admin_login; $('[name="notifications_cooldown_seconds"]', nf).value = settings.notifications_cooldown_seconds; const authInput = $('[name="notifications_auth_header"]', nf); authInput.value = ''; authInput.placeholder = settings.notifications_auth_header_set ? '•••••••• (set — leave blank to keep)' : 'Optional, e.g. Bearer <token>'; $('#notifications-auth-note').textContent = settings.notifications_auth_header_set ? 'An Authorization header is configured. Leave blank to keep it; type a new value to replace it.' : ''; $('#clear-notifications-auth').hidden = !settings.notifications_auth_header_set; authHeaderDirty = false; authHeaderClear = false; await loadGlobalActivity();
}
async function saveSettings() { const settingsForm = $('#settings-form'), fallbackForm = $('#fallback-form'), notificationsForm = $('#notifications-form'); if (!settingsForm.reportValidity() || !fallbackForm.reportValidity() || !notificationsForm.reportValidity()) return; const settingsValues = new FormData(settingsForm), fallbackValues = new FormData(fallbackForm), notificationsValues = new FormData(notificationsForm); const buttons = [$('#save-settings-top'), $('#save-settings-bottom')]; buttons.forEach(b => b.disabled = true); $('#settings-error').textContent = ''; $('#fallback-error').textContent = ''; $('#notifications-error').textContent = '';   const body = { default_logging_enabled: settingsValues.get('default_logging_enabled') === 'on', default_retention_days: Number(settingsValues.get('default_retention_days')), log_error_bodies: settingsValues.get('log_error_bodies') === 'on', fallback_timeout_seconds: Number(fallbackValues.get('fallback_timeout_seconds')), fallback_cooldown_seconds: Number(fallbackValues.get('fallback_cooldown_seconds')), notifications_enabled: notificationsValues.get('notifications_enabled') === 'on', notifications_webhook_url: notificationsValues.get('notifications_webhook_url') || '', notifications_event_fallback: notificationsValues.get('notifications_event_fallback') === 'on', notifications_event_all_failed: notificationsValues.get('notifications_event_all_failed') === 'on', notifications_event_client_key_created: notificationsValues.get('notifications_event_client_key_created') === 'on', notifications_event_client_key_deleted: notificationsValues.get('notifications_event_client_key_deleted') === 'on', notifications_event_admin_login: notificationsValues.get('notifications_event_admin_login') === 'on', notifications_cooldown_seconds: Number(notificationsValues.get('notifications_cooldown_seconds')) }; if (authHeaderDirty) body.notifications_auth_header = notificationsValues.get('notifications_auth_header') || ''; if (authHeaderClear) body.notifications_auth_header = ''; try { await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify(body) }); authHeaderDirty = false; authHeaderClear = false; flash('Settings saved.'); await loadSettings(); } catch (error) { const message = errorMessage(error); $('#settings-error').textContent = message; $('#fallback-error').textContent = message; $('#notifications-error').textContent = message; } finally { buttons.forEach(b => b.disabled = false); } }
$('#save-settings-top').addEventListener('click', saveSettings);
$('#save-settings-bottom').addEventListener('click', saveSettings);
$('[name="log_error_bodies"]', $('#settings-form')).addEventListener('change', async event => { try { await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify({ log_error_bodies: event.target.checked }) }); flash(event.target.checked ? 'Detailed error logging enabled.' : 'Detailed error logging disabled.'); } catch (error) { event.target.checked = !event.target.checked; $('#settings-error').textContent = errorMessage(error); } });
$('[name="notifications_auth_header"]', $('#notifications-form')).addEventListener('input', () => { authHeaderDirty = true; authHeaderClear = false; });
$('#clear-notifications-auth').addEventListener('click', async () => { const button = $('#clear-notifications-auth'); button.disabled = true; $('#notifications-error').textContent = ''; try { await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify({ notifications_auth_header: '' }) }); authHeaderClear = false; authHeaderDirty = false; $('[name="notifications_auth_header"]', $('#notifications-form')).value = ''; $('#notifications-auth-note').textContent = 'Authorization header cleared.'; $('#clear-notifications-auth').hidden = true; } catch (error) { $('#notifications-error').textContent = errorMessage(error); } finally { button.disabled = false; } });
$('#send-test-notification').addEventListener('click', async () => { const button = $('#send-test-notification'); button.disabled = true; $('#notifications-error').textContent = ''; try { const nf = $('#notifications-form'); const body = { notifications_webhook_url: $('[name="notifications_webhook_url"]', nf).value || '' }; if (authHeaderDirty) body.notifications_auth_header = $('[name="notifications_auth_header"]', nf).value || ''; if (authHeaderClear) body.notifications_auth_header = ''; await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify(body) }); authHeaderDirty = false; authHeaderClear = false; await api('/api/admin/notifications/test', { method: 'POST' }); flash('Test notification delivered.'); } catch (error) { $('#notifications-error').textContent = errorMessage(error); } finally { button.disabled = false; } });

// Settings tabs. Account is hosted-only; the others carry the existing config
// cards. The hash reflects the sub-tab (#settings/<tab>) so deep links work.
const SETTINGS_TABS = ['account', 'routing', 'logging', 'notifications', 'data'];
let settingsTab = 'routing';
function showSettingsTab(tab) {
  if (!SETTINGS_TABS.includes(tab) || (tab === 'account' && runtimeMode !== 'hosted')) tab = 'routing';
  settingsTab = tab;
  $$('.settings-tab').forEach(btn => btn.classList.toggle('active', btn.dataset.settingsTab === tab));
  $$('.settings-panel').forEach(panel => { const active = panel.dataset.settingsPanel === tab; panel.classList.toggle('active', active); panel.hidden = !active; });
  $('#settings-tab-account').hidden = runtimeMode !== 'hosted';
  // Account actions save themselves; the shared save bar only applies to config.
  $('#save-settings-top').hidden = tab === 'account';
  $('#save-settings-bottom').hidden = tab === 'account';
}
$$('.settings-tab').forEach(btn => btn.addEventListener('click', () => { showSettingsTab(btn.dataset.settingsTab); const hash = btn.dataset.settingsTab === 'routing' ? '#settings' : `#settings/${btn.dataset.settingsTab}`; if (location.hash !== hash) history.pushState(null, '', hash); if (btn.dataset.settingsTab === 'account') loadAccount(); }));
$('#settings-tab-account').hidden = runtimeMode !== 'hosted';

async function loadAccount() {
  if (runtimeMode !== 'hosted') return;
  try {
    const [profile, usage, plan] = await Promise.all([api('/api/auth/account'), api('/api/admin/usage'), api('/api/auth/account/plan')]);
    $('#account-email').textContent = profile.email;
    $('#account-id').textContent = profile.account_id;
    $('#account-plan').textContent = profile.plan;
    $('#account-status').textContent = profile.account_status;
    $('#account-created').textContent = profile.created_at ? new Date(profile.created_at).toLocaleString() : '—';
    $('#account-delete-hint').textContent = profile.email;
    const googleEnabled = !!hostedAuthOptions.google_enabled, googleLinked = !!profile.google_linked;
    $('#account-google-card').hidden = !googleEnabled && !googleLinked;
    $('#account-google-note').hidden = !googleEnabled;
    $('#account-google-status').hidden = !googleEnabled;
    $('#account-google-status').textContent = googleLinked ? 'Google is linked to this account.' : 'Google is not linked to this account.';
    $('#account-google-link-form').hidden = googleLinked || !profile.password_enabled || !googleEnabled;
    $('#account-google-reauth').hidden = !googleLinked || !googleEnabled;
    $('#account-google-unlink').hidden = !googleLinked || !profile.password_enabled;
    $('#account-google-unlink-form').hidden = true;
    $('#account-password-auth-hint').textContent = profile.password_enabled ? '' : 'Confirm with Google before setting a password.';
    $('#account-current-password').required = profile.password_enabled;
    $('#account-email-password').required = profile.password_enabled;
    $('#account-delete-password').required = profile.password_enabled;
    $('#account-delete-password').disabled = false;
    $('#account-delete-password').placeholder = profile.password_enabled ? '' : 'Use Google confirmation';
    const windows = usage.client_keys ? Object.values(usage.client_keys) : [];
    const sum = windowKey => windows.reduce((total, w) => total + ((w?.[windowKey]?.tokens ?? w?.[windowKey] ?? 0) || 0), 0);
    $('#account-usage').innerHTML = [['1h', sum('1h')], ['24h', sum('24h')], ['7d', sum('7d')]].map(([label, value]) => `<div class="metric"><strong>${Number(value || 0).toLocaleString()}</strong><span>${label} tokens</span></div>`).join('');
    renderPlanCard(plan);
  } catch (error) {
    flash(errorMessage(error, 'Could not load account details.'), 'error');
  }
}

// renderPlanCard shows the plan caps and current consumption. A cap of -1 is
// rendered as "Unlimited" rather than a number.
function renderPlanCard(plan) {
  const el = $('#account-plan-card'); if (!el) return;
  const fmt = value => (value === -1 ? 'Unlimited' : String(value));
  const usage = plan.usage || {}; const limits = plan.limits || {};
  const row = (label, used, cap) => `<div class="plan-row"><span class="plan-label">${h(label)}</span><span class="plan-value${cap !== -1 && used >= cap ? ' plan-full' : ''}">${h(used)} / ${h(fmt(cap))}</span></div>`;
  el.innerHTML = [
    row('Providers', usage.providers ?? 0, limits.max_providers ?? -1),
    row('Client keys', usage.client_keys ?? 0, limits.max_client_keys ?? -1),
    row('Virtual models', usage.virtual_models ?? 0, limits.max_virtual_models ?? -1),
    row('Concurrent streams', '—', limits.max_concurrent_streams ?? -1),
    row('Monthly requests', usage.monthly_requests ?? 0, limits.monthly_requests ?? -1),
    `<div class="plan-row"><span class="plan-label">Activity retention</span><span class="plan-value">${h(fmt(limits.activity_retention_days ?? -1))} days</span></div>`,
  ].join('');
}

// loadFooterVersion shows the deployed version/commit with an AGPL source link.
async function loadFooterVersion() {
  try {
    const info = await fetch('/health/version').then(res => res.json());
    const commit = info.commit || ''; const version = info.version || '';
    const label = [version, commit].filter(Boolean).join(' · ') || 'development build';
    const url = commit ? `https://github.com/dellarb/tiller-router/commit/${encodeURIComponent(commit)}` : 'https://github.com/dellarb/tiller-router';
    $('#footer-source').innerHTML = `${h(label)} — <a href="${h(url)}" target="_blank" rel="noopener">source</a>`;
  } catch { /* footer is best-effort */ }
}

// showLegalDocument renders a published legal document in the full-width legal
// shell. Bodies are plain text (no rich rendering), so they are inserted as
// textContent with preserved whitespace to avoid any injection path.
async function showLegalDocument(slug) {
  $('#login-shell').hidden = true; $('#app').hidden = true; $('#platform-shell').hidden = true;
  $('#legal-shell').hidden = false;
  $('#legal-shell').scrollTop = 0;
  const body = $('#legal-body');
  body.textContent = 'Loading…';
  try {
    const doc = await api(`/api/legal/${encodeURIComponent(slug)}`);
    $('#legal-title').textContent = doc.title;
    $('#legal-updated').textContent = doc.updated_at ? `Last updated ${date(doc.updated_at)}` : '';
    body.textContent = doc.body;
  } catch (error) {
    $('#legal-title').textContent = 'Not found';
    $('#legal-updated').textContent = '';
    body.textContent = errorMessage(error, 'This document is not available.');
  }
}
$('#legal-back').onclick = () => showLogin();

// refreshWizardButton shows/hides the top-bar Get started button based on
// whether onboarding is still outstanding, and auto-opens the wizard on the
// first hosted login when setup is incomplete.
async function refreshWizardButton(autoOpen = false) {
  if (runtimeMode !== 'hosted') { $('#open-wizard').hidden = true; return; }
  try {
    const status = await api('/api/auth/onboarding');
    const show = Boolean(status.needs_onboarding);
    $('#open-wizard').hidden = !show;
    if (show && autoOpen) openWizard();
  } catch { $('#open-wizard').hidden = true; }
}

const WIZARD_STEPS = ['Provider', 'Client key', 'Virtual route', 'Connect'];
let wizardStep = 0;
const wizardState = { clientType: '', clientName: '', virtualName: '' };

function openWizard() {
  wizardStep = 0;
  wizardState.clientType = ''; wizardState.clientName = ''; wizardState.virtualName = '';
  renderWizard();
  const dialog = $('#wizard-dialog'); if (dialog && !dialog.open) dialog.showModal();
}

// wizardAdvance moves to the next step when an action launched from the wizard
// completes. It is a no-op if the wizard was closed in the meantime.
function wizardAdvance() {
  if (!$('#wizard-dialog')?.open) return;
  if (wizardStep < WIZARD_STEPS.length - 1) { wizardStep++; renderWizard(); }
}

// modelNameForSnippet picks the model identity the Connect step should show: a
// virtual route if one was created, else the Single key's exposed name, else the
// first available real model, else a neutral fallback.
function modelNameForSnippet() {
  if (wizardState.virtualName) return wizardState.virtualName;
  if (wizardState.clientType === 'single' && wizardState.clientName) return wizardState.clientName;
  return state.models.find(model => model.available)?.canonical_model_id || 'main';
}

function renderWizard() {
  const steps = $('#wizard-steps');
  if (steps) steps.innerHTML = WIZARD_STEPS.map((label, index) => `<span class="wizard-step${index === wizardStep ? ' active' : ''}${index < wizardStep ? ' done' : ''}">${h(label)}</span>`).join('');
  const body = $('#wizard-body'); if (!body) return;
  $('#wizard-prev').disabled = wizardStep === 0;
  $('#wizard-next').textContent = wizardStep === WIZARD_STEPS.length - 1 ? 'Done' : 'Continue';
  if (wizardStep === 0) {
    body.innerHTML = `<h3>Connect a provider</h3><p>Add the AI provider you want Tiller to route to. Your credential is encrypted at rest and never shown again.</p><button class="btn btn-primary" id="wizard-add-provider" type="button">Add provider</button><p class="meta-line">${state.providers.length ? h(state.providers.length + ' provider(s) configured.') : 'No providers configured yet.'}</p>`;
    const button = $('#wizard-add-provider'); if (button) button.onclick = () => openProvider(null, wizardAdvance);
  } else if (wizardStep === 1) {
    body.innerHTML = `<h3>Create a client key</h3><p>A client key is the API key your tools use. Tiller shows the secret once. First, choose how much this key can reach.</p><div class="wizard-actions"><button class="btn btn-primary" id="wizard-client-all" type="button">All models</button><button class="btn btn-secondary" id="wizard-client-subset" type="button">Choose models</button></div><p class="meta-line">All models reaches every model, including ones added later. You can change this in the client's settings.</p>`;
    const allButton = $('#wizard-client-all'); if (allButton) allButton.onclick = () => wizardCreateClient('all');
    const subsetButton = $('#wizard-client-subset'); if (subsetButton) subsetButton.onclick = () => wizardCreateClient('subset');
  } else if (wizardStep === 2) {
    body.innerHTML = `<h3>Create a virtual route (optional)</h3><p>Map a stable model name to one or more real models, with ordered fallback if the first target fails.</p><div class="wizard-actions"><button class="btn btn-secondary" id="wizard-add-virtual" type="button">Create virtual route</button></div><p class="meta-line">Optional — continue to use a real model directly.</p>`;
    const button = $('#wizard-add-virtual'); if (button) button.onclick = () => openVirtualModel(null, name => { if (name) wizardState.virtualName = name; wizardAdvance(); });
  } else {
    const modelName = modelNameForSnippet();
    const base = location.origin + '/v1';
    const snippet = `curl ${base}/chat/completions -H "Authorization: Bearer $TILLER_API_KEY" -H "Content-Type: application/json" -d '{"model":"${modelName}","messages":[{"role":"user","content":"Hello"}]}'`;
    body.innerHTML = `<h3>Point your tool at Tiller</h3><p>Use this endpoint and your client key (model name: <code>${h(modelName)}</code>).</p><div class="secret-box"><code>${h(snippet)}</code><button class="btn btn-secondary" id="wizard-copy" type="button">Copy</button></div><p class="meta-line">Setup completes automatically when your first request routes successfully.</p>`;
    const button = $('#wizard-copy'); if (button) button.onclick = () => navigator.clipboard?.writeText(snippet);
  }
}

// wizardCreateClient runs the wizard's access-mode choice against the shared
// client form. "all" grants the new catalogue key every model (current and
// future); "subset" hands off to the permissions dialog and only advances once
// that catalogue is saved. Single keys carry their own target, so the mode is
// ignored for them.
function wizardCreateClient(mode) {
  openClient(null, async (result, payload) => {
    wizardState.clientType = result?.type || '';
    if (wizardState.clientType === 'single') wizardState.clientName = payload?.single_model_name || 'main';
    if (wizardState.clientType === 'catalogue' && mode === 'all') {
      try { await enableAllClientModels(result); } catch (error) { flash(errorMessage(error, 'Could not grant all-model access.'), 'error'); return; }
      wizardAdvance();
    } else if (wizardState.clientType === 'catalogue' && mode === 'subset') {
      // Wait for the one-time secret dialog to close so permissions opens on top
      // of the wizard, not behind the key reveal.
      const secret = $('#secret-dialog');
      if (secret?.open) secret.addEventListener('close', () => openPermissions(result, wizardAdvance), { once: true });
      else openPermissions(result, wizardAdvance);
    } else {
      wizardAdvance();
    }
  }, 'catalogue');
}
$('#wizard-prev').addEventListener('click', () => { if (wizardStep > 0) { wizardStep--; renderWizard(); } });
$('#wizard-next').addEventListener('click', async () => {
  if (wizardStep < WIZARD_STEPS.length - 1) { wizardStep++; renderWizard(); return; }
  $('#wizard-dialog').close();
  await refreshWizardButton(false);
});
$('#wizard-dismiss').addEventListener('click', async () => {
  try { await api('/api/auth/onboarding/dismiss', { method: 'POST', body: '{}' }); } catch { /* best-effort */ }
  $('#wizard-dialog').close(); $('#open-wizard').hidden = true;
});
$('#close-wizard').addEventListener('click', () => $('#wizard-dialog').close());
$('#open-wizard').addEventListener('click', openWizard);
$('#legal-back').addEventListener('click', () => { history.replaceState(null, '', '/login'); showLogin(); });

$('#account-password-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget); $('#account-password-error').textContent = '';
  try { await api('/api/auth/account/password', { method: 'POST', body: JSON.stringify({ current_password: form.get('current_password'), new_password: form.get('new_password') }) }); event.currentTarget.reset(); flash('Password updated. Other sessions were signed out.'); }
  catch (error) { $('#account-password-error').textContent = errorMessage(error, 'Could not update the password.'); }
});
$('#account-google-link-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget); $('#account-google-link-error').textContent = '';
  try {
    const result = await api('/api/auth/google/link/start', { method: 'POST', body: JSON.stringify({ current_password: form.get('current_password') }) });
    location.assign(result.redirect_url);
  } catch (error) { $('#account-google-link-error').textContent = errorMessage(error, 'Could not start Google linking.'); }
});
$('#account-google-reauth').addEventListener('click', async () => {
  $('#account-google-error').textContent = '';
  try {
    const result = await api('/api/auth/google/reauth/start', { method: 'POST', body: '{}' });
    location.assign(result.redirect_url);
  } catch (error) { $('#account-google-error').textContent = errorMessage(error, 'Could not start Google confirmation.'); }
});
$('#account-google-unlink').addEventListener('click', () => { $('#account-google-unlink-form').hidden = !$('#account-google-unlink-form').hidden; });
$('#account-google-unlink-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget);
  try {
    await api('/api/auth/account/google', { method: 'DELETE', body: JSON.stringify({ password: form.get('password') }) });
    await loadAccount(); flash('Google was unlinked from your account.');
  } catch (error) { $('#account-google-error').textContent = errorMessage(error, 'Could not unlink Google.'); }
});
$('#account-email-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget); const note = $('#account-email-error'); note.style.color = ''; note.textContent = '';
  try { const result = await api('/api/auth/account/email', { method: 'POST', body: JSON.stringify({ new_email: form.get('new_email'), password: form.get('password') }) }); event.currentTarget.reset(); note.style.color = 'var(--green)'; note.textContent = result.message || 'Check the new address for a confirmation link.'; }
  catch (error) { note.textContent = errorMessage(error, 'Could not start the email change.'); }
});
$('#account-revoke-sessions').addEventListener('click', async () => {
  const button = $('#account-revoke-sessions'); button.disabled = true; $('#account-sessions-error').textContent = '';
  try { await api('/api/auth/account/sessions/revoke-all', { method: 'POST', body: '{}' }); showLogin(); }
  catch (error) { $('#account-sessions-error').textContent = errorMessage(error, 'Could not sign out sessions.'); button.disabled = false; }
});
$('#account-export').addEventListener('click', async event => {
  event.preventDefault(); $('#account-export-error').textContent = '';
  try {
    const response = await fetch('/api/auth/account/export', { credentials: 'same-origin' });
    if (!response.ok) { const payload = await response.json().catch(() => ({})); throw new Error(payload?.error?.message || `Export failed (${response.status})`); }
    const blob = await response.blob();
    const url = URL.createObjectURL(blob); const link = document.createElement('a');
    link.href = url; link.download = `tiller-account-export-${new Date().toISOString().slice(0, 10)}.zip`;
    document.body.appendChild(link); link.click(); link.remove(); URL.revokeObjectURL(url);
  } catch (error) { $('#account-export-error').textContent = errorMessage(error, 'Could not export your data.'); }
});
$('#account-delete-form').addEventListener('submit', async event => {
  event.preventDefault(); const form = new FormData(event.currentTarget); $('#account-delete-error').textContent = '';
  if (!window.confirm('Delete your account now? This is immediate and irreversible.')) return;
  const button = $('#account-delete-form button[type="submit"]'); button.disabled = true;
  try { const result = await api('/api/auth/account', { method: 'DELETE', body: JSON.stringify({ confirm: form.get('confirm'), password: form.get('password') }) }); flash(result.message || 'Account deleted.'); showLogin(); }
  catch (error) { $('#account-delete-error').textContent = errorMessage(error, 'Could not delete the account.'); button.disabled = false; }
});

let entitySubmit = null;
// Monotonic id for the currently-open entity dialog. A submit captures it and
// only closes/updates the dialog if no newer openEntity() replaced it while the
// request (and its follow-up reloads) were in flight. Without this, a slow
// save's trailing close() shut a dialog the user had already reopened.
let entitySubmitSeq = 0;
function openEntity({ eyebrow, title, fields, submit, onMount, onSubmit }) { const dialog = $('#form-dialog'), form = $('#entity-form'); $('#dialog-eyebrow').textContent = eyebrow; $('#dialog-title').textContent = title; $('#dialog-fields').innerHTML = fields; $('#dialog-submit').textContent = submit; $('#dialog-submit').disabled = false; $('#dialog-error').textContent = ''; entitySubmit = onSubmit; entitySubmitSeq += 1; form.onsubmit = handleEntitySubmit; dialog.showModal(); onMount?.(form); setTimeout(() => $('input:not([type="checkbox"]),select,textarea', form)?.focus(), 0); }
async function handleEntitySubmit(event) { const form = event.currentTarget, button = $('#dialog-submit'); if (event.submitter?.value === 'cancel') return; event.preventDefault(); if (!form.reportValidity()) return; const submit = entitySubmit, seq = entitySubmitSeq; button.disabled = true; $('#dialog-error').textContent = ''; try { await submit(form); if (seq === entitySubmitSeq) $('#form-dialog').close(); } catch (error) { if (seq === entitySubmitSeq) $('#dialog-error').textContent = errorMessage(error); } finally { if (seq === entitySubmitSeq) button.disabled = false; } }

function confirmAction({ title, copy, action, breaking = false, typeMatch = null, typeLabel = 'name' }) { return new Promise(resolve => { const dialog = $('#confirm-dialog'), form = $('form', dialog), checkWrap = $('#confirm-check-wrap'), check = $('#confirm-check'), typeWrap = $('#confirm-type-wrap'), typeInput = $('#confirm-type'); $('#confirm-title').textContent = title; $('#confirm-copy').textContent = copy; $('#confirm-action').textContent = action; $('#confirm-error').textContent = ''; checkWrap.hidden = !breaking; check.checked = false; typeWrap.hidden = !typeMatch; typeInput.value = ''; if (typeMatch) $('#confirm-type-label').textContent = `Type the ${typeLabel} to confirm`; const valid = () => !typeMatch || typeInput.value === typeMatch; const close = event => { dialog.removeEventListener('close', close); resolve(dialog.returnValue === 'confirm' && (!breaking || check.checked) && valid()); }; form.onsubmit = event => { if (event.submitter?.value !== 'confirm') return; if (breaking && !check.checked) { event.preventDefault(); $('#confirm-error').textContent = 'Acknowledge the breaking client-facing change first.'; return; } if (typeMatch && !valid()) { event.preventDefault(); $('#confirm-error').textContent = `Type the ${typeLabel} exactly to confirm.`; } }; dialog.addEventListener('close', close); dialog.showModal(); if (typeMatch) setTimeout(() => typeInput.focus(), 0); }); }

function selectSecretText() { const node = $('#secret-value'); const sel = window.getSelection(); sel.removeAllRanges(); const range = document.createRange(); range.selectNodeContents(node); sel.addRange(range); }
function showSecret(secret) { $('#secret-value').textContent = secret; const secure = window.isSecureContext && navigator.clipboard?.writeText; $('#copy-secret').hidden = !secure; $('#copy-state').textContent = ''; $('#secret-dialog').showModal(); if (!secure) { selectSecretText(); $('#copy-state').textContent = 'Key selected — press Ctrl/Cmd+C to copy it.'; } }
$('#copy-secret').onclick = async () => { const text = $('#secret-value').textContent; const state = $('#copy-state'); if (!(window.isSecureContext && navigator.clipboard?.writeText)) return; try { await navigator.clipboard.writeText(text); state.textContent = 'Copied to clipboard.'; } catch { selectSecretText(); state.textContent = 'Clipboard copy was denied — press Ctrl/Cmd+C to copy it.'; } };
$('#close-secret').onclick = () => { $('#secret-value').textContent = ''; $('#secret-dialog').close(); };

document.addEventListener('keydown', event => { if (event.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) { event.preventDefault(); const input = $(`#view-${state.view} input[type="search"]`); input?.focus(); } });

// === ACTIVITY LIVING PANE ===
// The graph module + vendored D3 are lazy-loaded on first entry so other
// views never pay the download/parse cost. d3.min.js is same-origin
// (satisfies `script-src 'self'`); the CDN tag from the mockup is never used.
let activityGraphModule = null;
let activityGraphReady = false;
let activityGraphFailed = false;
// Latest catalogue snapshot for the pane; reassigned on refresh so the node
// click-through always reads current rows rather than the load-time closure.
let activityGraphData = { clients: [], virtualModels: [], models: [] };

// A live delta can reference a model or client added after the pane captured
// its catalogue. On that miss we re-fetch the catalogue and relabel the live
// nodes. The minimum interval bounds fetch frequency when an id the catalogue
// never carries keeps appearing (e.g. beyond the client page limit).
const ACTIVITY_CATALOGUE_MIN_INTERVAL = 5000;
let activityCatalogueRefreshTimer = 0;
let activityCatalogueRefreshing = false;
let activityCatalogueRefreshedAt = 0;
function noteActivityCatalogueMiss(ids) {
  if (!ids || !ids.length) return;
  scheduleActivityCatalogueRefresh();
}
function scheduleActivityCatalogueRefresh() {
  if (activityCatalogueRefreshing || activityCatalogueRefreshTimer) return;
  const wait = Math.max(0, ACTIVITY_CATALOGUE_MIN_INTERVAL - (Date.now() - activityCatalogueRefreshedAt));
  activityCatalogueRefreshTimer = setTimeout(() => {
    activityCatalogueRefreshTimer = 0;
    refreshActivityCatalogue();
  }, wait);
}
async function refreshActivityCatalogue() {
  if (activityCatalogueRefreshing || !activityGraphReady || state.view !== 'activity') return;
  activityCatalogueRefreshing = true;
  try {
    const [clients, virtualModels, models] = await Promise.all([
      api('/api/admin/client-keys?limit=200'),
      api('/api/admin/virtual-models?limit=200'),
      api('/api/admin/models?all=1'),
    ]);
    if (!activityGraphReady || state.view !== 'activity') return;
    activityGraphData = {
      clients: clients.data || [],
      virtualModels: virtualModels.data || [],
      models: models.data || [],
    };
    activityGraphModule.updateCatalogue(activityGraphData);
  } catch {
    // Best-effort: the pane keeps its raw-id fallback until the next miss.
  } finally {
    activityCatalogueRefreshing = false;
    activityCatalogueRefreshedAt = Date.now();
  }
}
async function ensureActivityGraph() {
  if (activityGraphModule) return activityGraphModule;
  if (activityGraphFailed) return null;
  try {
    if (typeof d3 === 'undefined') {
      await new Promise((resolve, reject) => {
        const tag = document.createElement('script');
        tag.src = '/d3.min.js';
        tag.onload = resolve;
        tag.onerror = () => reject(new Error('d3 load failed'));
        document.head.appendChild(tag);
      });
    }
    activityGraphModule = await import('/activity-graph.js');
    return activityGraphModule;
  } catch (error) {
    activityGraphFailed = true;
    flash('Activity graph could not load (D3 failed). The Settings table still works.', 'error');
    return null;
  }
}
async function loadActivityView() {
  const token = ++state.loadToken;
  const mod = await ensureActivityGraph();
  if (token !== state.loadToken) return;
  if (!mod) return;
  try {
    // Catalogue payloads resolve live deltas into nodes. No providers fetch:
    // provider names arrive inside virtual targets and real models. No
    // activity seed: the pane starts empty and materializes legs from live
    // `activity` deltas only.
    const [clients, virtualModels, models] = await Promise.all([
      api('/api/admin/client-keys?limit=200'),
      api('/api/admin/virtual-models?limit=200'),
      api('/api/admin/models?all=1'),
    ]);
    if (token !== state.loadToken) return;
    activityGraphData = {
      clients: clients.data || [],
      virtualModels: virtualModels.data || [],
      models: models.data || [],
    };
    activityGraphReady = mod.init($('#view-activity'), {
      ...activityGraphData,
    }, {
      onNodeClick: node => openGraphActivity(node, activityGraphData),
    }) === true;
    // Seed currently-hot legs from live state in case deltas were missed
    // while the view was hidden (state keeps the client + leg lanes).
    if (activityGraphReady) noteActivityCatalogueMiss(mod.onSnapshotSeed({ inflight_client_routes: state.liveRoutes, inflight_targets: state.liveLegs }));
    if (activityGraphReady) mod.onCooldowns(state.usage?.target_cooldown || {});
    renderMobileActivity();
  } catch (error) {
    flash(errorMessage(error), 'error');
  }
}
function mobileActivityCatalogue() {
  return activityGraphData || { clients: [], virtualModels: [], models: [] };
}
function mobileActivityTarget(routeID, targetID) {
  const data = mobileActivityCatalogue();
  const virtual = data.virtualModels.find(model => model.id === routeID);
  if (virtual) {
    const target = (virtual.targets || []).find(item => item.provider_model_id === targetID || `${item.provider_name}/${item.upstream_model_id}` === targetID);
    return target ? `${target.provider_name}/${target.upstream_model_id}` : targetID;
  }
  const model = data.models.find(item => item.id === targetID || item.canonical_model_id === targetID);
  return model ? model.canonical_model_id : targetID;
}
function renderMobileActivity() {
  const data = mobileActivityCatalogue();
  const byRoute = new Map();
  Object.entries(state.liveRoutes || {}).filter(([, item]) => item && item.active > 0).forEach(([key, item]) => {
    const separator = key.indexOf('\u0000');
    const clientID = item.client_id || (separator >= 0 ? key.slice(0, separator) : '');
    const client = data.clients.find(candidate => candidate.id === clientID);
    const routeID = item.route_id || item.routeID;
    const virtual = data.virtualModels.find(model => model.id === routeID);
    const real = data.models.find(model => model.id === routeID || model.canonical_model_id === routeID);
    const routeLabel = virtual?.canonical_model_id || real?.canonical_model_id || item.requested_model || routeID || 'Unknown route';
    const targets = Object.entries(state.liveLegs || {}).filter(([targetKey, target]) => targetKey.startsWith(`${routeID}\u0000`) && target?.active > 0).map(([targetKey]) => mobileActivityTarget(routeID, targetKey.slice(targetKey.indexOf('\u0000') + 1)));
    const targetLabel = item.resolved_model || targets[0] || 'Waiting for upstream target';
    const entryKey = `${clientID}\u0000${routeID}`;
    const existing = byRoute.get(entryKey);
    if (existing) {
      existing.streaming = existing.streaming || item.streaming > 0;
      existing.targets = [...new Set([...existing.targets, ...targets])];
      if (item.resolved_model) existing.target = item.resolved_model;
      return;
    }
    byRoute.set(entryKey, { client: client?.name || clientID || 'Unknown client', route: routeLabel, target: targetLabel, requested: item.requested_model || routeLabel, streaming: item.streaming > 0, targets });
  });
  const entries = [...byRoute.values()];
  const list = $('#mobile-activity-body');
  if (!list) return;
  $('#mobile-activity-empty').hidden = entries.length > 0;
  list.innerHTML = entries.map(entry => { const sameRoute = entry.requested === entry.route; const chain = sameRoute ? `<span>${h(entry.requested)}</span><b aria-hidden="true">→</b><span>${h(entry.target)}</span>` : `<span>${h(entry.requested)}</span><b aria-hidden="true">→</b><span>${h(entry.route)}</span><b aria-hidden="true">→</b><span>${h(entry.target)}</span>`; return `<article class="live-activity-card"><div class="live-activity-top"><span class="live-pulse" aria-hidden="true"></span><strong>${h(entry.client)}</strong><span class="live-activity-state">${entry.streaming ? 'Streaming' : 'In flight'}</span></div><div class="live-activity-chain">${chain}</div>${entry.targets.length > 1 ? `<p class="live-activity-note">${entry.targets.length} upstream targets active</p>` : ''}<button class="btn btn-small btn-secondary" data-live-activity-client="${h(entry.client)}">View details</button></article>`; }).join('');
  $$('[data-live-activity-client]', list).forEach(button => button.onclick = () => {
    const client = data.clients.find(item => item.name === button.dataset.liveActivityClient);
    if (client) openActivity(client);
  });
}
function openGraphActivity(node, data) {
  const id = node.id.slice(2);
  const item = node.kind === 'client'
    ? data.clients.find(client => client.id === id)
    : node.kind === 'route'
      ? data.virtualModels.find(model => model.id === id)
      : data.models.find(model => model.id === id);
  if (!item) {
    flash('Activity details are no longer available. Refresh the Activity view.', 'error');
    return;
  }
  if (node.kind === 'client') openActivity(item);
  else openModelActivity(item, node.kind === 'route' ? 'virtual' : 'real');
}
function destroyActivityView() {
  if (activityGraphModule && activityGraphReady) {
    try { activityGraphModule.destroy(); } catch { /* teardown is best-effort */ }
  }
  activityGraphReady = false;
  if (activityCatalogueRefreshTimer) {
    clearTimeout(activityCatalogueRefreshTimer);
    activityCatalogueRefreshTimer = 0;
  }
}

// === LIVE REFRESH ===
// A single session-lifetime SSE connection pushes outcome deltas (resolution
// icons) and usage snapshots (token/cache counters). DOM writes are suppressed
// while a dialog is open and while the active view does not render the touched
// cells; state is always kept current so a reconcile paint on close/nav is
// instant.
const live = new LiveStream('/api/admin/live', { onAuthFailure: () => showLogin() });
let livePendingReconcile = false;
const liveDialogOpen = () => document.querySelector('dialog[open]') !== null;
const liveViewActive = (...views) => views.includes(state.view);

function markChanged(el) {
  if (!el || el.classList.contains('is-flip')) return;
  el.classList.add('is-flip');
  el.addEventListener('animationend', () => el.classList.remove('is-flip'), { once: true });
}

// Patch a single token cell (Mtok + Cache) from the current state. Mutates
// the existing .tok element in place: rebuilds inner HTML when the
// populated/empty structure changes, otherwise only flips the leaves whose
// text or class actually moved. The cell itself is never replaced, so
// repeated updates cannot nest .tok .tok and a token value reverting to
// zero always clears stale Mtok/cache markup.
function patchTokenCell(cell, tokens, pct) {
  // Once usage has arrived, a still-unknown cell is a genuine empty state, not
  // loading. Rebuild from the loading spinner to the "—"/populated structure.
  if (cell.querySelector('.tok-loading')) {
    if (tokLoading(tokens, pct)) return;
    cell.removeAttribute('aria-busy');
    cell.innerHTML = renderTokInner(tokens, pct);
    const first = $('b', cell);
    if (first) markChanged(first);
    return;
  }
  const populated = Boolean(tokens) || (pct != null && !isNaN(pct));
  const numEl = $('b', cell);
  const cacheEl = $('.cache-hit b', cell);
  const hasStructured = Boolean(numEl) || Boolean(cacheEl);
  if (populated !== hasStructured) {
    cell.innerHTML = renderTokInner(tokens, pct);
    const newNum = $('b', cell);
    if (newNum) markChanged(newNum);
    const newCache = $('.cache-hit b', cell);
    if (newCache) markChanged(newCache);
    return;
  }
  if (!populated) return;
  const num = tokens ? `${(tokens / 1e6).toFixed(2)}` : '';
  const cache = (pct != null && !isNaN(pct)) ? `${Math.round(pct)}%` : '';
  if (numEl && numEl.textContent !== num) {
    numEl.textContent = num;
    markChanged(numEl);
  }
  if (cacheEl && cacheEl.textContent !== cache) {
    cacheEl.textContent = cache;
    markChanged(cacheEl);
  }
}

// Patch the resolution icon for one target line from the current state.
// Always refresh aria-label and title so the tooltip reflects the current
// sub-state (e.g. "No activity recorded" vs "No activity in 24h" share
// the same "neutral" class). SVG and class are only swapped when the
// resolution status itself changes.
function patchResolution(line, target) {
  const [status, label] = resolutionStatus(target);
  const indicator = $('.resolution-indicator', line);
  if (!indicator) return;
  const cls = `resolution-${status}`;
  const row = line.closest('tr[data-virtual-id]');
  const key = targetActivityKey(row?.dataset.virtualId || '', line.dataset.targetKey);
  const active = state.liveLegs[key]?.active > 0;
  const needsSwap = !indicator.classList.contains(cls) || !$('.resolution-indicator-spin', indicator);
  if (needsSwap) {
    indicator.innerHTML = `${RESOLUTION_ICONS[status]}<span class="resolution-indicator-spin" aria-hidden="true"></span>`;
  }
  indicator.className = `resolution-indicator ${cls}${active ? ' resolution-active' : ''}`;
  indicator.setAttribute('aria-label', active ? 'Target request in flight' : label);
  indicator.setAttribute('title', active ? 'Target request in flight' : label);
}

// Reconcile paint: apply the current state to every live cell in the active
// view. Called on snapshot, on dialog close, and on view navigation.
function reconcileLive() {
  if (liveDialogOpen()) { livePendingReconcile = true; return; }
  livePendingReconcile = false;
  // One-time correction after usage first arrives: a models table rendered under
  // a usage sort while usage was unknown is in catalogue order. Re-sort only if
  // the models view is active and the active sort is usage-based; a
  // canonical/provider sort needs no correction. If the view is elsewhere, drop
  // the flag — the next loadModels() renders already-sorted with usage present.
  if (modelsResortPending) {
    modelsResortPending = false;
    if (liveViewActive('models') && MODEL_USAGE_SORTS.has(sortState.column)) reorderModelRows();
  }
  if (liveViewActive('virtual')) {
    state.virtualModels.forEach(model => {
      const row = $(`tr[data-virtual-id="${CSS.escape(model.id)}"]`);
      if (!row) return;
      (model.targets || []).forEach(target => {
        const key = target.provider_model_id || `${target.provider_name}/${target.upstream_model_id}`;
        const line = $(`[data-target-key="${CSS.escape(key)}"]`, row);
        if (line) patchResolution(line, target);
      });
      const canonical = model.canonical_model_id;
      ['1h', '24h', '7d'].forEach(window => {
        const cell = $(`.tok[data-window="${window}"]`, row);
        if (cell) patchTokenCell(cell, state.usage?.virtual_models?.[canonical]?.[window], state.usage?.virtual_cache?.[canonical]?.[window]);
      });
      patchVirtualSpinner(row, routeActivity(model.id));
    });
  }
  if (liveViewActive('models')) {
    state.models.forEach(model => {
      const row = $(`tr[data-model-id="${CSS.escape(model.id)}"]`);
      if (!row) return;
      const canonical = model.canonical_model_id;
      ['1h', '24h', '7d'].forEach(window => {
        const cell = $(`.tok[data-window="${window}"]`, row);
        if (cell) patchTokenCell(cell, state.usage?.real_models?.[canonical]?.[window], state.usage?.real_cache?.[canonical]?.[window]);
      });
    });
  }
  if (liveViewActive('clients')) {
    state.clients.forEach(client => {
      const row = $(`tr[data-client-id="${CSS.escape(client.id)}"]`);
      if (row) {
        ['1h', '24h', '7d'].forEach(window => {
          const cell = $(`.tok[data-window="${window}"]`, row);
          if (cell) patchTokenCell(cell, state.usage?.client_keys?.[client.id]?.[window], state.usage?.client_cache?.[client.id]?.[window]);
        });
        applyClientRoundel($('.status-roundel', row), client, state.liveRequests[client.id]);
      }
      const card = $(`article.client-card[data-client-id="${CSS.escape(client.id)}"]`);
      if (card) {
        ['1h', '24h', '7d'].forEach(window => {
          const cells = $$(`.tok[data-window="${window}"]`, card);
          const values = state.usage?.client_keys?.[client.id]?.[window];
          const caches = state.usage?.client_cache?.[client.id]?.[window];
          cells.forEach(cell => patchTokenCell(cell, values, caches));
        });
        applyClientRoundel($('.status-roundel', card), client, state.liveRequests[client.id]);
      }
    });
  }
}

live.on('outcome', payload => {
  if (!state.usage) state.usage = {};
  if (!state.usage.target_last_outcome) state.usage.target_last_outcome = {};
// Only degrading outcomes update main-page target health. A genuine upstream
// failure degrades its target even when a later fallback served the request;
// skipped (never-called) and client-caused outcomes are non-degrading. The
// graph still receives every attempt's explicit outcome below.
  const degrading = new Set();
  for (const [key, outcome] of Object.entries(payload || {})) {
    if (outcome && outcome.degrading === false) continue;
    state.usage.target_last_outcome[key] = outcome;
    degrading.add(key);
  }
  if (liveViewActive('virtual') && !liveDialogOpen()) {
    state.virtualModels.forEach(model => (model.targets || []).forEach(target => {
      const key = target.provider_model_id || `${target.provider_name}/${target.upstream_model_id}`;
      if (!degrading.has(key)) return;
      const row = $(`tr[data-virtual-id="${CSS.escape(model.id)}"]`);
      const line = row && $(`[data-target-key="${CSS.escape(key)}"]`, row);
      if (line) patchResolution(line, target);
    }));
  }
  // Activity living pane: the explicit outcome colours the model roundel
  // (served/failed/skipped); the `activity` deltas drive the flow itself.
  if (liveViewActive('activity') && activityGraphReady && activityGraphModule) {
    try { activityGraphModule.onOutcome(payload); } catch { /* pane update is best-effort */ }
  }
});

live.on('snapshot', payload => {
  if (!state.usage) state.usage = {};
  ['target_last_outcome', 'target_cooldown', 'target_health', 'virtual_models', 'client_keys', 'real_models', 'virtual_cache', 'client_cache', 'real_cache'].forEach(key => {
    if (payload[key] !== undefined) state.usage[key] = payload[key];
  });
  // The SSE baseline snapshot already carries the usage envelope, so mark it
  // fresh: a view load in the next USAGE_REUSE_MS window reuses it instead of
  // firing a redundant /api/admin/usage request on first open. markUsageReady
  // also flips the token placeholders from the loading spinner to real
  // values/empty and flags the one-time model-table re-sort.
  state.usageAt = Date.now(); markUsageReady();
  if (payload.modules?.inflight_clients !== undefined) state.liveRequests = payload.modules.inflight_clients || {};
  if (payload.modules?.inflight_client_routes !== undefined) state.liveRoutes = payload.modules.inflight_client_routes || {};
  if (payload.modules?.inflight_targets !== undefined) state.liveLegs = payload.modules.inflight_targets || {};
  if (liveViewActive('activity')) renderMobileActivity();
  // Activity living pane: seed currently-hot legs on every snapshot so a
  // missed delta self-heals without a refresh.
  if (liveViewActive('activity') && activityGraphReady && activityGraphModule) {
    try { noteActivityCatalogueMiss(activityGraphModule.onSnapshotSeed(payload.modules)); } catch { /* pane update is best-effort */ }
    try { activityGraphModule.onCooldowns(state.usage?.target_cooldown || {}); } catch { /* pane update is best-effort */ }
  }
  reconcileLive();
});

live.on('activity', delta => {
  // An explicit terminal skip carries full client + route + target context
  // but is not an in-flight request, so it must not touch the live counters.
  // Forward it to the graph (which paints the amber roundel) and stop.
  if (delta.result === 'skipped') {
    if (liveViewActive('activity') && activityGraphReady && activityGraphModule) {
      try { noteActivityCatalogueMiss(activityGraphModule.onActivityDelta(delta)); } catch { /* pane update is best-effort */ }
    }
    return;
  }
  // Single-ticket liveness: client deltas (with route ID) accumulate in both
  // liveRoutes (per client+route, for spinners and the Activity graph) and
  // liveRequests (per-client aggregate, for the client roundel); target deltas
  // accumulate in liveLegs. No separate route-level lane exists, so there is
  // nothing to double-count.
  if (delta.client_id) {
    const routeID = delta.id || '';
    const ticket = routeTicketKey(delta.client_id, routeID);
    const current = state.liveRoutes[ticket] || { active: 0, streaming: 0 };
    current.active += delta.active || 0;
    current.streaming += delta.streaming || 0;
    if (delta.id) current.routeID = delta.id;
    if (delta.requested_model) current.requestedModel = delta.requested_model;
    if (delta.resolved_model) current.resolvedModel = delta.resolved_model;
    if (current.active <= 0 && current.streaming <= 0) delete state.liveRoutes[ticket];
    else state.liveRoutes[ticket] = current;
    // Per-client aggregate for the client status roundel.
    const agg = state.liveRequests[delta.client_id] || { active: 0, streaming: 0 };
    agg.active += delta.active || 0;
    agg.streaming += delta.streaming || 0;
    if (agg.active <= 0 && agg.streaming <= 0) delete state.liveRequests[delta.client_id];
    else state.liveRequests[delta.client_id] = agg;
    if (liveViewActive('virtual') && !liveDialogOpen() && delta.id) {
      const row = $(`tr[data-virtual-id="${CSS.escape(delta.id)}"]`);
      if (row) patchVirtualSpinner(row, routeActivity(delta.id));
    }
    if (liveViewActive('clients') && !liveDialogOpen()) {
      const row = $(`tr[data-client-id="${CSS.escape(delta.client_id)}"]`);
      if (row) patchClientRoundelRow(row);
      const card = $(`article.client-card[data-client-id="${CSS.escape(delta.client_id)}"]`);
      if (card) {
        const client = state.clients.find(item => item.id === delta.client_id);
        if (client) applyClientRoundel($('.status-roundel', card), client, state.liveRequests[delta.client_id]);
      }
    }
    if (liveViewActive('activity')) renderMobileActivity();
  }
  if (delta.target_id) {
    const key = targetActivityKey(delta.id || '', delta.target_id);
    const current = state.liveLegs[key] || { active: 0 };
    current.active += delta.active || 0;
    if (current.active <= 0) delete state.liveLegs[key];
    else state.liveLegs[key] = current;
    if (liveViewActive('virtual') && !liveDialogOpen()) {
      const row = $(`tr[data-virtual-id="${CSS.escape(delta.id || '')}"]`, $('#virtual-body'));
      const line = row && $(`[data-target-key="${CSS.escape(delta.target_id)}"]`, row);
      const model = row && state.virtualModels.find(item => item.id === row.dataset.virtualId);
      const target = model && (model.targets || []).find(item => (item.provider_model_id || `${item.provider_name}/${item.upstream_model_id}`) === delta.target_id);
      if (line && target) patchResolution(line, target);
    }
    if (liveViewActive('activity')) renderMobileActivity();
  }
  // Activity living pane: client + target deltas drive the flow.
  if (liveViewActive('activity') && activityGraphReady && activityGraphModule) {
    try { noteActivityCatalogueMiss(activityGraphModule.onActivityDelta(delta)); } catch { /* pane update is best-effort */ }
  }
});

// Reconcile once when the last dialog closes.
$$('dialog').forEach(dialog => dialog.addEventListener('close', () => {
  if (!liveDialogOpen() && livePendingReconcile) reconcileLive();
}));

// Reconcile on view navigation so a freshly-rendered view reflects current state.
const liveNavigate = navigate;
navigate = function (view) {
  liveNavigate(view);
  if (liveViewActive('models', 'virtual', 'clients')) reconcileLive();
  if (liveViewActive('activity') && activityGraphReady && activityGraphModule) {
    try { noteActivityCatalogueMiss(activityGraphModule.onSnapshotSeed({ inflight_client_routes: state.liveRoutes, inflight_targets: state.liveLegs })); } catch { /* pane update is best-effort */ }
    try { activityGraphModule.onCooldowns(state.usage?.target_cooldown || {}); } catch { /* pane update is best-effort */ }
  }
};

function liveStart() { live.start(); }
function liveStop() { live.stop(); }

(async function initialise() {
  try {
    const runtime = await fetch('/api/runtime', { credentials: 'same-origin' }).then(res => res.json());
    runtimeMode = runtime.mode === 'hosted' ? 'hosted' : 'local';
    if (runtimeMode === 'hosted') {
      $('#login-identity-label').firstChild.textContent = 'Email ';
      $('#login-submit').textContent = 'Sign in';
      try { hostedAuthOptions = await api('/api/auth/options'); } catch { hostedAuthOptions = {}; }
      $('#google-signin').hidden = !hostedAuthOptions.google_enabled;
    }
    const path = location.pathname;
    const query = new URLSearchParams(location.search);
    const token = query.get('token');
    if (runtimeMode === 'hosted' && path.startsWith('/platform')) {
      try {
        const session = await api('/api/platform/session');
        state.csrf = session.csrf_token;
        $('#login-shell').hidden = true;
        $('#platform-shell').hidden = false;
        await loadPlatformDashboard();
      } catch { showLogin(); }
      return;
    }
    if (runtimeMode === 'hosted' && path === '/verify-email' && token) {
      authView('verify-panel');
      try { const result = await api('/api/auth/verify-email', { method: 'POST', body: JSON.stringify({ token }) }); history.replaceState(null, '', '/login'); if (result && result.authenticated) { $('#verify-message').textContent = 'Your email is verified.'; showApp(result); return; } $('#verify-message').textContent = 'Your email is verified.'; $('#verify-login').hidden = false; } catch (error) { $('#verify-message').textContent = ''; $('#verify-error').textContent = errorMessage(error, 'Verification failed.'); $('#verify-email-wrap').hidden = false; exposeResend('#resend-verification', '#verify-email', '#verify-error'); }
      return;
    }
    if (runtimeMode === 'hosted' && path === '/reset-password' && token) authView('reset-form');
    else if (runtimeMode === 'hosted' && query.get('google_signup') === '1') {
      history.replaceState(null, '', '/login');
      authView('google-consent-form');
      return;
    }
    else if (runtimeMode === 'hosted' && path === '/confirm-email-change' && token) {
      authView('verify-panel');
      try {
        const result = await api('/api/auth/email-change/confirm', { method: 'POST', body: JSON.stringify({ token }) });
        $('#verify-message').textContent = `Your email address is now ${result.email}.`;
        $('#verify-login').hidden = false;
        history.replaceState(null, '', '/login');
      } catch (error) {
        $('#verify-message').textContent = '';
        showAuthError('verify-error', error, 'This email-change link is invalid or expired.');
      }
      return;
    }
    else if (runtimeMode === 'hosted' && path.startsWith('/legal/')) {
      await showLegalDocument(path.slice('/legal/'.length));
      return;
    }
    else {
      state.view = viewFromHash();
      const sessionPath = runtimeMode === 'hosted' ? '/api/auth/session' : '/api/admin/session';
      const authError = runtimeMode === 'hosted' ? query.get('auth_error') : '';
      const googleLinked = runtimeMode === 'hosted' && query.get('google_linked') === '1';
      const googleReauth = runtimeMode === 'hosted' && query.get('google_reauth') === '1';
      if (authError || googleLinked || googleReauth) history.replaceState(null, '', location.pathname + location.hash);
      try {
        const session = await api(sessionPath);
        showApp(session);
        if (googleLinked) flash('Google is linked to your account.');
        else if (googleReauth) flash('Google confirmed your identity. Complete the account change within five minutes.');
        else if (authError) flash(googleAuthErrorMessage(authError), 'error');
      } catch {
        showLogin();
        if (authError) $('#login-error').textContent = googleAuthErrorMessage(authError);
      }
    }
  } catch { showLogin(); }
})();

function googleAuthErrorMessage(code) {
  const messages = {
    google_failed: 'Google sign-in could not be completed. Try again.',
    google_expired: 'That Google sign-in link expired. Start again.',
    google_unavailable: 'Google sign-in is temporarily unavailable.',
    google_link_required: 'This Google email already has a Tiller account. Sign in to it, then link Google in Account settings.',
    google_already_linked: 'That Google account is already linked to a Tiller account.',
    google_session_expired: 'Your Tiller session expired. Sign in and try again.',
    signup_unavailable: 'Signup is currently unavailable.',
  };
  return messages[code] || 'Google sign-in could not be completed. Try again.';
}

async function loadPlatformDashboard() {
  const token = ++state.platformUsersLoadToken;
  const settings = await api('/api/platform/settings');
  $('#backup-card').hidden = true;
  const form = $('#platform-settings-form');
  form.elements.hosted_signup_enabled.checked = !!settings.hosted_signup_enabled;
  form.elements.audit_retention_days.value = settings.audit_retention_days;
  form.elements.mail_provider.value = settings.mail?.provider || '';
  applyMailProviderVisibility(settings.mail?.provider || '');
  form.elements.mail_from.value = settings.mail?.from || '';
   form.elements.mail_smtp_host.value = settings.mail?.smtp_host || '';
   form.elements.mail_smtp_username.value = settings.mail?.smtp_username || '';
   form.elements.mail_smtp_port.value = settings.mail?.smtp_port || '';
  form.elements.mail_smtp_mode.value = settings.mail?.smtp_mode || 'starttls';
  form.elements.google_signin_enabled.checked = !!settings.google?.enabled;
  form.elements.google_client_id.value = settings.google?.client_id || '';
  form.elements.google_client_secret.value = '';
  form.elements.clear_google_client_secret.checked = false;
  form.elements.turnstile_enabled.checked = !!settings.turnstile?.enabled;
  form.elements.turnstile_site_key.value = settings.turnstile?.site_key || '';
  form.elements.turnstile_secret.value = '';
  form.elements.clear_turnstile_secret.checked = false;
  $('#google-settings-status').textContent = `Client secret ${settings.google?.secret_configured ? 'stored' : 'missing'}. Redirect URI: ${settings.google?.redirect_uri || ''}`;
  $('#turnstile-settings-status').textContent = `Secret key ${settings.turnstile?.secret_configured ? 'stored' : 'missing'}. Challenge hostname: ${settings.turnstile?.hostname || ''}`;
  $('#platform-mail-status').textContent = settings.mail?.configured ? `Mail configured (${settings.mail.provider}); secret ${settings.mail.secret_configured ? 'stored' : 'missing'}.` : 'Mail is not configured.';
  await loadPlatformPlans();
  const params = new URLSearchParams({ limit: '100', offset: String(state.platformUsersOffset) });
  if (state.platformUsersSearch) params.set('search', state.platformUsersSearch);
  const users = await api(`/api/platform/users?${params}`);
  if (token !== state.platformUsersLoadToken) return;
  const userRows = users.data || [];
  $('#platform-users-list').innerHTML = userRows.map(user => `<div class="platform-list-item"><strong>${h(user.email)}</strong><small>${h(user.account_id)} · ${h(user.plan)} · <span class="platform-status ${user.account_status === 'active' ? 'good' : 'bad'}">${h(user.account_status)}</span></small><div class="platform-list-actions">${user.account_status === 'deleting' ? `<button class="btn btn-small btn-danger" data-account-retry="${h(user.account_id)}">Retry deletion</button>` : `<span class="plan-assign"><select class="plan-select" data-account-plan="${h(user.account_id)}" aria-label="Plan for ${h(user.email)}">${planOptionList(user.plan)}</select><button class="btn btn-small btn-secondary" data-account-plan-save="${h(user.account_id)}">Save plan</button></span><button class="btn btn-small btn-secondary" data-account-status="${h(user.account_id)}" data-status="${user.account_status === 'suspended' ? 'active' : 'suspended'}">${user.account_status === 'suspended' ? 'Unsuspend' : 'Suspend'}</button><button class="btn btn-small btn-danger" data-account-delete="${h(user.account_id)}">Delete</button>`}</div></div>`).join('') || '<p class="meta-line">No hosted users.</p>';
  $('#platform-users-count').textContent = userRows.length ? `${state.platformUsersOffset + 1}–${state.platformUsersOffset + userRows.length}` : '0 results';
  $('#platform-users-prev').disabled = state.platformUsersOffset === 0;
  $('#platform-users-next').disabled = state.platformUsersOffset >= 10000 || userRows.length < 100;
  const audit = await api('/api/platform/audit?limit=100');
  $('#platform-audit-list').innerHTML = (audit.data || []).map(row => `<div class="platform-list-item"><strong>${h(row.event)}</strong><small>${h(row.created_at)} · ${h(row.target_id || '')}</small></div>`).join('') || '<p class="meta-line">No platform events.</p>';
  try {
    const queue = await api('/api/platform/mail/queue');
    $('#platform-mail-queued').textContent = queue.queued;
    $('#platform-mail-sent').textContent = queue.sent_recent;
    $('#platform-mail-dead').textContent = queue.dead_recent;
    $('#platform-mail-log').innerHTML = (queue.log || []).map(row => `<div class="platform-list-item"><strong>${h(mailTypeLabel(row.type))}</strong><small>${h(row.recipient)} · ${h(row.status)} · ${h(row.attempts)} attempt(s) · ${h(row.created_at)}</small></div>`).join('') || '<p class="meta-line">No recent mail.</p>';
    $('#platform-mail-error').textContent = '';
  } catch (error) {
    $('#platform-mail-error').textContent = errorMessage(error, 'Could not load the mail queue.');
  }
  await loadPlatformLegal();
}

// mailTypeLabel turns a mail_outbox type into a short human label for the
// operator log; unknown types fall through to the raw value.
function mailTypeLabel(type) {
  const labels = { verify_email: 'Verification', password_reset: 'Password reset', email_change_confirm: 'Email change', email_change_warning: 'Email change warning' };
  return labels[type] || type;
}

// planOptionList builds the <option> set for an account's plan dropdown from
// the loaded plan catalogue, keeping the account's current plan selected even if
// it is no longer in the catalogue.
function planOptionList(current) {
  const names = state.platformPlans.slice();
  if (current && !names.includes(current)) names.push(current);
  return names.map(name => `<option value="${h(name)}"${name === current ? ' selected' : ''}>${h(name)}</option>`).join('');
}

// PLAN_FIELDS drives both the caps dialog and the row summary.
const PLAN_FIELDS = [['max_providers', 'Providers'], ['max_client_keys', 'Client keys'], ['max_virtual_models', 'Virtual models'], ['max_concurrent_streams', 'Streams'], ['activity_retention_days', 'Retention days'], ['monthly_requests', 'Monthly requests']];

// planSummary is the compact one-line cap readout shown on each plan row.
function planSummary(plan) {
  return PLAN_FIELDS.map(([key, label]) => `${plan[key] === -1 ? '∞' : h(plan[key])} ${h(label.toLowerCase())}`).join(' · ');
}

// openPlanEditor opens the shared entity dialog to add or edit a plan. The name
// is renameable on edit (the backend repoints account assignments); add
// requires a lowercase slug. Cancel comes from the dialog itself.
function openPlanEditor(plan) {
  const editing = Boolean(plan);
  const nameRules = editing ? '' : ' pattern="[a-z0-9][a-z0-9_-]{0,63}" title="Lowercase letters, digits, dashes or underscores" required';
  const nameField = `<label>Name<input name="name" type="text" value="${editing ? h(plan.name) : ''}" placeholder="e.g. pro"${nameRules} autocomplete="off" spellcheck="false"></label>`;
  const caps = PLAN_FIELDS.map(([key, label]) => `<label>${h(label)}<input type="number" min="-1" name="${key}" value="${editing ? h(plan[key]) : '-1'}"></label>`).join('');
  openEntity({
    eyebrow: editing ? 'EDIT PLAN' : 'NEW PLAN',
    title: editing ? `Edit ${plan.name}` : 'Add plan',
    fields: nameField + `<div class="platform-plan-fields">${caps}</div>`,
    submit: editing ? 'Save plan' : 'Add plan',
    onSubmit: async form => {
      const values = new FormData(form);
      const name = String(values.get('name') || '').trim();
      if (!name) throw new Error('Enter a plan name.');
      const payload = { name };
      for (const [key] of PLAN_FIELDS) payload[key] = Number(values.get(key));
      if (editing) await api(`/api/platform/plans/${encodeURIComponent(plan.name)}`, { method: 'PUT', body: JSON.stringify(payload) });
      else await api('/api/platform/plans', { method: 'POST', body: JSON.stringify(payload) });
      flash(editing ? 'Plan saved.' : 'Plan added.');
      await loadPlatformDashboard();
    },
  });
}

// loadPlatformPlans renders the entitlements catalogue as compact rows. A row
// (or its Edit button) opens the caps dialog; Delete is guarded server-side.
async function loadPlatformPlans() {
  const list = $('#platform-plans-list'); if (!list) return;
  try {
    const result = await api('/api/platform/plans');
    const plans = result.data || [];
    state.platformPlans = plans.map(plan => plan.name);
    state.platformPlanData = plans;
    list.innerHTML = plans.map(plan => `<div class="platform-list-item platform-plan-row" data-plan="${h(plan.name)}" role="button" tabindex="0"><div><strong>${h(plan.name)}</strong><small>${planSummary(plan)}</small></div><div class="platform-list-actions"><button class="btn btn-small btn-secondary" type="button" data-plan-edit="${h(plan.name)}">Edit</button><button class="btn btn-small btn-danger" type="button" data-plan-delete="${h(plan.name)}">Delete</button></div></div>`).join('') || '<p class="meta-line">No plans.</p>';
    $('#platform-plans-error').textContent = '';
  } catch (error) {
    $('#platform-plans-error').textContent = errorMessage(error, 'Could not load plans.');
  }
}

// loadPlatformLegal renders the editable legal documents.
async function loadPlatformLegal() {
  const list = $('#platform-legal-list'); if (!list) return;
  try {
    const result = await api('/api/platform/legal');
    list.innerHTML = (result.data || []).map(doc => `<form class="platform-legal-form" data-slug="${h(doc.slug)}"><strong>${h(doc.title)}</strong><label>Title<input type="text" name="title" value="${h(doc.title)}"></label><label>Body<textarea name="body" rows="8">${h(doc.body)}</textarea></label><button class="btn btn-small btn-secondary" type="submit">Publish</button></form>`).join('') || '<p class="meta-line">No documents.</p>';
    $('#platform-legal-error').textContent = '';
  } catch (error) {
    $('#platform-legal-error').textContent = errorMessage(error, 'Could not load legal documents.');
  }
}

function planForRow(el) { return state.platformPlanData.find(plan => plan.name === el.dataset.plan) || null; }
$('#platform-plan-add').addEventListener('click', () => openPlanEditor(null));
$('#platform-plans-list').addEventListener('click', async event => {
  const edit = event.target.closest('[data-plan-edit]');
  const del = event.target.closest('[data-plan-delete]');
  const row = event.target.closest('.platform-plan-row');
  try {
    if (edit) { const plan = state.platformPlanData.find(item => item.name === edit.dataset.planEdit); if (plan) openPlanEditor(plan); return; }
    if (del) { const name = del.dataset.planDelete; if (!await confirmAction({ title: `Delete ${name}?`, copy: 'Accounts still on this plan must be moved first. This cannot be undone.', action: 'Delete plan', typeMatch: name, typeLabel: 'plan name' })) return; await api(`/api/platform/plans/${encodeURIComponent(name)}`, { method: 'DELETE' }); flash('Plan deleted.'); await loadPlatformDashboard(); return; }
    if (row) { const plan = planForRow(row); if (plan) openPlanEditor(plan); }
  } catch (error) { $('#platform-plans-error').style.color = ''; $('#platform-plans-error').textContent = errorMessage(error, 'Plan operation failed.'); }
});
$('#platform-plans-list').addEventListener('keydown', event => {
  if (event.key !== 'Enter' && event.key !== ' ') return;
  if (event.target.closest('[data-plan-edit],[data-plan-delete]')) return;
  const row = event.target.closest('.platform-plan-row'); if (!row) return;
  event.preventDefault(); const plan = planForRow(row); if (plan) openPlanEditor(plan);
});
$('#platform-legal-list').addEventListener('submit', async event => {
  const form = event.target.closest('.platform-legal-form'); if (!form) return;
  event.preventDefault(); const data = new FormData(form);
  try { await api(`/api/platform/legal/${encodeURIComponent(form.dataset.slug)}`, { method: 'PUT', body: JSON.stringify({ title: data.get('title'), body: data.get('body') }) }); $('#platform-legal-error').textContent = 'Published.'; $('#platform-legal-error').style.color = 'var(--green)'; }
  catch (error) { $('#platform-legal-error').style.color = ''; $('#platform-legal-error').textContent = errorMessage(error, 'Could not publish the document.'); }
});
function applyMailProviderVisibility(provider) {
  document.querySelectorAll('#platform-settings-form [data-mail-when]').forEach(el => {
    el.hidden = provider === '' || (el.dataset.mailWhen !== 'any' && el.dataset.mailWhen !== provider);
  });
}
document.querySelector('#platform-settings-form [name="mail_provider"]').addEventListener('change', event => applyMailProviderVisibility(event.target.value));
$('#platform-settings-form').addEventListener('submit', async event => { event.preventDefault(); const form = new FormData(event.currentTarget); const payload = { hosted_signup_enabled: form.get('hosted_signup_enabled') === 'on', audit_retention_days: Number(form.get('audit_retention_days')), mail_provider: form.get('mail_provider'), mail_from: form.get('mail_from'), mail_smtp_host: form.get('mail_smtp_host'), mail_smtp_port: Number(form.get('mail_smtp_port')) || 0, mail_smtp_mode: form.get('mail_smtp_mode'), mail_smtp_username: form.get('mail_smtp_username'), google_signin_enabled: form.get('google_signin_enabled') === 'on', google_client_id: form.get('google_client_id'), clear_google_client_secret: form.get('clear_google_client_secret') === 'on', turnstile_enabled: form.get('turnstile_enabled') === 'on', turnstile_site_key: form.get('turnstile_site_key'), clear_turnstile_secret: form.get('clear_turnstile_secret') === 'on' }; const resendKey = String(form.get('mail_resend_api_key') || ''); const brevoKey = String(form.get('mail_brevo_api_key') || ''); const smtpPassword = String(form.get('mail_smtp_password') || ''); const googleSecret = String(form.get('google_client_secret') || ''); const turnstileSecret = String(form.get('turnstile_secret') || ''); if (resendKey) payload.mail_resend_api_key = resendKey; if (brevoKey) payload.mail_brevo_api_key = brevoKey; if (smtpPassword) payload.mail_smtp_password = smtpPassword; if (googleSecret) payload.google_client_secret = googleSecret; if (turnstileSecret) payload.turnstile_secret = turnstileSecret; try { await api('/api/platform/settings', { method: 'PUT', body: JSON.stringify(payload) }); $('#platform-settings-error').textContent = 'Saved.'; $('#platform-settings-error').style.color = 'var(--green)'; await loadPlatformDashboard(); } catch (error) { showAuthError('platform-settings-error', error, 'Could not save platform settings.'); } });
$('#platform-users-list').addEventListener('click', async event => { const status = event.target.closest('[data-account-status]'); const deletion = event.target.closest('[data-account-delete], [data-account-retry]'); const planSave = event.target.closest('[data-account-plan-save]'); try { if (status) { await api(`/api/platform/accounts/${encodeURIComponent(status.dataset.accountStatus)}/${status.dataset.status === 'active' ? 'unsuspend' : 'suspend'}`, { method: 'POST', body: '{}' }); await loadPlatformDashboard(); } if (deletion) { const accountID = deletion.dataset.accountDelete || deletion.dataset.accountRetry; if (deletion.dataset.accountRetry || window.confirm(`Delete account ${accountID}? This is immediate and irreversible.`)) { await api(`/api/platform/accounts/${encodeURIComponent(accountID)}`, { method: 'DELETE', body: JSON.stringify({ confirm: accountID }) }); await loadPlatformDashboard(); } } if (planSave) { const accountID = planSave.dataset.accountPlanSave; const plan = planSave.closest('.plan-assign')?.querySelector('select')?.value; if (plan) { await api(`/api/platform/accounts/${encodeURIComponent(accountID)}/plan`, { method: 'POST', body: JSON.stringify({ plan }) }); await loadPlatformDashboard(); } } } catch (error) { $('#platform-users-error').textContent = errorMessage(error, 'Platform operation failed.'); } });
  $('#platform-users-prev').onclick = () => { state.platformUsersOffset = Math.max(0, state.platformUsersOffset - 100); loadPlatformDashboard().catch(error => { $('#platform-users-error').textContent = errorMessage(error, 'Could not load hosted users.'); }); };
$('#platform-users-next').onclick = () => { state.platformUsersOffset += 100; loadPlatformDashboard().catch(error => { $('#platform-users-error').textContent = errorMessage(error, 'Could not load hosted users.'); }); };
filterInput('#platform-user-search', value => { state.platformUsersSearch = value.trim(); state.platformUsersOffset = 0; loadPlatformDashboard().catch(error => { $('#platform-users-error').textContent = errorMessage(error, 'Could not load hosted users.'); }); });
