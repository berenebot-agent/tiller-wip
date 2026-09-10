// Activity living pane: one shared force-layout SVG, three pinned columns
// (clients → tiller models → providers). Grey legs are configured but
// inactive; they light up while carrying a request.
//
// Animation is a single activity signal (no request/response split): packets
// flow client → provider while an `activity` delta reports the leg hot, and
// the `outcome` event decides the settle colour (green served / red failed)
// before the leg fades back to idle grey.
//
// Depends on the vendored D3 build (/d3.min.js, same-origin for the
// `script-src 'self'` CSP). The host (app.js) owns data fetching and the SSE
// subscription; this module owns the simulation, packets, badges, feed, and
// detail bar. Call init once per view entry, destroy on nav-away.

const BLUE = '#2778b8';
const GREEN = '#25845b';
const RED = '#b5403c';
const AMBER = '#a66312';
const PURP = '#7a5fb5';

// Cap real-model nodes so a large catalogue cannot blow up the simulation.
// Virtuals are uncapped (they are user-created and few); providers uncapped.
const MAX_REAL_NODES = 100;
const IDLE_REST_MS = 3500;
const FAILED_REST_MS = 4500;

let sim = null;
let svg = null;
let gL = null;
let gB = null;
let gN = null;
let gP = null;
let nodeSel = null;
let detailEl = null;
let feedEl = null;
let emptyEl = null;
let statsEls = null;
let tickHandler = null;

// nodes: [{id, kind, label, sub, x, y, ...}] — d3 mutates x/y/fx/fy.
let nodes = [];
const byId = new Map();
// forceLinks: [{source, target}] structural pairs for the simulation.
// EDGES: "a|b" -> {a, b, line, state, meta, restTimer} rendered legs.
let forceLinks = [];
const EDGES = new Map();
let packets = []; // {e, t, speed, color}
let badges = []; // {e, g}
let counters = { req: 0, fb: 0, err: 0 };
let hotLegs = new Map(); // edgeKey -> active count (drives packet emission)
// modelIndex: providerModelID -> {routeID, provider} for outcome colouring.
const modelIndex = new Map();
// pendingFallback: routeID -> {legs: [{edge, num}], timer} groups rapid
// successive target deltas into one numbered fallback sequence.
const pendingFallback = new Map();

function edgeKey(a, b) {
  return a.id + '|' + b.id;
}

function ensureEdge(a, b) {
  const key = edgeKey(a, b);
  let e = EDGES.get(key);
  if (!e) {
    const line = gL.append('line').attr('class', 'edge idle').style('cursor', 'pointer');
    line.on('click', () => {
      const cur = EDGES.get(key);
      if (cur && cur.meta) showDetail(cur.meta);
    });
    line.append('title').text(a.label + ' → ' + b.label);
    e = { a, b, line, state: 'idle', meta: null, restTimer: 0 };
    EDGES.set(key, e);
  }
  return e;
}

function setEdge(e, state, meta) {
  e.state = state;
  if (meta !== undefined) e.meta = meta;
  if (e.restTimer) {
    clearTimeout(e.restTimer);
    e.restTimer = 0;
  }
  e.line.attr('class', 'edge ' + state);
}

function restEdge(e, delay) {
  if (e.restTimer) clearTimeout(e.restTimer);
  e.restTimer = setTimeout(() => {
    e.restTimer = 0;
    if (e.state !== 'idle') setEdge(e, 'idle');
  }, delay == null ? IDLE_REST_MS : delay);
}

function flow(e, color, n) {
  for (let k = 0; k < (n || 4); k++) {
    packets.push({ e, t: -k * 0.22, speed: 0.014 + Math.random() * 0.008, color });
  }
}

function addBadge(e, num, kind) {
  const g = gB.append('g');
  g.append('circle').attr('class', 'try-badge-bg').attr('r', 9)
    .attr('fill', kind === 'failed' ? RED : kind === 'skipped' ? AMBER : GREEN);
  g.append('text').attr('class', 'try-badge').attr('dy', 3.5).text('0' + num);
  const b = { e, g };
  badges.push(b);
  return b;
}

function clearBadgesFor(routeID) {
  const entry = pendingFallback.get(routeID);
  if (!entry) return;
  if (entry.timer) clearTimeout(entry.timer);
  entry.legs.forEach(l => l.badge.g.remove());
  badges = badges.filter(b => !entry.legs.some(l => l.badge === b));
  pendingFallback.delete(routeID);
}

function bump(which, v) {
  counters[which] = v;
  if (statsEls && statsEls[which]) statsEls[which].textContent = v;
}

function showDetail(html) {
  if (detailEl) detailEl.innerHTML = '<code>' + html + '</code>';
}

