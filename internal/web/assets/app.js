const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const state = { csrf: '', view: 'providers', providers: [], models: [], groups: [], virtualModels: [], clients: [], permissionData: null, providerTypes: [], usage: null };
const collapsedModels = new Set(); const collapsedVirtual = new Set();
const GROUP_ARROW = { up: '▼', down: '▶' };
const capabilities = model => `<span class="meta-line">Context: ${model.context_length ? h(model.context_length) : '—'}</span><span class="meta-line">Output: ${model.max_output_tokens ? h(model.max_output_tokens) : '—'}</span>`;
const h = value => String(value ?? '').replace(/[&<>'"]/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' })[char]);
const date = value => value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Never';
const mtok = n => n ? `<span class="mtok">${(n / 1e6).toFixed(2)}<small>Mtok</small></span>` : '—';
const VIEWS = ['providers', 'models', 'virtual', 'clients', 'settings'];
const viewFromHash = () => { const v = (location.hash.replace(/^#\/?/, '') || 'providers'); return VIEWS.includes(v) ? v : 'providers'; };

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

function showLogin() { $('#app').hidden = true; $('#login-shell').hidden = false; state.csrf = ''; history.replaceState(null, '', '#/providers'); }
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
$$('[data-view]').forEach(button => button.addEventListener('click', () => navigate(button.dataset.view)));
window.addEventListener('popstate', () => navigate(viewFromHash()));
$('#mobile-menu').addEventListener('click', event => { const links = $('#nav-links'); links.classList.toggle('open'); event.currentTarget.setAttribute('aria-expanded', String(links.classList.contains('open'))); });
$$('[data-refresh-view]').forEach(button => button.addEventListener('click', () => navigate(button.dataset.refreshView)));

let filterTimers = new Map();
function filterInput(selector, callback) { $(selector).addEventListener('input', event => { clearTimeout(filterTimers.get(selector)); filterTimers.set(selector, setTimeout(() => callback(event.target.value), 180)); }); }
filterInput('#provider-search', loadProviders); filterInput('#model-search', loadModels); filterInput('#virtual-search', loadVirtual); filterInput('#client-search', loadClients);
$('#show-retired').addEventListener('change', renderModels);

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
  return `<label>Provider name <input name="name" value="${h(provider?.name || '')}" placeholder="openai-main" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" required><small>Lowercase namespace used in client-facing model IDs.</small></label>
    <label>Provider type <select name="type" ${provider ? 'disabled' : ''} required>${options}</select></label>
    <label>Base URL <input name="base_url" type="url" value="${h(provider?.base_url || '')}" placeholder="https://api.example.com/v1" required></label>
    ${provider ? '' : '<label>API credential <input name="credential" type="password" autocomplete="new-password"><small>Write-only. Leave empty only when the provider permits unauthenticated access.</small></label>'}
    ${provider ? '<label>Replacement credential <input name="credential" type="password" autocomplete="new-password"><small>Leave blank to keep the configured credential.</small></label>' : ''}
    <fieldset class="protocol-select" data-protocol-config><legend>Declared native protocols</legend>${['chat','responses','messages'].map(protocol => `<label><input type="checkbox" name="protocol" value="${protocol}" ${selectedProtocols.includes(protocol) ? 'checked' : ''}> ${protocol}</label>`).join('')}<small>Generic providers default to Chat Completions. Declare only surfaces the upstream implements natively.</small></fieldset>
    <label class="toggle-label"><input class="switch" name="enabled" type="checkbox" ${provider?.enabled !== false ? 'checked' : ''}> Provider enabled</label>
    ${provider ? '<label class="confirm-check"><input name="confirm_breaking_change" type="checkbox"> <span>Confirm if the provider name changes; every direct model ID will change.</span></label>' : ''}`;
}
function openProvider(provider = null) {
  openEntity({ eyebrow: provider ? 'EDIT UPSTREAM' : 'REGISTER UPSTREAM', title: provider ? `Edit ${provider.name}` : 'Add provider', fields: providerFields(provider), submit: provider ? 'Save provider' : 'Add & discover', onMount: form => { const select = $('[name="type"]', form); const protocolConfig = $('[data-protocol-config]', form); const showProtocols = () => protocolConfig.hidden = !['generic-openai','vllm'].includes(provider?.type || select.value); if (!provider) { const base = $('[name="base_url"]', form); const credential = $('[name="credential"]', form); const apply = () => { const item = state.providerTypes.find(t => t.type === select.value); if (!base.value || base.dataset.auto === 'true') { base.value = item?.default_base_url || ''; base.dataset.auto = 'true'; } credential.required = Boolean(item?.credential_needed); showProtocols(); }; base.addEventListener('input', () => base.dataset.auto = 'false'); select.addEventListener('change', apply); apply(); } else showProtocols(); }, onSubmit: async form => {
    const values = new FormData(form); const payload = { name: values.get('name'), base_url: values.get('base_url'), enabled: values.get('enabled') === 'on', protocols: values.getAll('protocol') };
    if (provider) { payload.confirm_breaking_change = values.get('confirm_breaking_change') === 'on'; await api(`/api/admin/providers/${provider.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); if (values.get('credential')) await api(`/api/admin/providers/${provider.id}/credential`, { method: 'PUT', body: JSON.stringify({ credential: values.get('credential') }) }); flash('Provider configuration updated.'); }
    else { payload.type = values.get('type'); payload.credential = values.get('credential'); const result = await api('/api/admin/providers', { method: 'POST', body: JSON.stringify(payload) }); flash(result.refresh_error || 'Provider saved and catalogue discovered.', result.refresh_error ? 'info' : 'success'); }
    await loadProviders();
  }});
}
async function refreshProvider(id) { const button = $(`[data-provider-refresh="${CSS.escape(id)}"]`); button.disabled = true; try { await api(`/api/admin/providers/${id}/refresh`, { method: 'POST' }); flash('Catalogue refresh completed.'); await loadProviders(); } catch (error) { flash(errorMessage(error), 'error'); await loadProviders(); } finally { button.disabled = false; } }
async function refreshModels(id) { const button = $(`[data-refresh-models="${CSS.escape(id)}"]`); button.disabled = true; try { await api(`/api/admin/providers/${id}/refresh`, { method: 'POST' }); flash('Catalogue refresh completed.'); } catch (error) { flash(errorMessage(error), 'error'); } finally { await loadModels(); await loadProviders(); button.disabled = false; } }
async function deleteProvider(id) { const provider = state.providers.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${provider.name}?`, copy: 'All discovered models and their client permissions will be removed. Deletion is blocked while a virtual model references this provider.', action: 'Delete provider', typeMatch: provider.name, typeLabel: 'provider name' })) return; try { await api(`/api/admin/providers/${id}`, { method: 'DELETE' }); flash('Provider deleted.'); await loadProviders(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadModels(search = $('#model-search').value) { const [result, usage] = await Promise.all([api(`/api/admin/models?all=1&search=${encodeURIComponent(search || '')}`), api('/api/admin/usage')]); state.models = result.data; state.usage = usage; renderModels(); }
function groupBanner(kind, key, label, note, count, actions = '') { const collapsed = (kind === 'models' ? collapsedModels : collapsedVirtual).has(key); return `<tr class="group-toggle" data-group-toggle="${kind}" data-group-key="${h(key)}" data-expanded="${collapsed ? 'false' : 'true'}" aria-expanded="${collapsed ? 'false' : 'true'}"><td colspan="9"><span class="group-arrow">${collapsed ? GROUP_ARROW.down : GROUP_ARROW.up}</span><span class="group-label">${h(label)}</span><span class="count-badge">${h(count)}</span><span class="meta-line">${h(note)}</span>${actions ? `<span class="banner-actions">${actions}</span>` : ''}</td></tr>`; }
function toggleGroup(event) { const header = event.currentTarget; const expanded = header.dataset.expanded === 'true'; let next = header.nextElementSibling; const rows = []; while (next && !next.classList.contains('group-toggle')) { rows.push(next); next = next.nextElementSibling; } const nowExpanded = !expanded; rows.forEach(row => row.classList.toggle('group-row-hidden', !nowExpanded)); header.dataset.expanded = String(nowExpanded); header.setAttribute('aria-expanded', String(nowExpanded)); $('.group-arrow', header).textContent = nowExpanded ? GROUP_ARROW.up : GROUP_ARROW.down; const store = header.dataset.groupToggle === 'models' ? collapsedModels : collapsedVirtual; const key = header.dataset.groupKey; if (nowExpanded) store.delete(key); else store.add(key); }
const groupRows = (rows, key, collapsed) => `${rows.map(row => `<tr class="group-row${collapsed ? ' group-row-hidden' : ''}">${row}</tr>`).join('')}`;
function renderModels() { const shown = state.models.filter(item => $('#show-retired').checked || item.available); $('#models-empty').hidden = shown.length > 0; const byProvider = new Map(); shown.forEach(model => { if (!byProvider.has(model.provider_name)) byProvider.set(model.provider_name, []); byProvider.get(model.provider_name).push(model); }); const html = [...byProvider.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([provider, models]) => { const available = models.filter(m => m.available).length; const retired = models.length - available; const collapsed = collapsedModels.has(provider); const note = retired ? `${retired} retired` : 'provider'; const actions = `<button class="btn btn-small btn-secondary" data-refresh-models="${h(models[0].provider_id)}">Refresh models</button>`; return groupBanner('models', provider, provider, note, `${available} available`, actions) + groupRows(models.map(model => `<td><code class="model-id">${h(model.canonical_model_id)}</code></td><td><code class="model-id">${h(model.upstream_model_id)}</code></td><td>${h(model.provider_name)}</td><td>${badge(model.available, model.available ? 'Available' : 'Retired', model.available ? 'good' : 'warn')}</td><td>${capabilities(model)}</td><td><span class="meta-line">${date(model.first_seen_at)}</span></td><td>${mtok(state.usage?.real_models?.[model.canonical_model_id]?.['1h'])}</td><td>${mtok(state.usage?.real_models?.[model.canonical_model_id]?.['24h'])}</td><td>${mtok(state.usage?.real_models?.[model.canonical_model_id]?.['7d'])}</td>`), provider, collapsed); }).join(''); $('#models-body').innerHTML = html; $$('.group-toggle', $('#models-body')).forEach(header => header.onclick = toggleGroup); $$('[data-refresh-models]', $('#models-body')).forEach(button => button.onclick = event => { event.stopPropagation(); refreshModels(button.dataset.refreshModels); }); }

async function loadVirtual(search = $('#virtual-search').value) {
  const [groups, virtualModels, providersResult, modelsResult, usage] = await Promise.all([
    api('/api/admin/virtual-groups?limit=200'), api(`/api/admin/virtual-models?limit=200&search=${encodeURIComponent(search || '')}`), api('/api/admin/providers?limit=200'), api('/api/admin/models?limit=200'), api('/api/admin/usage')
  ]);
  state.groups = groups.data; state.virtualModels = virtualModels.data; state.providers = providersResult.data; state.models = modelsResult.data; state.usage = usage; renderVirtual();
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
    return groupBanner('virtual', name, name, note, `${models.length} model${models.length === 1 ? '' : 's'}`, actions) + groupRows(models.map(model => `<td><code class="model-id">${h(model.canonical_model_id)}</code><span class="meta-line">Stable client identity</span></td><td></td><td><code class="model-id">${h(model.target_provider_name)}/${h(model.target_upstream_model_id)}</code><span class="meta-line">${h(model.target_provider_name)}</span></td><td>${capabilities(model)}</td><td>${badge(model.available, model.available ? 'Routable' : 'Broken target', model.available ? 'good' : 'bad')}${model.warning ? `<span class="error-text">${h(model.warning)}</span>` : ''}</td><td>${mtok(state.usage?.virtual_models?.[model.canonical_model_id]?.['1h'])}</td><td>${mtok(state.usage?.virtual_models?.[model.canonical_model_id]?.['24h'])}</td><td>${mtok(state.usage?.virtual_models?.[model.canonical_model_id]?.['7d'])}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-virtual-edit="${h(model.id)}">Remap / edit</button><button class="btn btn-small btn-danger" data-virtual-delete="${h(model.id)}">Delete</button></div></td>`), name, collapsed);
  }).join('');
  $('#virtual-body').innerHTML = html;
  $$('.group-toggle', $('#virtual-body')).forEach(header => header.onclick = toggleGroup);
  $$('[data-virtual-edit]').forEach(button => button.onclick = () => openVirtualModel(state.virtualModels.find(item => item.id === button.dataset.virtualEdit)));
  $$('[data-virtual-delete]').forEach(button => button.onclick = () => deleteVirtualModel(button.dataset.virtualDelete));
  $$('[data-group-edit]').forEach(button => button.onclick = event => { event.stopPropagation(); openVirtualGroup(state.groups.find(item => item.id === button.dataset.groupEdit)); });
  $$('[data-group-delete]').forEach(button => button.onclick = event => { event.stopPropagation(); deleteVirtualGroup(button.dataset.groupDelete); });
}
$('#add-virtual-group').onclick = () => openVirtualGroup(); $('#add-virtual-model').onclick = () => openVirtualModel();
function openVirtualGroup(group = null) { openEntity({ eyebrow: 'VIRTUAL NAMESPACE', title: group ? `Rename ${group.name}` : 'Create virtual group', fields: `<label>Group name <input name="name" value="${h(group?.name || '')}" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" placeholder="virtual" required><small>Lowercase slug. Shares the provider namespace.</small></label>${group ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm" type="checkbox"> <span>I understand that every model ID in this group will change.</span></label>' : ''}`, submit: group ? 'Rename group' : 'Create group', onMount: group ? form => { const nameInput = $('[name="name"]', form), wrap = $('[data-confirm-wrap]', form); const sync = () => { wrap.hidden = nameInput.value === group.name; if (wrap.hidden) { const cb = $('[name="confirm"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', sync); sync(); } : null, onSubmit: async form => { const values = new FormData(form); if (group) await api(`/api/admin/virtual-groups/${group.id}`, { method: 'PATCH', body: JSON.stringify({ name: values.get('name'), confirm_breaking_change: values.get('confirm') === 'on' }) }); else await api('/api/admin/virtual-groups', { method: 'POST', body: JSON.stringify({ name: values.get('name') }) }); flash(group ? 'Virtual group renamed.' : 'Virtual group created.'); await loadVirtual(); } }); }
async function deleteVirtualGroup(id) { const group = state.groups.find(item => item.id === id); if (!await confirmAction({ title: `Delete group ${group.name}?`, copy: 'Only empty virtual groups can be deleted.', action: 'Delete group', typeMatch: group.name, typeLabel: 'group name' })) return; try { await api(`/api/admin/virtual-groups/${id}`, { method: 'DELETE' }); flash('Virtual group deleted.'); await loadVirtual(); } catch (error) { flash(errorMessage(error), 'error'); } }
function combobox({ input, hidden, options, placeholder, onSelect }) {
  const list = document.createElement('ul'); list.className = 'combobox-list'; list.setAttribute('role', 'listbox'); list.hidden = true;
  input.setAttribute('role', 'combobox'); input.setAttribute('aria-autocomplete', 'list'); input.setAttribute('aria-expanded', 'false'); input.setAttribute('autocomplete', 'off'); input.setAttribute('spellcheck', 'false'); input.placeholder = placeholder || 'Type to filter…';
  input.parentNode.appendChild(list);
  let items = [], active = -1, open = false;
  const close = () => { open = false; list.hidden = true; input.setAttribute('aria-expanded', 'false'); active = -1; };
  const render = (showAll = false) => {
    const term = showAll ? '' : input.value.trim().toLowerCase();
    items = options.filter(opt => !term || opt.label.toLowerCase().includes(term));
    list.innerHTML = items.map((opt, i) => `<li role="option" data-i="${i}" ${i === active ? 'aria-selected="true"' : ''}>${h(opt.label)}${opt.muted ? '<small> — retired</small>' : ''}</li>`).join('');
    list.hidden = !items.length; open = !list.hidden; input.setAttribute('aria-expanded', String(open));
    if (active >= items.length) active = items.length - 1;
  };
  const select = i => { const opt = items[i]; if (!opt) return; hidden.value = opt.value; input.value = opt.label; onSelect?.(opt); close(); };
  input.addEventListener('input', () => { if (hidden.value && !options.some(o => o.value === hidden.value && o.label === input.value)) hidden.value = ''; active = -1; render(); });
  input.addEventListener('click', () => { if (open) close(); else render(true); });
  input.addEventListener('keydown', event => {
    if (!open && (event.key === 'ArrowDown' || event.key === 'ArrowUp')) { event.preventDefault(); render(true); return; }
    if (event.key === 'ArrowDown') { event.preventDefault(); active = Math.min(active + 1, items.length - 1); render(); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); active = Math.max(active - 1, 0); render(); }
    else if (event.key === 'Enter') { event.preventDefault(); if (active >= 0) select(active); }
    else if (event.key === 'Escape') { close(); }
  });
  list.addEventListener('mousedown', event => { event.preventDefault(); const li = event.target.closest('li[data-i]'); if (li) select(Number(li.dataset.i)); });
  input.addEventListener('blur', () => setTimeout(close, 120));
  return { setOptions: next => { options = next; if (hidden.value && !options.some(o => o.value === hidden.value)) { hidden.value = ''; input.value = ''; } active = -1; close(); }, select };
}
function virtualModelFields(model) {
  const groupOptions = state.groups.map(group => `<option value="${h(group.id)}" ${model?.group_id === group.id ? 'selected' : ''}>${h(group.name)}</option>`).join('');
  const groupField = state.groups.length ? `<label>Virtual group <select name="group_id" ${model ? 'disabled' : ''} required>${groupOptions}</select></label>` : `<label>New virtual group <input name="group_name" value="${h(model?.group_name || 'virtual')}" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" placeholder="virtual" required><small>No group exists yet; this creates one.</small></label>`;
  return `<div class="row">${groupField}<label>Virtual model name <input name="name" value="${h(model?.name || '')}" placeholder="coding" required><small>Stable client-facing identity.</small></label></div><label>Target provider <div class="combobox"><input type="text" placeholder="Type to filter providers…"><input type="hidden" name="target_provider_id" required></div></label><label>Target real model <div class="combobox"><input type="text" placeholder="Type to filter models…"><input type="hidden" name="target_model_id" required></div><small>Only models belonging to the selected provider are eligible.</small></label>${model ? '<label class="confirm-check" data-confirm-wrap hidden><input name="confirm" type="checkbox"> <span>Confirm if changing the virtual model name; this is a breaking client-facing rename.</span></label>' : ''}`;
}
function openVirtualModel(model = null) { if (!state.models.length) { flash('Discover at least one real model before creating a virtual route.', 'info'); return; } openEntity({ eyebrow: model ? 'IMMEDIATE REMAP' : 'NEW STABLE IDENTITY', title: model ? `Edit ${model.canonical_model_id}` : 'Create virtual model', fields: virtualModelFields(model), submit: model ? 'Apply immediately' : 'Create route', onMount: form => {
  const providerBox = $('[name="target_provider_id"]', form).parentNode, modelBox = $('[name="target_model_id"]', form).parentNode;
  const providerInput = $('input[type="text"]', providerBox), providerHidden = $('input[type="hidden"]', providerBox);
  const modelInput = $('input[type="text"]', modelBox), modelHidden = $('input[type="hidden"]', modelBox);
  const providerOptions = state.providers.map(p => ({ value: p.id, label: p.name }));
  const fetchProviderModels = async providerId => { if (!providerId) return []; const res = await api(`/api/admin/providers/${providerId}/models?all=1`); return res.data.map(item => ({ value: item.id, label: item.upstream_model_id, muted: !item.available })); };
  const providerCb = combobox({ input: providerInput, hidden: providerHidden, options: providerOptions, placeholder: 'Type to filter providers…', onSelect: async () => { try { modelCb.setOptions(await fetchProviderModels(providerHidden.value)); } catch (error) { flash(errorMessage(error), 'error'); } } });
  const modelCb = combobox({ input: modelInput, hidden: modelHidden, options: [], placeholder: 'Type to filter models…' });
  if (model) { const p = state.providers.find(p => p.id === model.target_provider_id); if (p) { providerHidden.value = p.id; providerInput.value = p.name; } (async () => { try { const models = await fetchProviderModels(providerHidden.value); modelCb.setOptions(models); const m = models.find(m => m.value === model.target_model_id); if (m) { modelHidden.value = m.id; modelInput.value = m.label; } } catch (error) { flash(errorMessage(error), 'error'); } })(); }
  const nameInput = $('[name="name"]', form); if (model) { const wrap = $('[data-confirm-wrap]', form); const sync = () => { wrap.hidden = nameInput.value === model.name; if (wrap.hidden) { const cb = $('[name="confirm"]', form); if (cb) cb.checked = false; } }; nameInput.addEventListener('input', sync); sync(); }
}, onSubmit: async form => { const values = new FormData(form); const payload = { name: values.get('name'), target_provider_id: values.get('target_provider_id'), target_model_id: values.get('target_model_id') }; if (model) { payload.confirm_breaking_change = values.get('confirm') === 'on'; await api(`/api/admin/virtual-models/${model.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); flash('Virtual route remapped. New requests use the target immediately.'); } else { const groupID = values.get('group_id'); if (groupID) payload.group_id = groupID; else payload.group_name = values.get('group_name'); await api('/api/admin/virtual-models', { method: 'POST', body: JSON.stringify(payload) }); flash('Virtual route created.'); } await loadVirtual(); } }); }
async function deleteVirtualModel(id) { const model = state.virtualModels.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${model.canonical_model_id}?`, copy: 'Clients using this stable identity will receive model-not-found after deletion.', action: 'Delete virtual model' })) return; try { await api(`/api/admin/virtual-models/${id}`, { method: 'DELETE' }); flash('Virtual model deleted.'); await loadVirtual(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadClients(search = $('#client-search').value) { const [result, usage] = await Promise.all([api(`/api/admin/client-keys?limit=200&search=${encodeURIComponent(search || '')}`), api('/api/admin/usage')]); state.clients = result.data; state.usage = usage; renderClients(); }
function renderClients() { $('#clients-empty').hidden = state.clients.length > 0; $('#clients-body').innerHTML = state.clients.map(client => `<tr><td class="primary-cell"><strong>${h(client.name)}</strong><small>${h(client.description || 'No description')}</small></td><td><span class="secret-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</span></td><td><span class="meta-line">Created ${date(client.created_at)}${client.rotated_at ? `<br>Rotated ${date(client.rotated_at)}` : ''}</span></td><td>${badge(client.enabled, client.enabled ? 'Enabled' : 'Disabled', client.enabled ? 'good' : 'neutral')}</td><td>${mtok(state.usage?.client_keys?.[client.id]?.['1h'])}</td><td>${mtok(state.usage?.client_keys?.[client.id]?.['24h'])}</td><td>${mtok(state.usage?.client_keys?.[client.id]?.['7d'])}</td><td><div class="actions"><button class="btn btn-small btn-primary" data-client-permissions="${h(client.id)}">Permissions</button><button class="btn btn-small btn-secondary" data-client-activity="${h(client.id)}">Activity</button><button class="btn btn-small btn-secondary" data-client-rotate="${h(client.id)}">Rotate</button><button class="btn btn-small btn-secondary" data-client-edit="${h(client.id)}">Edit</button><button class="btn btn-small btn-danger" data-client-delete="${h(client.id)}">Delete</button></div></td></tr>`).join(''); $$('[data-client-permissions]').forEach(button => button.onclick = () => openPermissions(state.clients.find(item => item.id === button.dataset.clientPermissions))); $$('[data-client-activity]').forEach(button => button.onclick = () => openActivity(state.clients.find(item => item.id === button.dataset.clientActivity))); $$('[data-client-rotate]').forEach(button => button.onclick = () => rotateClient(button.dataset.clientRotate)); $$('[data-client-edit]').forEach(button => button.onclick = () => openClient(state.clients.find(item => item.id === button.dataset.clientEdit))); $$('[data-client-delete]').forEach(button => button.onclick = () => deleteClient(button.dataset.clientDelete)); }
$('#add-client').onclick = () => openClient();
function openClient(client = null) { openEntity({ eyebrow: client ? 'EDIT CLIENT' : 'ISSUE CREDENTIAL', title: client ? `Edit ${client.name}` : 'Create client key', fields: `<label>Client name <input name="name" value="${h(client?.name || '')}" placeholder="Hermes Server 3" required></label><label>Description <textarea name="description" rows="3" placeholder="Workload, owner, or deployment note">${h(client?.description || '')}</textarea></label>${client ? `<label class="toggle-label"><input class="switch" name="enabled" type="checkbox" ${client.enabled ? 'checked' : ''}> Client key enabled</label><label class="toggle-label"><input class="switch" name="logging_enabled" type="checkbox" ${client.logging_enabled ? 'checked' : ''}> Log requests for this client</label><label>Retention (days) <input name="retention_days" type="number" min="1" step="1" value="${h(client.retention_days)}" required><small>Request logs older than this are pruned.</small></label>` : ''}`, submit: client ? 'Save client' : 'Create & show key', onSubmit: async form => { const values = new FormData(form); if (client) { await api(`/api/admin/client-keys/${client.id}`, { method: 'PATCH', body: JSON.stringify({ name: values.get('name'), description: values.get('description'), enabled: values.get('enabled') === 'on', logging_enabled: values.get('logging_enabled') === 'on', retention_days: Number(values.get('retention_days')) }) }); flash('Client key metadata updated.'); } else { const result = await api('/api/admin/client-keys', { method: 'POST', body: JSON.stringify({ name: values.get('name'), description: values.get('description') }) }); showSecret(result.secret); } await loadClients(); } }); }
async function rotateClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Rotate ${client.name}?`, copy: 'The current secret will stop authenticating immediately. Permissions and metadata are preserved.', action: 'Rotate now' })) return; try { const result = await api(`/api/admin/client-keys/${id}/rotate`, { method: 'POST' }); showSecret(result.secret); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }
async function deleteClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${client.name}?`, copy: 'The client secret will be invalidated immediately and all permissions will be removed.', action: 'Delete client key' })) return; try { await api(`/api/admin/client-keys/${id}`, { method: 'DELETE' }); flash('Client key deleted and invalidated.'); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function openPermissions(client) {
  try { state.permissionData = await api(`/api/admin/client-keys/${client.id}/permissions`); $('#permissions-title').textContent = `${client.name} permissions`; $('#permission-search').value = ''; renderPermissions(''); $('#permissions-dialog').showModal(); }
  catch (error) { flash(errorMessage(error), 'error'); }
}
function renderPermissions(filter = '') {
  const term = filter.toLowerCase(); $('#permission-groups').innerHTML = state.permissionData.groups.map(group => {
    const models = group.models.map(model => `<label class="permission-row ${model.available ? '' : 'retired'}" ${term && !model.canonical_model_id.toLowerCase().includes(term) ? 'hidden' : ''}><code>${h(model.canonical_model_id)}</code><input class="switch" type="checkbox" data-permission-kind="${h(model.kind)}" data-model-id="${h(model.id)}" ${model.enabled ? 'checked' : ''} aria-label="Enable ${h(model.canonical_model_id)}"></label>`).join('');
    return `<section class="permission-group"><header class="permission-group-head"><h3>${h(group.name)} <span class="protocol">${h(group.kind)}</span></h3><label class="toggle-label">New models default <input class="switch" type="checkbox" data-default-kind="${h(group.kind)}" data-group-id="${h(group.id)}" ${group.new_models_enabled ? 'checked' : ''}></label></header><div class="permission-list">${models || '<p class="meta-line">No models in this group.</p>'}</div></section>`;
  }).join('');
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
}
$('#permission-search').addEventListener('input', event => renderPermissions(event.target.value));
function bulkSetCurrentPermissions(enabled) {
  // A bulk action targets only AVAILABLE models (retired/unavailable are
  // preserved), scoped to the current filter term when one is non-empty. It
  // mutates only the in-memory checkboxes; nothing touches new_models_enabled.
  const term = ($('#permission-search').value || '').toLowerCase();
  state.permissionData.groups.forEach(group => group.models.forEach(model => {
    if (!model.available) return;
    if (term && !model.canonical_model_id.toLowerCase().includes(term)) return;
    model.enabled = enabled;
  }));
  renderPermissions($('#permission-search').value);
}
$('#enable-current-permissions').onclick = () => bulkSetCurrentPermissions(true);
$('#disable-current-permissions').onclick = () => bulkSetCurrentPermissions(false);
$('#close-permissions').onclick = $('#cancel-permissions').onclick = () => $('#permissions-dialog').close();
$('#save-permissions').onclick = async () => { const button = $('#save-permissions'); button.disabled = true; $('#permissions-error').textContent = ''; try { const defaults = state.permissionData.groups.map(group => ({ kind: group.kind, group_id: group.id, enabled: group.new_models_enabled })); const permissions = state.permissionData.groups.flatMap(group => group.models.map(model => ({ kind: model.kind, model_id: model.id, enabled: model.enabled }))); await api(`/api/admin/client-keys/${state.permissionData.client_key_id}/permissions`, { method: 'PUT', body: JSON.stringify({ defaults, permissions }) }); $('#permissions-dialog').close(); flash('Client catalogue permissions saved.'); } catch (error) { $('#permissions-error').textContent = errorMessage(error); } finally { button.disabled = false; } };

const activityState = { client: null, rows: [], offset: 0, limit: 50, search: '', hasMore: true };
async function openActivity(client) { activityState.client = client; activityState.offset = 0; activityState.search = ''; $('#activity-search').value = ''; $('#activity-title').textContent = `${client.name} activity`; await loadActivity(); $('#activity-dialog').showModal(); }
async function loadActivity() { if (!activityState.client) return; activityState.controller?.abort(); activityState.controller = new AbortController(); const { signal } = activityState.controller; try { const result = await api(`/api/admin/client-keys/${activityState.client.id}/activity?limit=${activityState.limit + 1}&offset=${activityState.offset}&search=${encodeURIComponent(activityState.search || '')}`, { signal }); const fetched = result.data; activityState.hasMore = fetched.length > activityState.limit; activityState.rows = fetched.slice(0, activityState.limit); $('#activity-error').textContent = ''; renderActivity(); } catch (error) { if (error.name === 'AbortError') return; $('#activity-error').textContent = errorMessage(error); } }
function renderActivity() { $('#activity-empty').hidden = activityState.rows.length > 0; $('#activity-body').innerHTML = activityState.rows.map(row => `<tr><td><span class="meta-line">${date(row.created_at)}</span></td><td><code class="model-id">${h(row.requested_model)}</code></td><td>${row.resolved_provider ? `<code class="model-id">${h(row.resolved_provider)}/${h(row.resolved_model || '')}</code>` : '<span class="meta-line">—</span>'}</td><td><span class="protocol">${h(row.protocol)}</span>${row.streaming ? '<span class="protocol">stream</span>' : ''}</td><td>${badge(row.http_status >= 200 && row.http_status < 300, row.http_status, row.http_status >= 200 && row.http_status < 300 ? 'good' : 'bad')}</td><td><span class="meta-line">${row.latency_ms} ms</span></td><td><span class="meta-line">${row.input_tokens ?? '—'} / ${row.output_tokens ?? '—'}</span></td><td><code class="model-id">${h(row.client_request_id)}</code></td><td>${row.error_text ? `<span class="error-text">${h(row.error_text)}</span>` : ''}</td></tr>`).join(''); $('#activity-count').textContent = activityState.rows.length ? `${activityState.offset + 1}–${activityState.offset + activityState.rows.length}` : '0 results'; $('#activity-prev').disabled = activityState.offset === 0; $('#activity-next').disabled = !activityState.hasMore; }
filterInput('#activity-search', value => { activityState.search = value; activityState.offset = 0; loadActivity(); });
$('#activity-prev').onclick = () => { activityState.offset = Math.max(0, activityState.offset - activityState.limit); loadActivity(); };
$('#activity-next').onclick = () => { activityState.offset += activityState.limit; loadActivity(); };
$('#close-activity').onclick = $('#done-activity').onclick = () => $('#activity-dialog').close();
$('#clear-activity').onclick = async () => { if (!await confirmAction({ title: `Clear ${activityState.client.name} activity?`, copy: 'All logged requests for this client will be permanently deleted.', action: 'Clear logs' })) return; try { await api(`/api/admin/client-keys/${activityState.client.id}/activity`, { method: 'DELETE' }); activityState.offset = 0; await loadActivity(); flash('Client activity cleared.'); } catch (error) { $('#activity-error').textContent = errorMessage(error); } };

// Global activity is a read-only section in the Settings view, distinct from
// the per-client Activity dialog. It shows metadata across all client keys.
const globalActivityState = { rows: [], offset: 0, limit: 50, search: '', hasMore: true };
async function loadGlobalActivity() { globalActivityState.controller?.abort(); globalActivityState.controller = new AbortController(); const { signal } = globalActivityState.controller; try { const result = await api(`/api/admin/activity?limit=${globalActivityState.limit + 1}&offset=${globalActivityState.offset}&search=${encodeURIComponent(globalActivityState.search || '')}`, { signal }); const fetched = result.data; globalActivityState.hasMore = fetched.length > globalActivityState.limit; globalActivityState.rows = fetched.slice(0, globalActivityState.limit); $('#global-activity-error').textContent = ''; renderGlobalActivity(); } catch (error) { if (error.name === 'AbortError') return; $('#global-activity-error').textContent = errorMessage(error); } }
function renderGlobalActivity() { $('#global-activity-empty').hidden = globalActivityState.rows.length > 0; $('#global-activity-body').innerHTML = globalActivityState.rows.map(row => `<tr><td><span class="meta-line">${date(row.created_at)}</span></td><td><strong>${h(row.client_name)}</strong></td><td><code class="model-id">${h(row.requested_model)}</code></td><td>${row.resolved_provider ? `<code class="model-id">${h(row.resolved_provider)}/${h(row.resolved_model || '')}</code>` : '<span class="meta-line">—</span>'}</td><td><span class="protocol">${h(row.protocol)}</span>${row.streaming ? '<span class="protocol">stream</span>' : ''}</td><td>${badge(row.http_status >= 200 && row.http_status < 300, row.http_status, row.http_status >= 200 && row.http_status < 300 ? 'good' : 'bad')}</td><td><span class="meta-line">${row.latency_ms} ms</span></td><td><span class="meta-line">${row.input_tokens ?? '—'} / ${row.output_tokens ?? '—'}</span></td><td><code class="model-id">${h(row.client_request_id)}</code></td><td>${row.error_text ? `<span class="error-text">${h(row.error_text)}</span>` : ''}</td></tr>`).join(''); $('#global-activity-count').textContent = globalActivityState.rows.length ? `${globalActivityState.offset + 1}–${globalActivityState.offset + globalActivityState.rows.length}` : '0 results'; $('#global-activity-prev').disabled = globalActivityState.offset === 0; $('#global-activity-next').disabled = !globalActivityState.hasMore; }
filterInput('#global-activity-search', value => { globalActivityState.search = value; globalActivityState.offset = 0; loadGlobalActivity(); });
$('#global-activity-prev').onclick = () => { globalActivityState.offset = Math.max(0, globalActivityState.offset - globalActivityState.limit); loadGlobalActivity(); };
$('#global-activity-next').onclick = () => { globalActivityState.offset += globalActivityState.limit; loadGlobalActivity(); };

async function loadSettings() { const [health, settings] = await Promise.all([api('/api/admin/health'), api('/api/admin/settings')]); $('#top-status').textContent = health.status.toUpperCase(); $('#health-state').textContent = health.status.toUpperCase(); $('#health-metrics').innerHTML = `<dt>Provider instances</dt><dd>${health.providers}</dd><dt>Available real models</dt><dd>${health.available_models}</dd><dt>Retired real models</dt><dd>${health.retired_models}</dd><dt>Broken virtual routes</dt><dd>${health.broken_virtual_models}</dd><dt>Persistence</dt><dd>SQLITE / READY</dd>`; $('[name="default_logging_enabled"]', $('#settings-form')).checked = settings.default_logging_enabled; $('[name="default_retention_days"]', $('#settings-form')).value = settings.default_retention_days; await loadGlobalActivity(); }
$('#settings-form').addEventListener('submit', async event => { event.preventDefault(); const button = $('button[type="submit"]', event.currentTarget); button.disabled = true; $('#settings-error').textContent = ''; try { const values = new FormData(event.currentTarget); await api('/api/admin/settings', { method: 'PUT', body: JSON.stringify({ default_logging_enabled: values.get('default_logging_enabled') === 'on', default_retention_days: Number(values.get('default_retention_days')) }) }); flash('Logging defaults saved. New client keys will use them.'); } catch (error) { $('#settings-error').textContent = errorMessage(error); } finally { button.disabled = false; } });

let entitySubmit = null;
function openEntity({ eyebrow, title, fields, submit, onMount, onSubmit }) { const dialog = $('#form-dialog'), form = $('#entity-form'); $('#dialog-eyebrow').textContent = eyebrow; $('#dialog-title').textContent = title; $('#dialog-fields').innerHTML = fields; $('#dialog-submit').textContent = submit; $('#dialog-error').textContent = ''; entitySubmit = onSubmit; form.onsubmit = handleEntitySubmit; dialog.showModal(); onMount?.(form); setTimeout(() => $('input:not([type="checkbox"]),select,textarea', form)?.focus(), 0); }
async function handleEntitySubmit(event) { const form = event.currentTarget, button = $('#dialog-submit'); if (event.submitter?.value === 'cancel') return; event.preventDefault(); if (!form.reportValidity()) return; button.disabled = true; $('#dialog-error').textContent = ''; try { await entitySubmit(form); $('#form-dialog').close(); } catch (error) { $('#dialog-error').textContent = errorMessage(error); } finally { button.disabled = false; } }

function confirmAction({ title, copy, action, breaking = false, typeMatch = null, typeLabel = 'name' }) { return new Promise(resolve => { const dialog = $('#confirm-dialog'), form = $('form', dialog), checkWrap = $('#confirm-check-wrap'), check = $('#confirm-check'), typeWrap = $('#confirm-type-wrap'), typeInput = $('#confirm-type'); $('#confirm-title').textContent = title; $('#confirm-copy').textContent = copy; $('#confirm-action').textContent = action; $('#confirm-error').textContent = ''; checkWrap.hidden = !breaking; check.checked = false; typeWrap.hidden = !typeMatch; typeInput.value = ''; if (typeMatch) $('#confirm-type-label').textContent = `Type the ${typeLabel} to confirm`; const valid = () => !typeMatch || typeInput.value === typeMatch; const close = event => { dialog.removeEventListener('close', close); resolve(dialog.returnValue === 'confirm' && (!breaking || check.checked) && valid()); }; form.onsubmit = event => { if (event.submitter?.value !== 'confirm') return; if (breaking && !check.checked) { event.preventDefault(); $('#confirm-error').textContent = 'Acknowledge the breaking client-facing change first.'; return; } if (typeMatch && !valid()) { event.preventDefault(); $('#confirm-error').textContent = `Type the ${typeLabel} exactly to confirm.`; } }; dialog.addEventListener('close', close); dialog.showModal(); if (typeMatch) setTimeout(() => typeInput.focus(), 0); }); }

function showSecret(secret) { $('#secret-value').textContent = secret; $('#copy-state').textContent = ''; $('#secret-dialog').showModal(); }
$('#copy-secret').onclick = async () => { const text = $('#secret-value').textContent; try { await navigator.clipboard.writeText(text); $('#copy-state').textContent = 'Copied to clipboard.'; } catch { const ta = document.createElement('textarea'); ta.value = text; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.opacity = '0'; document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); $('#copy-state').textContent = 'Copied to clipboard.'; } catch { $('#copy-state').textContent = 'Clipboard access was denied. Select and copy the key manually.'; } document.body.removeChild(ta); } };
$('#close-secret').onclick = () => { $('#secret-value').textContent = ''; $('#secret-dialog').close(); };

document.addEventListener('keydown', event => { if (event.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) { event.preventDefault(); const input = $(`#view-${state.view} input[type="search"]`); input?.focus(); } });

(async function initialise() { state.view = viewFromHash(); try { const session = await api('/api/admin/session'); showApp(session); } catch { showLogin(); } })();
