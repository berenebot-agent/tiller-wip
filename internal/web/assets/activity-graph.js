// Activity living pane: one shared force-layout SVG with three soft lanes
// (clients → tiller models → real-model targets). Only active routes are
// shown: nodes and legs materialize from live `activity` deltas and fade away
// FADE_MS after going quiet. No provider nodes — a leg that fails points at
// the real model that failed (label "provider/model").
//
// Animation is a single activity signal (no request/response split): packets
// flow client → target while an `activity` delta reports the leg hot, and the
// `outcome` event decides the settle colour (green served / red failed)
// before the 30s fade.
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

// One fade timer for everything: legs linger this long after last activity,
// then melt away (2s CSS transition) along with any now-unused nodes.
const FADE_MS = 30000;
const SETTLE_MS = 4000;
// A leg released with no outcome waits this long for one before assuming
// failure: skipped-only legs (cooldown/unavailable) never emit outcomes, so
// without the grace they would settle green as if served.
const OUTCOME_GRACE_MS = 3000;

let sim = null;
let linkForce = null;
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
// Only nodes with at least one live leg exist here.
let nodes = [];
const byId = new Map();
// Catalogues for resolving deltas into nodes (never rendered directly).
const clientIndex = new Map(); // clientKeyID -> name
const routeIndex = new Map(); // routeID -> {label, sub}
// modelIndex: providerModelID -> {routeID, provider, upstream} for outcome
// colouring and right-column target nodes.
const modelIndex = new Map();
// forceLinks: [{source, target}] structural pairs for the simulation.
// EDGES: "a|b" -> {a, b, line, state, meta, fadeTimer} rendered legs.
let forceLinks = [];
const EDGES = new Map();
let packets = []; // {e, t, speed, color}
let badges = []; // {e, g}
let counters = { req: 0, fb: 0, err: 0 };
const hotLegs = new Map(); // edgeKey -> active in-flight count
// pendingFallback: routeID -> {legs: [{edge, badge}], timer} groups rapid
// successive target deltas into one numbered fallback sequence.
const pendingFallback = new Map();

const W = 1180;
const H = 600;
const laneX = d => (d.kind === 'client' ? W * 0.14 : d.kind === 'route' ? W * 0.45 : W * 0.84);

function edgeKey(a, b) {
  return a.id + '|' + b.id;
}

function refreshEmpty() {
  if (emptyEl) emptyEl.hidden = nodes.length > 0;
}

// Rebind the simulation after nodes/edges change. The key function keeps D3
// from rebinding the wrong DOM groups when nodes come and go.
function syncSim() {
  if (!sim) return;
  sim.nodes(nodes);
  if (linkForce) linkForce.links(forceLinks);
  nodeSel = gN.selectAll('g').data(nodes, d => d.id).join(
    enter => {
      const g = enter.append('g').attr('class', 'node-enter').call(d3.drag()
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
      g.append('circle').attr('r', d => (d.kind === 'target' ? 12 : d.kind === 'route' ? 15 : 13))
        .attr('fill', d => (d.kind === 'client' ? BLUE : d.kind === 'route' ? PURP : GREEN))
        .attr('stroke', '#fff').attr('stroke-width', 2);
      g.append('text').attr('class', 'node-label').attr('dy', 30).attr('text-anchor', 'middle')
        .text(d => (d.label.length > 24 ? d.label.slice(0, 23) + '…' : d.label));
      g.append('text').attr('class', 'node-sub').attr('dy', 43).attr('text-anchor', 'middle')
        .text(d => d.sub || '');
      return g;
    },
    update => update,
    exit => exit.remove(),
  );
  refreshEmpty();
  sim.alpha(0.5).restart();
}

function ensureNode(kind, id, label, sub) {
  let n = byId.get(id);
  if (n) return n;
  n = { id, kind, label, sub: sub || '', x: laneX({ kind }), y: H / 2 + (Math.random() - 0.5) * 240 };
  nodes.push(n);
  byId.set(id, n);
  syncSim();
  return n;
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
    e = { a, b, line, state: 'idle', meta: null, fadeTimer: 0 };
    EDGES.set(key, e);
    forceLinks.push({ source: a, target: b });
    syncSim();
  }
  // Any activity cancels a pending fade.
  if (e.fadeTimer) {
    clearTimeout(e.fadeTimer);
    e.fadeTimer = 0;
  }
  return e;
}

