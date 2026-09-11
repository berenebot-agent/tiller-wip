// Activity living pane: one shared force-layout SVG with three soft lanes
// (clients → tiller models → real-model targets). Only active routes are
// shown: nodes and legs materialize from live `activity` deltas and fade away
// FADE_MS after going quiet. No provider nodes — a leg points at the real
// model, labelled "provider/model".
//
// Legs are cubic Béziers, not straight lines. Edges that share an endpoint get
// a symmetric vertical fan so several resolutions from one key stay separate.
// Active legs are drawn in a front layer; idle legs absorb a best-effort bow
// to route around them, and a bounded barycenter pass reorders lanes only when
// independent chains are inverted. An orphan leg is never rendered: a route
// requires a client ingress and a target requires a fed route or direct client.
//
// Animation is a single activity signal: packets flow client → target while an
// `activity` delta reports the leg hot. The target roundel (node ring) owns the
// outcome — active (blue pulse) / served (green) / failed (red) / skipped
// (solid red dot, no ring) — and is only coloured by an explicit router signal:
// an `outcome` event or a terminal skip delta. Nothing is inferred from a
// missing outcome.
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

// A lit leg absent from this many consecutive snapshots is a lost-delta
// orphan: the backend no longer reports it, but its closing activity delta was
// dropped. One miss is tolerated so a just-started leg survives a snapshot
// generated before it began.
const SNAPSHOT_EVICT_AFTER = 2;

// Fan/bow geometry for curved legs. Shared-endpoint legs get symmetric
// vertical offsets; idle legs may additionally bow to avoid active legs.
const FAN_STEP = 16;
const FAN_MAX = 56;
const ROUTE_BOWS = [0, 22, -22, 44, -44];
const ROUTE_SAMPLES = 12;

let sim = null;
let linkForce = null;
let svg = null;
let gLIdle = null;
let gLActive = null;
let gN = null;
let gP = null;
let nodeSel = null;
let emptyEl = null;
let nodeClickHandler = null;
let rafId = 0;
let layoutTimer = 0;

// nodes: [{id, kind, label, sub, state, x, y, ...}] — d3 mutates x/y/fx/fy.
// Only nodes with at least one live leg exist here.
let nodes = [];
const byId = new Map();
// Catalogues for resolving deltas into nodes (never rendered directly).
const clientIndex = new Map(); // clientKeyID -> name
const routeIndex = new Map(); // routeID -> {label, sub}
// modelIndex: providerModelID -> {provider, upstream, direct} for outcome
// colouring and right-column target nodes. `direct` marks a real model that is
// itself a route (client → target, no middle lane). Unlike the old shape this
// is not keyed to a single route, because one real model can back many routes.
const modelIndex = new Map();
// coolingSet: provider_model_id (and legacy provider/upstream) keys currently
// in fallback cooldown, straight from the live snapshot. A cooled target is
// painted red immediately instead of waiting for a skip delta.
const coolingSet = new Set();
// forceLinks: [{source, target}] structural pairs for the simulation.
// EDGES: "a|b" -> {a, b, line, state, snapshotMisses, fadeTimer, removeTimer} rendered legs.
let forceLinks = [];
const EDGES = new Map();
let packets = []; // {e, t, speed, color}
const hotLegs = new Map(); // edgeKey -> active in-flight count

const W = 1180;
const H = 600;
const laneX = d => (d.kind === 'client' ? W * 0.14 : d.kind === 'route' ? W * 0.45 : W * 0.84);
const nodeRadius = d => (d.kind === 'target' ? 12 : d.kind === 'route' ? 15 : 13);
const nodeFill = d => (d.kind === 'client' ? BLUE : d.kind === 'route' ? PURP : GREEN);

function edgeKey(a, b) {
  return a.id + '|' + b.id;
}

