package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// hostedAccountServer builds a hosted Server with one verified customer and
// logs it in, returning an authenticated test API. Mail is delivered through
// the durable outbox; because no provider is configured, sends defer without
// touching the network.
func hostedAccountServer(t *testing.T) (*Server, *testAPI, string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{
		Mode: config.ModeHosted, TillerUser: "owner@example.com", TillerUserPassword: "correct horse battery staple",
		TillerPlatformAdminUser: "platform-admin", TillerPlatformAdminPassword: "platform-secret",
		PublicURL: "https://tiller.example.com", TrustedProxy: netip.MustParsePrefix("127.0.0.1/32"), DataDir: t.TempDir(),
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	app.liveHub.timings = testLiveTimings
	app.usageCacheTTL = 0

	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/auth/login", map[string]any{"email": "owner@example.com", "password": "correct horse battery staple"})
	if status != 200 {
		t.Fatalf("customer login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	return app, api, payload["account_id"].(string)
}

func TestHostedAccountPageProfileAndPassword(t *testing.T) {
	_, api, accountID := hostedAccountServer(t)

	status, profile, _ := api.request("GET", "/api/auth/account", nil)
	if status != 200 || profile["email"] != "owner@example.com" || profile["account_id"] != accountID {
		t.Fatalf("profile: %d %v", status, profile)
	}

	status, _, _ = api.request("POST", "/api/auth/account/password", map[string]any{"current_password": "wrong", "new_password": "another correct horse battery"})
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong current password status = %d, want 401", status)
	}
	status, _, _ = api.request("POST", "/api/auth/account/password", map[string]any{"current_password": "correct horse battery staple", "new_password": "another correct horse battery"})
	if status != 200 {
		t.Fatalf("change password status = %d, want 200", status)
	}
	// The session survives the change, so the account endpoint still works.
	status, _, _ = api.request("GET", "/api/auth/account", nil)
	if status != 200 {
		t.Fatalf("session revoked by own password change: %d", status)
	}
}

func TestHostedEmailChangeFlow(t *testing.T) {
	_, api, _ := hostedAccountServer(t)

	status, payload, _ := api.request("POST", "/api/auth/account/email", map[string]any{"new_email": "New@Example.com", "password": "correct horse battery staple"})
	if status != http.StatusAccepted {
		t.Fatalf("request email change: %d %v", status, payload)
	}

	// In tests no cipher is injected, so the outbox stores the raw token
	// verbatim; production encrypts it (covered in internal/mailoutbox).
	var rawToken string
	if err := api.server.db.SQL.QueryRow(`SELECT token_ciphertext FROM mail_outbox WHERE type='email_change_confirm'`).Scan(&rawToken); err != nil {
		t.Fatal(err)
	}
	if rawToken == "" {
		t.Fatal("confirmation mail carried no token")
	}
	var pending int
	if err := api.server.db.SQL.QueryRow(`SELECT count(*) FROM email_change_tokens`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending email-change tokens = %d, want 1", pending)
	}
	// The old address still logs in until the new one is confirmed.
	status, _, _ = api.request("POST", "/api/auth/login", map[string]any{"email": "owner@example.com", "password": "correct horse battery staple"})
	if status != 200 {
		t.Fatalf("old email login after change request = %d, want 200", status)
	}
	status, _, _ = api.request("POST", "/api/auth/email-change/confirm", map[string]any{"token": rawToken})
	if status != 200 {
		t.Fatalf("confirm email change: %d", status)
	}
	if _, confirm, _ := api.request("POST", "/api/auth/login", map[string]any{"email": "new@example.com", "password": "correct horse battery staple"}); confirm == nil {
		t.Fatal("new email could not log in after confirmation")
	}
}

func TestHostedSelfDeletePurgesAccountAndRevokesSession(t *testing.T) {
	app, api, accountID := hostedAccountServer(t)

	// Wrong confirmation value is rejected.
	status, _, _ := api.request("DELETE", "/api/auth/account", map[string]any{"confirm": "someone@else.com", "password": "correct horse battery staple"})
	if status != http.StatusBadRequest {
		t.Fatalf("bad confirmation status = %d, want 400", status)
	}
	status, payload, _ := api.request("DELETE", "/api/auth/account", map[string]any{"confirm": "owner@example.com", "password": "correct horse battery staple"})
	if status != 200 {
		t.Fatalf("self delete: %d %v", status, payload)
	}
	var accounts int
	if err := app.db.SQL.QueryRow(`SELECT count(*) FROM accounts WHERE id=?`, accountID).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if accounts != 0 {
		t.Fatalf("account row survived self delete: %d", accounts)
	}
	var users int
	if err := app.db.SQL.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Fatalf("user row survived self delete: %d", users)
	}
	// Audit history is deliberately retained.
	var audit int
	if err := app.db.SQL.QueryRow(`SELECT count(*) FROM account_audit_events WHERE account_id=? AND event='user.account_delete_requested'`, accountID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 1 {
		t.Fatalf("account-delete audit events = %d, want 1", audit)
	}
	// The session no longer authenticates.
	status, _, _ = api.request("GET", "/api/auth/account", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("session survived self delete: %d, want 401", status)
	}
}

func TestHostedRevokeAllSessions(t *testing.T) {
	_, api, _ := hostedAccountServer(t)
	status, _, _ := api.request("POST", "/api/auth/account/sessions/revoke-all", map[string]any{})
	if status != http.StatusNoContent {
		t.Fatalf("revoke-all status = %d, want 204", status)
	}
	status, _, _ = api.request("GET", "/api/auth/account", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("session survived revoke-all: %d, want 401", status)
	}
}

func TestHostedAccountEndpointsRequireAuth(t *testing.T) {
	app, _, _ := hostedAccountServer(t)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	for _, tc := range []struct {
		method, path string
	}{
		{"GET", "/api/auth/account"},
		{"POST", "/api/auth/account/password"},
		{"POST", "/api/auth/account/email"},
		{"POST", "/api/auth/account/sessions/revoke-all"},
		{"DELETE", "/api/auth/account"},
	} {
		req, err := http.NewRequest(tc.method, router.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated status = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestPlatformMailQueueEndpoint(t *testing.T) {
	app, _, _ := hostedAccountServer(t)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request("POST", "/api/platform/session", map[string]any{"username": "platform-admin", "password": "platform-secret"})
	if status != 200 {
		t.Fatalf("platform login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	status, queue, _ := api.request("GET", "/api/platform/mail/queue", nil)
	if status != 200 {
		t.Fatalf("mail queue: %d %v", status, queue)
	}
	if _, ok := queue["queued"]; !ok {
		t.Fatalf("mail queue payload missing queued: %v", queue)
	}
}
