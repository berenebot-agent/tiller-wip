package server

import "sync"

type inflightState struct {
	Active         int    `json:"active"`
	Streaming      int    `json:"streaming"`
	RequestedModel string `json:"requested_model,omitempty"`
	ResolvedModel  string `json:"resolved_model,omitempty"`
	// ClientID and RouteID are populated on composite (client+route) states so
	// live consumers can anchor a specific leg without parsing the map key.
	// They are empty on per-client aggregate and per-target states.
	ClientID string `json:"client_id,omitempty"`
	RouteID  string `json:"route_id,omitempty"`
}

type inflightDelta struct {
	ID             string `json:"id,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	TargetID       string `json:"target_id,omitempty"`
	Active         int    `json:"active"`
	Streaming      int    `json:"streaming"`
	RequestedModel string `json:"requested_model,omitempty"`
	ResolvedModel  string `json:"resolved_model,omitempty"`
	// Result marks an explicit terminal signal. Today it is set to "skipped"
	// for a target the router declined to call (cooldown, unavailable,
	// unsupported feature, etc.). It carries full client + route + target
	// context so consumers can render the leg without inference. Empty for
	// ordinary in-flight activity deltas.
	Result string `json:"result,omitempty"`
	// FailureClass accompanies Result and names the reason (e.g. "cooldown").
	FailureClass string `json:"failure_class,omitempty"`
}

// Single-ticket liveness: each in-flight request hangs one ticket, tracked in
// clientStates keyed by account + client key id + route id. Parallel requests
// from one client key are normal, so a ticket must be per (client, route):
// keying by client alone would let a second route overwrite the first route
// identity even though both remain active. Target legs keep their own map for
// per-attempt fallback granularity. All keys are account-scoped so a snapshot
// for one tenant can never include another's activity.
type inflightTracker struct {
	mu           sync.Mutex
	clientStates map[string]inflightState // keyed by account + NUL + client key id + NUL + route id
	targetStates map[string]inflightState // keyed by account + NUL + route id + NUL + provider model
	emit         func(accountID string, delta inflightDelta)
}

// inflightClientKey joins the identities a client ticket carries. An empty
// routeID collapses to the account+client key, matching the pre-resolution
// case.
func inflightClientKey(accountID, id, routeID string) string {
	return accountID + "\x00" + id + "\x00" + routeID
}

func inflightTargetKey(accountID, routeID, targetID string) string {
	return accountID + "\x00" + routeID + "\x00" + targetID
}

// clientStart begins client+route level tracking. routeID is the resolved route
// (virtual or real-model ID); it is echoed as the delta ID so live consumers
// see client and route identity together. Empty when resolution has not
// happened yet (delta keeps today's client-only shape).
func (t *inflightTracker) clientStart(accountID, id, routeID, requestedModel string) {
	t.mu.Lock()
	key := inflightClientKey(accountID, id, routeID)
	state := t.clientStates[key]
	state.Active++
	state.RequestedModel = requestedModel
	state.ClientID = id
	state.RouteID = routeID
	t.clientStates[key] = state
	t.mu.Unlock()
	t.emit(accountID, inflightDelta{ID: routeID, ClientID: id, Active: 1, RequestedModel: requestedModel})
}

func (t *inflightTracker) clientStreaming(accountID, id, routeID string) {
	t.mu.Lock()
	key := inflightClientKey(accountID, id, routeID)
	state := t.clientStates[key]
	state.Streaming++
	state.ClientID = id
	state.RouteID = routeID
	t.clientStates[key] = state
	t.mu.Unlock()
	t.emit(accountID, inflightDelta{ID: routeID, ClientID: id, Streaming: 1})
}

func (t *inflightTracker) clientResolved(accountID, id, routeID, resolvedModel string) {
	if resolvedModel == "" {
		return
	}
	t.mu.Lock()
	key := inflightClientKey(accountID, id, routeID)
	state := t.clientStates[key]
	state.ResolvedModel = resolvedModel
	state.ClientID = id
	state.RouteID = routeID
	t.clientStates[key] = state
	t.mu.Unlock()
	t.emit(accountID, inflightDelta{ID: routeID, ClientID: id, ResolvedModel: resolvedModel})
}

// clientEnd releases one client+route request. routeID mirrors clientStart so
// the closing delta carries the same client+route identity and only that
// ticket is decremented; a concurrent request on another route stays lit.
func (t *inflightTracker) clientEnd(accountID, id, routeID string, streamed bool) {
	t.mu.Lock()
	key := inflightClientKey(accountID, id, routeID)
	state := t.clientStates[key]
	if state.Active > 0 {
		state.Active--
	}
	if streamed && state.Streaming > 0 {
		state.Streaming--
	}
	if state.Active == 0 && state.Streaming == 0 {
		delete(t.clientStates, key)
	} else {
		t.clientStates[key] = state
	}
	t.mu.Unlock()
	delta := inflightDelta{ID: routeID, ClientID: id, Active: -1}
	if streamed {
		delta.Streaming = -1
	}
	t.emit(accountID, delta)
}

// clientRouteSnapshot returns the per (client, route) tickets for one account,
// keyed by the same composite the tracker uses. Each state carries its ClientID
// and RouteID so consumers need not parse the key.
func (t *inflightTracker) clientRouteSnapshot(accountID string) map[string]inflightState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]inflightState)
	prefix := accountID + "\x00"
	for key, state := range t.clientStates {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			out[key[len(prefix):]] = state
		}
	}
	return out
}

// clientSnapshot returns the per-client aggregate (all routes folded) for one
// account, keyed by client key id.
func (t *inflightTracker) clientSnapshot(accountID string) map[string]inflightState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]inflightState)
	prefix := accountID + "\x00"
	for key, state := range t.clientStates {
		if len(key) < len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		if state.ClientID == "" {
			continue
		}
		agg := out[state.ClientID]
		agg.Active += state.Active
		agg.Streaming += state.Streaming
		out[state.ClientID] = agg
	}
	return out
}

func (t *inflightTracker) targetStart(accountID, routeID, targetID string) {
	key := inflightTargetKey(accountID, routeID, targetID)
	t.mu.Lock()
	state := t.targetStates[key]
	state.Active++
	t.targetStates[key] = state
	t.mu.Unlock()
	t.emit(accountID, inflightDelta{ID: routeID, TargetID: targetID, Active: 1})
}

func (t *inflightTracker) targetEnd(accountID, routeID, targetID string) {
	key := inflightTargetKey(accountID, routeID, targetID)
	t.mu.Lock()
	state := t.targetStates[key]
	if state.Active > 0 {
		state.Active--
	}
	if state.Active == 0 {
		delete(t.targetStates, key)
	} else {
		t.targetStates[key] = state
	}
	t.mu.Unlock()
	t.emit(accountID, inflightDelta{ID: routeID, TargetID: targetID, Active: -1})
}

// targetSkipped emits one explicit terminal delta for a target the router
// declined to call. Unlike targetStart/targetEnd it does not touch the active
// counters (the target was never in flight); it exists so live consumers can
// render a distinct skipped leg with full client + route + target context.
func (t *inflightTracker) targetSkipped(accountID, routeID, clientID, targetID, failureClass string) {
	if routeID == "" || targetID == "" {
		return
	}
	t.emit(accountID, inflightDelta{ID: routeID, ClientID: clientID, TargetID: targetID, Result: "skipped", FailureClass: failureClass})
}

func (t *inflightTracker) targetSnapshot(accountID string) map[string]inflightState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]inflightState)
	prefix := accountID + "\x00"
	for key, state := range t.targetStates {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			out[key[len(prefix):]] = state
		}
	}
	return out
}
