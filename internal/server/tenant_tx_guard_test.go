package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tiller-router/tiller-router/internal/store"
)

// txCheckWriter asserts that no tenant transaction is open whenever the router
// writes a client-visible byte, i.e. tenant state is never held across client
// streaming (sass_tech.md §8.4).
type txCheckWriter struct {
	http.ResponseWriter
	violations *atomic.Int64
}

func (w *txCheckWriter) Write(p []byte) (int, error) {
	if store.ActiveTenantTransactions() != 0 {
		w.violations.Add(1)
	}
	return w.ResponseWriter.Write(p)
}

func (w *txCheckWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// TestNoTenantTxAcrossProviderIOOrClientStreaming is the runtime half of the
// tenant-transaction-lifetime guard: a streaming inference request must not
// have any tenant transaction open while the provider is called or while bytes
// are written to the client.
func TestNoTenantTxAcrossProviderIOOrClientStreaming(t *testing.T) {
	var upstreamViolations atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if store.ActiveTenantTransactions() != 0 {
			upstreamViolations.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"one","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		flusher.Flush()
		if store.ActiveTenantTransactions() != 0 {
			upstreamViolations.Add(1)
		}
		_, _ = io.WriteString(w, `data: {"id":"one","object":"chat.completion.chunk","model":"model-a","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`+"\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	api, _, _, secret := loggingTestHarness(t, upstream)

	var clientViolations atomic.Int64
	guarded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.server.Handler().ServeHTTP(&txCheckWriter{ResponseWriter: w, violations: &clientViolations}, r)
	}))
	t.Cleanup(guarded.Close)

	resp, _ := clientCall(t, guarded.URL, secret, "/v1/chat/completions", map[string]any{
		"model":    "provider-a/model-a",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("streaming request status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := upstreamViolations.Load(); got != 0 {
		t.Fatalf("tenant transaction open during provider I/O (%d observations)", got)
	}
	if got := clientViolations.Load(); got != 0 {
		t.Fatalf("tenant transaction open during client streaming (%d observations)", got)
	}
}
