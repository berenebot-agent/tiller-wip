package server

import (
	"sync"
	"testing"
)

func TestInflightTrackerTransitions(t *testing.T) {
	var deltas []inflightDelta
	tracker := &inflightTracker{states: map[string]inflightState{}, emit: func(delta inflightDelta) { deltas = append(deltas, delta) }}

	tracker.start("virtual-1")
	if got := tracker.snapshot()["virtual-1"]; got != (inflightState{Active: 1}) {
		t.Fatalf("after start = %+v", got)
	}
	tracker.streaming("virtual-1")
	if got := tracker.snapshot()["virtual-1"]; got != (inflightState{Active: 1, Streaming: 1}) {
		t.Fatalf("after streaming = %+v", got)
	}
	tracker.end("virtual-1", true)
	if len(tracker.snapshot()) != 0 {
		t.Fatalf("state remained after end: %+v", tracker.snapshot())
	}
	if len(deltas) != 3 || deltas[0] != (inflightDelta{ID: "virtual-1", Active: 1}) || deltas[1] != (inflightDelta{ID: "virtual-1", Streaming: 1}) || deltas[2] != (inflightDelta{ID: "virtual-1", Active: -1, Streaming: -1}) {
		t.Fatalf("deltas = %+v", deltas)
	}
}

func TestInflightTrackerKeepsConcurrentRequests(t *testing.T) {
	tracker := &inflightTracker{states: map[string]inflightState{}, emit: func(inflightDelta) {}}
	tracker.start("virtual-1")
	tracker.start("virtual-1")
	tracker.streaming("virtual-1")
	tracker.end("virtual-1", true)
	if got := tracker.snapshot()["virtual-1"]; got != (inflightState{Active: 1}) {
		t.Fatalf("after first concurrent end = %+v", got)
	}
	tracker.end("virtual-1", false)
	if len(tracker.snapshot()) != 0 {
		t.Fatalf("state remained after second end: %+v", tracker.snapshot())
	}
}

func TestInflightTrackerClientTransitions(t *testing.T) {
	var deltas []inflightDelta
	tracker := &inflightTracker{states: map[string]inflightState{}, clientStates: map[string]inflightState{}, emit: func(delta inflightDelta) { deltas = append(deltas, delta) }}

	tracker.clientStart("client-1", "route-1", "main")
	if got := tracker.clientSnapshot()["client-1"]; got != (inflightState{Active: 1, RequestedModel: "main", RouteID: "route-1"}) {
		t.Fatalf("after client start = %+v", got)
	}
	tracker.clientStreaming("client-1", "route-1")
	if got := tracker.clientSnapshot()["client-1"]; got != (inflightState{Active: 1, Streaming: 1, RequestedModel: "main", RouteID: "route-1"}) {
		t.Fatalf("after client streaming = %+v", got)
	}
	tracker.clientEnd("client-1", "route-1", true)
	if len(tracker.clientSnapshot()) != 0 {
		t.Fatalf("client state remained after end: %+v", tracker.clientSnapshot())
	}
	if len(deltas) != 3 || deltas[0] != (inflightDelta{ID: "route-1", ClientID: "client-1", Active: 1, RequestedModel: "main"}) || deltas[1] != (inflightDelta{ID: "route-1", ClientID: "client-1", Streaming: 1}) || deltas[2] != (inflightDelta{ID: "route-1", ClientID: "client-1", Active: -1, Streaming: -1}) {
		t.Fatalf("client deltas = %+v", deltas)
	}
}

func TestInflightTrackerKeepsConcurrentClientRequests(t *testing.T) {
	tracker := &inflightTracker{states: map[string]inflightState{}, clientStates: map[string]inflightState{}, emit: func(inflightDelta) {}}
	tracker.clientStart("client-1", "route-1", "main")
	tracker.clientStart("client-1", "route-1", "main")
	tracker.clientStreaming("client-1", "route-1")
	tracker.clientEnd("client-1", "route-1", true)
	if got := tracker.clientSnapshot()["client-1"]; got != (inflightState{Active: 1, RequestedModel: "main", RouteID: "route-1"}) {
		t.Fatalf("after first concurrent client end = %+v", got)
	}
	tracker.clientEnd("client-1", "route-1", false)
	if len(tracker.clientSnapshot()) != 0 {
		t.Fatalf("client state remained after second end: %+v", tracker.clientSnapshot())
	}
}

