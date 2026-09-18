package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

// TestCodexUpstreamUsesExactSSEAccept guards the Codex streaming contract: the
// ChatGPT Codex backend only switches to SSE when Accept is exactly
// text/event-stream (the value codex_cli_rs sends). A combined Accept leaves it
// on the buffered JSON path, which stalls long generations behind proxy read
// timeouts.
func TestCodexUpstreamUsesExactSSEAccept(t *testing.T) {
	var mu sync.Mutex
	var gotAccept, gotSessionID, gotClientRequestID, gotRoutingHint, gotContentType string
	var gotRequest map[string]any
	allowComplete := make(chan struct{})
	upstreamDone := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowComplete) }) }
	t.Cleanup(release)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true}}})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
		case "/responses":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotAccept = r.Header.Get("Accept")
			gotSessionID = r.Header.Get("session-id")
			gotClientRequestID = r.Header.Get("x-client-request-id")
			gotRoutingHint = r.Header.Get("x-codex-routing-hint")
			gotContentType = r.Header.Get("Content-Type")
			_ = json.Unmarshal(body, &gotRequest)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-allowComplete
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"model\":\"gpt-5.6-sol\",\"output\":[]}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			close(upstreamDone)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	app := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "codex-mock", "type": "codex-subscription", "base_url": upstream.URL, "protocols": []any{"responses"}})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)

	future := time.Now().Add(time.Hour)
	store := oauth.NewStore(db.SQL)
	if err := store.Put(context.Background(), oauth.TokenRecord{
		ProviderID:   providerID,
		AccessToken:  "live-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    &future,
		AuthState:    oauth.AuthConnected,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "gpt-5.6-sol" {
			modelID = m["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose gpt-5.6-sol")
	}

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "codex-accept", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	encoded, _ := json.Marshal(map[string]any{"model": "codex-mock/gpt-5.6-sol", "input": "hello", "reasoning": map[string]any{"effort": "xhigh"}})
	request, _ := http.NewRequest("POST", api.base+"/v1/responses", bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+clientSecret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Opencode-Session", "journey-board-test")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		release()
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("request status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "response.output_text.delta") {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			release()
			t.Fatalf("read first SSE event: %v", readErr)
		}
		received.WriteString(line)
	}
	select {
	case <-upstreamDone:
		t.Fatal("upstream completed before the first SSE event reached the client")
	default:
	}
	release()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	received.Write(rest)
	if !strings.Contains(received.String(), "response.completed") {
		t.Fatalf("translated SSE did not complete: %s", received.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAccept != "text/event-stream" {
		t.Fatalf("Codex upstream Accept = %q, want text/event-stream", gotAccept)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Codex upstream Content-Type = %q, want application/json", gotContentType)
	}
	if gotSessionID == "" {
		t.Fatal("Codex upstream did not receive session-id")
	}
	if gotClientRequestID == "" || gotClientRequestID != resp.Header.Get("X-Tiller-Request-Id") {
		t.Fatalf("Codex x-client-request-id = %q, Tiller request id = %q", gotClientRequestID, resp.Header.Get("X-Tiller-Request-Id"))
	}
	if gotRoutingHint != "model=gpt-5.6-sol" {
		t.Fatalf("Codex x-codex-routing-hint = %q", gotRoutingHint)
	}
	if gotRequest["stream"] != true || gotRequest["store"] != false {
		t.Fatalf("Codex request stream/store = %v/%v, want true/false", gotRequest["stream"], gotRequest["store"])
	}
	reasoning, _ := gotRequest["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("Codex request reasoning effort = %v, want xhigh", reasoning["effort"])
	}
}
