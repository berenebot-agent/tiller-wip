// LiveStream is a thin wrapper over EventSource for the admin UI's live
// refresh. It exposes a subscribe-style API and manages the connection
// lifecycle: opened after login, closed on logout/pagehide, and closed while
// the tab is hidden so a background tab costs zero server queries.
//
// The server pushes three event types:
//   - "outcome": a micro-delta of per-target last request outcomes (drives the
//     resolution icons instantly, no DB cost).
//   - "activity": a transient in-flight request delta keyed by virtual model ID.
//   - "snapshot": the full usage/health envelope (drives token/cache counters
//     and reconciles any dropped delta).
export class LiveStream {
  constructor(url) {
    this.url = url;
    this.handlers = new Map();
    this.es = null;
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
    this.es = es;
    es.onmessage = (e) => this.dispatch('message', e.data);
    es.addEventListener('outcome', (e) => this.dispatch('outcome', e.data));
    es.addEventListener('activity', (e) => this.dispatch('activity', e.data));
    es.addEventListener('snapshot', (e) => this.dispatch('snapshot', e.data));
    es.onerror = () => {
      // EventSource auto-reconnects; just drop the dead reference so a later
      // open() (e.g. on tab re-show) starts a fresh connection.
      if (this.es === es) this.es = null;
    };
  }

  close() {
    if (this.es) {
      this.es.close();
      this.es = null;
    }
  }

  dispatch(event, data) {
    let payload;
    try { payload = JSON.parse(data); } catch { return; }
    const set = this.handlers.get(event);
    if (!set) return;
    for (const handler of set) handler(payload);
  }
}
