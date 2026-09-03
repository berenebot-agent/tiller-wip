package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// readSSE reads the next SSE event from a stream, returning its event name and
// raw data payload. It skips comment lines (heartbeats).
func readSSE(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var event, data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if event != "" || data != "" {
				return event, data
			}
		}
	}
}

// TestLiveRequiresAuth asserts the live endpoint is admin-gated.
func TestLiveRequiresAuth(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	req, _ := http.NewRequest(http.MethodGet, api.base+"/api/admin/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("live without session = %d, want 401", resp.StatusCode)
	}
}

// TestLiveStreamsOutcomeAndSnapshot connects to the live stream, drives a real
// proxied request through the mock upstream, and asserts the stream emits an
// outcome delta followed by a debounced snapshot.
func TestLiveStreamsOutcomeAndSnapshot(t *testing.T) {
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))

	req, _ := http.NewRequest(http.MethodGet, api.base+"/api/admin/live", nil)
	req.AddCookie(api.client.Jar.Cookies(req.URL)[0])
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("content-type = %q, want event-stream", ct)
	}
	reader := bufio.NewReader(resp.Body)

	// First event must be the baseline snapshot.
	event, data := readSSE(t, reader)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	var snap liveSnapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Modules == nil {
		t.Fatal("snapshot missing reserved modules seam")
	}

	// Drive a real request so recordLastOutcome emits a delta.
	clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})

	// In-flight activity deltas (client + virtual) may arrive before the outcome
	// delta, so skip them and wait for the first outcome event.
	event, data = readSSE(t, reader)
	for event != "outcome" {
		event, data = readSSE(t, reader)
	}
	var delta map[string]lastOutcome
	if err := json.Unmarshal([]byte(data), &delta); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if len(delta) == 0 {
		t.Fatal("outcome delta was empty")
	}
	for _, o := range delta {
		if !o.IsSuccess {
			t.Fatalf("expected success outcome, got %+v", o)
		}
	}

	// The debounced snapshot follows within the debounce window.
	deadline := time.Now().Add(4 * time.Second)
	for {
		event, data = readSSE(t, reader)
		if event == "snapshot" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot within debounce window; last event %q", event)
		}
	}
	var snap2 liveSnapshot
	if err := json.Unmarshal([]byte(data), &snap2); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(snap2.TargetLastOutcome) == 0 {
		t.Fatal("snapshot did not include the recorded outcome")
	}
	_ = clientID
}

// TestLiveUnsubscribeStopsDispatcher asserts that when the last subscriber
// disconnects, the dispatcher goroutine is cancelled (no leak) and a later
// subscribe starts a fresh one.
func TestLiveUnsubscribeStopsDispatcher(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))

	req, _ := http.NewRequest(http.MethodGet, api.base+"/api/admin/live", nil)
	req.AddCookie(api.client.Jar.Cookies(req.URL)[0])
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	readSSE(t, reader) // baseline snapshot
	resp.Body.Close()

	// After close, the hub should have no subscribers and no active dispatcher.
	api.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		api.server.liveHub.mu.Lock()
		active := len(api.server.liveHub.subs) > 0 || api.server.liveHub.cancel != nil
		api.server.liveHub.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher did not stop after last subscriber disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLiveBroadcastFanout asserts a single outcome delta reaches multiple
// subscribers.
func TestLiveBroadcastFanout(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	h := api.server.liveHub

	ch1 := h.subscribe()
	ch2 := h.subscribe()
	defer h.unsubscribe(ch1)
	defer h.unsubscribe(ch2)

	h.broadcast("outcome", map[string]lastOutcome{"pm": {IsSuccess: true}})

	for _, ch := range []chan []byte{ch1, ch2} {
		select {
		case msg := <-ch:
			if !strings.Contains(string(msg), "event: outcome") {
				t.Fatalf("fanout message missing event: %s", msg)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive broadcast")
		}
	}
}

func TestLiveOutcomeIsDroppedWithoutSubscribers(t *testing.T) {
	h := &liveHub{outcomeCh: make(chan map[string]lastOutcome, liveOutcomeBuffer)}
	h.emitOutcome(map[string]lastOutcome{"pm": {IsSuccess: true}})

	select {
	case <-h.outcomeCh:
		t.Fatal("outcome was queued without a live subscriber")
	default:
	}

	ch := h.subscribe()
	defer h.unsubscribe(ch)
	h.emitOutcome(map[string]lastOutcome{"pm": {IsSuccess: true}})
	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "event: outcome") {
			t.Fatalf("outcome message missing event: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("outcome was not queued with a live subscriber")
	}
}

// TestLiveContextCancelUnsubscribes asserts a subscriber whose request context
// is cancelled is removed from the hub.
func TestLiveContextCancelUnsubscribes(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	h := api.server.liveHub

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api.base+"/api/admin/live", nil)
	req.AddCookie(api.client.Jar.Cookies(req.URL)[0])
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	readSSE(t, reader) // baseline snapshot
	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		active := len(h.subs) > 0 || h.cancel != nil
		h.mu.Unlock()
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("subscriber not removed after context cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveTerminatesWhenSessionIsRevoked(t *testing.T) {
	previous := liveSessionCheckInterval
	liveSessionCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { liveSessionCheckInterval = previous })

	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	req, _ := http.NewRequest(http.MethodGet, api.base+"/api/admin/live", nil)
	cookie := api.client.Jar.Cookies(req.URL)[0]
	req.AddCookie(cookie)
	resp, err := api.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	readSSE(t, bufio.NewReader(resp.Body)) // baseline snapshot

	closed := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		closed <- err
	}()
	api.server.sessions.Delete(cookie.Value)

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("read revoked stream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revoked live session remained connected")
	}
}
