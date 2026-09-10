package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
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
	var gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true}}})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
		case "/responses":
			mu.Lock()
			gotAccept = r.Header.Get("Accept")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-1", "object": "response", "model": "gpt-5.6-sol", "output_text": "ok"})
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

	resp, _ := clientCall(t, api.base, clientSecret, "/v1/responses", map[string]any{"model": "codex-mock/gpt-5.6-sol", "input": "hello"})
	if resp.StatusCode != 200 {
		t.Fatalf("request status = %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAccept != "text/event-stream" {
		t.Fatalf("Codex upstream Accept = %q, want text/event-stream", gotAccept)
	}
}
