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

// TestAdminSessionSurvivesRestart verifies the headline behaviour: a login
// session persists across a full process/container restart (DB closed and
// reopened, fresh server) without requiring the administrator to log in again.
func TestAdminSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	jar, _ := cookiejar.New(nil)

	// First "boot".
	db, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: dir, ListenAddr: ":8080"}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	if status, _, _ := api.request("GET", "/api/admin/session", nil); status != 200 {
		t.Fatalf("session status before restart: %d", status)
	}
	router.Close()
	db.Close()

	// Second "boot" against the same data directory.
	db2, err := database.Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	app2, err := New(config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: dir, ListenAddr: ":8080"}, db2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	router2 := httptest.NewServer(app2.Handler())
	defer router2.Close()
	api2 := &testAPI{t: t, base: router2.URL, client: &http.Client{Jar: jar}}
	if status, payload, _ := api2.request("GET", "/api/admin/session", nil); status != 200 {
		t.Fatalf("session did not survive restart: %d %v", status, payload)
	}
}
