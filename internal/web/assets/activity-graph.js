// Activity living pane: one shared force-layout SVG with three soft lanes
// (clients → tiller models → real-model targets). Only active routes are
// shown: nodes and legs materialize from live `activity` deltas and fade away
// FADE_MS after going quiet. No provider nodes — a leg points at the real
// model, labelled "provider/model".
//
// Animation is a single activity signal: packets flow client → target while an
// `activity` delta reports the leg hot. The target roundel (node ring) owns the
// outcome — active (blue pulse) / served (green) / failed (red) / skipped
// (amber) — and is only coloured by an explicit router signal: an `outcome`
// event or a terminal skip delta. Nothing is inferred from a missing outcome.
//
// Rendering is driven by a requestAnimationFrame loop independent of the force
// simulation. The simulation only lays nodes out; if it cools while legs are
// still emitting packets, the loop keeps animating instead of freezing.
//
// Depends on the vendored D3 build (/d3.min.js, same-origin for the
// `script-src 'self'` CSP). The host (app.js) owns data fetching and the SSE
// subscription; this module owns the simulation, packets, roundels, feed, and
// detail bar. Call init once per view entry, destroy on nav-away.

const BLUE = '#2778b8';
const GREEN = '#25845b';
const PURP = '#7a5fb5';

// One fade timer for everything: legs linger this long after settling, then
// melt away (2s CSS transition) along with any now-unused nodes.
const FADE_MS = 30000;

let sim = null;
let linkForce = null;
let svg = null;
let gL = null;
let gN = null;
let gP = null;
let nodeSel = null;
let detailEl = null;
let feedEl = null;
let emptyEl = null;
let statsEls = null;
let rafId = 0;

// nodes: [{id, kind, label, sub, state, x, y, ...}] — d3 mutates x/y/fx/fy.
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
// EDGES: "a|b" -> {a, b, line, state, meta, fadeTimer, removeTimer} rendered legs.
let forceLinks = [];
const EDGES = new Map();
let packets = []; // {e, t, speed, color}
let counters = { req: 0, fb: 0, err: 0 };
const hotLegs = new Map(); // edgeKey -> active in-flight count

const W = 1180;
const H = 600;
const laneX = d => (d.kind === 'client' ? W * 0.14 : d.kind === 'route' ? W * 0.45 : W * 0.84);
const nodeRadius = d => (d.kind === 'target' ? 12 : d.kind === 'route' ? 15 : 13);
const nodeFill = d => (d.kind === 'client' ? BLUE : d.kind === 'route' ? PURP : GREEN);

function edgeKey(a, b) {
  return a.id + '|' + b.id;
}

function refreshEmpty() {
  if (emptyEl) emptyEl.hidden = nodes.length > 0;
}

function applyNodeState(d) {
  if (!d.ring) return;
  d.ring.attr('class', 'node-ring st-' + (d.state || 'idle'));
}

// The roundel is the real-model node's outcome indicator. Only explicit router
// signals (an outcome event or a skip delta) move it to served/failed/skipped.
function setNodeState(node, state) {
  if (!node || node.state === state) return;
  node.state = state;
  applyNodeState(node);
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
          if (sim) sim.alphaTarget(0.3).restart();
          d.fx = d.x;
          d.fy = d.y;
        })
        .on('drag', (e, d) => {
          d.fx = e.x;
          d.fy = e.y;
        })
        .on('end', (e, d) => {
          if (sim) sim.alphaTarget(0);
          d.fx = null;
          d.fy = null;
        }));
      g.append('circle').attr('class', 'node-ring').attr('r', d => nodeRadius(d) + 5).attr('fill', 'none');
      g.append('circle').attr('class', 'node-body').attr('r', nodeRadius)
        .attr('fill', nodeFill).attr('stroke', '#fff').attr('stroke-width', 2);
      g.append('text').attr('class', 'node-label').attr('dy', 30).attr('text-anchor', 'middle')
        .text(d => (d.label.length > 24 ? d.label.slice(0, 23) + '…' : d.label));
      g.append('text').attr('class', 'node-sub').attr('dy', 43).attr('text-anchor', 'middle')
        .text(d => d.sub || '');
      return g;
    },
    update => update,
    exit => exit.remove(),
  );
  nodeSel.each(function (d) {
    d.ring = d3.select(this).select('.node-ring');
  });
  nodeSel.each(applyNodeState);
  refreshEmpty();
  // Avoid reheating the layout to a fixed high alpha for every new leg.
  sim.alpha(Math.max(sim.alpha(), 0.15)).restart();
  ensureRenderLoop();
}

