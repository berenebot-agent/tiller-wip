package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// sseUpstream serves provider discovery plus a chat completion in one of the
// streaming shapes exercised by the empty/error probe tests.
//
//	mode "empty" role delta then finish, no content
//	mode "content" role delta, one content delta, then finish
//	mode "error" role delta then a 200 SSE carrying a top-level error object
//	mode "reasoning" role delta, one reasoning delta, then finish
//	mode "silent" SSE headers flush immediately, then no output for longer
//	than a short keepalive interval before content arrives
//	mode "silent-empty" like "silent" but finishes with no content
func sseUpstream(modelID, mode string, reached *[]string, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": modelID}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if reached != nil {
			mu.Lock()
			*reached = append(*reached, modelID)
			mu.Unlock()
		}
		var input map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &input)
		if streaming, _ := input["stream"].(bool); !streaming {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": modelID, "object": "chat.completion", "model": modelID,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello-from-" + modelID}, "finish_reason": "stop"}},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(payload map[string]any) {
			encoded, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk := func(delta map[string]any, finish any) {
			write(map[string]any{
				"id": modelID, "object": "chat.completion.chunk", "model": modelID,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			})
		}
		switch mode {
		case "empty":
			chunk(map[string]any{"role": "assistant"}, nil)
			chunk(map[string]any{}, "stop")
		case "content":
			chunk(map[string]any{"role": "assistant"}, nil)
			chunk(map[string]any{"content": "hello-from-" + modelID}, nil)
			chunk(map[string]any{}, "stop")
		case "error":
			chunk(map[string]any{"role": "assistant"}, nil)
			write(map[string]any{
				"id": modelID, "object": "chat.completion.chunk", "model": modelID,
				"error":   map[string]any{"code": "error", "message": "Stream error occurred"},
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ""}, "finish_reason": "error"}},
			})
		case "reasoning":
			chunk(map[string]any{"role": "assistant"}, nil)
			chunk(map[string]any{"reasoning": "thinking"}, nil)
			chunk(map[string]any{}, "stop")
		case "silent":
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(120 * time.Millisecond)
			chunk(map[string]any{"role": "assistant"}, nil)
			chunk(map[string]any{"content": "hello-from-" + modelID}, nil)
			chunk(map[string]any{}, "stop")
		case "silent-empty":
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(120 * time.Millisecond)
			chunk(map[string]any{"role": "assistant"}, nil)
			chunk(map[string]any{}, "stop")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func streamingChat(t *testing.T, api *testAPI, secret, model string) string {
	t.Helper()
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{
		"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true,
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func reachedOrder(mu *sync.Mutex, reached *[]string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), (*reached)...)
}

func TestOrderedFallbackEmptyStreamFallsThrough(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	api, secret, canonical, _ := cooldownTestHarness(t,
		sseUpstream("model-a", "empty", &reached, &mu),
		sseUpstream("model-b", "content", &reached, &mu))

	body := streamingChat(t, api, secret, canonical)
	if !bytes.Contains([]byte(body), []byte("hello-from-model-b")) {
		t.Fatalf("expected fallback content from model-b, got: %s", body)
	}
	if got := reachedOrder(&mu, &reached); len(got) != 2 || got[0] != "model-a" || got[1] != "model-b" {
		t.Fatalf("attempt order = %v, want [model-a model-b]", got)
	}
}

func TestOrderedFallbackStreamErrorFallsThrough(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	api, secret, canonical, _ := cooldownTestHarness(t,
		sseUpstream("model-a", "error", &reached, &mu),
		sseUpstream("model-b", "content", &reached, &mu))

	body := streamingChat(t, api, secret, canonical)
	if !bytes.Contains([]byte(body), []byte("hello-from-model-b")) {
		t.Fatalf("expected fallback content from model-b, got: %s", body)
	}
	if got := reachedOrder(&mu, &reached); len(got) != 2 || got[0] != "model-a" || got[1] != "model-b" {
		t.Fatalf("attempt order = %v, want [model-a model-b]", got)
	}
}

func TestOrderedFallbackReasoningOnlyDoesNotFallBack(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	api, secret, canonical, _ := cooldownTestHarness(t,
		sseUpstream("model-a", "reasoning", &reached, &mu),
		sseUpstream("model-b", "content", &reached, &mu))

	body := streamingChat(t, api, secret, canonical)
	if !bytes.Contains([]byte(body), []byte("thinking")) {
		t.Fatalf("expected reasoning output from model-a, got: %s", body)
	}
	if got := reachedOrder(&mu, &reached); len(got) != 1 || got[0] != "model-a" {
		t.Fatalf("reasoning-only must not fall back, got attempt order %v", got)
	}
}