// TestInflightTrackerRealRouteBalance mirrors the direct real-model proxy
// path: route-level start/end plus a single target start/end keyed by the
// provider-model ID on both sides (RouteModelID == ProviderModelID for a real
// route). The full cycle must leave no residue in any snapshot.
func TestInflightTrackerRealRouteBalance(t *testing.T) {
	var deltas []inflightDelta
	tracker := &inflightTracker{states: map[string]inflightState{}, clientStates: map[string]inflightState{}, targetStates: map[string]inflightState{}, emit: func(delta inflightDelta) { deltas = append(deltas, delta) }}

	const pmID = "provider-model-1"
	tracker.clientStart("client-1", pmID, "provider-a/model-a")
	tracker.start(pmID)
	tracker.targetStart(pmID, pmID)
	if got := tracker.targetSnapshot()[pmID+"\x00"+pmID]; got != (inflightState{Active: 1}) {
		t.Fatalf("after real target start = %+v", got)
	}
	tracker.targetEnd(pmID, pmID)
	tracker.end(pmID, false)
	tracker.clientEnd("client-1", pmID, false)
	if len(tracker.snapshot()) != 0 || len(tracker.clientSnapshot()) != 0 || len(tracker.targetSnapshot()) != 0 {
		t.Fatalf("state remained after real route cycle: route=%+v client=%+v target=%+v", tracker.snapshot(), tracker.clientSnapshot(), tracker.targetSnapshot())
	}
	want := []inflightDelta{
		{ID: pmID, ClientID: "client-1", Active: 1, RequestedModel: "provider-a/model-a"},
		{ID: pmID, Active: 1},
		{ID: pmID, TargetID: pmID, Active: 1},
		{ID: pmID, TargetID: pmID, Active: -1},
		{ID: pmID, Active: -1},
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
// direct real-model request: streaming() increments are cleared by the same
// deferred end(id, streamed=true) the proxy uses.
func TestInflightTrackerRealRouteStreamingBalance(t *testing.T) {
	tracker := &inflightTracker{states: map[string]inflightState{}, clientStates: map[string]inflightState{}, targetStates: map[string]inflightState{}, emit: func(inflightDelta) {}}

	const pmID = "provider-model-1"
	tracker.clientStart("client-1", pmID, "provider-a/model-a")
	tracker.start(pmID)
	tracker.targetStart(pmID, pmID)
	tracker.streaming(pmID)
	tracker.clientStreaming("client-1", pmID)
	tracker.targetEnd(pmID, pmID)
	tracker.end(pmID, true)
	tracker.clientEnd("client-1", pmID, true)
	if len(tracker.snapshot()) != 0 || len(tracker.clientSnapshot()) != 0 || len(tracker.targetSnapshot()) != 0 {
		t.Fatalf("state remained after streaming real route cycle: route=%+v client=%+v target=%+v", tracker.snapshot(), tracker.clientSnapshot(), tracker.targetSnapshot())
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
	api.server.inflight.emit = func(d inflightDelta) {
		mu.Lock()
		deltas = append(deltas, d)
		mu.Unlock()
		origEmit(d)
	}

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}

	if n := len(api.server.inflight.snapshot()); n != 0 {
		t.Fatalf("route state remained: %+v", api.server.inflight.snapshot())
	}
	if n := len(api.server.inflight.clientSnapshot()); n != 0 {
		t.Fatalf("client state remained: %+v", api.server.inflight.clientSnapshot())
	}
	if n := len(api.server.inflight.targetSnapshot()); n != 0 {
		t.Fatalf("target state remained: %+v", api.server.inflight.targetSnapshot())
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