function setEdge(e, state, meta) {
  e.state = state;
  if (meta !== undefined) e.meta = meta;
  if (e.graceTimer) {
    clearTimeout(e.graceTimer);
    e.graceTimer = 0;
  }
  if (state === 'req') startEmitter(e);
  else stopEmitter(e);
  e.line.attr('class', 'edge ' + state);
}

// Settle a released leg into `pending` (still breathing, outcome unknown).
// The first outcome to arrive colours it; if none arrives within
// OUTCOME_GRACE_MS the leg is assumed failed (covers skip-only legs, which
// never emit outcomes) and goes red into the shared 30s fade.
function settlePending(e, meta, label) {
  setEdge(e, 'pending', meta);
  if (e.graceTimer) clearTimeout(e.graceTimer);
  e.graceTimer = setTimeout(() => {
    e.graceTimer = 0;
    if (e.state !== 'pending') return;
    bump('err', counters.err + 1);
    feedItem('✗ ' + label + ' · no response', false);
    fadeEdge(e, 'failed', meta + ' · FAILED — fades in 30s');
  }, OUTCOME_GRACE_MS);
}

// Schedule the settle → fade → remove lifecycle for a leg. A fresh delta on
// the leg cancels and restarts it via ensureEdge.
function fadeEdge(e, settleState, settleMeta, settleMs) {
  setEdge(e, settleState, settleMeta);
  if (e.fadeTimer) clearTimeout(e.fadeTimer);
  e.fadeTimer = setTimeout(() => {
    e.fadeTimer = 0;
    e.line.attr('class', 'edge fading');
    setTimeout(() => removeEdge(e), 2100);
  }, settleMs == null ? SETTLE_MS : settleMs);
}

function removeEdge(e) {
  const key = edgeKey(e.a, e.b);
  if (!EDGES.has(key)) return;
  stopEmitter(e);
  if (e.fadeTimer) clearTimeout(e.fadeTimer);
  if (e.graceTimer) clearTimeout(e.graceTimer);
  e.line.remove();
  EDGES.delete(key);
  packets = packets.filter(p => p.e !== e);
  forceLinks = forceLinks.filter(l => !(l.source === e.a && l.target === e.b));
  pruneNodes();
  syncSim();
}

// Drop nodes left with no legs. Shared targets survive while any leg uses
// them, so a busy real model never blinks mid-traffic.
function pruneNodes() {
  const used = new Set();
  EDGES.forEach(e => {
    used.add(e.a.id);
    used.add(e.b.id);
  });
  const before = nodes.length;
  nodes = nodes.filter(n => used.has(n.id));
  if (nodes.length === before) return;
  const gone = new Set();
  byId.forEach((n, id) => {
    if (!used.has(id)) gone.add(id);
  });
  gone.forEach(id => byId.delete(id));
}

function flow(e, color, n, dir) {
  for (let k = 0; k < (n || 4); k++) {
    packets.push({ e, t: (dir || 1) === 1 ? -k * 0.22 : 1 + k * 0.22, speed: 0.014 + Math.random() * 0.008, dir: dir || 1, color });
  }
}

