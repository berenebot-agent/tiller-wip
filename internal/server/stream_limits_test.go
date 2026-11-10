package server

import (
	"net/http/httptest"

	"bytes"
	"fmt"
	"github.com/tiller-router/tiller-router/internal/config"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// trickleUpstream streams one small chunk every interval until the request
// context ends, like a provider that never finishes but never goes idle.
func trickleUpstream(model string, interval time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"object":"list","data":[{"id":%q}]}`, model))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for {
			_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tick\"},\"finish_reason\":null}]}\n\n")
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
		}
	}
}

func TestStreamEndsAtMaxDurationWithClearError(t *testing.T) {
	api, secret, canonical, _ := cooldownTestHarness(t, trickleUpstream("model-a", 20*time.Millisecond), trickleUpstream("model-b", 20*time.Millisecond))
	api.server.config.MaxStreamDuration = 400 * time.Millisecond
	start := time.Now()
	body := streamingChat(t, api, secret, canonical)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stream ran %v, want it cut near 400ms", elapsed)
	}
	if !strings.Contains(body, "tick") || !strings.Contains(body, codeStreamTimeLimit) {
		t.Fatalf("expected streamed content then a %s error event, got: %q", codeStreamTimeLimit, body)
	}
}

func TestStalledRequestReturns504AtMaxDuration(t *testing.T) {
	stall := func(model string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				_, _ = io.WriteString(w, fmt.Sprintf(`{"object":"list","data":[{"id":%q}]}`, model))
				return
			}
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
	}
	api, secret, canonical, _ := cooldownTestHarness(t, stall("model-a"), stall("model-b"))
	api.server.config.MaxStreamDuration = 300 * time.Millisecond
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{
		"model": canonical, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d %v, want 504", resp.StatusCode, payload)
	}
	if !strings.Contains(strings.ToLower(fmt.Sprint(payload)), codeStreamTimeLimit) {
		t.Fatalf("payload missing %s: %v", codeStreamTimeLimit, payload)
	}
}

func TestNoLimitWhenMaxDurationUnset(t *testing.T) {
	s := &Server{config: config.Config{Mode: config.ModeLocal}}
	r, end := s.limitRequest(httptest.NewRequest("POST", "/v1/chat/completions", nil), "acct")
	defer end()
	if _, ok := r.Context().Deadline(); ok {
		t.Fatal("local mode with no configured limit must not set a deadline")
	}
}

func TestSuspendCancelsOnlyThatAccountsStreams(t *testing.T) {
	api, secret, canonical, _ := cooldownTestHarness(t, trickleUpstream("model-a", 20*time.Millisecond), trickleUpstream("model-b", 20*time.Millisecond))
	api.server.config.MaxStreamDuration = time.Minute
	done := make(chan string, 1)
	go func() { done <- streamingChat(t, api, secret, canonical) }()
	time.Sleep(300 * time.Millisecond)
	api.server.cancelAccountStreams("some-other-account")
	select {
	case body := <-done:
		t.Fatalf("another account's suspension ended this stream: %q", body)
	case <-time.After(300 * time.Millisecond):
	}
	api.server.cancelAccountStreams(database.LocalAccountID)
	select {
	case body := <-done:
		if !bytes.Contains([]byte(body), []byte(codeAccountSuspended)) {
			t.Fatalf("expected %s error event, got %q", codeAccountSuspended, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after account suspension")
	}
}
