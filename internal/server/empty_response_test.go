package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
)

// sseUpstream serves provider discovery plus a chat completion in one of the
// streaming shapes exercised by the empty/error probe tests.
//
//	mode "empty"     role delta then finish, no content
//	mode "content"   role delta, one content delta, then finish
//	mode "error"     role delta then a 200 SSE carrying a top-level error object
//	mode "reasoning" role delta, one reasoning delta, then finish
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
