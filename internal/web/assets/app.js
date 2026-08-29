const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
const state = { csrf: '', view: 'providers', providers: [], models: [], groups: [], virtualModels: [], clients: [], permissionData: null, providerTypes: [] };
const h = value => String(value ?? '').replace(/[&<>'"]/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' })[char]);
const date = value => value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Never';
const VIEWS = ['providers', 'models', 'virtual', 'clients', 'system'];
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
  try { if (view === 'providers') await loadProviders(); if (view === 'models') await loadModels(); if (view === 'virtual') await loadVirtual(); if (view === 'clients') await loadClients(); if (view === 'system') await loadSystem(); }
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
async function deleteProvider(id) { const provider = state.providers.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${provider.name}?`, copy: 'All discovered models and their client permissions will be removed. Deletion is blocked while a virtual model references this provider.', action: 'Delete provider' })) return; try { await api(`/api/admin/providers/${id}`, { method: 'DELETE' }); flash('Provider deleted.'); await loadProviders(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadModels(search = $('#model-search').value) { const result = await api(`/api/admin/models?limit=200&search=${encodeURIComponent(search || '')}`); state.models = result.data; renderModels(); }
function renderModels() { const shown = state.models.filter(item => $('#show-retired').checked || item.available); $('#models-empty').hidden = shown.length > 0; $('#models-body').innerHTML = shown.map(model => `<tr><td><code class="model-id">${h(model.canonical_model_id)}</code></td><td><code class="model-id">${h(model.upstream_model_id)}</code></td><td>${h(model.provider_name)}</td><td>${badge(model.available, model.available ? 'Available' : 'Retired', model.available ? 'good' : 'warn')}</td><td><span class="meta-line">${date(model.first_seen_at)}</span></td></tr>`).join(''); }

async function loadVirtual(search = $('#virtual-search').value) {
  const [groups, virtualModels, providersResult, modelsResult] = await Promise.all([
    api('/api/admin/virtual-groups?limit=200'), api(`/api/admin/virtual-models?limit=200&search=${encodeURIComponent(search || '')}`), api('/api/admin/providers?limit=200'), api('/api/admin/models?limit=200')
  ]);
  state.groups = groups.data; state.virtualModels = virtualModels.data; state.providers = providersResult.data; state.models = modelsResult.data; renderVirtual();
}
function renderVirtual() {
  $('#virtual-empty').hidden = state.virtualModels.length > 0;
  $('#virtual-body').innerHTML = state.virtualModels.map(model => `<tr><td><code class="model-id">${h(model.canonical_model_id)}</code><span class="meta-line">Stable client identity</span></td><td></td><td><code class="model-id">${h(model.target_provider_name)}/${h(model.target_upstream_model_id)}</code><span class="meta-line">${h(model.target_provider_name)}</span></td><td>${badge(model.available, model.available ? 'Routable' : 'Broken target', model.available ? 'good' : 'bad')}${model.warning ? `<span class="error-text">${h(model.warning)}</span>` : ''}</td><td><div class="actions"><button class="btn btn-small btn-secondary" data-virtual-edit="${h(model.id)}">Remap / edit</button><button class="btn btn-small btn-danger" data-virtual-delete="${h(model.id)}">Delete</button></div></td></tr>`).join('');
  $('#virtual-groups').innerHTML = state.groups.length ? state.groups.map(group => `<div class="group-chip"><code>${h(group.name)}</code><small>${group.model_count} model${group.model_count === 1 ? '' : 's'}</small><button class="chip-action" data-group-edit="${h(group.id)}" aria-label="Rename ${h(group.name)}">Edit</button><button class="chip-action" data-group-delete="${h(group.id)}" aria-label="Delete ${h(group.name)}">×</button></div>`).join('') : '<span class="meta-line">No virtual provider groups.</span>';
  $$('[data-virtual-edit]').forEach(button => button.onclick = () => openVirtualModel(state.virtualModels.find(item => item.id === button.dataset.virtualEdit)));
  $$('[data-virtual-delete]').forEach(button => button.onclick = () => deleteVirtualModel(button.dataset.virtualDelete));
  $$('[data-group-edit]').forEach(button => button.onclick = () => openVirtualGroup(state.groups.find(item => item.id === button.dataset.groupEdit)));
  $$('[data-group-delete]').forEach(button => button.onclick = () => deleteVirtualGroup(button.dataset.groupDelete));
}
$('#add-virtual-group').onclick = () => openVirtualGroup(); $('#add-virtual-model').onclick = () => openVirtualModel();
function openVirtualGroup(group = null) { openEntity({ eyebrow: 'VIRTUAL NAMESPACE', title: group ? `Rename ${group.name}` : 'Create virtual group', fields: `<label>Group name <input name="name" value="${h(group?.name || '')}" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" placeholder="main" required><small>Lowercase slug. Shares the provider namespace.</small></label>${group ? '<label class="confirm-check"><input name="confirm" type="checkbox" required> <span>I understand that every model ID in this group will change.</span></label>' : ''}`, submit: group ? 'Rename group' : 'Create group', onSubmit: async form => { const values = new FormData(form); if (group) await api(`/api/admin/virtual-groups/${group.id}`, { method: 'PATCH', body: JSON.stringify({ name: values.get('name'), confirm_breaking_change: values.get('confirm') === 'on' }) }); else await api('/api/admin/virtual-groups', { method: 'POST', body: JSON.stringify({ name: values.get('name') }) }); flash(group ? 'Virtual group renamed.' : 'Virtual group created.'); await loadVirtual(); } }); }
async function deleteVirtualGroup(id) { const group = state.groups.find(item => item.id === id); if (!await confirmAction({ title: `Delete group ${group.name}?`, copy: 'Only empty virtual groups can be deleted.', action: 'Delete group' })) return; try { await api(`/api/admin/virtual-groups/${id}`, { method: 'DELETE' }); flash('Virtual group deleted.'); await loadVirtual(); } catch (error) { flash(errorMessage(error), 'error'); } }
function virtualModelFields(model) {
  const groupOptions = state.groups.map(group => `<option value="${h(group.id)}" ${model?.group_id === group.id ? 'selected' : ''}>${h(group.name)}</option>`).join('');
  const providerOptions = state.providers.map(provider => `<option value="${h(provider.id)}" ${model?.target_provider_id === provider.id ? 'selected' : ''}>${h(provider.name)}</option>`).join('');
  const groupField = state.groups.length ? `<label>Virtual group <select name="group_id" ${model ? 'disabled' : ''} required>${groupOptions}</select></label>` : `<label>New virtual group <input name="group_name" value="${h(model?.group_name || 'main')}" pattern="[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" placeholder="main" required><small>No group exists yet; this creates one.</small></label>`;
  return `<div class="row">${groupField}<label>Virtual model name <input name="name" value="${h(model?.name || '')}" placeholder="coding" required><small>Stable client-facing identity.</small></label></div><label>Target provider <select name="target_provider_id" required>${providerOptions}</select></label><label>Target real model <select name="target_model_id" required></select><small>Only models belonging to the selected provider are eligible.</small></label>${model ? '<label class="confirm-check"><input name="confirm" type="checkbox"> <span>Confirm if changing the virtual model name; this is a breaking client-facing rename.</span></label>' : ''}`;
}
function openVirtualModel(model = null) { if (!state.models.length) { flash('Discover at least one real model before creating a virtual route.', 'info'); return; } openEntity({ eyebrow: model ? 'IMMEDIATE REMAP' : 'NEW STABLE IDENTITY', title: model ? `Edit ${model.canonical_model_id}` : 'Create virtual model', fields: virtualModelFields(model), submit: model ? 'Apply immediately' : 'Create route', onMount: form => { const provider = $('[name="target_provider_id"]', form), target = $('[name="target_model_id"]', form); const options = () => { const matches = state.models.filter(item => item.provider_id === provider.value); target.innerHTML = matches.map(item => `<option value="${h(item.id)}" ${model?.target_model_id === item.id ? 'selected' : ''}>${h(item.upstream_model_id)}${item.available ? '' : ' — retired'}</option>`).join(''); }; provider.addEventListener('change', options); options(); }, onSubmit: async form => { const values = new FormData(form); const payload = { name: values.get('name'), target_provider_id: values.get('target_provider_id'), target_model_id: values.get('target_model_id') }; if (model) { payload.confirm_breaking_change = values.get('confirm') === 'on'; await api(`/api/admin/virtual-models/${model.id}`, { method: 'PATCH', body: JSON.stringify(payload) }); flash('Virtual route remapped. New requests use the target immediately.'); } else { const groupID = values.get('group_id'); if (groupID) payload.group_id = groupID; else payload.group_name = values.get('group_name'); await api('/api/admin/virtual-models', { method: 'POST', body: JSON.stringify(payload) }); flash('Virtual route created.'); } await loadVirtual(); } }); }
async function deleteVirtualModel(id) { const model = state.virtualModels.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${model.canonical_model_id}?`, copy: 'Clients using this stable identity will receive model-not-found after deletion.', action: 'Delete virtual model' })) return; try { await api(`/api/admin/virtual-models/${id}`, { method: 'DELETE' }); flash('Virtual model deleted.'); await loadVirtual(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function loadClients(search = $('#client-search').value) { const result = await api(`/api/admin/client-keys?limit=200&search=${encodeURIComponent(search || '')}`); state.clients = result.data; renderClients(); }
function renderClients() { $('#clients-empty').hidden = state.clients.length > 0; $('#clients-body').innerHTML = state.clients.map(client => `<tr><td class="primary-cell"><strong>${h(client.name)}</strong><small>${h(client.description || 'No description')}</small></td><td><span class="secret-fingerprint">sk-tr-••••••••.${h(client.fingerprint)}</span></td><td><span class="meta-line">Created ${date(client.created_at)}${client.rotated_at ? `<br>Rotated ${date(client.rotated_at)}` : ''}</span></td><td>${badge(client.enabled, client.enabled ? 'Enabled' : 'Disabled', client.enabled ? 'good' : 'neutral')}</td><td><div class="actions"><button class="btn btn-small btn-primary" data-client-permissions="${h(client.id)}">Permissions</button><button class="btn btn-small btn-secondary" data-client-rotate="${h(client.id)}">Rotate</button><button class="btn btn-small btn-secondary" data-client-edit="${h(client.id)}">Edit</button><button class="btn btn-small btn-danger" data-client-delete="${h(client.id)}">Delete</button></div></td></tr>`).join(''); $$('[data-client-permissions]').forEach(button => button.onclick = () => openPermissions(state.clients.find(item => item.id === button.dataset.clientPermissions))); $$('[data-client-rotate]').forEach(button => button.onclick = () => rotateClient(button.dataset.clientRotate)); $$('[data-client-edit]').forEach(button => button.onclick = () => openClient(state.clients.find(item => item.id === button.dataset.clientEdit))); $$('[data-client-delete]').forEach(button => button.onclick = () => deleteClient(button.dataset.clientDelete)); }
$('#add-client').onclick = () => openClient();
function openClient(client = null) { openEntity({ eyebrow: client ? 'EDIT CLIENT' : 'ISSUE CREDENTIAL', title: client ? `Edit ${client.name}` : 'Create client key', fields: `<label>Client name <input name="name" value="${h(client?.name || '')}" placeholder="Hermes Server 3" required></label><label>Description <textarea name="description" rows="3" placeholder="Workload, owner, or deployment note">${h(client?.description || '')}</textarea></label>${client ? `<label class="toggle-label"><input class="switch" name="enabled" type="checkbox" ${client.enabled ? 'checked' : ''}> Client key enabled</label>` : ''}`, submit: client ? 'Save client' : 'Create & show key', onSubmit: async form => { const values = new FormData(form); if (client) { await api(`/api/admin/client-keys/${client.id}`, { method: 'PATCH', body: JSON.stringify({ name: values.get('name'), description: values.get('description'), enabled: values.get('enabled') === 'on' }) }); flash('Client key metadata updated.'); } else { const result = await api('/api/admin/client-keys', { method: 'POST', body: JSON.stringify({ name: values.get('name'), description: values.get('description') }) }); showSecret(result.secret); } await loadClients(); } }); }
async function rotateClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Rotate ${client.name}?`, copy: 'The current secret will stop authenticating immediately. Permissions and metadata are preserved.', action: 'Rotate now' })) return; try { const result = await api(`/api/admin/client-keys/${id}/rotate`, { method: 'POST' }); showSecret(result.secret); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }
async function deleteClient(id) { const client = state.clients.find(item => item.id === id); if (!await confirmAction({ title: `Delete ${client.name}?`, copy: 'The client secret will be invalidated immediately and all permissions will be removed.', action: 'Delete client key' })) return; try { await api(`/api/admin/client-keys/${id}`, { method: 'DELETE' }); flash('Client key deleted and invalidated.'); await loadClients(); } catch (error) { flash(errorMessage(error), 'error'); } }

