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
	"github.com/tiller-router/tiller-router/internal/identity"
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
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: dir, ListenAddr: ":8080"}, db)
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
	app2 := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: dir, ListenAddr: ":8080"}, db2)
	router2 := httptest.NewServer(app2.Handler())
	defer router2.Close()
	api2 := &testAPI{t: t, base: router2.URL, client: &http.Client{Jar: jar}}
	if status, payload, _ := api2.request("GET", "/api/admin/session", nil); status != 200 {
		t.Fatalf("session did not survive restart: %d %v", status, payload)
	}
}

// TestLocalOperatorCredentialChangeOnRestart is the regression for the broken
// seed/override/recovery contract: booting with username A and restarting the
// same database with username B and password B must make B the working login,
// invalidate the old A session, and stop A from authenticating at all. Before
// the fix the operator users row kept A's synthetic email, so B was rejected
// before the password was checked.
func TestLocalOperatorCredentialChangeOnRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")

	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestServer(t, config.Config{TillerUser: "alice", TillerUserPassword: "alice-password", DataDir: dir, ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	operator, err := app.identity.LocalOperatorUser(context.Background())
	if err != nil {
		t.Fatalf("operator row missing after first boot: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	if status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "alice", "password": "alice-password"}); status != 200 {
		t.Fatalf("first-boot login: %d %v", status, payload)
	}
	router.Close()
	db.Close()

	// Second boot: same database, username and password both changed.
	db2, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	app2 := newTestServer(t, config.Config{TillerUser: "bob", TillerUserPassword: "bob-password", DataDir: dir, ListenAddr: ":8080"}, db2)
	router2 := httptest.NewServer(app2.Handler())
	t.Cleanup(router2.Close)

	// The operator identity is stable across the credential change.
	operator2, err := app2.identity.LocalOperatorUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if operator2.ID != operator.ID {
		t.Fatalf("operator id changed across credential sync: %q -> %q", operator.ID, operator2.ID)
	}
	if operator2.Email != identity.LocalOperatorEmail("bob") {
		t.Fatalf("operator email = %q, want %q", operator2.Email, identity.LocalOperatorEmail("bob"))
	}

	api2 := &testAPI{t: t, base: router2.URL, client: &http.Client{Jar: jar}, server: app2}
	// The pre-restart A session must be revoked by the credential change.
	if status, _, _ := api2.request("GET", "/api/admin/session", nil); status != http.StatusUnauthorized {
		t.Fatalf("old session survived the credential change: %d", status)
	}
	// The old username/password no longer authenticates.
	if status, _, _ := api2.request("POST", "/api/admin/session", map[string]any{"username": "alice", "password": "alice-password"}); status != http.StatusUnauthorized {
		t.Fatalf("old credentials still authenticate: %d", status)
	}
	// The new username/password works.
	status, payload, _ := api2.request("POST", "/api/admin/session", map[string]any{"username": "bob", "password": "bob-password"})
	if status != 200 {
		t.Fatalf("new credentials after restart: %d %v", status, payload)
	}
	if payload["username"] != "bob" {
		t.Fatalf("login username = %v, want bob", payload["username"])
	}
}