func TestEmptyResponseCooldownSkipsTargetOnNextRequest(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	api, secret, canonical, _ := cooldownTestHarness(t,
		sseUpstream("model-a", "empty", &reached, &mu),
		sseUpstream("model-b", "content", &reached, &mu))

	_ = streamingChat(t, api, secret, canonical)
	_ = streamingChat(t, api, secret, canonical)

	got := reachedOrder(&mu, &reached)
	want := []string{"model-a", "model-b", "model-b"}
	if len(got) != len(want) {
		t.Fatalf("attempt order = %v, want %v (model-a should be cooled)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("attempt order = %v, want %v", got, want)
		}
	}
}

// TestOrderedFallbackProbeKeepaliveDuringSilence is the regression for the
// ordered-fallback probe hole: an upstream can return 200/SSE headers and then
// sit silent while a long reasoning prefill produces no deltas. Tiller probes
// before committing, so without keepalives the client-facing connection can be
// cut by a reverse proxy read timeout. The silent target must still serve, and
// the client stream must carry keepalive frames during the probe window.
func TestOrderedFallbackProbeKeepaliveDuringSilence(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	api, secret, canonical, _ := cooldownTestHarness(t,
		sseUpstream("model-a", "silent", &reached, &mu),
		sseUpstream("model-b", "content", &reached, &mu))
	api.server.sseKeepalive = 30 * time.Millisecond

	body := streamingChat(t, api, secret, canonical)
	if !bytes.Contains([]byte(body), []byte(": keepalive")) {
		t.Fatalf("expected keepalive comment frames during probe silence, got: %q", body)
	}
	if !bytes.Contains([]byte(body), []byte("hello-from-model-a")) {
		t.Fatalf("expected model-a output after the silent probe, got: %q", body)
	}
	if got := reachedOrder(&mu, &reached); len(got) != 1 || got[0] != "model-a" {
		t.Fatalf("silent-but-alive target must serve, got attempt order %v", got)
	}
}

// TestOrderedFallbackProbeKeepaliveThenExhausted verifies that when the probe
// commit has happened and every target is silent-then-empty, the exhausted
// chain is surfaced as an SSE failure frame rather than a JSON body on a
// committed 200 stream.
func TestOrderedFallbackProbeKeepaliveThenExhausted(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	api, secret, canonical, _ := cooldownTestHarness(t,
		sseUpstream("model-a", "silent-empty", &reached, &mu),
		sseUpstream("model-b", "silent-empty", &reached, &mu))
	api.server.sseKeepalive = 30 * time.Millisecond

	body := streamingChat(t, api, secret, canonical)
	if !bytes.Contains([]byte(body), []byte(": keepalive")) {
		t.Fatalf("expected keepalive frames from the committed stream, got: %q", body)
	}
	if !bytes.Contains([]byte(body), []byte("virtual_model_unavailable")) {
		t.Fatalf("expected an SSE failure frame after exhausting silent targets, got: %q", body)
	}
}

func sseUpstreamWithHeaders(modelID, mode string, reached *[]string, mu *sync.Mutex, extraHeaders map[string]string) http.HandlerFunc {
	base := sseUpstream(modelID, mode, reached, mu)
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		base(w, r)
	}
}

// TestOrderedFallbackEarlyCommitDoesNotLeakProviderHeaders verifies that
// provider-specific headers from a failed ordered-fallback target are not
// carried into the client response when the stream is committed early.
func TestOrderedFallbackEarlyCommitDoesNotLeakProviderHeaders(t *testing.T) {
	var mu sync.Mutex
	reached := []string{}
	// Target A returns SSE with provider-specific headers but no usable output.
	upstreamA := sseUpstreamWithHeaders("model-a", "silent-empty", &reached, &mu, map[string]string{
		"Request-Id":        "req-a",
		"X-RateLimit-Limit": "10",
	})
	// Target B returns SSE with different provider-specific headers and valid output.
	upstreamB := sseUpstreamWithHeaders("model-b", "content", &reached, &mu, map[string]string{
		"Request-Id":        "req-b",
		"X-RateLimit-Limit": "20",
	})
	api, secret, canonical, _ := cooldownTestHarness(t, upstreamA, upstreamB)

	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{
		"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true,
	})
	defer resp.Body.Close()

	// Target A's provider-specific headers must not appear in the client response.
	if got := resp.Header.Get("Request-Id"); got != "" {
		t.Errorf("client response carries a Request-Id %q from the non-serving target", got)
	}
	if got := resp.Header.Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("client response carries X-RateLimit-Limit %q from the non-serving target", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "" {
		t.Errorf("client response carries X-RateLimit-Remaining %q from the non-serving target", got)
	}
	if got := resp.Header.Get("X-RateLimit-Reset"); got != "" {
		t.Errorf("client response carries X-RateLimit-Reset %q from the non-serving target", got)
	}
	// Target B's content must still be delivered.
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("hello-from-model-b")) {
		t.Fatalf("expected fallback content from model-b, got: %s", body)
	}
	if got := reachedOrder(&mu, &reached); len(got) != 2 || got[0] != "model-a" || got[1] != "model-b" {
		t.Fatalf("attempt order = %v, want [model-a model-b]", got)
	}
}
