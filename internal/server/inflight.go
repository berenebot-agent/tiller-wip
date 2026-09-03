package server

import "sync"

type inflightState struct {
	Active    int `json:"active"`
	Streaming int `json:"streaming"`
}

type inflightDelta struct {
	ID        string `json:"id,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	TargetID  string `json:"target_id,omitempty"`
	Active    int    `json:"active"`
	Streaming int    `json:"streaming"`
}

type inflightTracker struct {
	mu           sync.Mutex
	states       map[string]inflightState // keyed by virtual model id
	clientStates map[string]inflightState // keyed by client key id
	targetStates map[string]inflightState // keyed by virtual model/provider model pair
	emit         func(inflightDelta)
}

func (t *inflightTracker) start(id string) {
	t.mu.Lock()
	state := t.states[id]
	state.Active++
	t.states[id] = state
	t.mu.Unlock()
	t.emit(inflightDelta{ID: id, Active: 1})
}

func (t *inflightTracker) streaming(id string) {
	t.mu.Lock()
	state := t.states[id]
	state.Streaming++
	t.states[id] = state
	t.mu.Unlock()
	t.emit(inflightDelta{ID: id, Streaming: 1})
}

func (t *inflightTracker) end(id string, streamed bool) {
	t.mu.Lock()
	state := t.states[id]
	if state.Active > 1 {
		state.Active--
	} else {
		state.Active = 0
	}
	if streamed && state.Streaming > 0 {
		state.Streaming--
	}
	if state.Active == 0 && state.Streaming == 0 {
		delete(t.states, id)
	} else {
		t.states[id] = state
	}
	t.mu.Unlock()
	delta := inflightDelta{ID: id, Active: -1}
	if streamed {
		delta.Streaming = -1
	}
	t.emit(delta)
}

func (t *inflightTracker) snapshot() map[string]inflightState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]inflightState, len(t.states))
	for id, state := range t.states {
		out[id] = state
	}
	return out
}

func (t *inflightTracker) clientStart(id string) {
	t.mu.Lock()
	state := t.clientStates[id]
	state.Active++
	t.clientStates[id] = state
	t.mu.Unlock()
	t.emit(inflightDelta{ClientID: id, Active: 1})
}

func (t *inflightTracker) clientStreaming(id string) {
	t.mu.Lock()
	state := t.clientStates[id]
	state.Streaming++
	t.clientStates[id] = state
	t.mu.Unlock()
	t.emit(inflightDelta{ClientID: id, Streaming: 1})
}

func (t *inflightTracker) clientEnd(id string, streamed bool) {
	t.mu.Lock()
	state := t.clientStates[id]
	if state.Active > 1 {
		state.Active--
	} else {
		state.Active = 0
	}
	if streamed && state.Streaming > 0 {
		state.Streaming--
	}
	if state.Active == 0 && state.Streaming == 0 {
		delete(t.clientStates, id)
	} else {
		t.clientStates[id] = state
	}
	t.mu.Unlock()
	delta := inflightDelta{ClientID: id, Active: -1}
	if streamed {
		delta.Streaming = -1
	}
	t.emit(delta)
}

func (t *inflightTracker) clientSnapshot() map[string]inflightState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]inflightState, len(t.clientStates))
	for id, state := range t.clientStates {
		out[id] = state
	}
	return out
}

func (t *inflightTracker) targetStart(virtualID, targetID string) {
	key := virtualID + "\x00" + targetID
	t.mu.Lock()
	state := t.targetStates[key]
	state.Active++
	t.targetStates[key] = state
	t.mu.Unlock()
	t.emit(inflightDelta{ID: virtualID, TargetID: targetID, Active: 1})
}

func (t *inflightTracker) targetEnd(virtualID, targetID string) {
	key := virtualID + "\x00" + targetID
	t.mu.Lock()
	state := t.targetStates[key]
	if state.Active > 1 {
		state.Active--
	} else {
		state.Active = 0
	}
	if state.Active == 0 {
		delete(t.targetStates, key)
	} else {
		t.targetStates[key] = state
	}
	t.mu.Unlock()
	t.emit(inflightDelta{ID: virtualID, TargetID: targetID, Active: -1})
}

func (t *inflightTracker) targetSnapshot() map[string]inflightState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]inflightState, len(t.targetStates))
	for key, state := range t.targetStates {
		out[key] = state
	}
	return out
}
