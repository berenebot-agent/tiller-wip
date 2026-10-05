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
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// localPasskeyServer builds a local-mode server with WebAuthn configured (via
// TILLER_PUBLIC_URL) and an environment admin credential, and logs the operator
// in. This is the standalone path that now shares the users table with hosted.
func localPasskeyServer(t *testing.T) (*Server, *testAPI) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Config{
		DataDir:            t.TempDir(),
		ListenAddr:         ":8080",
		PublicURL:          "https://tiller.example.com",
		TillerUser:         "admin",
		TillerUserPassword: "correct horse",
	}
	app, err := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	if !app.identity.PasskeysEnabled() {
		t.Fatal("local server did not configure WebAuthn from PublicURL")
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "correct horse"})
	if status != 200 {
		t.Fatalf("local login = %d %v", status, payload)
	}
	api.csrf, _ = payload["csrf_token"].(string)
	return app, api
}

// TestLocalOperatorLoginResolvesIdentity proves the env-admin login now resolves
// the local operator users row (unified credential storage) and can read the
// local account profile.
func TestLocalOperatorLoginResolvesIdentity(t *testing.T) {
	app, api := localPasskeyServer(t)
	u, err := app.identity.LocalOperatorUser(context.Background())
	if err != nil {
		t.Fatalf("local operator row missing: %v", err)
	}
	if u.AccountID != database.LocalAccountID {
		t.Fatalf("operator account = %q, want LocalAccountID", u.AccountID)
	}
	status, payload, _ := api.request("GET", "/api/admin/account", nil)
	if status != 200 {
		t.Fatalf("local account profile = %d %v", status, payload)
	}
	if payload["passkeys_enabled"] != true {
		t.Fatalf("passkeys_enabled = %v, want true", payload["passkeys_enabled"])
	}
}

// TestLocalPasskeyRegisterAndLogin drives the full standalone passkey lifecycle:
// register a passkey on the operator via the admin session, then log out and
// sign in with it, ending with a usable admin session.
func TestLocalPasskeyRegisterAndLogin(t *testing.T) {
	app, api := localPasskeyServer(t)
	auth := newHTTPTestAuthenticator(t)
	operator, err := app.identity.LocalOperatorUser(context.Background())
	if err != nil {
		t.Fatalf("local operator row missing: %v", err)
	}

	// Register (management endpoints are behind requireAdminUser locally).
	status, begin, _ := api.request("POST", "/api/auth/account/passkeys/register/begin", map[string]any{})
	if status != 200 {
		t.Fatalf("register begin = %d %v", status, begin)
	}
	token := begin["challenge_token"].(string)
	body := auth.registrationBody(t, "tiller.example.com", beginChallenge(t, begin), "https://tiller.example.com")
	status, payload, _ := api.requestWithHeaders("POST", "/api/auth/account/passkeys/register/finish?name=Laptop", body, map[string]string{challengeHeader: token})
	if status != 200 {
		t.Fatalf("register finish = %d %v", status, payload)
	}

	// Log out, then sign in with the passkey.
	status, _, _ = api.request("DELETE", "/api/admin/session", nil)
	if status != http.StatusNoContent {
		t.Fatalf("logout = %d", status)
	}
	status, begin, _ = api.request("POST", "/api/auth/passkey/begin", map[string]any{})
	if status != 200 {
		t.Fatalf("passkey login begin = %d %v", status, begin)
	}
	challenge := beginChallenge(t, begin)
	beginToken := begin["challenge_token"].(string)
	assertion := auth.assertionBody(t, "tiller.example.com", challenge, "https://tiller.example.com", operator.ID)
	status, session, _ := api.requestWithHeaders("POST", "/api/auth/passkey/finish", assertion, map[string]string{challengeHeader: beginToken})
	if status != 200 {
		t.Fatalf("passkey login finish = %d %v", status, session)
	}
	if session["authenticated"] != true {
		t.Fatalf("passkey login did not authenticate locally: %v", session)
	}
	api.csrf, _ = session["csrf_token"].(string)
	// The minted admin session must work on a protected endpoint.
	status, _, _ = api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("passkey-minted admin session could not read settings: %d", status)
	}
}

// TestLocalPasskeyUnavailableWithoutPublicURL proves a plain local install (no
// PublicURL) reports passkeys disabled and 501s the endpoints.
func TestLocalPasskeyUnavailableWithoutPublicURL(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{DataDir: t.TempDir(), ListenAddr: ":8080", TillerUser: "admin", TillerUserPassword: "correct horse"}, db)
	if app.identity.PasskeysEnabled() {
		t.Fatal("passkeys enabled without a public URL")
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("GET", "/api/runtime", nil)
	if status != 200 || payload["passkeys_enabled"] != false {
		t.Fatalf("runtime passkeys_enabled = %v, want false", payload["passkeys_enabled"])
	}
	status, _, _ = api.request("POST", "/api/auth/passkey/begin", map[string]any{})
	if status != http.StatusNotImplemented {
		t.Fatalf("passkey begin without PublicURL = %d, want 501", status)
	}
}