function feedItem(text, ok) {
  if (!feedEl) return;
  const el = document.createElement('span');
  el.className = 'feed-item ' + (ok ? 'ok' : 'fail');
  el.textContent = text;
  feedEl.prepend(el);
  while (feedEl.children.length > 6) feedEl.lastChild.remove();
  setTimeout(() => {
    el.style.transition = 'opacity .6s';
    el.style.opacity = 0;
    setTimeout(() => el.remove(), 650);
  }, 9000);
}

function tick() {
  EDGES.forEach(e => {
    e.line.attr('x1', e.a.x).attr('y1', e.a.y).attr('x2', e.b.x).attr('y2', e.b.y);
  });
  if (nodeSel) nodeSel.attr('transform', d => 'translate(' + d.x + ',' + d.y + ')');
  badges.forEach(b => {
    b.g.attr('transform', 'translate(' + ((b.e.a.x + b.e.b.x) / 2) + ',' + ((b.e.a.y + b.e.b.y) / 2 - 14) + ')');
  });
  packets.forEach(p => {
    p.t += p.speed;
  });
  packets = packets.filter(p => p.t < 1.15);
  gP.selectAll('circle').data(packets).join('circle')
    .attr('class', 'packet').attr('r', 3.4).attr('fill', d => d.color)
    .attr('cx', d => {
      const t = Math.max(0, Math.min(1, d.t));
      return d.e.a.x + (d.e.b.x - d.e.a.x) * t;
    })
    .attr('cy', d => {
      const t = Math.max(0, Math.min(1, d.t));
      return d.e.a.y + (d.e.b.y - d.e.a.y) * t;
    });
}

// renderTopology builds nodes + idle edges from catalogue payloads plus a
// seed of recent activity rows (so configured legs exist as grey edges on
// first paint). Payload shapes are the live admin JSON views.
function renderTopology(data) {
  nodes = [];
  byId.clear();
  forceLinks = [];
  modelIndex.clear();

  const push = n => {
    nodes.push(n);
    byId.set(n.id, n);
  };
  (data.clients || []).forEach(c => push({ id: 'c:' + c.id, kind: 'client', label: c.name, refID: c.id }));
  (data.virtualModels || []).forEach(v => {
    push({ id: 'r:' + v.id, kind: 'route', label: v.canonical_model_id, sub: v.routing_mode === 'ordered_fallback' ? 'virtual · ordered_fallback' : 'virtual · fixed', refID: v.id, routingMode: v.routing_mode });
    (v.targets || []).forEach((t, i) => {
      modelIndex.set(t.provider_model_id, { routeID: v.id, provider: t.provider_name, upstream: t.upstream_model_id, position: t.position != null ? t.position : i + 1 });
    });
  });
  const reals = (data.models || []).slice(0, MAX_REAL_NODES);
  reals.forEach(m => {
    push({ id: 'r:' + m.id, kind: 'route', label: m.canonical_model_id, sub: 'real · direct', refID: m.id });
    modelIndex.set(m.id, { routeID: m.id, provider: m.provider_name, upstream: m.upstream_model_id, position: 1 });
  });
  (data.providers || []).forEach(p => push({ id: 'p:' + p.id, kind: 'provider', label: p.name, refID: p.id }));

  const providerByName = new Map((data.providers || []).map(p => [p.name, p]));
  const link = (aID, bID) => {
    const a = byId.get(aID);
    const b = byId.get(bID);
    if (!a || !b) return;
    forceLinks.push({ source: a, target: b });
    ensureEdge(a, b);
  };
  // Virtual targets: route → provider legs from the stored target order.
  (data.virtualModels || []).forEach(v => {
    (v.targets || []).forEach(t => {
      const p = (data.providers || []).find(x => x.id === t.provider_id) || providerByName.get(t.provider_name);
      if (p) link('r:' + v.id, 'p:' + p.id);
    });
  });
  // Real models: route → provider legs from the catalogue join.
  reals.forEach(m => {
    if (m.provider_id) link('r:' + m.id, 'p:' + m.provider_id);
  });
  // Client → route legs from recent activity (route_model_id is the stable
  // key; legacy rows fall back to the canonical route_model name).
  const routeByCanonical = new Map();
  (data.virtualModels || []).forEach(v => routeByCanonical.set(v.canonical_model_id, v.id));
  reals.forEach(m => routeByCanonical.set(m.canonical_model_id, m.id));
  (data.activity || []).forEach(row => {
    let routeID = row.route_model_id || null;
    if (!routeID && row.route_model) routeID = routeByCanonical.get(row.route_model) || null;
    if (!routeID || !row.client_key_id) return;
    link('c:' + row.client_key_id, 'r:' + routeID);
  });

  // (Re)build the simulation and node selection.
  if (sim) sim.stop();
  const W = 1180;
  const H = 600;
  sim = d3.forceSimulation(nodes)
    .force('link', d3.forceLink(forceLinks).id(d => d.id).distance(170))
    .force('charge', d3.forceManyBody().strength(-500))
    .force('x', d3.forceX(d => (d.kind === 'client' ? W * 0.14 : d.kind === 'route' ? W * 0.5 : W * 0.86)).strength(0.5))
    .force('y', d3.forceY(H / 2).strength(0.15))
    .force('collide', d3.forceCollide(56));
  gN.selectAll('*').remove();
  nodeSel = gN.selectAll('g').data(nodes).join('g').call(d3.drag()
    .on('start', (e, d) => {
      sim.alphaTarget(0.3).restart();
      d.fx = d.x;
      d.fy = d.y;
    })
    .on('drag', (e, d) => {
      d.fx = e.x;
      d.fy = e.y;
    })
    .on('end', (e, d) => {
      sim.alphaTarget(0);
      d.fx = null;
      d.fy = null;
    }));
  nodeSel.append('circle').attr('r', d => (d.kind === 'route' ? 15 : 13))
    .attr('fill', d => (d.kind === 'client' ? BLUE : d.kind === 'route' ? PURP : GREEN))
    .attr('stroke', '#fff').attr('stroke-width', 2);
  nodeSel.append('text').attr('class', 'node-label').attr('dy', 30).attr('text-anchor', 'middle')
    .text(d => (d.label.length > 24 ? d.label.slice(0, 23) + '…' : d.label));
  nodeSel.append('text').attr('class', 'node-sub').attr('dy', 43).attr('text-anchor', 'middle')
    .text(d => d.sub || '');
  if (tickHandler) sim.on('tick', null);
  tickHandler = tick;
  sim.on('tick', tickHandler);

  if (emptyEl) emptyEl.hidden = nodes.length > 0;
}

