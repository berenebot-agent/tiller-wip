// Request-level live view. No completed-history seed or browser persistence.
// The graph retains its separate aggregate feed.
export class LiveRequests {
  constructor(root, { api, h, date, when, requestIdentity, rowCache }) {
    Object.assign(this, { root, api, h, date, when, requestIdentity, rowCache });
    this.rows = new Map();
    this.expanded = new Set();
    this.omitted = new Map();
    // IDs already painted at least once this session. render() re-inserts
    // existing nodes on every pass, so an enter animation must be driven off
    // this set rather than the act of insertion or the whole list would replay
    // it on each tick.
    this.seen = new Set();
    this.generation = 0;
    this.active = false;
    this.connection = 'connecting';
    this.clockOffset = 0;
    root.addEventListener('click', event => {
      const button = event.target.closest('[data-live-fallback]');
      if (!button || !this.active) return;
      const id = button.dataset.liveFallback;
      if (this.expanded.has(id)) this.expanded.delete(id);
      else this.expanded.add(id);
      this.render();
    });
  }

  async start() {
    this.stop();
    this.active = true;
    this.openedAt = this.now();
    this.omittedCount = 0;
    this.message = '';
    const generation = this.generation;
    this.render();
    this.timer = setInterval(() => this.tick(), 1000);
    try {
      const result = await this.api('/api/admin/live/requests');
      if (!this.active || generation !== this.generation) return;
      this.syncClock(result.generated_at);
      for (const row of result.data || []) this.upsert(row);
      this.render();
    } catch {
      if (this.active && generation === this.generation) {
        this.message = 'Could not load live requests. Waiting for the next live snapshot.';
        this.render();
      }
    }
  }

  stop() {
    this.active = false;
    this.generation++;
    clearInterval(this.timer);
    this.rows.clear();
    this.expanded.clear();
    this.omitted.clear();
    this.seen.clear();
    this.reconciling = false;
    for (const node of [...this.root.querySelector('#live-requests-body').tBodies]) node.remove();
    this.root.querySelector('#mobile-activity-body').replaceChildren();
  }

  setConnection(state) {
    this.connection = state;
    if (this.active) this.paintStatus();
  }

  now() { return Date.now() + this.clockOffset; }

  syncClock(generatedAt) {
    const serverTime = Date.parse(generatedAt);
    if (!Number.isFinite(serverTime)) return;
    const offset = serverTime - Date.now();
    // Preserve the session boundary in server time if browser/server clocks
    // differ. Expiry and elapsed time must not depend on the operator's clock.
    if (this.openedAt != null) this.openedAt += offset - this.clockOffset;
    this.clockOffset = offset;
  }

  upsert(row) {
    if (!row?.id) return;
    if (row.state === 'removed') {
      this.rows.delete(row.id);
      this.expanded.delete(row.id);
      return;
    }
    if (this.omitted.has(row.id)) return;
    const old = this.rows.get(row.id);
    // A late baseline/REST response must not undo a newer delta or completion.
    if (old && ((old.completed_at && !row.completed_at) || (row.revision && old.revision > row.revision))) return;
    if (row.completed_at && !old && Date.parse(row.created_at) < this.openedAt) return;
    if (row.completed_at && this.now() - Date.parse(row.completed_at) >= 30000 && !this.expanded.has(row.id)) {
      this.rows.delete(row.id);
      return;
    }
    this.rows.set(row.id, row);
  }

  onRequest(row) {
    if (!this.active) return;
    this.upsert(row);
    this.render();
  }

  onSnapshot(rows, generatedAt) {
    if (!this.active || !Array.isArray(rows)) return;
    this.syncClock(generatedAt);
    this.message = '';
    const present = new Set(rows.map(row => row.id));
    for (const row of rows) this.upsert(row);
    const missing = [...this.rows.values()].filter(row => !row.completed_at && !present.has(row.id)).map(row => row.id);
    if (missing.length) this.reconcile(missing);
    this.render();
  }

  async reconcile(ids) {
    if (this.reconciling) return;
    this.reconciling = true;
    const generation = this.generation;
    try {
      // Batches bound request size without limiting the number of running rows.
      for (let offset = 0; offset < ids.length; offset += 1000) {
        const result = await this.api('/api/admin/live/requests/reconcile', { method: 'POST', body: JSON.stringify({ ids: ids.slice(offset, offset + 1000) }) });
        if (!this.active || generation !== this.generation) return;
        this.syncClock(result.generated_at);
        for (const row of result.data || []) this.upsert(row);
        for (const id of result.missing || []) {
          const row = this.rows.get(id);
          if (row && !row.completed_at) {
            this.rows.delete(id);
            this.expanded.delete(id);
            this.message = 'Some tracked requests are no longer available. Check Activity history for retained records.';
          }
        }
      }
      this.render();
    } catch {
      if (this.active && generation === this.generation) {
        this.message = 'Request reconciliation failed. Retrying on the next live snapshot.';
        this.paintStatus();
      }
    } finally {
      if (generation === this.generation) this.reconciling = false;
    }
  }

