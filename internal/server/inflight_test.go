package server

import (
	"sync"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

const testAcct = "acct-1"

// The route-level lane is gone (single-ticket liveness): the client ticket
// is the only request-presence signal, so these tests cover the client
// lifecycle carrying the route ID.
func TestInflightTrackerClientTransitions(t *testing.T) {
	var deltas []inflightDelta
	tracker := &inflightTracker{clientStates: map[string]inflightState{}, emit: func(_ string, delta inflightDelta) { deltas = append(deltas, delta) }}

	tracker.clientStart(testAcct, "client-1", "route-1", "main")
	if got := tracker.clientRouteSnapshot(testAcct)[inflightClientKey(testAcct, "client-1", "route-1")]; got != (inflightState{Active: 1, RequestedModel: "main", ClientID: "client-1", RouteID: "route-1"}) {
		t.Fatalf("after client start = %+v", got)
	}
	tracker.clientStreaming(testAcct, "client-1", "route-1")
	if got := tracker.clientRouteSnapshot(testAcct)[inflightClientKey(testAcct, "client-1", "route-1")]; got != (inflightState{Active: 1, Streaming: 1, RequestedModel: "main", ClientID: "client-1", RouteID: "route-1"}) {
		t.Fatalf("after client streaming = %+v", got)
	}
	tracker.clientEnd(testAcct, "client-1", "route-1", true)
	if len(tracker.clientRouteSnapshot(testAcct)) != 0 {
		t.Fatalf("client state remained after end: %+v", tracker.clientRouteSnapshot(testAcct))
	}
	if len(deltas) != 3 || deltas[0] != (inflightDelta{ID: "route-1", ClientID: "client-1", Active: 1, RequestedModel: "main"}) || deltas[1] != (inflightDelta{ID: "route-1", ClientID: "client-1", Streaming: 1}) || deltas[2] != (inflightDelta{ID: "route-1", ClientID: "client-1", Active: -1, Streaming: -1}) {
		t.Fatalf("client deltas = %+v", deltas)
	}
}

func TestInflightTrackerKeepsConcurrentClientRequests(t *testing.T) {
	tracker := &inflightTracker{clientStates: map[string]inflightState{}, emit: func(string, inflightDelta) {}}
	tracker.clientStart(testAcct, "client-1", "route-1", "main")
	tracker.clientStart(testAcct, "client-1", "route-1", "main")
	tracker.clientStreaming(testAcct, "client-1", "route-1")
	tracker.clientEnd(testAcct, "client-1", "route-1", true)
	if got := tracker.clientSnapshot(testAcct)["client-1"]; got != (inflightState{Active: 1, Streaming: 0}) {
		t.Fatalf("after first concurrent client end = %+v", got)
	}
	tracker.clientEnd(testAcct, "client-1", "route-1", false)
	if len(tracker.clientSnapshot(testAcct)) != 0 {
		t.Fatalf("client state remained after second end: %+v", tracker.clientSnapshot(testAcct))
	}
}

// TestInflightTrackerKeepsConcurrentRoutesForOneClient covers the bug this
// tracker shape exists to prevent: one client key issues parallel requests on
// two different routes. Both tickets must stay lit and carry their own route
// identity; ending one must not tear down the other. It also checks the
// per-client aggregate folds both tickets for the client status roundel.
func TestInflightTrackerKeepsConcurrentRoutesForOneClient(t *testing.T) {
	tracker := &inflightTracker{clientStates: map[string]inflightState{}, emit: func(string, inflightDelta) {}}
	tracker.clientStart(testAcct, "client-1", "main", "main")
	tracker.clientStart(testAcct, "client-1", "coding", "coding")

	routes := tracker.clientRouteSnapshot(testAcct)
	if got := routes[inflightClientKey(testAcct, "client-1", "main")]; got != (inflightState{Active: 1, RequestedModel: "main", ClientID: "client-1", RouteID: "main"}) {
		t.Fatalf("main ticket = %+v", got)
	}
	if got := routes[inflightClientKey(testAcct, "client-1", "coding")]; got != (inflightState{Active: 1, RequestedModel: "coding", ClientID: "client-1", RouteID: "coding"}) {
		t.Fatalf("coding ticket = %+v", got)
	}
	if len(routes) != 2 {
		t.Fatalf("expected two concurrent client+route tickets, got %+v", routes)
	}
	if agg := tracker.clientSnapshot(testAcct)["client-1"]; agg.Active != 2 {
		t.Fatalf("per-client aggregate = %+v, want active 2", agg)
	}

	tracker.clientEnd(testAcct, "client-1", "main", false)
	routes = tracker.clientRouteSnapshot(testAcct)
	if _, ok := routes[inflightClientKey(testAcct, "client-1", "coding")]; !ok {
		t.Fatalf("coding ticket was torn down with main: %+v", routes)
	}
	if _, ok := routes[inflightClientKey(testAcct, "client-1", "main")]; ok {
		t.Fatalf("main ticket survived its end: %+v", routes)
	}
	if agg := tracker.clientSnapshot(testAcct)["client-1"]; agg.Active != 1 {
		t.Fatalf("per-client aggregate after one end = %+v, want active 1", agg)
	}

	tracker.clientEnd(testAcct, "client-1", "coding", false)
	if len(tracker.clientRouteSnapshot(testAcct)) != 0 || len(tracker.clientSnapshot(testAcct)) != 0 {
		t.Fatalf("state remained after both ends: routes=%+v clients=%+v", tracker.clientRouteSnapshot(testAcct), tracker.clientSnapshot(testAcct))
	}
}

// TestInflightTrackerRealRouteBalance mirrors the direct real-model proxy
// path: the client ticket plus a single target start/end keyed by the
// provider-model ID on both sides (RouteModelID == ProviderModelID for a real
// route). The full cycle must leave no residue in any snapshot.
func TestInflightTrackerRealRouteBalance(t *testing.T) {
	var deltas []inflightDelta
	tracker := &inflightTracker{clientStates: map[string]inflightState{}, targetStates: map[string]inflightState{}, emit: func(_ string, delta inflightDelta) { deltas = append(deltas, delta) }}

	const pmID = "provider-model-1"
	tracker.clientStart(testAcct, "client-1", pmID, "provider-a/model-a")
	tracker.targetStart(testAcct, pmID, pmID)
	if got := tracker.targetSnapshot(testAcct)[pmID+"\x00"+pmID]; got != (inflightState{Active: 1}) {
		t.Fatalf("after real target start = %+v", got)
	}
	tracker.targetEnd(testAcct, pmID, pmID)
	tracker.clientEnd(testAcct, "client-1", pmID, false)
	if len(tracker.clientRouteSnapshot(testAcct)) != 0 || len(tracker.targetSnapshot(testAcct)) != 0 {
		t.Fatalf("state remained after real route cycle: client=%+v target=%+v", tracker.clientRouteSnapshot(testAcct), tracker.targetSnapshot(testAcct))
	}
	want := []inflightDelta{
		{ID: pmID, ClientID: "client-1", Active: 1, RequestedModel: "provider-a/model-a"},
		{ID: pmID, TargetID: pmID, Active: 1},
		{ID: pmID, TargetID: pmID, Active: -1},
		{ID: pmID, ClientID: "client-1", Active: -1},
	}
	if len(deltas) != len(want) {
		t.Fatalf("deltas = %+v, want %+v", deltas, want)
	}
	for i := range want {
		if deltas[i] != want[i] {
			t.Fatalf("delta %d = %+v, want %+v", i, deltas[i], want[i])
		}
	}
}

// TestInflightTrackerRealRouteStreamingBalance covers the streaming leg of a
// direct real-model request: clientStreaming increments are cleared by the
// same deferred clientEnd(id, routeID, streamed=true) the proxy uses.
func TestInflightTrackerRealRouteStreamingBalance(t *testing.T) {
	tracker := &inflightTracker{clientStates: map[string]inflightState{}, targetStates: map[string]inflightState{}, emit: func(string, inflightDelta) {}}

	const pmID = "provider-model-1"
	tracker.clientStart(testAcct, "client-1", pmID, "provider-a/model-a")
	tracker.targetStart(testAcct, pmID, pmID)
	tracker.clientStreaming(testAcct, "client-1", pmID)
	tracker.targetEnd(testAcct, pmID, pmID)
	tracker.clientEnd(testAcct, "client-1", pmID, true)
	if len(tracker.clientRouteSnapshot(testAcct)) != 0 || len(tracker.targetSnapshot(testAcct)) != 0 {
		t.Fatalf("state remained after streaming real route cycle: client=%+v target=%+v", tracker.clientRouteSnapshot(testAcct), tracker.targetSnapshot(testAcct))
	}
}

// TestDirectRealRouteEmitsBalancedTargetDeltas drives a real direct
// real-model request through the proxy and asserts the 1:1 leg emits
// targetStart/targetEnd deltas (ID and TargetID both the provider-model ID)
// and leaves no residue in any inflight snapshot.
func TestDirectRealRouteEmitsBalancedTargetDeltas(t *testing.T) {
	api, _, _, secret := loggingTestHarness(t, mockUpstream(t))

	var mu sync.Mutex
	var deltas []inflightDelta
	origEmit := api.server.inflight.emit
	api.server.inflight.emit = func(accountID string, d inflightDelta) {
		mu.Lock()
		deltas = append(deltas, d)
		mu.Unlock()
		origEmit(accountID, d)
	}

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}

	if n := len(api.server.inflight.clientSnapshot(database.LocalAccountID)); n != 0 {
		t.Fatalf("client state remained: %+v", api.server.inflight.clientSnapshot(database.LocalAccountID))
	}
	if n := len(api.server.inflight.targetSnapshot(database.LocalAccountID)); n != 0 {
		t.Fatalf("target state remained: %+v", api.server.inflight.targetSnapshot(database.LocalAccountID))
	}

	mu.Lock()
	defer mu.Unlock()
	var starts, ends int
	var targetID string
	for _, d := range deltas {
		if d.TargetID != "" {
			targetID = d.TargetID
			if d.Active == 1 {
				starts++
			} else if d.Active == -1 {
				ends++
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("target starts=%d ends=%d, want 1/1 (deltas=%+v)", starts, ends, deltas)
	}
	// For a direct real route the route ID is the provider-model ID, so
	// ID and TargetID coincide on the same 1:1 leg.
	var routeID string
	for _, d := range deltas {
		if d.TargetID != "" {
			routeID = d.ID
		}
	}
	if routeID == "" || routeID != targetID {
		t.Fatalf("route ID %q != target ID %q, want equal 1:1 leg", routeID, targetID)
	}
	// The client deltas carry the same route ID (dual identity), which is
	// what anchors the client → route leg on the Activity graph.
	var clientStarts, clientEnds int
	for _, d := range deltas {
		if d.ClientID == "" {
			continue
		}
		if d.ID != routeID {
			t.Fatalf("client delta missing route ID: %+v (want ID %q)", d, routeID)
		}
		if d.Active == 1 {
			clientStarts++
		} else if d.Active == -1 {
			clientEnds++
		}
	}
	if clientStarts != 1 || clientEnds != 1 {
		t.Fatalf("client starts=%d ends=%d, want 1/1 (deltas=%+v)", clientStarts, clientEnds, deltas)
	}
}