// Continuous flow: while a leg is hot it breathes — forward packets plus a
// lighter return trickle so "pending" reads as waiting, not dead. One
// interval per hot leg, capped so a storm cannot flood the DOM.
const FLOW_TICK_MS = 250;
const MAX_FWD = 6;
const MAX_BACK = 3;
const BACK_COLOR = '#7fb8dd';
const emitters = new Map(); // edgeKey -> interval id
function countPackets(e, dir) {
  let n = 0;
  for (const p of packets) {
    if (p.e === e && p.dir === dir) n++;
  }
  return n;
}
function startEmitter(e) {
  const key = edgeKey(e.a, e.b);
  if (emitters.has(key)) return;
  emitters.set(key, setInterval(() => {
    if (!EDGES.has(key) || (e.state !== 'req' && e.state !== 'pending')) {
      stopEmitter(e);
      return;
    }
    if (countPackets(e, 1) < MAX_FWD) packets.push({ e, t: -0.05, speed: 0.014 + Math.random() * 0.008, dir: 1, color: BLUE });
    if (countPackets(e, -1) < MAX_BACK && Math.random() < 0.6) packets.push({ e, t: 1.05, speed: 0.010 + Math.random() * 0.006, dir: -1, color: BACK_COLOR });
  }, FLOW_TICK_MS));
}
function stopEmitter(e) {
  const key = edgeKey(e.a, e.b);
  const iv = emitters.get(key);
  if (iv) {
    clearInterval(iv);
    emitters.delete(key);
  }
}
function stopAllEmitters() {
  emitters.forEach(iv => clearInterval(iv));
  emitters.clear();
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
    if (!EDGES.has(edgeKey(b.e.a, b.e.b))) return;
    b.g.attr('transform', 'translate(' + ((b.e.a.x + b.e.b.x) / 2) + ',' + ((b.e.a.y + b.e.b.y) / 2 - 14) + ')');
  });
  packets.forEach(p => {
    p.t += p.speed * p.dir;
  });
  packets = packets.filter(p => p.t < 1.15 && p.t > -0.15 && EDGES.has(edgeKey(p.e.a, p.e.b)));
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

// buildIndexes records catalogue payloads for resolving deltas into nodes.
// Nothing is rendered here: the pane starts empty and materializes legs from
// live `activity` deltas. Payload shapes are the live admin JSON views.
function buildIndexes(data) {
  clientIndex.clear();
  routeIndex.clear();
  modelIndex.clear();
  (data.clients || []).forEach(c => clientIndex.set(c.id, c.name));
  (data.virtualModels || []).forEach(v => {
    routeIndex.set(v.id, {
      label: v.canonical_model_id,
      sub: v.routing_mode === 'ordered_fallback' ? 'virtual · ordered_fallback' : 'virtual · fixed',
    });
    (v.targets || []).forEach((t, i) => {
      modelIndex.set(t.provider_model_id, { routeID: v.id, provider: t.provider_name, upstream: t.upstream_model_id, position: t.position != null ? t.position : i + 1 });
    });
  });
  (data.models || []).forEach(m => {
    routeIndex.set(m.id, { label: m.canonical_model_id, sub: 'real · direct' });
    modelIndex.set(m.id, { routeID: m.id, provider: m.provider_name, upstream: m.upstream_model_id, position: 1 });
  });
}

// Right-column node for a virtual target: the real model that was tried,
// labelled "provider/model". Direct real routes never reach here (their leg
// ends at the middle-column route node).
function ensureTargetNode(pmID) {
  const idx = modelIndex.get(pmID);
  const label = idx ? idx.provider + '/' + idx.upstream : pmID;
  return ensureNode('target', 't:' + pmID, label, 'real · target');
}

function resolveRouteNode(routeID) {
  const rec = routeIndex.get(routeID);
  if (!rec) return ensureNode('route', 'r:' + routeID, routeID, '');
  return ensureNode('route', 'r:' + routeID, rec.label, rec.sub);
}

function resolveClientNode(clientID) {
  return ensureNode('client', 'c:' + clientID, clientIndex.get(clientID) || clientID, '');
}