  visibleRows() {
    const now = this.now();
    for (const [id, row] of this.rows) {
      if (row.completed_at && now - Date.parse(row.completed_at) >= 30000 && !this.expanded.has(id)) this.rows.delete(id);
    }
    for (const [id, until] of this.omitted) if (now >= until) this.omitted.delete(id);
    const rows = [...this.rows.values()].sort((a, b) => {
      if (!!a.completed_at !== !!b.completed_at) return a.completed_at ? 1 : -1;
      return Date.parse(b.completed_at || b.created_at) - Date.parse(a.completed_at || a.created_at) || a.id.localeCompare(b.id);
    });
    let completed = 0;
    return rows.filter(row => {
      if (!row.completed_at || this.expanded.has(row.id) || ++completed <= 200) return true;
      this.rows.delete(row.id);
      this.omitted.set(row.id, Date.parse(row.completed_at) + 30000);
      this.omittedCount++;
      return false;
    });
  }

  fallbackHTML(row) {
    if (!row.fallback_used) return '';
    const expanded = this.expanded.has(row.id);
    return `<button type="button" class="btn btn-small btn-secondary live-fallback" data-live-fallback="${this.h(row.id)}" aria-expanded="${expanded}">Fallback ${expanded ? '▴' : '▾'}</button>`;
  }

  targetHTML(row) {
    const last = row.attempts?.at(-1);
    const provider = row.resolved_provider || last?.provider;
    const model = row.resolved_model || last?.model;
    const route = provider ? `${provider}/${model || ''}` : row.completed_at ? 'Unresolved' : 'Resolving…';
    const cls = row.state === 'success' ? 'attempt-success' : row.state === 'failed' ? 'attempt-failed' : 'attempt-neutral';
    const status = row.completed_at ? (row.http_status ? `HTTP ${row.http_status}` : row.state) : row.state === 'streaming' ? 'Streaming' : 'Running';
    return `<div class="live-model-target"><div class="activity-attempt live-model-box ${cls}"><span class="attempt-route"><code class="model-id" title="${this.h(route)}">${this.h(route)}</code></span><span class="attempt-status">${this.h(status)}</span></div>${this.fallbackHTML(row)}</div>${row.error_text ? `<span class="error-text live-model-error">${this.h(row.error_text)}</span>` : ''}`;
  }

  chainHTML(row) {
    if (!this.expanded.has(row.id)) return '';
    return `<div class="attempt-sequence live-request-attempts">${(row.attempts || []).map((a, i) => {
      const route = `${a.provider}/${a.model}`;
      const cls = a.result === 'failed' ? 'attempt-failed' : a.result === 'success' ? 'attempt-success' : a.failure_class === 'cooldown' ? 'attempt-cooldown' : 'attempt-neutral';
      return `<div class="activity-attempt ${cls}"><span class="attempt-number">${String(i + 1).padStart(2, '0')}</span><span class="attempt-route"><code title="${this.h(route)}">${this.h(route)}</code></span><span class="attempt-status">${this.h(a.result)}${a.http_status ? ` · HTTP ${this.h(a.http_status)}` : ''}${a.failure_class ? ` · ${this.h(a.failure_class)}` : ''}${a.latency_ms ? ` · ${this.h(a.latency_ms)} ms` : ''}</span></div>`;
    }).join('')}</div>${row.completed_at ? '<p class="meta-line">Held open until collapsed.</p>' : ''}`;
  }

  elapsedHTML(row) {
    return `<span class="meta-line" data-live-elapsed="${this.h(row.id)}">${this.elapsed(row)} ms</span>`;
  }

  // Relative age, refreshed every tick alongside the latency cell. Completed
  // rows freeze at their completion time so the value stops moving once served.
  ageHTML(row) {
    return `<span class="meta-line" data-live-when="${this.h(row.id)}">${this.h(this.when(this.elapsed(row)))}</span>`;
  }

  elapsed(row) {
    return row.completed_at ? row.latency_ms : Math.max(0, this.now() - Date.parse(row.created_at));
  }

