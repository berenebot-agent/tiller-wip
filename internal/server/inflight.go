package server

import "sync"

type inflightState struct {
	Active    int `json:"active"`
	Streaming int `json:"streaming"`
}

type inflightDelta struct {
	ID        string `json:"id"`
	Active    int    `json:"active"`
	Streaming int    `json:"streaming"`
}

type inflightTracker struct {
	mu     sync.Mutex
	states map[string]inflightState
	emit   func(inflightDelta)
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
