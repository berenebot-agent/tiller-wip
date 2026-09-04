// LiveStream is a thin wrapper over EventSource for the admin UI's live
// refresh. It exposes a subscribe-style API and manages the connection
// lifecycle: opened after login, closed on logout/pagehide, and closed while
// the tab is hidden so a background tab costs zero server queries.
//
// The server pushes three event types:
//   - "outcome": a micro-delta of per-target last request outcomes (drives the
//     resolution icons instantly, no DB cost).
//   - "activity": transient in-flight request deltas keyed by virtual model,
//     client key, or virtual-model/provider-model pair.
//   - "snapshot": the full usage/health envelope (drives token/cache counters
//     and reconciles any dropped delta).
//
// The connection is gated by an explicit enabled flag so that visibility
// changes only reconnect while the session is active. showLogin()/logout
// call stop() which disables the stream; showApp() calls start() which
// re-enables it. This prevents unauthorised reconnects after explicit
// logout and avoids stale UI after passive session expiry.
export class LiveStream {
  constructor(url, { onAuthFailure } = {}) {
    this.url = url;
    this.handlers = new Map();
    this.es = null;
    this.generation = 0;
    this.enabled = false;
    this.visible = !document.hidden;
    this.onAuthFailure = onAuthFailure;
    this._authTimer = null;
    document.addEventListener('visibilitychange', () => {
      this.visible = !document.hidden;
      if (this.visible && this.enabled) this.open();
      else if (!this.visible) this.close();
    });
  }

  on(event, handler) {
    if (!this.handlers.has(event)) this.handlers.set(event, new Set());
    this.handlers.get(event).add(handler);
    return this;
  }

  open() {
    if (this.es || !this.visible || !this.enabled) return;
    if (this._authTimer) { clearTimeout(this._authTimer); this._authTimer = null; }
    const es = new EventSource(this.url);
    const generation = ++this.generation;
    this.es = es;
    const current = () => this.es === es && this.generation === generation;
    es.onmessage = (e) => { if (current()) this.dispatch('message', e.data); };
    es.addEventListener('outcome', (e) => { if (current()) this.dispatch('outcome', e.data); });
    es.addEventListener('activity', (e) => { if (current()) this.dispatch('activity', e.data); });
    es.addEventListener('snapshot', (e) => { if (current()) this.dispatch('snapshot', e.data); });
    // On error, allow EventSource's built-in reconnect to recover from
    // transient failures. If the connection stays down (e.g. server-side
    // session expiry closes the stream), schedule an auth-failure callback
    // so the UI can return to the login screen rather than showing stale
    // data. The timer is cleared on the next successful open().
    es.onerror = () => {
      if (!current() || !this.enabled || this._authTimer) return;
      this._authTimer = setTimeout(() => {
        this._authTimer = null;
        if (this.enabled && this.onAuthFailure) this.onAuthFailure();
      }, 5000);
    };
  }

  close() {
    const es = this.es;
    if (es) {
      es.close();
      this.es = null;
    }
    if (this._authTimer) { clearTimeout(this._authTimer); this._authTimer = null; }
    this.generation++;
  }

  // start() enables the stream and opens the connection. Called after
  // successful login. Idempotent: calling start() on an already-enabled
  // stream is a no-op.
  start() {
    if (this.enabled) return;
    this.enabled = true;
    this.open();
  }

  // stop() disables the stream and closes the connection. Called on
  // logout. The visibility handler will not reconnect until start() is
  // called again.
  stop() {
    if (!this.enabled) return;
    this.enabled = false;
    this.close();
  }

  dispatch(event, data) {
    let payload;
    try { payload = JSON.parse(data); } catch { return; }
    const set = this.handlers.get(event);
    if (!set) return;
    for (const handler of set) handler(payload);
  }
}
