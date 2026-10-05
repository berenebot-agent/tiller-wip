package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// firstRunHarness builds a local server with no admin credential, exactly like
// a fresh `docker compose up`, and returns a cookie-jar API client.
func firstRunHarness(t *testing.T) (*Server, *testAPI) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return app, &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
}

func TestRuntimeFirstRunSignals(t *testing.T) {
	_, api := firstRunHarness(t)
	status, payload, _ := api.request("GET", "/api/runtime", nil)
	if status != 200 {
		t.Fatalf("runtime status = %d", status)
	}
	if payload["setup_required"] != true {
		t.Fatalf("fresh local runtime = %v, want setup_required true", payload)
	}
	if payload["wizard_enabled"] != true {
		t.Fatalf("fresh local runtime wizard_enabled = %v, want true", payload["wizard_enabled"])
	}
}

// TestSetupClaimsInstanceAndMintsSession is the happy path: the first visitor
// sets credentials, gets an authenticated session, and the endpoint disappears.
func TestSetupClaimsInstanceAndMintsSession(t *testing.T) {
	app, api := firstRunHarness(t)
	status, payload, header := api.request("POST", "/api/admin/setup", map[string]any{"username": "owner", "password": "wizard-password"})
	if status != 200 {
		t.Fatalf("setup status = %d %v", status, payload)
	}
	if payload["authenticated"] != true || payload["username"] != "owner" {
		t.Fatalf("setup payload = %v", payload)
	}
	if !strings.Contains(strings.Join(header.Values("Set-Cookie"), ";"), sessionCookie) {
		t.Fatalf("setup did not set a session cookie: %v", header.Values("Set-Cookie"))
	}
	if payload["csrf_token"] == nil || payload["csrf_token"] == "" {
		t.Fatalf("setup did not return a csrf token: %v", payload)
	}
	if !app.sessions.CredentialConfigured() {
		t.Fatal("credential not configured after setup")
	}
	// The setup also materialises the local operator identity row (unified
	// credential storage), which the Account panel and passkeys depend on.
	if _, err := app.identity.LocalOperatorUser(context.Background()); err != nil {
		t.Fatalf("setup did not create the local operator row: %v", err)
	}
	status, payload, _ = api.request("GET", "/api/admin/account", nil)
	if status != 200 {
		t.Fatalf("local account profile = %d %v, want 200", status, payload)
	}
	if payload["account_id"] != database.LocalAccountID {
		t.Fatalf("local account profile account_id = %v, want LocalAccountID", payload["account_id"])
	}

	// The session minted by setup is immediately usable on an admin endpoint.
	api.csrf, _ = payload["csrf_token"].(string)
	status, _, _ = api.request("GET", "/api/admin/settings", nil)
	if status != 200 {
		t.Fatalf("setup session could not read settings: %d", status)
	}

	// Runtime flips and the setup route is gone.
	status, payload, _ = api.request("GET", "/api/runtime", nil)
	if status != 200 || payload["setup_required"] != false {
		t.Fatalf("runtime after setup = %d %v, want setup_required false", status, payload)
	}
	status, payload, _ = api.request("POST", "/api/admin/setup", map[string]any{"username": "intruder", "password": "intruder-password"})
	if status != http.StatusNotFound {
		t.Fatalf("post-setup claim = %d %v, want 404", status, payload)
	}
}

// TestSetupSecondClaimConflicts proves the one-shot write: a racing or replayed
// claim after the first gets 409 (or 404 once the route disappears) and can
// never replace the administrator.
func TestSetupSecondClaimConflicts(t *testing.T) {
	app, api := firstRunHarness(t)
	// Pre-set the credential behind the route to simulate a winning concurrent
	// claim, then the loser's request must conflict rather than overwrite.
	if err := app.sessions.SetCredential("first", "first-password"); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("POST", "/api/admin/setup", map[string]any{"username": "second", "password": "second-password"})
	if status != http.StatusNotFound && status != http.StatusConflict {
		t.Fatalf("losing claim = %d %v, want 404 or 409", status, payload)
	}
	if !app.sessions.VerifyCredential("first", "first-password") {
		t.Fatal("losing claim replaced the administrator credential")
	}
	if app.sessions.VerifyCredential("second", "second-password") {
		t.Fatal("losing claim's credential was stored")
	}
}

