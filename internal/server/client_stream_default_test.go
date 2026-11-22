package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database/dbtest"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

// TestClientWantsNonStreaming pins the OpenAI default: `stream` is optional and
// defaults to false, so an omitted field asks for one aggregated JSON object.
// Only an explicit `"stream": true` opts into incremental relay.
func TestClientWantsNonStreaming(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"omitted", `{"model":"m","messages":[]}`, true},
		{"explicit false", `{"model":"m","stream":false,"messages":[]}`, true},
		{"explicit true", `{"model":"m","stream":true,"messages":[]}`, false},
		{"non-boolean stream", `{"model":"m","stream":"true"}`, true},
		{"malformed body", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clientWantsNonStreaming([]byte(tc.body), providers.ProtocolChat); got != tc.want {
				t.Fatalf("clientWantsNonStreaming(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// streamingOnlyTarget serves a Codex Responses provider that only ever answers
// with SSE, which is the shape that used to leak event frames to a client that
// had asked for a single JSON object.
func streamingOnlyTarget(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{
				"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true,
			}}})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
		case "/responses":
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			write := func(event, payload string) {
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
			write("response.created", `{"type":"response.created","response":{"id":"resp-1","object":"response","model":"gpt-5.6-sol","output":[]}}`)
			write("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hello"}`)
			write("response.completed", `{"type":"response.completed","response":{"id":"resp-1","object":"response","model":"gpt-5.6-sol","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4},"output":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// streamingTargetFixture wires a Codex-mock provider, model and client key
// through the admin API, mirroring the sequence used by the other Codex tests.
func streamingTargetFixture(t *testing.T) (base, clientSecret string) {
	t.Helper()
	upstream := streamingOnlyTarget(t)

	db, err := dbtest.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	app.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != http.StatusOK {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "codex-mock", "type": "codex-subscription", "base_url": upstream.URL, "protocols": []any{"responses"}})
	if status != http.StatusCreated {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	future := time.Now().Add(time.Hour)
	putOAuthToken(t, db, oauth.TokenRecord{
		ProviderID:   providerID,
		AccessToken:  "live-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    &future,
		AuthState:    oauth.AuthConnected,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	status, payload, _ = api.request("POST", "/api/admin/providers/"+providerID+"/refresh", nil)
	if status != 200 {
		t.Fatalf("refresh after seeding the OAuth token: %d %v", status, payload)
	}
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != http.StatusOK {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		model := raw.(map[string]any)
		if model["upstream_model_id"] == "gpt-5.6-sol" {
			modelID = model["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose gpt-5.6-sol")
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "stream-default", "type": "catalogue"})
	if status != http.StatusCreated {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID, secret := payload["id"].(string), payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != http.StatusNoContent {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	return router.URL, secret
}

// TestOmittedStreamFieldYieldsAggregatedJSON proves that a client which omits
// `stream` receives one application/json completion even when the target only
// speaks SSE. Before the fix these callers were handed raw event frames and
// OpenAI SDKs reported "no choices".
func TestOmittedStreamFieldYieldsAggregatedJSON(t *testing.T) {
	t.Parallel()
	base, secret := streamingTargetFixture(t)

	payload, _ := json.Marshal(map[string]any{
		"model":    "codex-mock/gpt-5.6-sol",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json for an omitted stream field", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		t.Fatalf("body was not a single JSON object: %v (body %q)", err, raw)
	}
	if len(completion.Choices) == 0 {
		t.Fatalf("no choices in aggregated body: %q", raw)
	}
	if content := completion.Choices[0].Message.Content; !strings.Contains(content, "hello") {
		t.Fatalf("message content = %q, want it to contain the upstream delta", content)
	}
}

// TestExplicitStreamTrueStillRelaysSSE keeps the other direction pinned: a
// client that asks for incremental relay still gets event frames.
func TestExplicitStreamTrueStillRelaysSSE(t *testing.T) {
	t.Parallel()
	base, secret := streamingTargetFixture(t)

	payload, _ := json.Marshal(map[string]any{
		"model":    "codex-mock/gpt-5.6-sol",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream for an explicit stream request", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hello") {
		t.Fatalf("streamed body lost the upstream delta: %q", raw)
	}
}
