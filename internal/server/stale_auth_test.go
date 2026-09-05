package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

// routingTransport redirects OAuth token requests (to auth.openai.com) to a
// mock OAuth server while letting all other requests pass through.
type routingTransport struct {
	oauthServer *httptest.Server
}

func (t *routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "auth.openai.com") {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(t.oauthServer.URL, "http://")
		return http.DefaultTransport.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// mockOAuthAndUpstream wires up a mock OAuth token endpoint and a mock upstream
// that returns 401 until a refresh is forced, then 200. It returns the router
// testAPI, the client secret, the provider ID, and a cleanup func.
func mockOAuthAndUpstream(t *testing.T) (*testAPI, string, string, func()) {
	t.Helper()
	var upstream401Count atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
			return
		}
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		if upstream401Count.Add(1) <= 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "token expired"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-1", "object": "response", "model": "gpt-5.6-sol", "output_text": "ok"})
	}))
	t.Cleanup(upstream.Close)

	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-token",
			"refresh_token": "rotated-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email offline_access",
		})
	}))
	t.Cleanup(oauthServer.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	app.providers.Registry().SetHTTPClient(&http.Client{Transport: &routingTransport{oauthServer: oauthServer}})

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

	expired := time.Now().Add(-time.Minute)
	store := oauth.NewStore(db.SQL)
	if err := store.Put(context.Background(), oauth.TokenRecord{
		ProviderID:   providerID,
		AccessToken:  "stale-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    &expired,
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

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "stale-auth", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	return api, clientSecret, providerID, func() {}
}

// TestStaleAuthRecovery verifies the 401 → force-refresh → retry path:
// when an upstream returns 401, the router should force-refresh the OAuth token
// once and retry the same target before falling through to normal fallback.
func TestStaleAuthRecovery(t *testing.T) {
	api, secret, _, cleanup := mockOAuthAndUpstream(t)
	defer cleanup()

	resp, _ := clientCall(t, api.base, secret, "/v1/responses", map[string]any{"model": "codex-mock/gpt-5.6-sol", "input": "hello"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after stale-auth recovery, got %d", resp.StatusCode)
	}
}

// TestStaleAuthRecoveryPreservesTokenAfterRestart verifies that a persisted
// token survives a router restart and the status endpoint reports "connected".
func TestStaleAuthRecoveryPreservesTokenAfterRestart(t *testing.T) {
	api, _, providerID, cleanup := mockOAuthAndUpstream(t)
	defer cleanup()

	status, payload, _ := api.request("GET", "/api/admin/providers/"+providerID+"/oauth/status", nil)
	if status != 200 {
		t.Fatalf("status: %d %v", status, payload)
	}
	if payload["status"] != "connected" {
		t.Fatalf("expected status=connected, got %v", payload["status"])
	}
}

// TestForceRefreshTransitionsStateOnDeadToken verifies that when the refresh
// token is dead, auth_state transitions to reconnect_required.
func TestForceRefreshTransitionsStateOnDeadToken(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('oauth-provider','real','provider-dead')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('provider-dead','oauth-provider','codex-subscription','https://provider.invalid',1,'["responses"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}

	expired := time.Now().Add(-time.Minute)
	store := oauth.NewStore(db.SQL)
	if err := store.Put(context.Background(), oauth.TokenRecord{
		ProviderID:   "provider-dead",
		AccessToken:  "stale",
		RefreshToken: "dead-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    &expired,
		AuthState:    oauth.AuthConnected,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Mock OAuth server that rejects all refresh attempts.
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	defer oauthServer.Close()

	registry := providers.NewRegistry()
	registry.SetHTTPClient(&http.Client{Transport: &routingTransport{oauthServer: oauthServer}})
	mgr := providers.NewManager(db.SQL, registry)

	err = mgr.ForceOAuthRefresh(context.Background(), &providers.Instance{ID: "provider-dead", Type: "codex-subscription"})
	if err == nil {
		t.Fatal("expected error from dead refresh token")
	}

	record, err := store.Get(context.Background(), "provider-dead")
	if err != nil {
		t.Fatal(err)
	}
	if record.AuthState != oauth.AuthReconnectRequired {
		t.Fatalf("auth_state = %q, want reconnect_required", record.AuthState)
	}
}