async function openPermissions(client) {
  try { state.permissionData = await api(`/api/admin/client-keys/${client.id}/permissions`); $('#permissions-title').textContent = `${client.name} permissions`; renderPermissions(); $('#permissions-dialog').showModal(); }
  catch (error) { flash(errorMessage(error), 'error'); }
}
function renderPermissions(filter = '') {
  const term = filter.toLowerCase(); $('#permission-groups').innerHTML = state.permissionData.groups.map(group => {
    const models = group.models.map(model => `<label class="permission-row ${model.available ? '' : 'retired'}" ${term && !model.canonical_model_id.toLowerCase().includes(term) ? 'hidden' : ''}><code>${h(model.canonical_model_id)}</code><input class="switch" type="checkbox" data-permission-kind="${h(model.kind)}" data-model-id="${h(model.id)}" ${model.enabled ? 'checked' : ''} aria-label="Enable ${h(model.canonical_model_id)}"></label>`).join('');
    return `<section class="permission-group"><header class="permission-group-head"><h3>${h(group.name)} <span class="protocol">${h(group.kind)}</span></h3><label class="toggle-label">New models default <input class="switch" type="checkbox" data-default-kind="${h(group.kind)}" data-group-id="${h(group.id)}" ${group.new_models_enabled ? 'checked' : ''}></label></header><div class="permission-list">${models || '<p class="meta-line">No models in this group.</p>'}</div></section>`;
  }).join('');
}
$('#permission-search').addEventListener('input', event => renderPermissions(event.target.value));
$('#close-permissions').onclick = $('#cancel-permissions').onclick = () => $('#permissions-dialog').close();
$('#save-permissions').onclick = async () => { const button = $('#save-permissions'); button.disabled = true; $('#permissions-error').textContent = ''; try { const defaults = $$('[data-group-id]', $('#permissions-dialog')).map(input => ({ kind: input.dataset.defaultKind, group_id: input.dataset.groupId, enabled: input.checked })); const permissions = $$('[data-model-id]', $('#permissions-dialog')).map(input => ({ kind: input.dataset.permissionKind, model_id: input.dataset.modelId, enabled: input.checked })); await api(`/api/admin/client-keys/${state.permissionData.client_key_id}/permissions`, { method: 'PUT', body: JSON.stringify({ defaults, permissions }) }); $('#permissions-dialog').close(); flash('Client catalogue permissions saved.'); } catch (error) { $('#permissions-error').textContent = errorMessage(error); } finally { button.disabled = false; } };

