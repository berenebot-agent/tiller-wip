package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// TestClientKeyGroup tests the organisational group on client keys: defaulting,
// create/update, listing return, search, and filter.
func TestClientKeyGroup(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	defer router.Close()
	jar, _ := cookiejar.New(nil)
	apiClient := &http.Client{Jar: jar}
	api := &testAPI{t: t, base: router.URL, client: apiClient}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	// Default group is "default" when unspecified.
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "hermes", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key: %d %v", status, payload)
	}
	defaultID := payload["id"].(string)

	// Explicit group on create.
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "lab", "group": "testing", "type": "catalogue"})
	if status != 201 {
		t.Fatalf("create key with group: %d %v", status, payload)
	}
	groupedID := payload["id"].(string)

	// Invalid group is rejected.
	status, payload, _ = api.request("POST", "/api/admin/client-keys", map[string]any{"name": "bad", "group": "bad//group", "type": "catalogue"})
	if status != 400 {
		t.Fatalf("invalid group must be rejected: %d %v", status, payload)
	}
	if code, _ := payload["error"].(map[string]any)["code"].(string); code != "invalid_group" {
		t.Fatalf("invalid group code = %v, want invalid_group", code)
	}

	// Listing returns group, defaulting to "default".
	status, payload, _ = api.request("GET", "/api/admin/client-keys", nil)
	if status != 200 {
		t.Fatalf("list keys: %d %v", status, payload)
	}
	byID := map[string]map[string]any{}
	for _, raw := range payload["data"].([]any) {
		c := raw.(map[string]any)
		byID[c["id"].(string)] = c
	}
	if byID[defaultID]["group"] != "default" {
		t.Fatalf("default group = %v, want default", byID[defaultID]["group"])
	}
	if byID[groupedID]["group"] != "testing" {
		t.Fatalf("grouped key group = %v, want testing", byID[groupedID]["group"])
	}

	// Filter by group.
	status, payload, _ = api.request("GET", "/api/admin/client-keys?group=testing", nil)
	if status != 200 {
		t.Fatalf("list filtered: %d %v", status, payload)
	}
	filtered := payload["data"].([]any)
	if len(filtered) != 1 || filtered[0].(map[string]any)["id"] != groupedID {
		t.Fatalf("group filter returned %v, want only grouped key", filtered)
	}

	// Search matches group name.
	status, payload, _ = api.request("GET", "/api/admin/client-keys?search=testin", nil)
	if status != 200 {
		t.Fatalf("list search: %d %v", status, payload)
	}
	searched := payload["data"].([]any)
	if len(searched) != 1 || searched[0].(map[string]any)["id"] != groupedID {
		t.Fatalf("group search returned %v, want only grouped key", searched)
	}

	// Update group.
	status, _, _ = api.request("PATCH", "/api/admin/client-keys/"+defaultID, map[string]any{"group": "prod"})
	if status != 204 {
		t.Fatalf("update group: %d", status)
	}
	status, payload, _ = api.request("GET", "/api/admin/client-keys", nil)
	if status != 200 {
		t.Fatalf("list after update: %d %v", status, payload)
	}
	for _, raw := range payload["data"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == defaultID && c["group"] != "prod" {
			t.Fatalf("updated group = %v, want prod", c["group"])
		}
	}
}