  render() {
    if (!this.active) return;
    const focused = this.root.contains(document.activeElement) ? document.activeElement.dataset.liveFallback : null;
    const focusedMobile = document.activeElement.closest?.('#mobile-activity-body');
    const rows = this.visibleRows();
    const table = this.root.querySelector('#live-requests-body');
    const cards = this.root.querySelector('#mobile-activity-body');
    const keep = new Set(rows.map(row => row.id));
    for (const container of [table, cards]) {
      for (const node of [...container.children]) if (node.dataset.liveRequest && !keep.has(node.dataset.liveRequest)) node.remove();
    }
    const groupsByID = new Map([...table.tBodies].map(node => [node.dataset.liveRequest, node]));
    const cardsByID = new Map([...cards.children].map(node => [node.dataset.liveRequest, node]));
    let tableCursor = table.tBodies[0] || null;
    let cardCursor = cards.firstElementChild;
    for (const row of rows) {
      const key = JSON.stringify([row, this.expanded.has(row.id)]);
      // Only genuinely new rows animate. A row already painted this session is
      // re-inserted silently, and a row that was already old by the time it
      // first reached the pane (e.g. a burst after a reconnect) does not replay
      // the settle animation for every entry in the backlog.
      const fresh = !this.seen.has(row.id);
      if (fresh) this.seen.add(row.id);
      const entering = fresh && this.now() - Date.parse(row.created_at) < 1000;
      let group = groupsByID.get(row.id);
      if (!group) { group = document.createElement('tbody'); group.dataset.liveRequest = row.id; }
      if (group.dataset.renderKey !== key) {
        group.dataset.renderKey = key;
        group.innerHTML = `<tr><td>${this.ageHTML(row)}</td><td><strong class="client-name">${this.h(row.client_name)}</strong></td><td>${this.requestIdentity(row)}</td><td>${this.targetHTML(row)}</td><td>${this.elapsedHTML(row)}</td><td>${this.rowCache(row)}</td></tr>${this.expanded.has(row.id) ? `<tr class="live-chain-row"><td colspan="6">${this.chainHTML(row)}</td></tr>` : ''}`;
      }
      group.classList.toggle('live-row-enter', entering);
      if (group !== tableCursor) table.insertBefore(group, tableCursor);
      else tableCursor = tableCursor.nextElementSibling;
      let card = cardsByID.get(row.id);
      if (!card) { card = document.createElement('article'); card.className = 'live-activity-card'; card.dataset.liveRequest = row.id; }
      if (card.dataset.renderKey !== key) {
        card.dataset.renderKey = key;
        card.innerHTML = `<div class="live-activity-top"><strong>${this.h(row.client_name)}</strong><div class="live-request-mobile-route">${this.requestIdentity(row)}<span aria-hidden="true"> → </span>${this.targetHTML(row)}</div></div>${this.chainHTML(row)}`;
      }
      card.classList.toggle('live-row-enter', entering);
      if (card !== cardCursor) cards.insertBefore(card, cardCursor);
      else cardCursor = cardCursor.nextElementSibling;
    }
    this.root.querySelector('#live-requests-empty').hidden = rows.length > 0;
    this.root.querySelector('#mobile-activity-empty').hidden = rows.length > 0;
    this.paintStatus();
    this.tickElapsed();
    if (focused) (focusedMobile ? cards : table).querySelector(`[data-live-fallback="${CSS.escape(focused)}"]`)?.focus({ preventScroll: true });
  }

  paintStatus() {
    const labels = { connecting: 'Connecting…', connected: 'Live', reconnecting: 'Reconnecting…', paused: 'Paused while tab is hidden' };
    for (const node of this.root.querySelectorAll('[data-live-connection]')) node.textContent = labels[this.connection] || this.connection;
    for (const node of this.root.querySelectorAll('[data-live-request-notice]')) node.textContent = [this.message, this.omittedCount ? `${this.omittedCount} older completed rows omitted this session (200-row limit).` : ''].filter(Boolean).join(' ');
  }

  tickElapsed() {
    for (const node of this.root.querySelectorAll('[data-live-when]')) {
      const row = this.rows.get(node.dataset.liveWhen);
      if (row) node.textContent = this.when(this.elapsed(row));
    }
    for (const node of this.root.querySelectorAll('[data-live-elapsed]')) {
      const row = this.rows.get(node.dataset.liveElapsed);
      if (row && !row.completed_at) node.textContent = `${this.elapsed(row)} ms`;
    }
  }

  tick() {
    if ([...this.rows.values()].some(row => row.completed_at && !this.expanded.has(row.id) && this.now() - Date.parse(row.completed_at) >= 30000)) this.render();
    else this.tickElapsed();
  }
}