// TestSetupRejectsWrongOriginAndBadInput covers the pre-login endpoint's
// validation layers.
func TestSetupRejectsWrongOriginAndBadInput(t *testing.T) {
	app, api := firstRunHarness(t)

	// Cross-site Origin is rejected before any credential work.
	body := strings.NewReader(`{"username":"owner","password":"wizard-password"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/setup", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-origin setup = %d, want 400", w.Code)
	}
	if app.sessions.CredentialConfigured() {
		t.Fatal("cross-origin setup configured a credential")
	}

	// Short password rejected.
	status, payload, _ := api.request("POST", "/api/admin/setup", map[string]any{"username": "owner", "password": "short"})
	if status != http.StatusBadRequest || errorCode(payload) != "invalid_password" {
		t.Fatalf("short password = %d %v, want 400 invalid_password", status, payload)
	}
	// Empty username rejected.
	status, payload, _ = api.request("POST", "/api/admin/setup", map[string]any{"username": "   ", "password": "wizard-password"})
	if status != http.StatusBadRequest || errorCode(payload) != "invalid_username" {
		t.Fatalf("empty username = %d %v, want 400 invalid_username", status, payload)
	}
	if app.sessions.CredentialConfigured() {
		t.Fatal("rejected setup still configured a credential")
	}
}

// TestSetupRateLimited proves the per-IP attempt budget bounds unauthenticated
// argon2id work.
func TestSetupRateLimited(t *testing.T) {
	_, api := firstRunHarness(t)
	for i := 0; i < 20; i++ {
		status, payload, _ := api.request("POST", "/api/admin/setup", map[string]any{"username": "owner", "password": "bad"})
		if status != http.StatusBadRequest {
			t.Fatalf("attempt %d = %d %v, want 400", i+1, status, payload)
		}
	}
	status, payload, _ := api.request("POST", "/api/admin/setup", map[string]any{"username": "owner", "password": "wizard-password"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("attempt beyond budget = %d %v, want 429", status, payload)
	}
}

// TestSetupAbsentWithEnvAdmin proves env-admin installs never offer setup and
// hide the onboarding wizard.
func TestSetupAbsentWithEnvAdmin(t *testing.T) {
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

	status, payload, _ := api.request("GET", "/api/runtime", nil)
	if status != 200 || payload["setup_required"] != false {
		t.Fatalf("env-admin runtime = %d %v, want setup_required false", status, payload)
	}
	if payload["wizard_enabled"] != false {
		t.Fatalf("env-admin wizard_enabled = %v, want false", payload["wizard_enabled"])
	}
	status, _, _ = api.request("POST", "/api/admin/setup", map[string]any{"username": "intruder", "password": "intruder-password"})
	if status != http.StatusNotFound {
		t.Fatalf("env-admin setup = %d, want 404", status)
	}
}

// TestLoginUsesStoredCredentialAfterRestart proves the wizard-created
// credential authenticates through the login endpoint after a database reopen
// with no env credentials, and that the UI identity reflects it.
func TestLoginUsesStoredCredentialAfterRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestServer(t, config.Config{DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, header := api.request("POST", "/api/admin/setup", map[string]any{"username": "owner", "password": "wizard-password"})
	if status != 200 {
		t.Fatalf("setup: %d %v", status, payload)
	}
	if !strings.Contains(strings.Join(header.Values("Set-Cookie"), ";"), sessionCookie) {
		t.Fatal("setup did not authenticate")
	}
	router.Close()
	db.Close()

	reopened, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	restarted := newTestServer(t, config.Config{DataDir: t.TempDir(), ListenAddr: ":8081"}, reopened)
	router2 := httptest.NewServer(restarted.Handler())
	t.Cleanup(router2.Close)
	jar2, _ := cookiejar.New(nil)
	api2 := &testAPI{t: t, base: router2.URL, client: &http.Client{Jar: jar2}, server: restarted}

	status, payload, _ = api2.request("GET", "/api/runtime", nil)
	if status != 200 || payload["setup_required"] != false {
		t.Fatalf("restart runtime = %d %v, want configured", status, payload)
	}
	status, payload, _ = api2.request("POST", "/api/admin/session", map[string]any{"username": "owner", "password": "wizard-password"})
	if status != 200 {
		t.Fatalf("post-restart login = %d %v", status, payload)
	}
	if payload["username"] != "owner" {
		t.Fatalf("post-restart login username = %v, want owner", payload["username"])
	}
	status, payload, _ = api2.request("POST", "/api/admin/session", map[string]any{"username": "owner", "password": "wrong-password"})
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong password after restart = %d %v, want 401", status, payload)
	}
}

// TestLocalOnboardingRoutes proves the wizard state API works against the
// implicit local account and is auth-gated.
func TestLocalOnboardingRoutes(t *testing.T) {
	app, api := firstRunHarness(t)
	// Unauthenticated state read is rejected.
	status, _, _ := api.request("GET", "/api/auth/onboarding", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated onboarding = %d, want 401", status)
	}
	status, payload, _ := api.request("POST", "/api/admin/setup", map[string]any{"username": "owner", "password": "wizard-password"})
	if status != 200 {
		t.Fatalf("setup: %d %v", status, payload)
	}
	api.csrf, _ = payload["csrf_token"].(string)
	status, payload, _ = api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 || payload["needs_onboarding"] != true {
		t.Fatalf("fresh local onboarding = %d %v, want needs_onboarding true", status, payload)
	}
	status, _, _ = api.request("POST", "/api/auth/onboarding/dismiss", nil)
	if status != http.StatusNoContent {
		t.Fatalf("local dismiss = %d, want 204", status)
	}
	status, payload, _ = api.request("GET", "/api/auth/onboarding", nil)
	if status != 200 || payload["needs_onboarding"] != false || payload["dismissed"] != true {
		t.Fatalf("local dismissal not persisted: %d %v", status, payload)
	}
	_ = app
}

// TestHostedRuntimePayloadUnchanged keeps hosted mode's public payload as-is:
// setup fields are a local-mode addition only.
func TestHostedRuntimePayloadUnchanged(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{
		Mode: config.ModeHosted, TillerPlatformAdminUser: "platform-admin", TillerPlatformAdminPassword: "platform-secret",
		PublicURL: "https://tiller.example.com", TrustedProxy: netip.MustParsePrefix("127.0.0.1/32"), DataDir: t.TempDir(),
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	app.usageCacheTTL = 0
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	resp, err := http.Get(router.URL + "/api/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["mode"] != "hosted" {
		t.Fatalf("hosted runtime mode = %v", payload["mode"])
	}
	for _, key := range []string{"setup_required", "wizard_enabled"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("hosted runtime leaked local-only %q: %v", key, payload)
		}
	}
}