function ensureNode(kind, id, label, sub) {
  let n = byId.get(id);
  if (n) return n;
  n = { id, kind, label, sub: sub || '', state: 'idle', x: laneX({ kind }), y: H / 2 + (Math.random() - 0.5) * 240 };
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
    e = { a, b, line, state: 'idle', meta: null, fadeTimer: 0, removeTimer: 0 };
    EDGES.set(key, e);
    forceLinks.push({ source: a, target: b });
    syncSim();
  }
  // Any activity cancels a pending fade.
  if (e.fadeTimer) {
    clearTimeout(e.fadeTimer);
    e.fadeTimer = 0;
  }
  if (e.removeTimer) {
    clearTimeout(e.removeTimer);
    e.removeTimer = 0;
  }
  return e;
}

function setEdge(e, state, meta) {
  e.state = state;
  if (meta !== undefined) e.meta = meta;
  if (state === 'req') startEmitter(e);
  else stopEmitter(e);
  e.line.attr('class', 'edge ' + state);
}

// Schedule the fade → remove lifecycle for a settled leg. A fresh delta on the
// leg cancels and restarts it via ensureEdge. Both timers are cleared on
// teardown and on re-activity so a stale removal cannot delete a live leg.
function fadeEdge(e, meta, settleMs) {
  if (meta !== undefined) e.meta = meta;
  if (e.fadeTimer) clearTimeout(e.fadeTimer);
  e.fadeTimer = setTimeout(() => {
    e.fadeTimer = 0;
    e.line.attr('class', 'edge fading');
    if (e.removeTimer) clearTimeout(e.removeTimer);
    e.removeTimer = setTimeout(() => {
      e.removeTimer = 0;
      removeEdge(e);
    }, 2100);
  }, settleMs == null ? FADE_MS : settleMs);
}

// A leg released without an explicit outcome is not an inferred failure: stop
// the flow, drop the roundel back to idle, and let the shared fade take it.
// An explicit outcome arriving later recolours the roundel.
function releaseLeg(e, destNode, label) {
  stopEmitter(e);
  setEdge(e, 'idle', label);
  if (destNode && destNode.state === 'active') setNodeState(destNode, 'idle');
  fadeEdge(e, label, FADE_MS);
}

function removeEdge(e) {
  const key = edgeKey(e.a, e.b);
  if (!EDGES.has(key)) return;
  stopEmitter(e);
  if (e.fadeTimer) clearTimeout(e.fadeTimer);
  if (e.removeTimer) clearTimeout(e.removeTimer);
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
    addPacket({ e, t: (dir || 1) === 1 ? -k * 0.22 : 1 + k * 0.22, speed: 0.014 + Math.random() * 0.008, dir: dir || 1, color });
  }
  ensureRenderLoop();
}

// Continuous flow: while a leg is hot it breathes — forward packets plus a
// lighter return trickle. One interval per hot leg, capped so a storm cannot
// flood the DOM.
const FLOW_TICK_MS = 250;
const MAX_FWD = 6;
const MAX_BACK = 3;
const MAX_PACKETS = 240;
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
    if (!EDGES.has(key) || e.state !== 'req') {
      stopEmitter(e);
      return;
    }
    if (countPackets(e, 1) < MAX_FWD) addPacket({ e, t: -0.05, speed: 0.014 + Math.random() * 0.008, dir: 1, color: BLUE });
    if (countPackets(e, -1) < MAX_BACK && Math.random() < 0.6) addPacket({ e, t: 1.05, speed: 0.010 + Math.random() * 0.006, dir: -1, color: BACK_COLOR });
    ensureRenderLoop();
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

function addPacket(packet) {
  if (packets.length >= MAX_PACKETS) return;
  packets.push(packet);
}

function bump(which, v) {
  counters[which] = v;
  if (statsEls && statsEls[which]) statsEls[which].textContent = v;
}

function showDetail(html) {
  if (detailEl) detailEl.innerHTML = '<code>' + html + '</code>';
}

function feedItem(text, kind) {
  if (!feedEl) return;
  const el = document.createElement('span');
  el.className = 'feed-item ' + (kind || 'warn');
  el.textContent = text;
  feedEl.prepend(el);
  while (feedEl.children.length > 6) feedEl.lastChild.remove();
  setTimeout(() => {
    el.style.transition = 'opacity .6s';
    el.style.opacity = 0;
    setTimeout(() => el.remove(), 650);
  }, 9000);
}

