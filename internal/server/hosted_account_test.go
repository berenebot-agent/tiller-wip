package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestHostedSelfDeleteConsumesGoogleReauthOnlyAfterEmailConfirmation(t *testing.T) {
	t.Parallel()
	app, api, accountID := hostedAccountServer(t)
	var userID string
	if err := app.db.SQL.QueryRow(`SELECT owner_user_id FROM accounts WHERE id=?`, accountID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SQL.Exec(`INSERT INTO user_identities(id,user_id,provider,subject,email,created_at) VALUES('google-id',?,'google','google-subject','owner@example.com','now')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SQL.Exec(`UPDATE users SET password_auth_enabled=0 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	baseURL, err := url.Parse(api.base)
	if err != nil {
		t.Fatal(err)
	}
	var sessionToken string
	for _, cookie := range api.client.Jar.Cookies(baseURL) {
		if cookie.Name == userSessionCookie {
			sessionToken = cookie.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("user session cookie missing")
	}
	app.grantGoogleReauth(sessionToken)
	status, _, _ := api.request("DELETE", "/api/auth/account", map[string]any{"confirm": "wrong@example.com", "password": ""})
	if status != http.StatusBadRequest {
		t.Fatalf("wrong email confirmation status = %d", status)
	}
	status, _, _ = api.request("DELETE", "/api/auth/account", map[string]any{"confirm": "owner@example.com", "password": ""})
	if status != http.StatusOK {
		t.Fatalf("self delete after valid email confirmation: %d", status)
	}
}

func TestHostedGoogleUnlinkRequiresGoogleReauthAndRestoresPassword(t *testing.T) {
	t.Parallel()
	app, api, accountID := hostedAccountServer(t)
	var userID string
	if err := app.db.SQL.QueryRow(`SELECT owner_user_id FROM accounts WHERE id=?`, accountID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SQL.Exec(`INSERT INTO user_identities(id,user_id,provider,subject,email,created_at) VALUES('google-id',?,'google','google-subject','owner@example.com','now')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SQL.Exec(`UPDATE users SET password_auth_enabled=0 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	status, _, _ := api.request("DELETE", "/api/auth/account/google", map[string]any{})
	if status != http.StatusUnauthorized {
		t.Fatalf("unlink without Google reauth = %d, want 401", status)
	}
	baseURL, err := url.Parse(api.base)
	if err != nil {
		t.Fatal(err)
	}
	var sessionToken string
	for _, cookie := range api.client.Jar.Cookies(baseURL) {
		if cookie.Name == userSessionCookie {
			sessionToken = cookie.Value
		}
	}
	app.grantGoogleReauth(sessionToken)
	status, _, _ = api.request("DELETE", "/api/auth/account/google", nil)
	if status != http.StatusNoContent {
		t.Fatalf("Google unlink = %d, want 204", status)
	}
	status, _, _ = api.request("POST", "/api/auth/login", map[string]any{"email": "owner@example.com", "password": "correct horse battery staple"})
	if status != http.StatusOK {
		t.Fatalf("password login after unlink = %d, want 200", status)
	}
}

func TestHostedGoogleOnlyUserCannotUnlinkLastSignInMethod(t *testing.T) {
	t.Parallel()
	app, api, accountID := hostedAccountServer(t)
	var userID string
	if err := app.db.SQL.QueryRow(`SELECT owner_user_id FROM accounts WHERE id=?`, accountID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SQL.Exec(`INSERT INTO user_identities(id,user_id,provider,subject,email,created_at) VALUES('google-id',?,'google','google-subject','owner@example.com','now')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SQL.Exec(`UPDATE users SET password_auth_enabled=0,password_hash='' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	baseURL, err := url.Parse(api.base)
	if err != nil {
		t.Fatal(err)
	}
	var sessionToken string
	for _, cookie := range api.client.Jar.Cookies(baseURL) {
		if cookie.Name == userSessionCookie {
			sessionToken = cookie.Value
		}
	}
	app.grantGoogleReauth(sessionToken)
	status, payload, _ := api.request("DELETE", "/api/auth/account/google", nil)
	if status != http.StatusConflict || payload["error"] == nil {
		t.Fatalf("unlink with no fallback auth = %d %v", status, payload)
	}
}

func TestHostedRevokeAllSessions(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if _, ok := queue["sent_recent"]; !ok {
		t.Fatalf("mail queue payload missing sent_recent: %v", queue)
	}
	if _, ok := queue["log"]; !ok {
		t.Fatalf("mail queue payload missing log: %v", queue)
	}
}

// TestVerifyEmailSetsPasswordFromLinkHolder drives the HTTP flow: verification
// requires a password, a weak one is rejected without burning the token, and
// the chosen one opens a session and logs in.
func TestVerifyEmailSetsPasswordFromLinkHolder(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	ctx := context.Background()
	signup, err := app.identity.CreateSignup(ctx, "squatted@example.com")
	if err != nil {
		t.Fatal(err)
	}
	anon := &testAPI{t: t, base: api.base, client: &http.Client{}, server: app}
	status, _, _ := anon.request("POST", "/api/auth/verify-email", map[string]any{"token": signup.VerificationToken, "password": "short"})
	if status != 400 {
		t.Fatalf("weak password status = %d, want 400", status)
	}
	status, payload, header := anon.request("POST", "/api/auth/verify-email", map[string]any{"token": signup.VerificationToken, "password": "owner chosen passphrase"})
	if status != 200 || payload["verified"] != true || payload["authenticated"] != true {
		t.Fatalf("verify: %d %v", status, payload)
	}
	if !strings.Contains(strings.Join(header.Values("Set-Cookie"), ";"), userSessionCookie) {
		t.Fatal("verification did not open a session")
	}
	status, _, _ = anon.request("POST", "/api/auth/login", map[string]any{"email": "squatted@example.com", "password": "owner chosen passphrase"})
	if status != 200 {
		t.Fatalf("login with chosen password = %d", status)
	}
}

// TestPlatformLoginGlobalLimitAcrossIPs proves guesses spread over many client
// addresses still hit a shared limit, so the per-IP lockout cannot be dodged.
func TestPlatformLoginGlobalLimitAcrossIPs(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	anon := &testAPI{t: t, base: api.base, client: &http.Client{}, server: app}
	login := func(ip, password string) int {
		status, _, _ := anon.requestWithHeaders("POST", "/api/platform/session", map[string]any{"username": "platform-admin", "password": password}, map[string]string{"X-Forwarded-For": ip})
		return status
	}
	for i := 0; i < 20; i++ {
		if got := login(fmt.Sprintf("203.0.113.%d", i+1), "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i+1, got)
		}
	}
	if got := login("198.51.100.7", "platform-secret"); got != http.StatusTooManyRequests {
		t.Fatalf("login from a fresh IP after 20 distributed failures = %d, want 429", got)
	}
}

// TestProviderRefreshRateLimitedPerAccount proves user-triggered provider
// refreshes are capped per account in hosted mode (10 per 10 minutes), with a
// Retry-After, so repeated discovery cannot be used to load the host.
func TestProviderRefreshRateLimitedPerAccount(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	for i := 0; i < 10; i++ {
		status, payload, _ := api.request("POST", "/api/admin/providers/missing/refresh", nil)
		if status == http.StatusTooManyRequests {
			t.Fatalf("refresh %d limited too early: %v", i+1, payload)
		}
	}
	status, _, header := api.request("POST", "/api/admin/providers/missing/refresh", nil)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Fatalf("11th refresh = %d (Retry-After %q), want 429 with Retry-After", status, header.Get("Retry-After"))
	}
}
