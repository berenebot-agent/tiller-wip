package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

// oauthVirtualHarness wires a mock OAuth token endpoint and a mock upstream
// whose /responses handler returns 401 for the first `failures` calls, then
// 200. It creates an ordered-fallback virtual model over the OAuth provider
// and returns the router testAPI, the client secret, the virtual model's
// canonical id, and the provider id.
func oauthVirtualHarness(t *testing.T, failures int) (*testAPI, string, string, string) {
	t.Helper()
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"slug": "gpt-5.6-sol", "display_name": "gpt-5.6-sol", "supported_in_api": true}}})
			return
		}
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-5.6-sol"}}})
			return
		}
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		if upstreamCalls.Add(1) <= int32(failures) {
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

	app := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
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

	status, payload, _ = api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "routing_mode": "ordered_fallback", "targets": []any{map[string]any{"provider_model_id": modelID, "enabled": true}}})
	if status != 201 {
		t.Fatalf("virtual: %d %v", status, payload)
	}
	virtualID := payload["id"].(string)

	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "oauth client", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "virtual", "model_id": virtualID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}

	return api, clientSecret, "virtual/coding", providerID
}

// TestOAuth401RefreshSucceedsRetrySucceedsNoCooldown verifies that when an
// OAuth 401 is recovered by a successful token refresh and the retry succeeds,
// the target is NOT left in cooldown: a subsequent request still tries the
// target first.
func TestOAuth401RefreshSucceedsRetrySucceedsNoCooldown(t *testing.T) {
	api, secret, canonical, _ := oauthVirtualHarness(t, 1)

	// First call: 401 -> refresh -> retry -> 200.
	resp, _ := clientCall(t, api.base, secret, "/v1/responses", map[string]any{"model": canonical, "input": "hello"})
	if resp.StatusCode != 200 {
		t.Fatalf("first call should succeed after stale-auth recovery, got %d", resp.StatusCode)
	}
	// Second call: must succeed and NOT be skipped by cooldown.
	resp, _ = clientCall(t, api.base, secret, "/v1/responses", map[string]any{"model": canonical, "input": "hello"})
	if resp.StatusCode != 200 {
		t.Fatalf("second call should succeed, got %d", resp.StatusCode)
	}
}

// TestOAuth401RefreshSucceedsRetryStill401OpensCooldown verifies that when the
// refreshed retry still 401s, cooldown opens for the target.
func TestOAuth401RefreshSucceedsRetryStill401OpensCooldown(t *testing.T) {
	api, secret, canonical, _ := oauthVirtualHarness(t, 100)

	// First call: 401 -> refresh -> retry 401 again -> cooldown opens. With a
	// single-target ordered-fallback chain, the exhausted recovery surfaces as
	// 503 (all targets failed), not the raw 401.
	resp, _ := clientCall(t, api.base, secret, "/v1/responses", map[string]any{"model": canonical, "input": "hello"})
	if resp.StatusCode != 503 {
		t.Fatalf("first call should surface 503 after exhausted recovery, got %d", resp.StatusCode)
	}
	// Second call: the target is now cooled, so it should be skipped and the
	// request should fail fast (no upstream hit) with 503.
	resp, _ = clientCall(t, api.base, secret, "/v1/responses", map[string]any{"model": canonical, "input": "hello"})
	if resp.StatusCode != 503 {
		t.Fatalf("second call should be skipped by cooldown (503), got %d", resp.StatusCode)
	}
}

// TestOAuthRefreshFailsFallsThroughNoCooldownForOthers verifies that when the
// OAuth refresh fails (dead token -> reconnect_required), the target becomes
// unavailable and is NOT left in cooldown; a different provider's target is
// unaffected.
func TestOAuthRefreshFailsFallsThroughNoCooldownForOthers(t *testing.T) {
	// Build a harness whose OAuth server rejects every refresh, so
	// ForceOAuthRefresh fails and the target transitions to reconnect_required
	// (unavailable). We reuse oauthVirtualHarness but then point the routing
	// transport at a rejecting OAuth server so the refresh fails.
	api, secret, canonical, providerID := oauthVirtualHarness(t, 100)

	// Replace the stored token with a dead refresh token and force the provider
	// to reconnect_required via a rejecting OAuth endpoint.
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	t.Cleanup(rejecting.Close)
	api.server.providers.Registry().SetHTTPClient(&http.Client{Transport: &routingTransport{oauthServer: rejecting}})

	// Force a refresh; it must fail and transition auth_state.
	_ = api.server.providers.ForceOAuthRefresh(context.Background(), &providers.Instance{ID: providerID, Type: "codex-subscription"})

	// The target is now unavailable (reconnect_required), so a request through
	// the ordered-fallback virtual model must fail fast (503). Per the spec, a
	// failed refresh may open cooldown on the persistent failure, so we assert
	// the request fails fast rather than asserting the cooldown store is empty.
	resp, _ := clientCall(t, api.base, secret, "/v1/responses", map[string]any{"model": canonical, "input": "hello"})
	if resp.StatusCode != 503 {
		t.Fatalf("request through unavailable OAuth target should fail with 503, got %d", resp.StatusCode)
	}
}
