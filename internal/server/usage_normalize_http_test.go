package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// TestStoredUsageNormalisesMessagesUpstreamOverHTTP is the wiring regression for
// the normalisation key: a Chat client translated onto a Messages target must
// store the whole prompt, not Anthropic's uncached remainder. It also proves the
// Chat client still sees an inclusive prompt_tokens without cache fields.
func TestStoredUsageNormalisesMessagesUpstreamOverHTTP(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-a"}}})
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant", "model": "model-a",
				"content":     []any{map[string]any{"type": "text", "text": "ok"}},
				"stop_reason": "end_turn", "stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":                10,
					"output_tokens":               5,
					"cache_read_input_tokens":     90,
					"cache_creation_input_tokens": 20,
				},
			})
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
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "claude", "type": "anthropic", "base_url": upstream.URL + "/v1", "credential": "provider-secret"})
	if status != 201 {
		t.Fatalf("create provider: %d %v", status, payload)
	}
	providerID := payload["id"].(string)
	status, payload, _ = api.request("GET", "/api/admin/providers/"+providerID+"/models", nil)
	if status != 200 {
		t.Fatal(payload)
	}
	var modelID string
	for _, raw := range payload["data"].([]any) {
		if m := raw.(map[string]any); m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose model-a")
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	secret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	resp, body := clientCall(t, router.URL, secret, "/v1/chat/completions", map[string]any{
		"model":    "claude/model-a",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat call: %d %v", resp.StatusCode, body)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(120) {
		t.Fatalf("client prompt_tokens = %v, want 120", usage["prompt_tokens"])
	}
	if _, present := usage["cache_read_input_tokens"]; present {
		t.Fatalf("Chat client usage must not carry Anthropic cache fields: %v", usage)
	}

	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/activity", nil)
	if status != 200 {
		t.Fatalf("activity: %d %v", status, payload)
	}
	rows := payload["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("activity rows = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["input_tokens"] != float64(120) {
		t.Fatalf("stored input_tokens = %v, want 120 (whole prompt)", row["input_tokens"])
	}
	if row["cache_read_input_tokens"] != float64(90) {
		t.Fatalf("stored cache_read_input_tokens = %v, want 90", row["cache_read_input_tokens"])
	}
	if row["cache_creation_input_tokens"] != float64(20) {
		t.Fatalf("stored cache_creation_input_tokens = %v, want 20", row["cache_creation_input_tokens"])
	}
}
