// LiveStream is a thin wrapper over EventSource for the admin UI's live
// refresh. It exposes a subscribe-style API and manages the connection
// lifecycle: opened after login, closed on logout/pagehide, and closed while
// the tab is hidden so a background tab costs zero server queries.
//
// The server pushes three event types:
//   - "outcome": a micro-delta of per-target last request outcomes (drives the
//     resolution icons instantly, no DB cost).
//   - "activity": transient in-flight request deltas keyed by virtual model,
//     client key, or provider model ID.
//   - "snapshot": the full usage/health envelope (drives token/cache counters
//     and reconciles any dropped delta).
export class LiveStream {
  constructor(url) {
    this.url = url;
    this.handlers = new Map();
    this.es = null;
    this.generation = 0;
    this.visible = !document.hidden;
    document.addEventListener('visibilitychange', () => {
      this.visible = !document.hidden;
      if (this.visible) this.open();
      else this.close();
    });
  }

  on(event, handler) {
    if (!this.handlers.has(event)) this.handlers.set(event, new Set());
    this.handlers.get(event).add(handler);
    return this;
  }

  open() {
    if (this.es || !this.visible) return;
    const es = new EventSource(this.url);
    const generation = ++this.generation;
    this.es = es;
    const current = () => this.es === es && this.generation === generation;
    es.onmessage = (e) => { if (current()) this.dispatch('message', e.data); };
    es.addEventListener('outcome', (e) => { if (current()) this.dispatch('outcome', e.data); });
    es.addEventListener('activity', (e) => { if (current()) this.dispatch('activity', e.data); });
    es.addEventListener('snapshot', (e) => { if (current()) this.dispatch('snapshot', e.data); });
    // Keep ownership during transient failures so EventSource can reconnect.
    // Explicit close() remains the only path that releases this connection.
    es.onerror = () => {};
  }

  close() {
    const es = this.es;
    if (es) {
      es.close();
      this.es = null;
    }
    this.generation++;
  }

  dispatch(event, data) {
    let payload;
    try { payload = JSON.parse(data); } catch { return; }
    const set = this.handlers.get(event);
    if (!set) return;
    for (const handler of set) handler(payload);
  }
}