// onActivityDelta lights legs from a live `activity` SSE delta
// ({id, client_id, target_id, active, streaming, ...}). Deltas with
// active > 0 materialize nodes/legs and start flow; active < 0 releases the
// leg toward its pending → settle → 30s-fade lifecycle. Rapid successive
// target legs on one route are grouped into a numbered fallback sequence.
// With {seed:true} the leg materializes without touching hot counts or
// session counters (snapshot reseed must be idempotent: the matching release
// already fired or is still in flight).
function onActivityDelta(delta, opts) {
  if (!sim) return;
  const seed = !!(opts && opts.seed);
  if (delta.client_id && delta.id) {
    const clientNode = resolveClientNode(delta.client_id);
    const routeNode = resolveRouteNode(delta.id);
    const e = ensureEdge(clientNode, routeNode);
    const key = edgeKey(clientNode, routeNode);
    if ((delta.active || 0) > 0) {
      if (!seed) hotLegs.set(key, (hotLegs.get(key) || 0) + 1);
      setEdge(e, 'req', clientNode.label + ' → ' + routeNode.label);
      flow(e, BLUE, 3);
      if (!seed) bump('req', counters.req + 1);
    } else if ((delta.active || 0) < 0) {
      const left = Math.max(0, (hotLegs.get(key) || 1) - 1);
      if (left === 0) {
        hotLegs.delete(key);
        settlePending(e, clientNode.label + ' → ' + routeNode.label, clientNode.label + ' → ' + routeNode.label);
      } else {
        hotLegs.set(key, left);
      }
    }
  }
  if (delta.target_id && delta.id) {
    const routeNode = resolveRouteNode(delta.id);
    // Direct real-model routes carry ID === TargetID === provider-model ID:
    // the visible 1:1 leg is client → route (lit by the client block above),
    // so a target delta here only reinforces that leg's flow.
    if (delta.id === delta.target_id) {
      if ((delta.active || 0) > 0) {
        let live = null;
        EDGES.forEach(cand => {
          if (cand.b === routeNode && cand.state === 'req' && !live) live = cand;
        });
        if (live) flow(live, BLUE, 2);
      }
      return;
    }
    const targetNode = ensureTargetNode(delta.target_id);
    const e = ensureEdge(routeNode, targetNode);
    const key = edgeKey(routeNode, targetNode);
    if ((delta.active || 0) > 0) {
      if (!seed) hotLegs.set(key, (hotLegs.get(key) || 0) + 1);
      setEdge(e, 'req', routeNode.label + ' → ' + targetNode.label);
      flow(e, BLUE, 3);
      // Group into a fallback sequence when a route lights a second leg
      // while the first is still pending.
      let entry = pendingFallback.get(delta.id);
      if (!entry) {
        entry = { legs: [], timer: 0 };
        pendingFallback.set(delta.id, entry);
      }
      if (!entry.legs.some(l => l.edge === e)) {
        entry.legs.push({ edge: e, badge: addBadge(e, entry.legs.length + 1, 'pending') });
        if (entry.legs.length > 1) bump('fb', counters.fb + 1);
      }
      if (entry.timer) clearTimeout(entry.timer);
      entry.timer = setTimeout(() => clearBadgesFor(delta.id), FADE_MS);
    } else if ((delta.active || 0) < 0) {
      const left = Math.max(0, (hotLegs.get(key) || 1) - 1);
      if (left === 0) {
        hotLegs.delete(key);
        settlePending(e, routeNode.label + ' → ' + targetNode.label, routeNode.label + ' → ' + targetNode.label);
      } else {
        hotLegs.set(key, left);
      }
    }
  }
}

// onOutcome colours legs from an `outcome` SSE delta keyed by
// provider-model ID ({pmID: {is_success}}). Success → green + feed tick;
// failure → red, counted. Both rest into the shared 30s fade. An outcome
// for a leg with no prior delta materializes it (missed-SSE self-heal).
function onOutcome(payload) {
  if (!sim) return;
  Object.entries(payload || {}).forEach(([pmID, o]) => {
    const idx = modelIndex.get(pmID);
    if (!idx) return;
    const routeNode = resolveRouteNode(idx.routeID);
    const direct = idx.routeID === pmID;
    const targetNode = direct ? routeNode : ensureTargetNode(pmID);
    if (targetNode === routeNode && !direct) return;
    // Direct real route: the visible leg is client-agnostic here, so colour
    // the route node side via any live client→route leg, else the point.
    let e;
    if (direct) {
      let live = null;
      EDGES.forEach(cand => {
        if (cand.b === routeNode && cand.state === 'req' && !live) live = cand;
      });
      if (!live) return;
      e = live;
    } else {
      e = ensureEdge(routeNode, targetNode);
    }
    const entry = pendingFallback.get(idx.routeID);
    const leg = entry ? entry.legs.find(l => l.edge === e) : null;
    if (o && o.is_success) {
      if (leg) {
        leg.badge.g.select('circle').attr('fill', GREEN);
        leg.badge.g.select('text').text('0' + (entry.legs.indexOf(leg) + 1));
      }
      feedItem('✓ ' + routeNode.label + ' → ' + idx.provider + '/' + idx.upstream, true);
      fadeEdge(e, 'ok', (direct ? e.a.label + ' → ' : routeNode.label + ' → ') + idx.provider + '/' + idx.upstream + ' · SERVED');
      setTimeout(() => clearBadgesFor(idx.routeID), SETTLE_MS);
    } else {
      bump('err', counters.err + 1);
      if (leg) leg.badge.g.select('circle').attr('fill', RED);
      feedItem('✗ ' + routeNode.label + ' → ' + idx.provider + '/' + idx.upstream, false);
      fadeEdge(e, 'failed', (direct ? e.a.label + ' → ' : routeNode.label + ' → ') + idx.provider + '/' + idx.upstream + ' · FAILED — fades in 30s');
      setTimeout(() => clearBadgesFor(idx.routeID), SETTLE_MS);
    }
  });
}