// onActivityDelta lights legs from a live `activity` SSE delta
// ({id, client_id, target_id, active, streaming, ...}). Deltas with
// active > 0 start/refresh flow; active < 0 releases the leg back to idle.
// Rapid successive target legs on one route are grouped into a numbered
// fallback sequence.
function onActivityDelta(delta) {
  if (!sim) return;
  const routeNode = delta.id ? byId.get('r:' + delta.id) : null;
  if (delta.client_id && routeNode) {
    const clientNode = byId.get('c:' + delta.client_id);
    if (clientNode) {
      const e = ensureEdge(clientNode, routeNode);
      if ((delta.active || 0) > 0) {
        setEdge(e, 'req', clientNode.label + ' → ' + routeNode.label);
        flow(e, BLUE, 3);
        bump('req', counters.req + 1);
      } else if ((delta.active || 0) < 0) {
        restEdge(e, IDLE_REST_MS);
      }
    }
  }
  if (delta.target_id && routeNode) {
    // TargetID is a provider-model ID for virtual targets; for direct
    // real-model routes ID === TargetID === provider-model ID (1:1 leg).
    const idx = modelIndex.get(delta.target_id);
    let providerNode = null;
    if (idx) {
      for (const n of nodes) {
        if (n.kind === 'provider' && n.label === idx.provider) {
          providerNode = n;
          break;
        }
      }
    } else {
      // Fallback: match "provider/model" shaped IDs by provider prefix.
      const slash = delta.target_id.indexOf('/');
      const pname = slash > 0 ? delta.target_id.slice(0, slash) : delta.target_id;
      for (const n of nodes) {
        if (n.kind === 'provider' && (n.label === pname || n.refID === delta.target_id)) {
          providerNode = n;
          break;
        }
      }
    }
    if (!providerNode) return;
    const e = ensureEdge(routeNode, providerNode);
    if ((delta.active || 0) > 0) {
      setEdge(e, 'req', routeNode.label + ' → ' + providerNode.label);
      flow(e, BLUE, 3);
      // Group into a fallback sequence when a route lights a second leg
      // while the first is still pending.
      let entry = pendingFallback.get(routeNode.refID);
      if (!entry) {
        entry = { legs: [], timer: 0 };
        pendingFallback.set(routeNode.refID, entry);
      }
      if (!entry.legs.some(l => l.edge === e)) {
        entry.legs.push({ edge: e, badge: addBadge(e, entry.legs.length + 1, 'pending') });
        if (entry.legs.length > 1) bump('fb', counters.fb + 1);
      }
      if (entry.timer) clearTimeout(entry.timer);
      entry.timer = setTimeout(() => clearBadgesFor(routeNode.refID), FAILED_REST_MS);
    } else if ((delta.active || 0) < 0) {
      const key = edgeKey(routeNode, providerNode);
      if (!hotLegs.has(key)) restEdge(e, IDLE_REST_MS);
    }
  }
}

