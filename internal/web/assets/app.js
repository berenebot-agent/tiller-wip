const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const state = { csrf: '', view: 'clients', providers: [], models: [], groups: [], virtualModels: [], clients: [], permissionData: null, providerTypes: [], usage: null };
const collapsedModels = new Set(); const collapsedVirtual = new Set(); const collapsedClients = new Set(); const collapsedPermissionGroups = new Set(); const collapsedPermissionSections = new Set();
const GROUP_ARROW = { up: '▼', down: '▶' };
const MODEL_EXPAND_BATCH_SIZE = 20;
const groupRevealFrames = new WeakMap();
const h = value => String(value ?? '').replace(/[&<>'"]/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' })[char]);
const date = value => value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Never';
const tok = (tokens, pct) => {
  if (!tokens && pct == null) return '—';
  const num = tokens ? `<b>${(tokens / 1e6).toFixed(2)}</b><small>Mtok</small>` : '';
  const cache = (pct != null && !isNaN(pct))
    ? `<span class="cache-hit"><b>${Math.round(pct)}%</b><small>Cache</small></span>`
    : `<span class="cache-hit na"><small>n.a. Cache</small></span>`;
  return `<span class="tok">${num}${cache}</span>`;
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
const VIEWS = ['providers', 'models', 'virtual', 'clients', 'settings'];
const viewFromHash = () => { const v = (location.hash.replace(/^#\/?/, '') || 'clients'); return VIEWS.includes(v) ? v : 'clients'; };

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body && !(options.body instanceof FormData)) headers.set('Content-Type', 'application/json');
  if (state.csrf && options.method && !['GET', 'HEAD'].includes(options.method)) headers.set('X-CSRF-Token', state.csrf);
  const response = await fetch(path, { credentials: 'same-origin', ...options, headers });
  const type = response.headers.get('content-type') || '';
  const payload = type.includes('json') ? await response.json().catch(() => ({})) : null;
  if (!response.ok) {
    if (response.status === 401 && path !== '/api/admin/session') showLogin();
    const error = new Error(payload?.error?.message || `Request failed (${response.status})`);
    error.code = payload?.error?.code;
    error.status = response.status;
    throw error;
  }
  return payload;
}

function showLogin() { $('#app').hidden = true; $('#login-shell').hidden = false; state.csrf = ''; history.replaceState(null, '', '#/clients'); }
function showApp(session) { state.csrf = session.csrf_token; $('#admin-name').textContent = session.username; $('#login-shell').hidden = true; $('#app').hidden = false; navigate(state.view); }
function flash(message, kind = 'success') { const box = $('#flash'); box.textContent = message; box.className = `flash flash-${kind}`; box.hidden = false; clearTimeout(flash.timer); flash.timer = setTimeout(() => box.hidden = true, 5000); }
function errorMessage(error, fallback = 'The operation could not be completed.') { return error?.message || fallback; }

$('#login-form').addEventListener('submit', async event => {
  event.preventDefault(); $('#login-error').textContent = '';
  const formElement = event.currentTarget; const form = new FormData(formElement); const button = $('button[type="submit"]', formElement); button.disabled = true;
  try { const session = await api('/api/admin/session', { method: 'POST', body: JSON.stringify({ username: form.get('username'), password: form.get('password') }) }); formElement.reset(); showApp(session); }
  catch (error) { $('#login-error').textContent = errorMessage(error, 'Login failed.'); }
  finally { button.disabled = false; }
});
$('#logout').addEventListener('click', async () => { try { await api('/api/admin/session', { method: 'DELETE' }); } finally { showLogin(); } });

async function navigate(view) {
  state.view = view; if (location.hash !== '#' + view) history.pushState(null, '', '#' + view); $$('.view').forEach(panel => panel.classList.toggle('active', panel.id === `view-${view}`)); $$('[data-view]').forEach(button => button.classList.toggle('active', button.dataset.view === view)); $('#nav-links').classList.remove('open'); $('#mobile-menu').setAttribute('aria-expanded', 'false');
  try { if (view === 'providers') await loadProviders(); if (view === 'models') await loadModels(); if (view === 'virtual') await loadVirtual(); if (view === 'clients') await loadClients(); if (view === 'settings') await loadSettings(); }
  catch (error) { flash(errorMessage(error), 'error'); }
}
$$('[data-view]').forEach(link => link.addEventListener('click', event => { if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return; event.preventDefault(); navigate(link.dataset.view); }));
window.addEventListener('popstate', () => navigate(viewFromHash()));
$('#mobile-menu').addEventListener('click', event => { const links = $('#nav-links'); links.classList.toggle('open'); event.currentTarget.setAttribute('aria-expanded', String(links.classList.contains('open'))); });
$$('[data-refresh-view]').forEach(button => button.addEventListener('click', () => navigate(button.dataset.refreshView)));

let filterTimers = new Map();
function filterInput(selector, callback) { $(selector).addEventListener('input', event => { clearTimeout(filterTimers.get(selector)); filterTimers.set(selector, setTimeout(() => callback(event.target.value), 180)); }); }
filterInput('#provider-search', loadProviders); filterInput('#model-search', loadModels); filterInput('#virtual-search', loadVirtual); filterInput('#client-search', loadClients);
$('#show-retired').addEventListener('change', renderModels);
$('#client-group-filter').addEventListener('change', loadClients);

async function loadProviders(search = $('#provider-search').value) {
  const [result, types] = await Promise.all([api(`/api/admin/providers?limit=200&search=${encodeURIComponent(search || '')}`), state.providerTypes.length ? Promise.resolve({ data: state.providerTypes }) : api('/api/admin/provider-types')]);
  state.providers = result.data; state.providerTypes = types.data; renderProviders();
}
function renderProviders() {
  const body = $('#providers-body'); $('#providers-empty').hidden = state.providers.length > 0; body.innerHTML = state.providers.map(provider => `<tr>
    <td class="primary-cell"><strong>${h(provider.name)}</strong><small>${h(provider.base_url)}</small>${provider.last_refresh_error ? `<span class="error-text">${h(provider.last_refresh_error)}</span>` : ''}</td>
    <td><strong>${h(typeLabel(provider.type))}</strong><div class="protocols">${provider.protocols.map(p => `<span class="protocol">${h(p)}</span>`).join('')}</div></td>
    <td><strong>${provider.available_model_count}</strong> available${provider.model_count !== provider.available_model_count ? `<span class="meta-line"> · ${provider.model_count - provider.available_model_count} retired</span>` : ''}</td>
    <td><span class="meta-line">${date(provider.last_refresh_at)}</span></td>
    <td>${badge(provider.enabled && !provider.last_refresh_error, provider.enabled ? (provider.last_refresh_error ? 'Refresh error' : 'Enabled') : 'Disabled', provider.enabled ? (provider.last_refresh_error ? 'warn' : 'good') : 'neutral')}<div class="meta-line">Credential: ${provider.credential_configured ? 'configured' : 'none'}</div></td>
    <td><div class="actions"><button class="btn btn-small btn-secondary" data-provider-refresh="${h(provider.id)}">Refresh</button><button class="btn btn-small btn-secondary" data-provider-edit="${h(provider.id)}">Edit</button><button class="btn btn-small btn-danger" data-provider-delete="${h(provider.id)}">Delete</button></div></td></tr>`).join('');
  const available = state.providers.reduce((sum, item) => sum + item.available_model_count, 0), retired = state.providers.reduce((sum, item) => sum + item.model_count - item.available_model_count, 0), errors = state.providers.filter(item => item.last_refresh_error).length;
  $('#provider-metrics').innerHTML = metric(state.providers.length, 'Provider instances') + metric(available, 'Available models') + metric(retired, 'Retired models') + metric(errors, 'Refresh errors');
  $$('[data-provider-refresh]').forEach(button => button.onclick = () => refreshProvider(button.dataset.providerRefresh));
  $$('[data-provider-edit]').forEach(button => button.onclick = () => openProvider(state.providers.find(p => p.id === button.dataset.providerEdit)));
  $$('[data-provider-delete]').forEach(button => button.onclick = () => deleteProvider(button.dataset.providerDelete));
}
const metric = (value, label) => `<div class="metric"><strong>${h(value)}</strong><span>${h(label)}</span></div>`;
const badge = (active, label, kind = active ? 'good' : 'bad') => `<span class="badge badge-${kind}">${h(label)}</span>`;
const typeLabel = type => state.providerTypes.find(item => item.type === type)?.label || type;

$('#add-provider').onclick = () => openProvider();
function providerFields(provider) {
  const options = state.providerTypes.map(item => `<option value="${h(item.type)}" ${provider?.type === item.type ? 'selected' : ''}>${h(item.label)}</option>`).join('');
  const selectedProtocols = provider?.protocols || ['chat'];
  return `<label>Provider name <input name="name" value="${h(provider?.name || '')}" placeholder="openai-main" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"><small>Lowercase namespace used in client-facing model IDs. Leave blank to use provider type (e.g. DeepSeek → deepseek).</small></label>
    <label>Provider type <select name="type" ${provider ? 'disabled' : ''} required>${options}</select></label>
    <label>Base URL <input name="base_url" type="url" value="${h(provider?.base_url || '')}" placeholder="https://api.example.com/v1" required></label>
    ${provider ? '' : '<label data-credential-create>API credential <input name="credential" type="password" autocomplete="new-password"><small>Write-only. Leave empty only when the provider permits unauthenticated access.</small></label>'}
    ${provider ? '<label data-credential-replace>Replacement credential <input name="credential" type="password" autocomplete="new-password"><small>Leave blank to keep the configured credential.</small></label>' : ''}
    <fieldset class="protocol-select" data-protocol-config><legend>Declared native protocols</legend>${['chat','responses','messages'].map(protocol => `<label><input type="checkbox" name="protocol" value="${protocol}" ${selectedProtocols.includes(protocol) ? 'checked' : ''}> ${protocol}</label>`).join('')}<small>Generic providers default to Chat Completions. Declare only surfaces the upstream implements natively.</small></fieldset>
    <label class="toggle-label"><input class="switch" name="enabled" type="checkbox" ${provider?.enabled !== false ? 'checked' : ''}> Provider enabled</label>
    ${provider ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm_breaking_change" type="checkbox"> <span>Confirm if the provider name changes; every direct model ID will change.</span></label>' : ''}`;
}
function openProvider(provider = null) {
  openEntity({ eyebrow: provider ? 'EDIT UPSTREAM' : 'REGISTER UPSTREAM', title: provider ? `Edit ${provider.name}` : 'Add provider', fields: providerFields(provider), submit: provider ? 'Save provider' : 'Add & discover', onMount: form => { const select = $('[name="type"]', form); const protocolConfig = $('[data-protocol-config]', form); const showProtocols = () => protocolConfig.hidden = !['generic-openai','vllm'].includes(provider?.type || select.value); if (!provider) { const base = $('[name="base_url"]', form); const nameInput = $('[name="name"]', form); const credential = $('[name="credential"]', form); const createWrap = $('[data-credential-create]', form); const apply = () => { const item = state.providerTypes.find(t => t.type === select.value); if (!base.value || base.dataset.auto === 'true') { base.value = item?.default_base_url || ''; base.dataset.auto = 'true'; }     if (nameInput) nameInput.placeholder = select.value || 'openai-main'; const isKeyless = item?.type === 'opencode-free'; credential.required = Boolean(item?.credential_needed); if (createWrap) { createWrap.hidden = isKeyless; if (createWrap.hidden) credential.value = ''; } showProtocols(); }; base.addEventListener('input', () => base.dataset.auto = 'false'); select.addEventListener('change', apply); apply(); } else {   const replaceWrap = $('[data-credential-replace]', form); if (replaceWrap) { replaceWrap.hidden = provider.type === 'opencode-free'; } showProtocols(); const nameInput = $('[name="name"]', form), confirmWrap = $('[data-confirm-wrap]', form); const syncConfirm = () => { confirmWrap.hidden = nameInput.value === provider.name; if (confirmWrap.hidden) { const cb = $('[name="confirm_breaking_change"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', syncConfirm); syncConfirm(); } }, onSubmit: async form => {
    const values = new FormData(form); const rawName = String(values.get('name') || '').trim(); const payload = { name: rawName || String(values.get('type') || '').trim(), base_url: values.get('base_url'), enabled: values.get('enabled') === 'on', protocols: values.getAll('protocol') };
    if (provider) { payload.confirm_breaking_change = values.get('confirm_breaking_change') === 'on'; await api(`/api/admin/providers/${provider.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); if (values.get('credential')) await api(`/api/admin/providers/${provider.id}/credential`, { method: 'PUT', body: JSON.stringify({ credential: values.get('credential') }) }); flash('Provider configuration updated.'); }
    else { payload.type = values.get('type'); payload.credential = values.get('credential'); const result = await api('/api/admin/providers', { method: 'POST', body: JSON.stringify(payload) }); flash(result.refresh_error || 'Provider saved and catalogue discovered.', result.refresh_error ? 'info' : 'success'); }
    await loadProviders();
  }});
}
async function refreshProvider(id) { const button = $(`[data-provider-refresh="${CSS.escape(id)}"]`); button.disabled = true; try { await api(`/api/admin/providers/${id}/refresh`, { method: 'POST' }); flash('Catalogue refresh completed.'); await loadProviders(); } catch (error) { flash(errorMessage(error), 'error'); await loadProviders(); } finally { button.disabled = false; } }
async function refreshModels(id) { const button = $(`[data-refresh-models="${CSS.escape(id)}"]`); button.disabled = true; try { await api(`/api/admin/providers/${id}/refresh`, { method: 'POST' }); flash('Catalogue refresh completed.'); } catch (error) { flash(errorMessage(error), 'error'); } finally { await loadModels(); await loadProviders(); button.disabled = false; } }
async function deleteProvider(id) { const provider = state.providers.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${provider.name}?`, copy: 'All discovered models and their client permissions will be removed. Deletion is blocked while a virtual model references this provider.', action: 'Delete provider', typeMatch: provider.name, typeLabel: 'provider name' })) return; try { await api(`/api/admin/providers/${id}`, { method: 'DELETE' }); flash('Provider deleted.'); await loadProviders(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadModels(search = $('#model-search').value) { const [result, usage] = await Promise.all([api(`/api/admin/models?all=1&search=${encodeURIComponent(search || '')}`), api('/api/admin/usage')]); state.models = result.data; state.usage = usage; renderModels(); }
function groupBanner(kind, key, label, note, count, actions = '') { const collapsed = (kind === 'models' ? collapsedModels : kind === 'clients' ? collapsedClients : collapsedVirtual).has(key); const columns = kind === 'virtual' ? 8 : kind === 'clients' ? 7 : 6; const noteMarkup = kind === 'virtual' ? '' : `<span class="meta-line">${h(note)}</span>`; return `<tr class="group-toggle" data-group-toggle="${kind}" data-group-key="${h(key)}" data-expanded="${collapsed ? 'false' : 'true'}" aria-expanded="${collapsed ? 'false' : 'true'}"><td colspan="${columns}"><span class="group-arrow">${collapsed ? GROUP_ARROW.down : GROUP_ARROW.up}</span><span class="group-label">${h(label)}</span><span class="count-badge">${h(count)}</span>${noteMarkup}${actions ? `<span class="banner-actions">${actions}</span>` : ''}</td></tr>`; }
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
const groupRows = (rows, key, collapsed) => `${rows.map(row => `<tr class="group-row${collapsed ? ' group-row-hidden' : ''}">${row}</tr>`).join('')}`;
function renderModels() { const shown = state.models.filter(item => $('#show-retired').checked || item.available); $('#models-empty').hidden = shown.length > 0; const byProvider = new Map(); shown.forEach(model => { if (!byProvider.has(model.provider_name)) byProvider.set(model.provider_name, []); byProvider.get(model.provider_name).push(model); }); const html = [...byProvider.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([provider, models]) => { const available = models.filter(m => m.available).length; const retired = models.length - available; const collapsed = collapsedModels.has(provider); const note = retired ? `${retired} retired` : 'provider'; const actions = `<button class="btn btn-small btn-secondary" data-refresh-models="${h(models[0].provider_id)}">Refresh models</button>`; return groupBanner('models', provider, provider, note, `${available} available`, actions) + groupRows(models.map(model => `<td><code class="model-id">${h(model.canonical_model_id)}</code></td><td><code class="model-id">${h(model.upstream_model_id)}</code></td><td>${tok(state.usage?.real_models?.[model.canonical_model_id]?.['1h'], state.usage?.real_cache?.[model.canonical_model_id]?.['1h'])}</td><td>${tok(state.usage?.real_models?.[model.canonical_model_id]?.['24h'], state.usage?.real_cache?.[model.canonical_model_id]?.['24h'])}</td><td>${tok(state.usage?.real_models?.[model.canonical_model_id]?.['7d'], state.usage?.real_cache?.[model.canonical_model_id]?.['7d'])}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-model-activity="${h(model.canonical_model_id)}">Activity</button><button class="btn btn-small btn-secondary" data-model-capabilities="${h(model.id)}">Capabilities</button></div></td>`), provider, collapsed); }).join(''); $('#models-body').innerHTML = html; $$('.group-toggle', $('#models-body')).forEach(header => header.onclick = toggleGroup); $$('[data-refresh-models]', $('#models-body')).forEach(button => button.onclick = event => { event.stopPropagation(); refreshModels(button.dataset.refreshModels); }); $$('[data-model-activity]', $('#models-body')).forEach(button => button.onclick = event => { event.stopPropagation(); openModelActivity(state.models.find(item => item.canonical_model_id === button.dataset.modelActivity), 'real'); }); $$('[data-model-capabilities]', $('#models-body')).forEach(button => button.onclick = event => { event.stopPropagation(); openRealModelCapabilities(state.models.find(item => item.id === button.dataset.modelCapabilities)); }); }

async function loadVirtual(search = $('#virtual-search').value) {
  const [groups, virtualModels, providersResult, modelsResult, usage] = await Promise.all([
    api('/api/admin/virtual-groups?limit=200'), api(`/api/admin/virtual-models?limit=200&search=${encodeURIComponent(search || '')}`), api('/api/admin/providers?limit=200'), api('/api/admin/models?all=1'), api('/api/admin/usage')
  ]);
  state.groups = groups.data; state.virtualModels = virtualModels.data; state.providers = providersResult.data; state.models = modelsResult.data; state.usage = usage; renderVirtual();
}
const RESOLUTION_STALE_MS = 24 * 3600 * 1000;
function resolutionIndicator(target) {
  const key = target.provider_model_id || target.target_model_id;
  const legacyKey = `${target.provider_name}/${target.upstream_model_id}`;
  const last = state.usage?.target_last_outcome?.[key];
  const status = last
    ? !last.at
      ? ['neutral', '○', 'No activity recorded']
      : (Date.now() - new Date(last.at).getTime()) > RESOLUTION_STALE_MS
        ? ['neutral', '○', 'No activity in 24h']
        : last.is_success
          ? ['good', '✓', 'Resolving successfully']
          : ['bad', '×', 'Last request failed']
    : (() => {
      const health = state.usage?.target_health?.[legacyKey];
      return health === undefined
        ? ['neutral', '○', 'No activity recorded']
        : !health.success_24h
          ? ['bad', '×', 'No successful resolution in the last 24 hours']
          : health.failure_1h
            ? ['warn', '−', 'Failures recorded in the last hour']
            : ['good', '✓', 'Resolving successfully'];
      })();
  return `<span class="resolution-indicator resolution-${status[0]}" role="img" aria-label="${status[2]}" title="${status[2]}">${status[1]}</span>`;
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
    return groupBanner('virtual', name, name, note, `${models.length} model${models.length === 1 ? '' : 's'}`, actions) + groupRows(models.map(model => { const targets = model.targets || []; const summary = targets.length ? `<div class="target-summary">${targets.map((target, index) => `<span class="meta-line">${index + 1}. ${resolutionIndicator(target)}${h(target.provider_name)}/${h(target.upstream_model_id)}${target.enabled ? '' : ' (disabled)'}</span>`).join('')}</div>` : `<span class="meta-line">${resolutionIndicator({provider_name:model.target_provider_name,upstream_model_id:model.target_upstream_model_id})}</span><code class="model-id">${h(model.target_provider_name || '')}/${h(model.target_upstream_model_id || '')}</code>`; return `<td><code class="model-id">${h(model.canonical_model_id)}</code><span class="meta-line">${h(model.routing_mode === 'ordered_fallback' ? 'Ordered fallback' : 'Fixed')}</span></td><td></td><td>${summary}</td><td>${badge(model.available, model.available ? 'Routable' : 'Broken target', model.available ? 'good' : 'bad')}${model.warning ? `<span class="error-text">${h(model.warning)}</span>` : ''}</td><td>${tok(state.usage?.virtual_models?.[model.canonical_model_id]?.['1h'], state.usage?.virtual_cache?.[model.canonical_model_id]?.['1h'])}</td><td>${tok(state.usage?.virtual_models?.[model.canonical_model_id]?.['24h'], state.usage?.virtual_cache?.[model.canonical_model_id]?.['24h'])}</td><td>${tok(state.usage?.virtual_models?.[model.canonical_model_id]?.['7d'], state.usage?.virtual_cache?.[model.canonical_model_id]?.['7d'])}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-model-activity="${h(model.canonical_model_id)}">Activity</button><button class="btn btn-small btn-secondary" data-virtual-capabilities="${h(model.id)}">Capabilities</button><button class="btn btn-small btn-secondary" data-virtual-edit="${h(model.id)}">Settings</button><button class="btn btn-small btn-danger" data-virtual-delete="${h(model.id)}">Delete</button></div></td>`; }), name, collapsed);
  }).join('');
  $('#virtual-body').innerHTML = html;
  $$('.group-toggle', $('#virtual-body')).forEach(header => header.onclick = toggleGroup);
  $$('[data-model-activity]', $('#virtual-body')).forEach(button => button.onclick = event => { event.stopPropagation(); openModelActivity(state.virtualModels.find(item => item.canonical_model_id === button.dataset.modelActivity), 'virtual'); });
  $$('[data-virtual-edit]').forEach(button => button.onclick = () => openVirtualModel(state.virtualModels.find(item => item.id === button.dataset.virtualEdit)));
  $$('[data-virtual-capabilities]').forEach(button => button.onclick = event => { event.stopPropagation(); openCapabilities(state.virtualModels.find(item => item.id === button.dataset.virtualCapabilities)); });
  $$('[data-virtual-delete]').forEach(button => button.onclick = () => deleteVirtualModel(button.dataset.virtualDelete));
  $$('[data-group-edit]').forEach(button => button.onclick = event => { event.stopPropagation(); openVirtualGroup(state.groups.find(item => item.id === button.dataset.groupEdit)); });
  $$('[data-group-delete]').forEach(button => button.onclick = event => { event.stopPropagation(); deleteVirtualGroup(button.dataset.groupDelete); });
}
const capabilityNumber = value => value ? new Intl.NumberFormat().format(value) : 'Not reported';
const capFlag = value => value === true ? '✓' : value === false ? '✗' : '—';
const capFlags = c => `<span class="capability-flag" title="Tool calling">T ${capFlag(c.supports_tools)}</span><span class="capability-flag" title="Vision (image input)">V ${capFlag(c.supports_vision)}</span><span class="capability-flag" title="Reasoning">R ${capFlag(c.supports_reasoning)}</span><span class="capability-flag" title="Structured output">S ${capFlag(c.supports_structured_output)}</span>`;
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
  $('#capabilities-content').innerHTML = `<div class="capability-effective"><p class="eyebrow">ADVERTISED TO HERMES / V1 MODELS</p><div class="capability-grid"><div><small>Context window</small><strong>${capabilityNumber(effectiveContext)}</strong></div><div><small>Max output</small><strong>${capabilityNumber(effectiveOutput)}</strong></div></div><div class="capability-flags">${capFlags(model)}</div></div><div class="capability-targets"><p class="eyebrow">CONFIGURED TARGETS</p>${targets.length ? targets.map((target, index) => `<div class="capability-target"><div><strong>${String(index + 1).padStart(2, '0')} · ${h(target.provider_name)}/${h(target.upstream_model_id)}</strong><span class="meta-line">${target.native_protocol ? h(target.native_protocol) : 'Provider default'} · ${target.enabled && target.available ? 'eligible' : h(target.warning || 'not eligible')}</span></div><div class="capability-values"><span><small>Context</small><b>${capabilityNumber(target.context_length)}</b></span><span><small>Output</small><b>${capabilityNumber(target.max_output_tokens)}</b></span></div><div class="capability-flags">${capFlags(target)}</div></div>`).join('') : '<p class="meta-line">No targets configured.</p>'}</div><p class="form-error" id="capabilities-refresh-error" role="alert"></p>`;
  $('#capabilities-dialog').showModal();
}
function openRealModelCapabilities(model) {
  if (!model) return;
  $('#capabilities-title').textContent = `${model.canonical_model_id} capabilities`;
  $('#refresh-capabilities').dataset.kind = 'real';
  $('#refresh-capabilities').dataset.modelId = model.id;
  const modalities = (list) => list && list.length ? h(list.join(', ')) : '—';
  $('#capabilities-content').innerHTML = `<div class="capability-effective"><p class="eyebrow">ADVERTISED TO HERMES / V1 MODELS</p><div class="capability-grid"><div><small>Context window</small><strong>${capabilityNumber(model.context_length)}</strong></div><div><small>Max output</small><strong>${capabilityNumber(model.max_output_tokens)}</strong></div></div><div class="capability-flags">${capFlags(model)}</div></div><div class="capability-targets"><p class="eyebrow">UPSTREAM</p><div class="capability-target"><div><strong>${h(model.provider_name)}/${h(model.upstream_model_id)}</strong><span class="meta-line">${model.native_protocol ? h(model.native_protocol) : 'Provider default'} · ${model.available ? 'available' : 'retired'} · first seen ${date(model.first_seen_at)}</span></div><div class="capability-values"><span><small>Input</small><b>${modalities(model.input_modalities)}</b></span><span><small>Output</small><b>${modalities(model.output_modalities)}</b></span></div></div></div><p class="form-error" id="capabilities-refresh-error" role="alert"></p>`;
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
  const width = Math.max(box.width, minWidth || 0);
  const left = Math.min(box.left, Math.max(8, vvWidth - width - 8));
  const spaceBelow = vvBottom - box.bottom - 8;
  const spaceAbove = box.top - vvTop - 8;
  const openUp = spaceBelow < 80;
  const maxH = Math.max(80, Math.min(360, openUp ? spaceAbove : spaceBelow));
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
  input.addEventListener('input', () => { if (hidden.value && !options.some(o => o.value === hidden.value && o.label === input.value)) hidden.value = ''; active = -1; render(); });
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
  return `<div class="row">${groupField}<label>Virtual model name <input name="name" value="${h(model?.name || '')}" placeholder="coding" required><small>Stable client-facing identity.</small></label></div><label>Routing mode <select name="routing_mode"><option value="fixed" ${model?.routing_mode !== 'ordered_fallback' ? 'selected' : ''}>Fixed</option><option value="ordered_fallback" ${model?.routing_mode === 'ordered_fallback' ? 'selected' : ''}>Ordered fallback</option></select></label><small class="fallback-hint" data-fallback-hint hidden>Models are tried in the order below. If one returns an error or fails to respond, the request is automatically retried with the next model below, seamlessly to the client.</small><div class="routing-targets" data-fixed-target></div><div class="routing-targets" data-fallback-targets hidden></div><button class="btn btn-small btn-secondary target-add" type="button" data-target-add hidden>+ Add target</button>${model ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm" type="checkbox"> <span>Confirm if changing the virtual model name; this is a breaking client-facing rename.</span></label>' : ''}`;
}
function openVirtualModel(model = null) { if (!state.models.length) { flash('Discover at least one real model before creating a virtual route.', 'info'); return; } openEntity({ eyebrow: model ? 'ROUTING POLICY' : 'NEW STABLE IDENTITY', title: model ? `Edit ${model.canonical_model_id}` : 'Create virtual model', fields: virtualModelFields(model), submit: model ? 'Apply' : 'Create route', onMount: form => {
  const fixed = $('[data-fixed-target]', form), fallback = $('[data-fallback-targets]', form), mode = $('[name="routing_mode"]', form), addButton = $('[data-target-add]', form), hint = $('[data-fallback-hint]', form);
  const options = state.models.map(item => ({ value:item.id, label:`${item.provider_name} / ${item.upstream_model_id}`, muted:!item.available }));
  const targets = model?.targets?.length ? model.targets : [{provider_model_id:model?.target_model_id,enabled:true}];
  const makePicker = (target, row = null) => { const box = document.createElement('div'); box.className='combobox'; box.innerHTML='<input type="text" placeholder="Type a provider or model name…"><input type="hidden" name="target_model" required>'; const picker=combobox({ input:$('input[type="text"]',box), hidden:$('input[type="hidden"]',box), options, placeholder:'Type a provider or model name…' }); picker.setOptions(options); const found=options.find(item=>item.value===target?.provider_model_id)||options[0]; if(found) picker.select(options.indexOf(found)); return box; };
  const updateControls = () => { const rows=$$('.target-row',fallback); rows.forEach((row,index)=>{ $('.target-index',row).textContent=String(index+1).padStart(2,'0'); $('[data-target-up]',row).disabled=index===0; $('[data-target-down]',row).disabled=index===rows.length-1; $('[data-target-remove]',row).disabled=rows.length===1; }); };
  const addFallback = target => { const row=document.createElement('div'); row.className='target-row'; row.append(Object.assign(document.createElement('span'),{className:'target-index'}),makePicker(target)); const actions=document.createElement('div'); actions.className='target-actions'; actions.innerHTML='<button type="button" data-target-up title="Move target up">↑</button><button type="button" data-target-down title="Move target down">↓</button><button type="button" data-target-remove title="Remove target">×</button>'; $('[data-target-up]',actions).onclick=()=>{ const previous=row.previousElementSibling; if(previous) { fallback.insertBefore(row,previous); updateControls(); } }; $('[data-target-down]',actions).onclick=()=>{ const next=row.nextElementSibling; if(next) { fallback.insertBefore(next,row); updateControls(); } }; $('[data-target-remove]',actions).onclick=()=>{ if($$('.target-row',fallback).length>1) { row.remove(); updateControls(); } }; row.append(actions); fallback.append(row); updateControls(); };
  fixed.append(makePicker(targets[0])); targets.forEach(addFallback); const syncMode=()=>{ const ordered=mode.value==='ordered_fallback'; fixed.hidden=ordered; fallback.hidden=!ordered; addButton.hidden=!ordered; hint.hidden=!ordered; }; mode.onchange=syncMode; syncMode(); addButton.onclick=()=>{ if($$('.target-row',fallback).length<5) addFallback(); else flash('The admin UI supports up to five targets.', 'info'); };
  const nameInput = $('[name="name"]', form); if (model) { const wrap = $('[data-confirm-wrap]', form); const sync = () => { wrap.hidden = nameInput.value === model.name; if (wrap.hidden) { const cb = $('[name="confirm"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', sync); sync(); }
}, onSubmit: async form => { const values = new FormData(form); const ordered=values.get('routing_mode')==='ordered_fallback'; const rows=ordered ? $$('.target-row',form) : [ $('[data-fixed-target]',form) ]; const targets=rows.map(row=>({provider_model_id:$('[name="target_model"]',row).value,enabled:true})); if(targets.some(target=>!target.provider_model_id)) throw new Error('Choose a target model.'); const payload = { name: values.get('name'), routing_mode: values.get('routing_mode'), targets }; if(!ordered) payload.fixed_target_id=targets[0].provider_model_id; if (model) { payload.confirm_breaking_change = values.get('confirm') === 'on'; await api(`/api/admin/virtual-models/${model.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); flash('Virtual routing updated. New requests use the new target immediately.'); } else { const groupID = values.get('group_id'); if (groupID) payload.group_id = groupID; else payload.group_name = values.get('group_name'); await api('/api/admin/virtual-models', { method: 'POST', body: JSON.stringify(payload) }); flash('Virtual route created.'); } await loadVirtual(); } }); }
async function deleteVirtualModel(id) { const model = state.virtualModels.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${model.canonical_model_id}?`, copy: 'Clients using this stable identity will receive model-not-found after deletion.', action: 'Delete virtual model' })) return; try { await api(`/api/admin/virtual-models/${id}`, { method: 'DELETE' }); flash('Virtual model deleted.'); await loadVirtual(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadClients() {
  const search = $('#client-search').value, group = $('#client-group-filter').value;
  const [result, usage, models, virtual, providers] = await Promise.all([api(`/api/admin/client-keys?limit=200&search=${encodeURIComponent(search || '')}&group=${encodeURIComponent(group || '')}`), api('/api/admin/usage'), api('/api/admin/models?all=1'), api('/api/admin/virtual-models?limit=200'), api('/api/admin/providers?limit=200')]);
  state.clients = result.data; state.usage = usage; state.models = models.data; state.virtualModels = virtual.data; state.providers = providers.data;
  renderClientGroupFilter();
  renderClients();
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
    ...state.virtualModels.map(item => ({ value:`virtual:${item.id}`, kind:'virtual', id:item.id, label:`Virtual · ${item.canonical_model_id}`, canonical:item.canonical_model_id, disabled:!item.available })),
    ...state.models.map(item => ({ value:`real:${item.id}`, kind:'real', id:item.id, label:`Real · ${item.canonical_model_id}`, canonical:item.canonical_model_id, disabled:!item.available || providerEnabled.get(item.provider_id) === false }))
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
    fields: `<label>Target <div class="combobox" data-single-target><input type="text"><input type="hidden" name="single_target" required></div><small>Search and select an available real or virtual model. New requests use the new target immediately.</small></label>`,
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
  const dialog = $('#form-dialog');
  if (!dialog.classList.contains('route-picker-dialog')) return;
  const vv = window.visualViewport;
  if (!vv) return;
  const keyboardHeight = Math.max(0, window.innerHeight - vv.height);
  if (keyboardHeight > 0) {
    // vv.height is already the space above the keyboard — don't subtract the
    // keyboard again (that compressed the sheet until Apply/Cancel vanished).
    dialog.style.bottom = `${keyboardHeight}px`;
    dialog.style.maxHeight = `${vv.height - 16}px`;
  } else {
    dialog.style.bottom = '';
    dialog.style.maxHeight = '';
  }
}
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', positionRoutePickerAboveKeyboard);
  window.visualViewport.addEventListener('scroll', positionRoutePickerAboveKeyboard);
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
  const statusDot = `<span class="status-dot ${client.enabled ? 'status-dot-enabled' : 'status-dot-disabled'}" role="img" aria-label="${client.enabled ? 'Enabled' : 'Disabled'}" title="${client.enabled ? 'Enabled' : 'Disabled'}"></span>`;
  const routeAction = client.type === 'single'
    ? `<button class="btn btn-small btn-secondary" data-client-route="${h(client.id)}">Change route</button>`
    : `<button class="btn btn-small btn-secondary" data-client-models="${h(client.id)}">Catalogue permissions</button>`;
  const routeSummary = client.type === 'single'
    ? `<code class="model-id ${client.single_target_available === false ? 'client-route-unavailable' : ''}">${h(client.single_target_canonical || 'Unavailable target')}</code>${client.single_target_available === false ? '<span class="client-route-broken">Unavailable</span>' : ''}`
    : `<span class="meta-line">Catalogue — model permissions</span>`;
  return `<article class="client-card" data-client-id="${h(client.id)}">
    <button class="client-card-head" data-card-toggle="${h(client.id)}" aria-expanded="false" aria-controls="client-detail-${h(client.id)}">
      ${statusDot}
      <span class="client-card-name">${h(client.name)}</span>
      ${client.group ? `<span class="group-badge">${h(client.group)}</span>` : ''}
      <span class="client-card-desc">${h(client.description || 'No description')}</span>
      <span class="secret-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</span>
      <span class="client-card-chevron" aria-hidden="true">▾</span>
    </button>
    <div class="client-card-detail" id="client-detail-${h(client.id)}" hidden>
      <div class="client-card-field"><span class="client-card-label">Route</span><div class="client-card-route">${routeSummary}${routeAction}</div></div>
      <div class="client-card-field"><span class="client-card-label">Description</span><span>${h(client.description || 'No description')}</span></div>
      <div class="client-card-field"><span class="client-card-label">Fingerprint</span><code class="secret-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</code></div>
      <div class="client-card-field"><span class="client-card-label">Created</span><span>${date(client.created_at)}</span></div>
      ${client.rotated_at ? `<div class="client-card-field"><span class="client-card-label">Rotated</span><span>${date(client.rotated_at)}</span></div>` : ''}
      <div class="client-card-field"><span class="client-card-label">Type</span><span>${client.type === 'single' ? 'Single' : 'Catalogue'}</span></div>
      ${client.group ? `<div class="client-card-field"><span class="client-card-label">Group</span><span>${h(client.group)}</span></div>` : ''}
      <div class="client-card-field"><span class="client-card-label">Usage</span><div class="client-card-usage">${tok(state.usage?.client_keys?.[client.id]?.['1h'], state.usage?.client_cache?.[client.id]?.['1h'])}${tok(state.usage?.client_keys?.[client.id]?.['24h'], state.usage?.client_cache?.[client.id]?.['24h'])}${tok(state.usage?.client_keys?.[client.id]?.['7d'], state.usage?.client_cache?.[client.id]?.['7d'])}</div></div>
      <div class="client-card-actions">
        <button class="btn btn-small btn-secondary" data-client-activity="${h(client.id)}">Activity</button>
        <button class="btn btn-small btn-secondary" data-client-rotate="${h(client.id)}">Rotate</button>
        <button class="btn btn-small btn-secondary" data-client-edit="${h(client.id)}">Settings</button>
        <button class="btn btn-small btn-danger" data-client-delete="${h(client.id)}">Delete</button>
      </div>
    </div>
  </article>`;
}
function clientRow(client) {
  const routeCell = client.type === 'single'
    ? `<div class="client-route-picker ${client.single_target_available === false ? 'route-picker-error' : ''}" data-client-id="${h(client.id)}"><div class="combobox" data-inline-route><input type="text" aria-label="Route for ${h(client.name)}"><input type="hidden"></div><div class="route-confirm" data-route-confirm hidden><button class="route-confirm-tick" data-route-tick type="button" title="Apply new route" aria-label="Apply new route">✓</button><button class="route-confirm-cancel" data-route-cancel type="button" title="Cancel" aria-label="Cancel route change">✕</button></div></div>`
    : `<button class="route-button" data-client-models="${h(client.id)}" aria-label="Manage models for ${h(client.name)}"><span>Catalogue</span><strong>Catalogue permissions</strong><i aria-hidden="true">›</i></button>`;
  return `<td class="primary-cell"><div class="client-name-line"><span class="status-dot ${client.enabled ? 'status-dot-enabled' : 'status-dot-disabled'}" role="img" aria-label="${client.enabled ? 'Enabled' : 'Disabled'}" title="${client.enabled ? 'Enabled' : 'Disabled'}"></span><strong>${h(client.name)}</strong></div><small>${h(client.description || 'No description')}</small></td><td class="key-info-cell"><span class="secret-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</span><small>Created ${date(client.created_at)}</small>${client.rotated_at ? `<small>Rotated ${date(client.rotated_at)}</small>` : ''}</td><td>${routeCell}</td><td>${tok(state.usage?.client_keys?.[client.id]?.['1h'], state.usage?.client_cache?.[client.id]?.['1h'])}</td><td>${tok(state.usage?.client_keys?.[client.id]?.['24h'], state.usage?.client_cache?.[client.id]?.['24h'])}</td><td>${tok(state.usage?.client_keys?.[client.id]?.['7d'], state.usage?.client_cache?.[client.id]?.['7d'])}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-client-activity="${h(client.id)}">Activity</button><button class="btn btn-small btn-secondary" data-client-rotate="${h(client.id)}">Rotate</button><button class="btn btn-small btn-secondary" data-client-edit="${h(client.id)}">Settings</button><button class="btn btn-small btn-danger" data-client-delete="${h(client.id)}">Delete</button></div></td>`;
}
function renderClients() {
  $('#clients-empty').hidden = state.clients.length > 0;
  $('#clients-cards').innerHTML = state.clients.map(clientCard).join('');
  const byGroup = new Map();
  state.clients.forEach(client => { const g = client.group || 'default'; if (!byGroup.has(g)) byGroup.set(g, []); byGroup.get(g).push(client); });
  $('#clients-body').innerHTML = [...byGroup.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([group, clients]) => {
    const singles = clients.filter(c => c.type === 'single').length;
    const catalogues = clients.length - singles;
    const note = singles && catalogues ? `${catalogues} catalogue · ${singles} single` : singles ? `${singles} single` : `${catalogues} catalogue`;
    const collapsed = collapsedClients.has(group);
    return groupBanner('clients', group, group, note, `${clients.length}`, '') + groupRows(clients.map(clientRow), group, collapsed);
  }).join('');
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
function openClient(client = null) {
  const singleFields = `<section data-single-fields ${client?.type === 'single' ? '' : 'hidden'}><label>Client-facing model name <input name="single_model_name" value="${h(client?.single_model_name || 'main')}" pattern="[A-Za-z0-9._~-](?:[A-Za-z0-9._~/-]{0,253}[A-Za-z0-9._~-])?" required><small>This is the only model identity exposed to the client.</small></label><label>Target <div class="combobox" data-single-target><input type="text"><input type="hidden" name="single_target" required></div><small>Search and select an available real or virtual model.</small></label>${client ? '<label class="confirm-check" data-single-confirm hidden><input name="confirm_model_name_change" type="checkbox"> <span>I understand changing this client-facing name may require client reconfiguration.</span></label>' : ''}</section>`;
  const typeField = `<label>Type <select name="type"><option value="catalogue" ${client?.type === 'catalogue' ? 'selected' : ''}>Catalogue — Choose which real and virtual models the client can access</option><option value="single" ${client?.type !== 'catalogue' ? 'selected' : ''}>Single — Expose one model to the client and route all requests to that single model</option></select><small>Single — Expose one model to the client and route all requests to that single model.</small><small>Catalogue — Choose which real and virtual models the client can access.</small></label>`;
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
      }
      await loadClients();
    }
  });
}
async function rotateClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Rotate ${client.name}?`, copy: 'The current secret will stop authenticating immediately. Permissions and metadata are preserved.', action: 'Rotate now' })) return; try { const result = await api(`/api/admin/client-keys/${id}/rotate`, { method: 'POST' }); showSecret(result.secret); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }
async function deleteClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${client.name}?`, copy: 'The client secret will be invalidated immediately and all permissions will be removed.', action: 'Delete client key' })) return; try { await api(`/api/admin/client-keys/${id}`, { method: 'DELETE' }); flash('Client key deleted and invalidated.'); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function openPermissions(client) {
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
$('#close-permissions').onclick = $('#cancel-permissions').onclick = () => $('#permissions-dialog').close();
$('#save-permissions').onclick = async () => {
  const button = $('#save-permissions'), client = state.modelClient;
  button.disabled = true;
  $('#permissions-error').textContent = '';
  try {
    const defaults = state.permissionData.groups.map(group => ({ kind: group.kind, group_id: group.id, enabled: group.new_models_enabled }));
    const permissions = state.permissionData.groups.flatMap(group => group.models.map(model => ({ kind: model.kind, model_id: model.id, enabled: model.enabled })));
    await api(`/api/admin/client-keys/${client.id}/permissions`, { method: 'PUT', body: JSON.stringify({ defaults, permissions }) });
    flash('Client catalogue permissions saved.');
    $('#permissions-dialog').close();
    await loadClients();
  } catch (error) { $('#permissions-error').textContent = errorMessage(error); }
  finally { button.disabled = false; }
};

const activityState = { kind: '', client: null, modelID: '', modelName: '', rows: [], offset: 0, limit: 50, search: '', hasMore: true };
async function openActivity(client) { activityState.kind = 'client'; activityState.client = client; activityState.modelID = ''; activityState.modelName = ''; activityState.offset = 0; activityState.search = ''; $('#activity-search').value = ''; $('#activity-title').textContent = `${client.name} activity`; $('#clear-activity').hidden = false; await loadActivity(); $('#activity-dialog').showModal(); }
async function openModelActivity(model, kind) { activityState.kind = kind; activityState.client = null; activityState.modelID = model.id; activityState.modelName = model.canonical_model_id; activityState.offset = 0; activityState.search = ''; $('#activity-search').value = ''; $('#activity-title').textContent = `${model.canonical_model_id} activity`; $('#clear-activity').hidden = true; await loadActivity(); $('#activity-dialog').showModal(); }
async function loadActivity() { if (!activityState.kind) return; activityState.controller?.abort(); activityState.controller = new AbortController(); const { signal } = activityState.controller; try { const base = activityState.kind === 'client' ? `/api/admin/client-keys/${activityState.client.id}/activity` : activityState.kind === 'real' ? `/api/admin/models/${activityState.modelID}/activity` : `/api/admin/virtual-models/${activityState.modelID}/activity`; const result = await api(`${base}?limit=${activityState.limit + 1}&offset=${activityState.offset}&search=${encodeURIComponent(activityState.search || '')}`, { signal }); const fetched = result.data; activityState.hasMore = fetched.length > activityState.limit; activityState.rows = fetched.slice(0, activityState.limit); await Promise.all(activityState.rows.filter(row => row.attempt_count > 1 || row.error_text).map(async row => { const attempts = await api(`/api/admin/activity/${row.id}/attempts`, { signal }); row.attempts = attempts.data || []; })); $('#activity-error').textContent = ''; renderActivity(); } catch (error) { if (error.name === 'AbortError') return; $('#activity-error').textContent = errorMessage(error); } }
function activityAttempt(attempt, index) { const route = `${attempt.provider}/${attempt.model}`; return `<div class="activity-attempt ${attempt.result === 'success' ? 'attempt-success' : 'attempt-failed'}"><span class="attempt-number">${String(index + 1).padStart(2, '0')}</span><span class="attempt-route"><code title="${h(route)}">${h(route)}</code></span><span class="attempt-latency">${attempt.latency_ms} ms</span><span class="attempt-status">${attempt.http_status || h(attempt.result)}</span></div>`; }
function attemptSequence(row) { if (!row.attempts?.length) return ''; return `<div class="attempt-sequence">${row.attempts.map(activityAttempt).join('')}</div>`; }
function resolvedActivity(row) { const fixedAttempt = row.resolved_provider ? { provider: row.resolved_provider, model: row.resolved_model || '', latency_ms: row.latency_ms, http_status: row.http_status, result: row.http_status >= 200 && row.http_status < 300 ? 'success' : 'failed' } : null; const resolved = row.fallback_used ? '' : fixedAttempt ? `<div class="attempt-sequence">${activityAttempt(fixedAttempt, 0)}</div>` : ''; const sequence = (row.fallback_used || !fixedAttempt) ? attemptSequence(row) : ''; const error = sequence ? '' : (row.error_text ? `<span class="error-text">${h(row.error_text)}</span>` : ''); return `${resolved}${sequence}${error}`; }
function requestIdentity(row) { const requestedModel = h(row.requested_model); const requestedTitle = h(row.requested_model); if (row.exposed_model && row.exposed_model !== row.requested_model) { return `<code class="model-id activity-requested-model" title="${requestedTitle}">${requestedModel}</code><br><span class="meta-line activity-exposed-model" title="${h(row.exposed_model)}">map &gt; ${h(row.exposed_model)}</span>`; } return `<code class="model-id activity-requested-model" title="${requestedTitle}">${requestedModel}</code>`; }
function activityRequestID(row) { const id = row.client_request_id || ''; const short = id.length > 8 ? `${id.slice(0, 8)}…` : id; const copyable = id && (window.isSecureContext && navigator.clipboard?.writeText); const attrs = copyable ? ` data-copy-request-id="${h(id)}" role="button" tabindex="0" aria-label="Copy request ID ${h(id)}"` : ''; return `<code class="model-id activity-request-id"${attrs} title="${h(id)}">${h(short)}</code>`; }
async function copyRequestID(button) { const id = button.dataset.copyRequestId; if (!id) return; if (!(window.isSecureContext && navigator.clipboard?.writeText)) return; try { await navigator.clipboard.writeText(id); const original = button.textContent; button.classList.add('copied'); button.textContent = 'Copied'; setTimeout(() => { button.classList.remove('copied'); button.textContent = original; }, 1200); } catch { const range = document.createRange(); range.selectNodeContents(button); const selection = window.getSelection(); selection.removeAllRanges(); selection.addRange(range); button.title = 'Press Ctrl/Cmd+C to copy'; } }
document.addEventListener('click', event => { const target = event.target.closest('[data-copy-request-id]'); if (target) copyRequestID(target); });
document.addEventListener('keydown', event => { if (event.key !== 'Enter' && event.key !== ' ') return; const target = event.target.closest('[data-copy-request-id]'); if (!target) return; event.preventDefault(); copyRequestID(target); });
function renderActivity() { const showClient = !activityState.client; $('#activity-client-head').hidden = !showClient; $('#activity-empty').hidden = activityState.rows.length > 0; $('#activity-body').innerHTML = activityState.rows.map(row => `<tr><td><span class="meta-line">${date(row.created_at)}</span></td>${showClient ? `<td><strong class="client-name">${h(row.client_name || '')}</strong></td>` : ''}<td>${requestIdentity(row)}</td><td>${resolvedActivity(row)}</td><td><span class="protocol">${h(row.protocol)}</span>${row.streaming ? '<span class="protocol">stream</span>' : ''}</td><td><span class="meta-line">${row.latency_ms} ms</span></td><td>${rowCache(row)}</td><td>${activityRequestID(row)}</td></tr>`).join(''); $('#activity-count').textContent = activityState.rows.length ? `${activityState.offset + 1}–${activityState.offset + activityState.rows.length}` : '0 results'; $('#activity-prev').disabled = activityState.offset === 0; $('#activity-next').disabled = !activityState.hasMore; }
filterInput('#activity-search', value => { activityState.search = value; activityState.offset = 0; loadActivity(); });
$('#activity-prev').onclick = () => { activityState.offset = Math.max(0, activityState.offset - activityState.limit); loadActivity(); };
$('#activity-next').onclick = () => { activityState.offset += activityState.limit; loadActivity(); };
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
// renders rows through the same helpers as the dialog (resolvedActivity(),
// requestIdentity(), rowCache(), lazy attempt fetches) so fallbacks appear
// identically — the extra Client column is the only difference.
const globalActivityState = { rows: [], offset: 0, limit: 50, search: '', hasMore: true };
async function loadGlobalActivity() { globalActivityState.controller?.abort(); globalActivityState.controller = new AbortController(); const { signal } = globalActivityState.controller; try { const result = await api(`/api/admin/activity?limit=${globalActivityState.limit + 1}&offset=${globalActivityState.offset}&search=${encodeURIComponent(globalActivityState.search || '')}`, { signal }); const fetched = result.data; globalActivityState.hasMore = fetched.length > globalActivityState.limit; globalActivityState.rows = fetched.slice(0, globalActivityState.limit); await Promise.all(globalActivityState.rows.filter(row => row.attempt_count > 1 || row.error_text).map(async row => { const attempts = await api(`/api/admin/activity/${row.id}/attempts`, { signal }); row.attempts = attempts.data || []; })); $('#global-activity-error').textContent = ''; renderGlobalActivity(); } catch (error) { if (error.name === 'AbortError') return; $('#global-activity-error').textContent = errorMessage(error); } }
function renderGlobalActivity() { $('#global-activity-empty').hidden = globalActivityState.rows.length > 0; $('#global-activity-body').innerHTML = globalActivityState.rows.map(row => `<tr><td><span class="meta-line">${date(row.created_at)}</span></td><td><strong class="client-name">${h(row.client_name)}</strong></td><td>${requestIdentity(row)}</td><td>${resolvedActivity(row)}</td><td><span class="protocol">${h(row.protocol)}</span>${row.streaming ? '<span class="protocol">stream</span>' : ''}</td><td><span class="meta-line">${row.latency_ms} ms</span></td><td>${rowCache(row)}</td><td>${activityRequestID(row)}</td></tr>`).join(''); $('#global-activity-count').textContent = globalActivityState.rows.length ? `${globalActivityState.offset + 1}–${globalActivityState.offset + globalActivityState.rows.length}` : '0 results'; $('#global-activity-prev').disabled = globalActivityState.offset === 0; $('#global-activity-next').disabled = !globalActivityState.hasMore; }
filterInput('#global-activity-search', value => { globalActivityState.search = value; globalActivityState.offset = 0; loadGlobalActivity(); });
$('#global-activity-prev').onclick = () => { globalActivityState.offset = Math.max(0, globalActivityState.offset - globalActivityState.limit); loadGlobalActivity(); };
$('#global-activity-next').onclick = () => { globalActivityState.offset += globalActivityState.limit; loadGlobalActivity(); };

let authHeaderDirty = false;
let authHeaderClear = false;
async function loadSettings() { const [health, settings] = await Promise.all([api('/api/admin/health'), api('/api/admin/settings')]); $('#top-status').textContent = health.status.toUpperCase(); $('[name="default_logging_enabled"]', $('#settings-form')).checked = settings.default_logging_enabled; $('[name="default_retention_days"]', $('#settings-form')).value = settings.default_retention_days; $('[name="fallback_timeout_seconds"]', $('#fallback-form')).value = settings.fallback_timeout_seconds; const nf = $('#notifications-form'); $('[name="notifications_enabled"]', nf).checked = settings.notifications_enabled; $('[name="notifications_webhook_url"]', nf).value = settings.notifications_webhook_url || ''; $('[name="notifications_event_fallback"]', nf).checked = settings.notifications_event_fallback; $('[name="notifications_event_all_failed"]', nf).checked = settings.notifications_event_all_failed; $('[name="notifications_event_client_key_created"]', nf).checked = settings.notifications_event_client_key_created; $('[name="notifications_event_client_key_deleted"]', nf).checked = settings.notifications_event_client_key_deleted; $('[name="notifications_event_admin_login"]', nf).checked = settings.notifications_event_admin_login; $('[name="notifications_cooldown_seconds"]', nf).value = settings.notifications_cooldown_seconds; const authInput = $('[name="notifications_auth_header"]', nf); authInput.value = ''; authInput.placeholder = settings.notifications_auth_header_set ? '•••••••• (set — leave blank to keep)' : 'Optional, e.g. Bearer <token>'; $('#notifications-auth-note').textContent = settings.notifications_auth_header_set ? 'An Authorization header is configured. Leave blank to keep it; type a new value to replace it.' : ''; $('#clear-notifications-auth').hidden = !settings.notifications_auth_header_set; authHeaderDirty = false; authHeaderClear = false; await loadGlobalActivity(); }
async function saveSettings() { const settingsForm = $('#settings-form'), fallbackForm = $('#fallback-form'), notificationsForm = $('#notifications-form'); if (!settingsForm.reportValidity() || !fallbackForm.reportValidity() || !notificationsForm.reportValidity()) return; const settingsValues = new FormData(settingsForm), fallbackValues = new FormData(fallbackForm), notificationsValues = new FormData(notificationsForm); const buttons = [$('#save-settings-top'), $('#save-settings-bottom')]; buttons.forEach(b => b.disabled = true); $('#settings-error').textContent = ''; $('#fallback-error').textContent = ''; $('#notifications-error').textContent = ''; const body = { default_logging_enabled: settingsValues.get('default_logging_enabled') === 'on', default_retention_days: Number(settingsValues.get('default_retention_days')), fallback_timeout_seconds: Number(fallbackValues.get('fallback_timeout_seconds')), notifications_enabled: notificationsValues.get('notifications_enabled') === 'on', notifications_webhook_url: notificationsValues.get('notifications_webhook_url') || '', notifications_event_fallback: notificationsValues.get('notifications_event_fallback') === 'on', notifications_event_all_failed: notificationsValues.get('notifications_event_all_failed') === 'on', notifications_event_client_key_created: notificationsValues.get('notifications_event_client_key_created') === 'on', notifications_event_client_key_deleted: notificationsValues.get('notifications_event_client_key_deleted') === 'on', notifications_event_admin_login: notificationsValues.get('notifications_event_admin_login') === 'on', notifications_cooldown_seconds: Number(notificationsValues.get('notifications_cooldown_seconds')) }; if (authHeaderDirty) body.notifications_auth_header = notificationsValues.get('notifications_auth_header') || ''; if (authHeaderClear) body.notifications_auth_header = ''; try { await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify(body) }); authHeaderDirty = false; authHeaderClear = false; flash('Settings saved.'); await loadSettings(); } catch (error) { const message = errorMessage(error); $('#settings-error').textContent = message; $('#fallback-error').textContent = message; $('#notifications-error').textContent = message; } finally { buttons.forEach(b => b.disabled = false); } }
$('#save-settings-top').addEventListener('click', saveSettings);
$('#save-settings-bottom').addEventListener('click', saveSettings);
$('[name="notifications_auth_header"]', $('#notifications-form')).addEventListener('input', () => { authHeaderDirty = true; authHeaderClear = false; });
$('#clear-notifications-auth').addEventListener('click', async () => { const button = $('#clear-notifications-auth'); button.disabled = true; $('#notifications-error').textContent = ''; try { await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify({ notifications_auth_header: '' }) }); authHeaderClear = false; authHeaderDirty = false; $('[name="notifications_auth_header"]', $('#notifications-form')).value = ''; $('#notifications-auth-note').textContent = 'Authorization header cleared.'; $('#clear-notifications-auth').hidden = true; } catch (error) { $('#notifications-error').textContent = errorMessage(error); } finally { button.disabled = false; } });
$('#send-test-notification').addEventListener('click', async () => { const button = $('#send-test-notification'); button.disabled = true; $('#notifications-error').textContent = ''; try { const nf = $('#notifications-form'); const body = { notifications_webhook_url: $('[name="notifications_webhook_url"]', nf).value || '' }; if (authHeaderDirty) body.notifications_auth_header = $('[name="notifications_auth_header"]', nf).value || ''; if (authHeaderClear) body.notifications_auth_header = ''; await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify(body) }); authHeaderDirty = false; authHeaderClear = false; await api('/api/admin/notifications/test', { method: 'POST' }); flash('Test notification delivered.'); } catch (error) { $('#notifications-error').textContent = errorMessage(error); } finally { button.disabled = false; } });

let entitySubmit = null;
function openEntity({ eyebrow, title, fields, submit, onMount, onSubmit }) { const dialog = $('#form-dialog'), form = $('#entity-form'); $('#dialog-eyebrow').textContent = eyebrow; $('#dialog-title').textContent = title; $('#dialog-fields').innerHTML = fields; $('#dialog-submit').textContent = submit; $('#dialog-error').textContent = ''; entitySubmit = onSubmit; form.onsubmit = handleEntitySubmit; dialog.showModal(); onMount?.(form); setTimeout(() => $('input:not([type="checkbox"]),select,textarea', form)?.focus(), 0); }
async function handleEntitySubmit(event) { const form = event.currentTarget, button = $('#dialog-submit'); if (event.submitter?.value === 'cancel') return; event.preventDefault(); if (!form.reportValidity()) return; button.disabled = true; $('#dialog-error').textContent = ''; try { await entitySubmit(form); $('#form-dialog').close(); } catch (error) { $('#dialog-error').textContent = errorMessage(error); } finally { button.disabled = false; } }

function confirmAction({ title, copy, action, breaking = false, typeMatch = null, typeLabel = 'name' }) { return new Promise(resolve => { const dialog = $('#confirm-dialog'), form = $('form', dialog), checkWrap = $('#confirm-check-wrap'), check = $('#confirm-check'), typeWrap = $('#confirm-type-wrap'), typeInput = $('#confirm-type'); $('#confirm-title').textContent = title; $('#confirm-copy').textContent = copy; $('#confirm-action').textContent = action; $('#confirm-error').textContent = ''; checkWrap.hidden = !breaking; check.checked = false; typeWrap.hidden = !typeMatch; typeInput.value = ''; if (typeMatch) $('#confirm-type-label').textContent = `Type the ${typeLabel} to confirm`; const valid = () => !typeMatch || typeInput.value === typeMatch; const close = event => { dialog.removeEventListener('close', close); resolve(dialog.returnValue === 'confirm' && (!breaking || check.checked) && valid()); }; form.onsubmit = event => { if (event.submitter?.value !== 'confirm') return; if (breaking && !check.checked) { event.preventDefault(); $('#confirm-error').textContent = 'Acknowledge the breaking client-facing change first.'; return; } if (typeMatch && !valid()) { event.preventDefault(); $('#confirm-error').textContent = `Type the ${typeLabel} exactly to confirm.`; } }; dialog.addEventListener('close', close); dialog.showModal(); if (typeMatch) setTimeout(() => typeInput.focus(), 0); }); }

function selectSecretText() { const node = $('#secret-value'); const sel = window.getSelection(); sel.removeAllRanges(); const range = document.createRange(); range.selectNodeContents(node); sel.addRange(range); }
function showSecret(secret) { $('#secret-value').textContent = secret; const secure = window.isSecureContext && navigator.clipboard?.writeText; $('#copy-secret').hidden = !secure; $('#copy-state').textContent = ''; $('#secret-dialog').showModal(); if (!secure) { selectSecretText(); $('#copy-state').textContent = 'Key selected — press Ctrl/Cmd+C to copy it.'; } }
$('#copy-secret').onclick = async () => { const text = $('#secret-value').textContent; const state = $('#copy-state'); if (!(window.isSecureContext && navigator.clipboard?.writeText)) return; try { await navigator.clipboard.writeText(text); state.textContent = 'Copied to clipboard.'; } catch { selectSecretText(); state.textContent = 'Clipboard copy was denied — press Ctrl/Cmd+C to copy it.'; } };
$('#close-secret').onclick = () => { $('#secret-value').textContent = ''; $('#secret-dialog').close(); };

document.addEventListener('keydown', event => { if (event.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) { event.preventDefault(); const input = $(`#view-${state.view} input[type="search"]`); input?.focus(); } });

(async function initialise() { state.view = viewFromHash(); try { const session = await api('/api/admin/session'); showApp(session); } catch { showLogin(); } })();