// renderFrame advances packets and redraws positions from current node x/y.
// It is driven by the rAF loop, not by the simulation, so flow keeps moving
// after the force layout cools. sim ticks call it too for layout updates.
function renderFrame() {
  if (!sim || !gP) return;
  EDGES.forEach(e => {
    e.line.attr('x1', e.a.x).attr('y1', e.a.y).attr('x2', e.b.x).attr('y2', e.b.y);
  });
  if (nodeSel) nodeSel.attr('transform', d => 'translate(' + d.x + ',' + d.y + ')');
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

function renderLoop() {
  rafId = 0;
  if (!sim) return;
  renderFrame();
  if (packets.length > 0 || emitters.size > 0) {
    rafId = requestAnimationFrame(renderLoop);
  }
}

function ensureRenderLoop() {
  if (rafId || !sim) return;
  rafId = requestAnimationFrame(renderLoop);
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

// Right-column node for a real model, labelled "provider/model". Virtual
// routes use it as their target; direct real routes use it as their only
// destination and skip the middle-column route node.
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

// handleSkip paints an explicit terminal skip (cooldown / unavailable /
// unsupported) from a delta that carries full client + route + target context,
// so the whole chain renders even if no ordinary activity delta was seen.
function handleSkip(delta) {
  const clientNode = delta.client_id ? resolveClientNode(delta.client_id) : null;
  const targetNode = ensureTargetNode(delta.target_id);
  const direct = !delta.id || delta.id === delta.target_id;
  let from = clientNode;
  if (!direct) {
    const routeNode = resolveRouteNode(delta.id);
    if (clientNode) ensureEdge(clientNode, routeNode);
    from = routeNode;
  }
  if (!from) return;
  const e = ensureEdge(from, targetNode);
  const label = from.label + ' → ' + targetNode.label;
  const reason = delta.failure_class ? ' (' + delta.failure_class + ')' : '';
  setEdge(e, 'idle', label);
  setNodeState(targetNode, 'skipped');
  feedItem('⚠ ' + label + ' · skipped' + reason, 'warn');
  fadeEdge(e, label + ' · SKIPPED' + reason, FADE_MS);
}

// onActivityDelta lights legs from a live `activity` SSE delta
// ({id, client_id, target_id, active, streaming, ...}). Deltas with active > 0
// materialize nodes/legs and start flow; active < 0 releases the leg. A delta
// with result: "skipped" is a terminal skip with full context. With
// {seed:true} the leg materializes without touching hot counts or counters
// (snapshot reseed must be idempotent).
function onActivityDelta(delta, opts) {
  if (!sim) return;
  if (delta.result === 'skipped') {
    handleSkip(delta);
    return;
  }
  const seed = !!(opts && opts.seed);
  if (delta.client_id && delta.id) {
    const clientNode = resolveClientNode(delta.client_id);
    const direct = modelIndex.get(delta.id)?.routeID === delta.id;
    const destination = direct ? ensureTargetNode(delta.id) : resolveRouteNode(delta.id);
    const e = ensureEdge(clientNode, destination);
    const key = edgeKey(clientNode, destination);
    const label = clientNode.label + ' → ' + destination.label;
    if ((delta.active || 0) > 0) {
      if (!seed) hotLegs.set(key, (hotLegs.get(key) || 0) + 1);
      setEdge(e, 'req', label);
      setNodeState(destination, 'active');
      flow(e, BLUE, 3);
      if (!seed) bump('req', counters.req + 1);
    } else if ((delta.active || 0) < 0) {
      const left = Math.max(0, (hotLegs.get(key) || 1) - 1);
      if (left === 0) {
        hotLegs.delete(key);
        releaseLeg(e, destination, label);
      } else {
        hotLegs.set(key, left);
      }
    }
  }
  if (delta.target_id && delta.id) {
    // Direct real-model routes carry ID === TargetID === provider-model ID:
    // the visible 1:1 leg is client → real target (lit by the client block
    // above), so a target delta here only reinforces that leg's flow.
    if (delta.id === delta.target_id) {
      if ((delta.active || 0) > 0) {
        let live = null;
        EDGES.forEach(cand => {
          if (cand.b.id === 't:' + delta.target_id && cand.state === 'req' && !live) live = cand;
        });
        if (live) flow(live, BLUE, 2);
      }
      return;
    }
    const routeNode = byId.get('r:' + delta.id);
    if (!routeNode) return;
    let hasIngress = false;
    EDGES.forEach(cand => {
      if (cand.b === routeNode && cand.a.kind === 'client') hasIngress = true;
    });
    if (!hasIngress) return;
    const targetNode = ensureTargetNode(delta.target_id);
    const e = ensureEdge(routeNode, targetNode);
    const key = edgeKey(routeNode, targetNode);
    const label = routeNode.label + ' → ' + targetNode.label;
    if ((delta.active || 0) > 0) {
      if (!seed) {
        // A second distinct target on the same route is a real fallback. The
        // earlier target's leg lingers (idle, fading) while the next begins.
        let priorTargets = 0;
        EDGES.forEach(cand => {
          if (cand.a === routeNode && cand.b.kind === 'target' && cand.b !== targetNode) priorTargets++;
        });
        if (priorTargets > 0) bump('fb', counters.fb + 1);
        hotLegs.set(key, (hotLegs.get(key) || 0) + 1);
      }
      setEdge(e, 'req', label);
      setNodeState(targetNode, 'active');
      flow(e, BLUE, 3);
    } else if ((delta.active || 0) < 0) {
      const left = Math.max(0, (hotLegs.get(key) || 1) - 1);
      if (left === 0) {
        hotLegs.delete(key);
        releaseLeg(e, targetNode, label);
      } else {
        hotLegs.set(key, left);
      }
    }
  }
}

// onOutcome colours the target roundel from an explicit `outcome` SSE delta
// keyed by provider-model ID ({pmID: {is_success, result, failure_class}}).
// Served → green, failed → red, skipped → amber. Outcomes without a matching
// live leg are ignored so a delayed aggregate cannot create an orphan node.
function onOutcome(payload) {
  if (!sim) return;
  Object.entries(payload || {}).forEach(([pmID, o]) => {
    const idx = modelIndex.get(pmID);
    if (!idx) return;
    const direct = idx.routeID === pmID;
    const targetNode = byId.get('t:' + pmID);
    if (!targetNode) return;
    let e = null;
    if (direct) {
      EDGES.forEach(cand => {
        if (!e && cand.b === targetNode && (cand.state === 'req' || cand.state === 'idle')) e = cand;
      });
    } else {
      const routeNode = byId.get('r:' + idx.routeID);
      if (!routeNode) return;
      e = EDGES.get(edgeKey(routeNode, targetNode));
    }
    if (!e) return;
    const route = e.a.label + ' → ' + idx.provider + '/' + idx.upstream;
    if (o && o.result === 'skipped') {
      setNodeState(targetNode, 'skipped');
      return;
    }
    if (o && o.is_success) {
      setNodeState(targetNode, 'served');
      feedItem('✓ ' + route, 'ok');
      fadeEdge(e, route + ' · SERVED', FADE_MS);
    } else {
      setNodeState(targetNode, 'failed');
      bump('err', counters.err + 1);
      feedItem('✗ ' + route + ' · ' + (o?.failure_class || 'failed'), 'fail');
      fadeEdge(e, route + ' · FAILED', FADE_MS);
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
  sim.on('tick', renderFrame);
  refreshEmpty();
  return true;
}

export function destroy() {
  if (rafId) {
    cancelAnimationFrame(rafId);
    rafId = 0;
  }
  if (sim) {
    sim.stop();
    sim = null;
  }
  linkForce = null;
  if (svg) d3.select(svg).selectAll('*').remove();
  svg = gL = gN = gP = nodeSel = null;
  detailEl = feedEl = emptyEl = statsEls = null;
  nodes = [];
  byId.clear();
  clientIndex.clear();
  routeIndex.clear();
  modelIndex.clear();
  forceLinks = [];
  EDGES.forEach(e => {
    if (e.fadeTimer) clearTimeout(e.fadeTimer);
    if (e.removeTimer) clearTimeout(e.removeTimer);
  });
  EDGES.clear();
  packets = [];
  stopAllEmitters();
  hotLegs.clear();
}

// The module namespace exposes init/destroy/onActivityDelta/onOutcome/
// onSnapshotSeed, consumed directly by app.js.
export { onActivityDelta, onOutcome, onSnapshotSeed };