// onOutcome colours legs from an `outcome` SSE delta keyed by
// provider-model ID ({pmID: {is_success}}). Success → green + feed tick;
// failure → red, counted, then both rest back to idle grey.
function onOutcome(payload) {
  if (!sim) return;
  Object.entries(payload || {}).forEach(([pmID, o]) => {
    const idx = modelIndex.get(pmID);
    if (!idx) return;
    const routeNode = byId.get('r:' + idx.routeID);
    if (!routeNode) return;
    let providerNode = null;
    for (const n of nodes) {
      if (n.kind === 'provider' && n.label === idx.provider) {
        providerNode = n;
        break;
      }
    }
    if (!providerNode) return;
    const e = ensureEdge(routeNode, providerNode);
    const entry = pendingFallback.get(routeNode.refID);
    const leg = entry ? entry.legs.find(l => l.edge === e) : null;
    if (o && o.is_success) {
      setEdge(e, 'ok', routeNode.label + ' → ' + providerNode.label + ' · SERVED');
      if (leg) {
        leg.badge.g.select('circle').attr('fill', GREEN);
        leg.badge.g.select('text').text('0' + (entry.legs.indexOf(leg) + 1));
      }
      feedItem('✓ ' + routeNode.label + ' → ' + idx.provider + '/' + idx.upstream, true);
      restEdge(e, IDLE_REST_MS);
      setTimeout(() => clearBadgesFor(routeNode.refID), IDLE_REST_MS);
    } else {
      bump('err', counters.err + 1);
      setEdge(e, 'failed', routeNode.label + ' → ' + providerNode.label + ' · NO RESPONSE — leg inactive');
      if (leg) leg.badge.g.select('circle').attr('fill', RED);
      feedItem('✗ ' + routeNode.label + ' → ' + idx.provider + '/' + idx.upstream, false);
      restEdge(e, FAILED_REST_MS);
      setTimeout(() => clearBadgesFor(routeNode.refID), FAILED_REST_MS);
    }
  });
}

// onSnapshotSeed paints currently-hot legs from the snapshot envelope's
// modules (inflight / inflight_clients / inflight_targets) on (re)entry so
// the pane is correct even if deltas were missed while hidden.
function onSnapshotSeed(modules) {
  if (!sim || !modules) return;
  const targets = modules.inflight_targets || {};
  Object.entries(targets).forEach(([key, st]) => {
    if (!st || !(st.active > 0)) return;
    // Snapshot keys join route and target IDs with a NUL separator
    // (mirrors targetActivityKey in app.js).
    const sep = key.indexOf('\x00');
    if (sep < 0) return;
    onActivityDelta({ id: key.slice(0, sep), target_id: key.slice(sep + 1), active: 1 });
  });
}

export function init(root, data) {
  destroy();
  svg = root.querySelector('#activity-pane');
  detailEl = root.querySelector('#graph-detail');
  feedEl = root.querySelector('#graph-feed');
  emptyEl = root.querySelector('#graph-empty');
  statsEls = {
    req: root.querySelector('#graph-req'),
    fb: root.querySelector('#graph-fb'),
    err: root.querySelector('#graph-err'),
  };
  if (!svg || typeof d3 === 'undefined') return false;
  gL = d3.select(svg).append('g');
  gB = d3.select(svg).append('g');
  gN = d3.select(svg).append('g');
  gP = d3.select(svg).append('g');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 113).attr('y', 30).text('CLIENTS');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 538).attr('y', 30).text('TILLER MODELS');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 962).attr('y', 30).text('PROVIDERS');
  counters = { req: 0, fb: 0, err: 0 };
  renderTopology(data || {});
  return true;
}

export function destroy() {
  if (sim) {
    sim.stop();
    sim = null;
  }
  tickHandler = null;
  if (svg) d3.select(svg).selectAll('*').remove();
  svg = gL = gB = gN = gP = nodeSel = null;
  detailEl = feedEl = emptyEl = statsEls = null;
  nodes = [];
  byId.clear();
  forceLinks = [];
  EDGES.clear();
  packets = [];
  badges = [];
  hotLegs = new Map();
  modelIndex.clear();
  pendingFallback.forEach(entry => {
    if (entry.timer) clearTimeout(entry.timer);
  });
  pendingFallback.clear();
}

// The module namespace exposes init/destroy/onActivityDelta/onOutcome/
// onSnapshotSeed, consumed directly by app.js.
export { onActivityDelta, onOutcome, onSnapshotSeed };