const LABEL_WRAP = 24;
function wrapLabel(label) {
  if (!label) return [''];
  const out = [];
  let rest = label;
  while (rest.length > LABEL_WRAP) {
    const slice = rest.slice(0, LABEL_WRAP + 1);
    const cut = Math.max(slice.lastIndexOf('/'), slice.lastIndexOf('-'), slice.lastIndexOf('_'), slice.lastIndexOf(' '));
    if (cut <= 0) {
      out.push(rest.slice(0, LABEL_WRAP));
      rest = rest.slice(LABEL_WRAP);
    } else {
      out.push(rest.slice(0, cut + 1).trimEnd());
      rest = rest.slice(cut + 1).trimStart();
    }
    if (!rest) break;
  }
  if (rest) out.push(rest);
  return out;
}

function refreshEmpty() {
  if (emptyEl) emptyEl.hidden = nodes.length > 0;
}

function applyNodeState(d) {
  // A cooling target reads as skipped (solid red, no ring) unless a request is
  // actually in flight against it (bypass retry), which takes visual priority.
  const state = d.cooling && d.state !== 'active' ? 'skipped' : (d.state || 'idle');
  if (d.ring) d.ring.attr('class', 'node-ring st-' + state);
  if (d.body) d.body.attr('class', 'node-body st-' + state);
}

// The roundel is the real-model node's outcome indicator. Only explicit router
// signals (an outcome event or a skip delta) move it to served/failed/skipped.
function setNodeState(node, state) {
  if (!node || node.state === state) return;
  node.state = state;
  applyNodeState(node);
}

// ---------------------------------------------------------------------------
// Curved-edge geometry.
// ---------------------------------------------------------------------------

// controlPoints builds the cubic control points for an edge at a given bow.
// Both control points share the horizontal midpoint, so a leg with no fan/bow
// is a visually straight (or very nearly straight) curve.
function controlPoints(e, bow) {
  const dx = (e.b.x - e.a.x) * 0.5;
  return [
    { x: e.a.x, y: e.a.y },
    { x: e.a.x + dx, y: e.a.y + (e.fanA || 0) + bow },
    { x: e.b.x - dx, y: e.b.y + (e.fanB || 0) + bow },
    { x: e.b.x, y: e.b.y },
  ];
}

function cubicAt(p, t) {
  const u = 1 - t;
  const a = u * u * u;
  const b = 3 * u * u * t;
  const c = 3 * u * t * t;
  const d = t * t * t;
  return {
    x: a * p[0].x + b * p[1].x + c * p[2].x + d * p[3].x,
    y: a * p[0].y + b * p[1].y + c * p[2].y + d * p[3].y,
  };
}

function sampleCurve(p, n) {
  const out = [];
  for (let i = 0; i <= n; i++) out.push(cubicAt(p, i / n));
  return out;
}

function segmentsCross(a1, a2, b1, b2) {
  const d = (a2.x - a1.x) * (b2.y - b1.y) - (a2.y - a1.y) * (b2.x - b1.x);
  if (d === 0) return false;
  const t = ((b1.x - a1.x) * (b2.y - b1.y) - (b1.y - a1.y) * (b2.x - b1.x)) / d;
  const u = ((b1.x - a1.x) * (a2.y - a1.y) - (b1.y - a1.y) * (a2.x - a1.x)) / d;
  return t > 0 && t < 1 && u > 0 && u < 1;
}

function polylinesCross(pa, pb) {
  for (let i = 0; i + 1 < pa.length; i++) {
    for (let j = 0; j + 1 < pb.length; j++) {
      if (segmentsCross(pa[i], pa[i + 1], pb[j], pb[j + 1])) return true;
    }
  }
  return false;
}

function packetPoint(p) {
  const e = p.e;
  if (!e.p0) {
    const cp = controlPoints(e, e.bow || 0);
    e.p0 = cp[0];
    e.p1 = cp[1];
    e.p2 = cp[2];
    e.p3 = cp[3];
  }
  const t = Math.max(0, Math.min(1, p.t));
  return cubicAt([e.p0, e.p1, e.p2, e.p3], t);
}

// ---------------------------------------------------------------------------
// Layout passes: shared-endpoint fans, idle bow routing, crossing reorder.
// ---------------------------------------------------------------------------