// onSnapshotSeed paints currently-hot legs from the snapshot envelope's
// modules (inflight_targets + inflight_clients) on (re)entry so the pane is
// correct even if deltas were missed while hidden.
function onSnapshotSeed(modules) {
  if (!sim || !modules) return;
  const clients = modules.inflight_clients || {};
  Object.entries(clients).forEach(([clientID, st]) => {
    if (!st || !(st.active > 0) || !st.route_id) return;
    onActivityDelta({ id: st.route_id, client_id: clientID, active: 1 }, { seed: true });
  });
  const targets = modules.inflight_targets || {};
  Object.entries(targets).forEach(([key, st]) => {
    if (!st || !(st.active > 0)) return;
    // Snapshot keys join route and target IDs with a NUL separator
    // (mirrors targetActivityKey in app.js).
    const sep = key.indexOf('\x00');
    if (sep < 0) return;
    onActivityDelta({ id: key.slice(0, sep), target_id: key.slice(sep + 1), active: 1 }, { seed: true });
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
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 478).attr('y', 30).text('TILLER MODELS');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 938).attr('y', 30).text('REAL MODELS');
  counters = { req: 0, fb: 0, err: 0 };
  buildIndexes(data || {});
  sim = d3.forceSimulation(nodes)
    .force('link', null);
  linkForce = d3.forceLink(forceLinks).id(d => d.id).distance(170);
  sim.force('link', linkForce)
    .force('charge', d3.forceManyBody().strength(-500))
    .force('x', d3.forceX(laneX).strength(0.5))
    .force('y', d3.forceY(H / 2).strength(0.15))
    .force('collide', d3.forceCollide(56));
  nodeSel = gN.selectAll('g').data(nodes, d => d.id);
  if (tickHandler) sim.on('tick', null);
  tickHandler = tick;
  sim.on('tick', tickHandler);
  refreshEmpty();
  return true;
}

export function destroy() {
  if (sim) {
    sim.stop();
    sim = null;
  }
  linkForce = null;
  tickHandler = null;
  if (svg) d3.select(svg).selectAll('*').remove();
  svg = gL = gB = gN = gP = nodeSel = null;
  detailEl = feedEl = emptyEl = statsEls = null;
  nodes = [];
  byId.clear();
  clientIndex.clear();
  routeIndex.clear();
  forceLinks = [];
  EDGES.forEach(e => {
    if (e.fadeTimer) clearTimeout(e.fadeTimer);
    if (e.graceTimer) clearTimeout(e.graceTimer);
  });
  EDGES.clear();
  packets = [];
  badges = [];
  stopAllEmitters();
  hotLegs.clear();
  modelIndex.clear();
  pendingFallback.forEach(entry => {
    if (entry.timer) clearTimeout(entry.timer);
  });
  pendingFallback.clear();
}

// The module namespace exposes init/destroy/onActivityDelta/onOutcome/
// onSnapshotSeed, consumed directly by app.js.
export { onActivityDelta, onOutcome, onSnapshotSeed };