async function loadSystem() { const health = await api('/api/admin/health'); $('#top-status').textContent = health.status.toUpperCase(); $('#health-state').textContent = health.status.toUpperCase(); $('#health-metrics').innerHTML = `<dt>Provider instances</dt><dd>${health.providers}</dd><dt>Available real models</dt><dd>${health.available_models}</dd><dt>Retired real models</dt><dd>${health.retired_models}</dd><dt>Broken virtual routes</dt><dd>${health.broken_virtual_models}</dd><dt>Persistence</dt><dd>SQLITE / READY</dd>`; }

let entitySubmit = null;
function openEntity({ eyebrow, title, fields, submit, onMount, onSubmit }) { const dialog = $('#form-dialog'), form = $('#entity-form'); $('#dialog-eyebrow').textContent = eyebrow; $('#dialog-title').textContent = title; $('#dialog-fields').innerHTML = fields; $('#dialog-submit').textContent = submit; $('#dialog-error').textContent = ''; entitySubmit = onSubmit; form.onsubmit = handleEntitySubmit; dialog.showModal(); onMount?.(form); setTimeout(() => $('input:not([type="checkbox"]),select,textarea', form)?.focus(), 0); }
async function handleEntitySubmit(event) { const form = event.currentTarget, button = $('#dialog-submit'); if (event.submitter?.value === 'cancel') return; event.preventDefault(); if (!form.reportValidity()) return; button.disabled = true; $('#dialog-error').textContent = ''; try { await entitySubmit(form); $('#form-dialog').close(); } catch (error) { $('#dialog-error').textContent = errorMessage(error); } finally { button.disabled = false; } }