function assignFans(groups, field, other) {
  groups.forEach(list => {
    if (list.length < 2) return;
    const sorted = list.slice().sort((x, y) => (x[other].y - y[other].y) || (x[other].id < y[other].id ? -1 : 1));
    const step = Math.min(FAN_STEP, (FAN_MAX * 2) / (sorted.length - 1));
    const mid = (sorted.length - 1) / 2;
    sorted.forEach((e, i) => { e[field] = (i - mid) * step; });
  });
}

// recomputeFans separates every leg that shares a source or a target node, so
// multiple resolutions from one key fan out instead of collapsing to a line.
function recomputeFans() {
  EDGES.forEach(e => {
    e.fanA = 0;
    e.fanB = 0;
  });
  const bySource = new Map();
  const byTarget = new Map();
  EDGES.forEach(e => {
    let s = bySource.get(e.a.id);
    if (!s) {
      s = [];
      bySource.set(e.a.id, s);
    }
    s.push(e);
    let t = byTarget.get(e.b.id);
    if (!t) {
      t = [];
      byTarget.set(e.b.id, t);
    }
    t.push(e);
  });
  assignFans(bySource, 'fanA', 'b');
  assignFans(byTarget, 'fanB', 'a');
}

// recomputeRoutes lets idle legs bow out of the way of active ones. Best-effort
// only: it never moves active legs, and caps work on edge count.
function recomputeRoutes() {
  if (!sim || EDGES.size === 0 || EDGES.size > 45) return;
  const active = [];
  EDGES.forEach(e => {
    if (e.state === 'req' || e.state === 'pending') active.push(e);
  });
  if (active.length === 0) {
    EDGES.forEach(e => { e.bow = 0; });
    return;
  }
  const activeSamples = active.map(e => sampleCurve(controlPoints(e, 0), ROUTE_SAMPLES));
  EDGES.forEach(e => {
    if (e.state === 'req' || e.state === 'pending') {
      e.bow = 0;
      return;
    }
    let best = 0;
    let bestScore = Infinity;
    for (const bow of ROUTE_BOWS) {
      const pts = sampleCurve(controlPoints(e, bow), ROUTE_SAMPLES);
      let score = Math.abs(bow) * 0.01;
      for (const ap of activeSamples) {
        if (polylinesCross(pts, ap)) score += 1;
      }
      if (score < bestScore) {
        bestScore = score;
        best = bow;
      }
    }
    e.bow = best;
  });
}

// independentCrossings returns pairs of legs that share no node yet swap
// vertical order between their endpoints — the cases that must cross.
function independentCrossings() {
  const list = Array.from(EDGES.values());
  const out = [];
  for (let i = 0; i < list.length; i++) {
    for (let j = i + 1; j < list.length; j++) {
      const e1 = list[i];
      const e2 = list[j];
      if (e1.a === e2.a || e1.a === e2.b || e1.b === e2.a || e1.b === e2.b) continue;
      if ((e1.a.y - e2.a.y) * (e1.b.y - e2.b.y) < 0) out.push([e1, e2]);
    }
  }
  return out;
}

function barycenter(n, neighborKind) {
  let sum = 0;
  let count = 0;
  EDGES.forEach(e => {
    if (e.a === n && e.b.kind === neighborKind) {
      sum += e.b.y;
      count++;
    } else if (e.b === n && e.a.kind === neighborKind) {
      sum += e.a.y;
      count++;
    }
  });
  return count ? sum / count : n.y;
}

// orderLane orders one lane by the mean y of its neighbours and anchors the
// nodes to evenly-spaced slots. Anchors live only between layout passes, so the
// force layout resumes as soon as the crossings are gone.
function orderLane(kind, neighborKind) {
  const list = nodes.filter(n => n.kind === kind && !n.dragging);
  if (list.length < 2) return;
  list.forEach(n => { n._bary = barycenter(n, neighborKind); });
  list.sort((x, y) => (x._bary - y._bary) || (x.id < y.id ? -1 : 1));
  const step = Math.min(100, (H - 140) / (list.length - 1));
  const start = H / 2 - (step * (list.length - 1)) / 2;
  list.forEach((n, i) => { n.fy = start + i * step; });
}

