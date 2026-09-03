package server

import "testing"

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
