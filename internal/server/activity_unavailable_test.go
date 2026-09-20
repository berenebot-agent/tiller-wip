package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// activityUnavailableHarness is loggingTestHarness with the Activity database
// handle removed before the server is built, simulating activity.db failing to
// open. The core database, provider, and client key are all set up normally.
func activityUnavailableHarness(t *testing.T, upstream http.HandlerFunc) (*testAPI, *database.DB, string, string) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the real handle so it can be closed, then make it unavailable.
	activity := db.Activity
	db.Activity = nil
	t.Cleanup(func() {
		if activity != nil {
			_ = activity.Close()
		}
		db.Close()
	})
	if db.Activity != nil {
		t.Fatal("test setup: activity handle should be nil")
	}
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
	status, payload, _ = api.request("POST", "/api/admin/providers", map[string]any{"name": "provider-a", "type": "generic-openai", "base_url": server.URL + "/v1", "credential": "provider-secret"})
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
		m := raw.(map[string]any)
		if m["upstream_model_id"] == "model-a" {
			modelID = m["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatal("mock upstream did not expose model-a")
	}
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "test client", "description": "logging", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)
	clientSecret := payload["secret"].(string)
	status, payload, _ = api.request("PUT", "/api/admin/client-keys/"+clientID+"/permissions", map[string]any{"defaults": []any{}, "permissions": []any{map[string]any{"kind": "real", "model_id": modelID, "enabled": true}}})
	if status != 204 {
		t.Fatalf("permissions: %d %v", status, payload)
	}
	return api, db, clientID, clientSecret
}

// TestInferenceSucceedsWhenActivityUnavailable proves routing is unaffected
// when activity.db cannot be opened: an inference request still succeeds and
// the best-effort Activity write does not fail the request.
func TestInferenceSucceedsWhenActivityUnavailable(t *testing.T) {
	api, _, _, secret := activityUnavailableHarness(t, mockUpstream(t))
	resp, payload := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("inference with Activity unavailable: %d %v", resp.StatusCode, payload)
	}
}

// TestActivityReadsReportUnavailable proves Activity endpoints return an
// explicit 503 activity_unavailable rather than an empty list or a generic
// database 500 when the Activity store is down.
func TestActivityReadsReportUnavailable(t *testing.T) {
	api, _, clientID, _ := activityUnavailableHarness(t, mockUpstream(t))

	status, payload, _ := api.request("GET", "/api/admin/activity?limit=50&offset=0", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("global activity status = %d, want 503 (%v)", status, payload)
	}
	if code := errorCode(payload); code != "activity_unavailable" {
		t.Fatalf("global activity code = %q, want activity_unavailable", code)
	}

	status, payload, _ = api.request("GET", "/api/admin/client-keys/"+clientID+"/activity?limit=50&offset=0", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("client activity status = %d, want 503 (%v)", status, payload)
	}
	if code := errorCode(payload); code != "activity_unavailable" {
		t.Fatalf("client activity code = %q, want activity_unavailable", code)
	}
}

// TestAdminHealthReportsActivityDegraded proves the admin health endpoint
// reports Activity unavailability as a degraded state rather than a total
// service failure.
func TestAdminHealthReportsActivityDegraded(t *testing.T) {
	api, _, _, _ := activityUnavailableHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/health", nil)
	if status != 200 {
		t.Fatalf("admin health status = %d, want 200 (%v)", status, payload)
	}
	if payload["status"] != "degraded" {
		t.Fatalf("admin health status = %v, want degraded", payload["status"])
	}
	if payload["activity_available"] != false {
		t.Fatalf("activity_available = %v, want false", payload["activity_available"])
	}
}

// errorCode extracts the nested error.code from an admin error payload.
func errorCode(payload map[string]any) string {
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		return ""
	}
	code, _ := errObj["code"].(string)
	return code
}