// resolveCrossings reorders lanes only when independent chains are actually
// inverted. Once anchored, nodes stay put (the force layout does not resume and
// drift them back into a crossing), which keeps the pane from oscillating.
function resolveCrossings() {
  if (!sim) return;
  if (independentCrossings().length === 0) return;
  orderLane('route', 'client');
  orderLane('target', 'route');
}

// scheduleLayoutPass coalesces fan/route/reorder work triggered by structural
// or activity changes. It does not run per animation frame, and the longer
// debounce keeps the pane from reshuffling under steady traffic.
function scheduleLayoutPass() {
  if (layoutTimer) return;
  layoutTimer = setTimeout(() => {
    layoutTimer = 0;
    resolveCrossings();
    recomputeFans();
    recomputeRoutes();
    renderFrame();
  }, 1200);
}

function activateNode(d) {
  if (nodeClickHandler) nodeClickHandler(d);
}

// ---------------------------------------------------------------------------
// Node / edge lifecycle.
// ---------------------------------------------------------------------------

// Rebind the simulation after nodes/edges change. The key function keeps D3
// from rebinding the wrong DOM groups when nodes come and go.
function syncSim() {
  if (!sim) return;
  sim.nodes(nodes);
  if (linkForce) linkForce.links(forceLinks);
  nodeSel = gN.selectAll('g').data(nodes, d => d.id).join(
    enter => {
      const g = enter.append('g')
        .attr('class', 'node-enter node-clickable')
        .attr('role', 'button')
        .attr('tabindex', 0)
        .attr('aria-label', d => `Open activity for ${d.label}`)
        .on('click', (e, d) => {
          e.stopPropagation();
          activateNode(d);
        })
        .on('keydown', (e, d) => {
          if (e.key !== 'Enter' && e.key !== ' ') return;
          e.preventDefault();
          activateNode(d);
        })
        .call(d3.drag()
        .clickDistance(6)
        .on('start', (e, d) => {
          if (sim) sim.alphaTarget(0.3).restart();
          d.dragging = true;
          d.fx = d.x;
          d.fy = d.y;
        })
        .on('drag', (e, d) => {
          d.fx = e.x;
          d.fy = e.y;
        })
        .on('end', (e, d) => {
          if (sim) sim.alphaTarget(0);
          d.dragging = false;
          d.fx = null;
          d.fy = null;
          scheduleLayoutPass();
        }));
      g.append('circle').attr('class', 'node-ring').attr('r', d => nodeRadius(d) + 5).attr('fill', 'none');
      g.append('circle').attr('class', 'node-body').attr('r', nodeRadius)
        .attr('fill', nodeFill).attr('stroke', '#fff').attr('stroke-width', 2);
      g.append('text').attr('class', 'node-label').attr('dy', 30).attr('text-anchor', 'middle')
        .each(function (d) {
          const text = d3.select(this);
          wrapLabel(d.label || '').forEach((line, i) => {
            text.append('tspan').attr('x', 0).attr('dy', i === 0 ? 0 : '1.2em').text(line);
          });
        });
      g.append('text').attr('class', 'node-sub').attr('dy', d => 30 + wrapLabel(d.label || '').length * 13).attr('text-anchor', 'middle')
        .text(d => d.sub || '');
      return g;
    },
    update => update,
    exit => exit.remove(),
  );
  nodeSel.each(function (d) {
    const g = d3.select(this);
    d.ring = g.select('.node-ring');
    d.body = g.select('.node-body');
  });
  nodeSel.each(applyNodeState);
  refreshEmpty();
  recomputeFans();
  // Avoid reheating the layout to a fixed high alpha for every new leg.
  sim.alpha(Math.max(sim.alpha(), 0.15)).restart();
  scheduleLayoutPass();
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

function placeEdge(e) {
  const g = e.state === 'req' || e.state === 'pending' ? gLActive : gLIdle;
  if (e.layer !== g) {
    g.node().appendChild(e.line.node());
    e.layer = g;
  }
}

function ensureEdge(a, b) {
  const key = edgeKey(a, b);
  let e = EDGES.get(key);
  if (!e) {
    const line = gLIdle.append('path').attr('class', 'edge idle');
    e = { a, b, line, state: 'idle', snapshotMisses: 0, fadeTimer: 0, removeTimer: 0, fanA: 0, fanB: 0, bow: 0, layer: gLIdle, p0: null, p1: null, p2: null, p3: null };
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

function setEdge(e, state) {
  if (state === 'req' && e.state !== 'req') e.snapshotMisses = 0;
  e.state = state;
  if (state === 'req') startEmitter(e);
  else stopEmitter(e);
  e.line.attr('class', 'edge ' + state);
  placeEdge(e);
  scheduleLayoutPass();
}

// Schedule the fade → remove lifecycle for a settled leg. A fresh delta on the
// leg cancels and restarts it via ensureEdge. Both timers are cleared on
// teardown and on re-activity so a stale removal cannot delete a live leg.
function fadeEdge(e, settleMs) {
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
function releaseLeg(e, destNode) {
  stopEmitter(e);
  setEdge(e, 'idle');
  if (destNode && destNode.state === 'active') setNodeState(destNode, 'idle');
  fadeEdge(e, FADE_MS);
}

// dropEdge removes one leg's DOM/timers/registrations without pruning nodes.
// Callers reconcile afterwards; this is what keeps pruneOrphans re-entrancy
// safe (it drops edges directly rather than recursing through removeEdge).
function dropEdge(e) {
  const key = edgeKey(e.a, e.b);
  if (!EDGES.has(key)) return false;
  stopEmitter(e);
  if (e.fadeTimer) clearTimeout(e.fadeTimer);
  if (e.removeTimer) clearTimeout(e.removeTimer);
  e.line.remove();
  EDGES.delete(key);
  packets = packets.filter(p => p.e !== e);
  forceLinks = forceLinks.filter(l => !(l.source === e.a && l.target === e.b));
  return true;
}

function removeEdge(e) {
  if (!dropEdge(e)) return;
  reconcileGraph(true);
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
  if (nodes.length === before) return false;
  const gone = new Set();
  byId.forEach((n, id) => {
    if (!used.has(id)) gone.add(id);
  });
  gone.forEach(id => byId.delete(id));
  return true;
}

// A route only exists because a client reaches it; a target only exists
// because a client reaches it directly or through a fed route. Enforcing that
// invariant is what stops an orphan purple tiller-model → real-model leg with
// no client key behind it.
function hasIngress(routeNode) {
  let found = false;
  EDGES.forEach(e => {
    if (e.b === routeNode && e.a.kind === 'client') found = true;
  });
  return found;
}

function targetIsFed(target) {
  let ok = false;
  EDGES.forEach(e => {
    if (e.b !== target) return;
    if (e.a.kind === 'client') ok = true;
    else if (e.a.kind === 'route' && hasIngress(e.a)) ok = true;
  });
  return ok;
}

function pruneOrphans() {
  let removed = false;
  let changed = true;
  let guard = 0;
  while (changed && guard++ < 100) {
    changed = false;
    const list = Array.from(EDGES.values());
    for (const e of list) {
      if (!EDGES.has(edgeKey(e.a, e.b))) continue;
      if (e.a.kind === 'route' && !hasIngress(e.a)) {
        if (dropEdge(e)) {
          changed = true;
          removed = true;
        }
        continue;
      }
      if (e.b.kind === 'route' && !hasIngress(e.b)) {
        if (dropEdge(e)) {
          changed = true;
          removed = true;
        }
        continue;
      }
      if (e.b.kind === 'target' && !targetIsFed(e.b)) {
        if (dropEdge(e)) {
          changed = true;
          removed = true;
        }
        continue;
      }
    }
  }
  return removed;
}

// reconcileGraph enforces the ingress invariant and rebinds the simulation only
// when the graph actually changed (an edge dropped, or nodes pruned). Rebinding
// restarts the force layout, so skipping it on no-op deltas keeps the pane from
// fidgeting under traffic. forceSync is set when an edge was dropped directly.
function reconcileGraph(forceSync) {
  const pruned = pruneOrphans();
  const dropped = pruneNodes();
  if (forceSync || pruned || dropped) syncSim();
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

// renderFrame advances packets and redraws positions from current node x/y.
// It is driven by the rAF loop, not by the simulation, so flow keeps moving
// after the force layout cools. sim ticks call it too for layout updates.
function renderFrame() {
  if (!sim || !gP) return;
  EDGES.forEach(e => {
    const p = controlPoints(e, e.bow || 0);
    e.p0 = p[0];
    e.p1 = p[1];
    e.p2 = p[2];
    e.p3 = p[3];
    const d = 'M ' + p[0].x + ' ' + p[0].y +
      ' C ' + p[1].x + ' ' + p[1].y + ' ' + p[2].x + ' ' + p[2].y +
      ' ' + p[3].x + ' ' + p[3].y;
    e.line.attr('d', d);
  });
  if (nodeSel) nodeSel.attr('transform', d => 'translate(' + d.x + ',' + d.y + ')');
  packets.forEach(p => {
    p.t += p.speed * p.dir;
  });
  packets = packets.filter(p => p.t < 1.15 && p.t > -0.15 && EDGES.has(edgeKey(p.e.a, p.e.b)));
  packets.forEach(p => {
    const pt = packetPoint(p);
    p.px = pt.x;
    p.py = pt.y;
  });
  gP.selectAll('circle').data(packets).join('circle')
    .attr('class', 'packet').attr('r', 3.4).attr('fill', d => d.color)
    .attr('cx', d => d.px)
    .attr('cy', d => d.py);
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
      const key = t.provider_model_id;
      const existing = modelIndex.get(key);
      modelIndex.set(key, {
        provider: t.provider_name,
        upstream: t.upstream_model_id,
        direct: existing ? existing.direct : false,
        position: t.position != null ? t.position : i + 1,
      });
    });
  });
  (data.models || []).forEach(m => {
    routeIndex.set(m.id, { label: m.canonical_model_id, sub: 'real · direct' });
    modelIndex.set(m.id, { provider: m.provider_name, upstream: m.upstream_model_id, direct: true, position: 1 });
  });
}

// Right-column node for a real model, labelled "provider/model". Virtual
// routes use it as their target; direct real routes use it as their only
// destination and skip the middle-column route node.
function ensureTargetNode(pmID) {
  const idx = modelIndex.get(pmID);
  const label = idx ? idx.provider + '/' + idx.upstream : pmID;
  const node = ensureNode('target', 't:' + pmID, label, 'real · target');
  node.cooling = coolingSet.has(pmID) || !!(idx && coolingSet.has(idx.provider + '/' + idx.upstream));
  applyNodeState(node);
  return node;
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
// unsupported) from a delta that carries client + route + target context, so
// the whole chain renders even if no ordinary activity delta was seen. A skip
// without client context is ignored rather than drawing an orphan route.
function handleSkip(delta) {
  const clientNode = delta.client_id ? resolveClientNode(delta.client_id) : null;
  if (!clientNode) return;
  const targetNode = ensureTargetNode(delta.target_id);
  const direct = !delta.id || delta.id === delta.target_id;
  let from = clientNode;
  if (!direct) {
    from = resolveRouteNode(delta.id);
    ensureEdge(clientNode, from);
  }
  const e = ensureEdge(from, targetNode);
  setEdge(e, 'idle');
  setNodeState(targetNode, 'skipped');
  fadeEdge(e, FADE_MS);
  reconcileGraph();
}

// applyActivityDelta lights legs from a live `activity` SSE delta
// ({id, client_id, target_id, active, streaming, ...}). Deltas with active > 0
// materialize nodes/legs and start flow; active < 0 releases the leg. With
// {seed:true} the leg materializes without touching hot counts
// (snapshot reseed must be idempotent).
function applyActivityDelta(delta, opts) {
  const seed = !!(opts && opts.seed);
  if (delta.client_id && delta.id) {
    const clientNode = resolveClientNode(delta.client_id);
    const idx = modelIndex.get(delta.id);
    const direct = !!idx && idx.direct;
    const destination = direct ? ensureTargetNode(delta.id) : resolveRouteNode(delta.id);
    const e = ensureEdge(clientNode, destination);
    const key = edgeKey(clientNode, destination);
    if ((delta.active || 0) > 0) {
      if (!seed) hotLegs.set(key, (hotLegs.get(key) || 0) + 1);
      setEdge(e, 'req');
      setNodeState(destination, 'active');
      flow(e, BLUE, 3);
    } else if ((delta.active || 0) < 0) {
      const left = Math.max(0, (hotLegs.get(key) || 1) - 1);
      if (left === 0) {
        hotLegs.delete(key);
        releaseLeg(e, destination);
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
    if (!hasIngress(routeNode)) return;
    const targetNode = ensureTargetNode(delta.target_id);
    const e = ensureEdge(routeNode, targetNode);
    const key = edgeKey(routeNode, targetNode);
    if ((delta.active || 0) > 0) {
      if (!seed) {
        // A second distinct target on the same route is a real fallback. The
        // earlier target's leg lingers (idle, fading) while the next begins.
        hotLegs.set(key, (hotLegs.get(key) || 0) + 1);
      }
      setEdge(e, 'req');
      setNodeState(targetNode, 'active');
      flow(e, BLUE, 3);
    } else if ((delta.active || 0) < 0) {
      const left = Math.max(0, (hotLegs.get(key) || 1) - 1);
      if (left === 0) {
        hotLegs.delete(key);
        releaseLeg(e, targetNode);
      } else {
        hotLegs.set(key, left);
      }
    }
  }
}

function onActivityDelta(delta, opts) {
  if (!sim) return;
  if (delta.result === 'skipped') {
    handleSkip(delta);
    return;
  }
  applyActivityDelta(delta, opts);
  reconcileGraph();
}

// onOutcome colours the target roundel from an explicit `outcome` SSE delta
// keyed by provider-model ID ({pmID: {is_success, result, failure_class}}).
// Served → green, failed → red, skipped → red dot. The live edge is resolved
// from the graph itself (a fed route or direct client leg), never from a
// single catalogue mapping, so a shared real model cannot revive another
// route's orphaned leg.
function onOutcome(payload) {
  if (!sim) return;
  Object.entries(payload || {}).forEach(([pmID, o]) => {
    const idx = modelIndex.get(pmID);
    if (!idx) return;
    const targetNode = byId.get('t:' + pmID);
    if (!targetNode) return;
    let chosen = null;
    EDGES.forEach(cand => {
      if (cand.b !== targetNode) return;
      const fed = cand.a.kind === 'client' || (cand.a.kind === 'route' && hasIngress(cand.a));
      if (!fed) return;
      if (cand.state === 'req' || cand.state === 'pending') chosen = cand;
      else if (!chosen) chosen = cand;
    });
    if (!chosen) return;
    if (o && o.result === 'skipped') {
      setNodeState(targetNode, 'skipped');
      return;
    }
    if (o && o.is_success) {
      targetNode.cooling = false;
      setNodeState(targetNode, 'served');
      fadeEdge(chosen, FADE_MS);
    } else {
      setNodeState(targetNode, 'failed');
      fadeEdge(chosen, FADE_MS);
    }
  });
  reconcileGraph();
}

// expectedActive returns the snapshot's live set: the client node ids with at
// least one active request, and the exact route\x00target pairs in flight. We
// match a lit leg on client presence rather than its reported route because
// the backend tracks a single route id per client, so concurrent requests on
// different routes would otherwise look stale. Direct target pairs collapse
// into the client leg and are intentionally not tracked separately.
function expectedActive(modules) {
  const clients = new Set();
  Object.entries(modules.inflight_clients || {}).forEach(([clientID, st]) => {
    if (st && st.active > 0) clients.add('c:' + clientID);
  });
  const targetPairs = new Set();
  Object.keys(modules.inflight_targets || {}).forEach(key => {
    const st = modules.inflight_targets[key];
    if (!st || !(st.active > 0)) return;
    const sep = key.indexOf('\x00');
    if (sep < 0) return;
    const routeID = key.slice(0, sep);
    const targetID = key.slice(sep + 1);
    if (routeID === targetID) return;
    targetPairs.add('r:' + routeID + '|t:' + targetID);
  });
  return { clients, targetPairs };
}

// evictStaleLegs releases lit legs the snapshot no longer reports. A dropped
// closing delta would otherwise leave a leg pulsing as active forever: the
// snapshot only re-lights live legs and would never clear it. A leg must be
// missing for SNAPSHOT_EVICT_AFTER consecutive snapshots before release, so a
// just-started leg is not torn down by a snapshot generated before it existed.
function evictStaleLegs(modules) {
  const { clients, targetPairs } = expectedActive(modules);
  const doomed = [];
  EDGES.forEach((e, key) => {
    if (e.state !== 'req' && e.state !== 'pending') {
      e.snapshotMisses = 0;
      return;
    }
    let expected;
    if (e.a.kind === 'client') expected = clients.has(e.a.id);
    else if (e.a.kind === 'route' && e.b.kind === 'target') expected = targetPairs.has(key);
    else expected = true;
    if (expected) {
      e.snapshotMisses = 0;
      return;
    }
    e.snapshotMisses = (e.snapshotMisses || 0) + 1;
    if (e.snapshotMisses >= SNAPSHOT_EVICT_AFTER) doomed.push(e);
  });
  doomed.forEach(e => {
    hotLegs.delete(edgeKey(e.a, e.b));
    releaseLeg(e, e.b);
  });
  return doomed.length > 0;
}

// onSnapshotSeed paints currently-hot legs from the snapshot envelope's
// modules (inflight_targets + inflight_clients) on (re)entry so the pane is
// correct even if deltas were missed while hidden, then evicts any lit leg the
// snapshot no longer reports so a dropped closing delta self-heals.
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
  reconcileGraph();
  if (evictStaleLegs(modules)) reconcileGraph();
}

// onCooldowns paints targets that are currently in fallback cooldown straight
// from the live snapshot, so a cooled target turns red immediately instead of
// waiting for the next request to trip a skip delta. The next snapshot clears
// it once the window expires.
function onCooldowns(map) {
  if (!sim) return;
  coolingSet.clear();
  Object.keys(map || {}).forEach(key => coolingSet.add(key));
  nodes.forEach(n => {
    if (n.kind !== 'target') return;
    const pmID = n.id.slice(2);
    const idx = modelIndex.get(pmID);
    const cooling = coolingSet.has(pmID) || !!(idx && coolingSet.has(idx.provider + '/' + idx.upstream));
    if (n.cooling !== cooling) {
      n.cooling = cooling;
      applyNodeState(n);
    }
  });
}

export function init(root, data, options = {}) {
  destroy();
  svg = root.querySelector('#activity-pane');
  emptyEl = root.querySelector('#graph-empty');
  if (!svg || typeof d3 === 'undefined') return false;
  nodeClickHandler = options.onNodeClick || null;
  gLIdle = d3.select(svg).append('g');
  gLActive = d3.select(svg).append('g');
  gN = d3.select(svg).append('g');
  gP = d3.select(svg).append('g');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 113).attr('y', 30).text('CLIENTS');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 478).attr('y', 30).text('TILLER MODELS');
  d3.select(svg).append('text').attr('class', 'col-title').attr('x', 938).attr('y', 30).text('REAL MODELS');
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
  if (layoutTimer) {
    clearTimeout(layoutTimer);
    layoutTimer = 0;
  }
  if (sim) {
    sim.stop();
    sim = null;
  }
  linkForce = null;
  if (svg) d3.select(svg).selectAll('*').remove();
  svg = gLIdle = gLActive = gN = gP = nodeSel = null;
  emptyEl = null;
  nodeClickHandler = null;
  nodes = [];
  byId.clear();
  clientIndex.clear();
  routeIndex.clear();
  modelIndex.clear();
  coolingSet.clear();
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
// onSnapshotSeed/onCooldowns, consumed directly by app.js.
export { onActivityDelta, onOutcome, onSnapshotSeed, onCooldowns };