function confirmAction({ title, copy, action, breaking = false }) { return new Promise(resolve => { const dialog = $('#confirm-dialog'), form = $('form', dialog), checkWrap = $('#confirm-check-wrap'), check = $('#confirm-check'); $('#confirm-title').textContent = title; $('#confirm-copy').textContent = copy; $('#confirm-action').textContent = action; $('#confirm-error').textContent = ''; checkWrap.hidden = !breaking; check.checked = false; const close = event => { dialog.removeEventListener('close', close); resolve(dialog.returnValue === 'confirm' && (!breaking || check.checked)); }; form.onsubmit = event => { if (event.submitter?.value === 'confirm' && breaking && !check.checked) { event.preventDefault(); $('#confirm-error').textContent = 'Acknowledge the breaking client-facing change first.'; } }; dialog.addEventListener('close', close); dialog.showModal(); }); }

function showSecret(secret) { $('#secret-value').textContent = secret; $('#copy-state').textContent = ''; $('#secret-dialog').showModal(); }
$('#copy-secret').onclick = async () => { try { await navigator.clipboard.writeText($('#secret-value').textContent); $('#copy-state').textContent = 'Copied to clipboard.'; } catch { $('#copy-state').textContent = 'Clipboard access was denied. Select and copy the key manually.'; } };
$('#close-secret').onclick = () => { $('#secret-value').textContent = ''; $('#secret-dialog').close(); };

document.addEventListener('keydown', event => { if (event.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) { event.preventDefault(); const input = $(`#view-${state.view} input[type="search"]`); input?.focus(); } });

(async function initialise() { state.view = viewFromHash(); try { const session = await api('/api/admin/session'); showApp(session); } catch { showLogin(); } })();
