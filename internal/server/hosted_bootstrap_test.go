package server

import (
	"context"
	"errors"
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
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

func openExistingLocalDatabase(t *testing.T) *database.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tiller-router.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	local := store.New(db.SQL).For(database.LocalAccountID)
	if err := local.CreateProvider(ctx, store.CreateProviderInput{
		ID: "provider-local", Name: "local-provider", Type: "openai",
		BaseURL: "https://provider.example.com", Credential: "local-secret",
		Enabled: true, Protocols: "chat",
	}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := local.SetSetting(ctx, store.SettingFallbackTimeoutSeconds, "12"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.FreshInstall {
		upgraded.Close()
		t.Fatal("reopened local database was classified as fresh")
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	return upgraded
}

func hostedBootstrapConfig(adminUsername, adminPassword string) config.Config {
	return config.Config{
		Mode:                        config.ModeHosted,
		TillerUser:                  adminUsername,
		TillerUserPassword:          adminPassword,
		TillerPlatformAdminUser:     "platform-admin",
		TillerPlatformAdminPassword: "platform-secret",
		PublicURL:                   "https://tiller.example.com",
		TrustedProxy:                netip.MustParsePrefix("127.0.0.1/32"),
	}
}

func TestHostedStartupRequiresTrustedProxy(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := hostedBootstrapConfig("", "")
	cfg.TrustedProxy = netip.Prefix{}
	_, err = New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err == nil || !strings.Contains(err.Error(), "TILLER_TRUSTED_PROXY") {
		t.Fatalf("hosted startup error = %v, want TILLER_TRUSTED_PROXY requirement", err)
	}
}

func TestHostedStartupRequiresBootstrapCredentialsForExistingLocalDatabase(t *testing.T) {
	db := openExistingLocalDatabase(t)
	_, err := New(hostedBootstrapConfig("", ""), db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if !errors.Is(err, identity.ErrBootstrapRequired) {
		t.Fatalf("startup error = %v, want hosted bootstrap credentials required", err)
	}
	assertLocalDataUnchanged(t, db)
}

func TestHostedStartupRejectsNonEmailBootstrapUsername(t *testing.T) {
	db := openExistingLocalDatabase(t)
	_, err := New(hostedBootstrapConfig("admin", "correct horse battery staple"), db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if !errors.Is(err, identity.ErrBootstrapInvalid) {
		t.Fatalf("startup error = %v, want invalid hosted bootstrap credentials", err)
	}
	assertLocalDataUnchanged(t, db)
}

func TestHostedStartupMigratesExistingLocalDatabase(t *testing.T) {
	db := openExistingLocalDatabase(t)
	app, err := New(hostedBootstrapConfig("owner@example.com", "correct horse battery staple"), db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	if app == nil {
		t.Fatal("hosted server was nil")
	}
	var email, status, verified, owner string
	if err := db.SQL.QueryRow(`SELECT u.email,u.status,u.email_verified_at,a.owner_user_id FROM users u JOIN accounts a ON a.owner_user_id=u.id WHERE a.id=?`, database.LocalAccountID).Scan(&email, &status, &verified, &owner); err != nil {
		t.Fatal(err)
	}
	if email != "owner@example.com" || status != "active" || verified == "" || owner == "" {
		t.Fatalf("migrated customer = email %q status %q verified %q owner %q", email, status, verified, owner)
	}
	var marker string
	if err := db.SQL.QueryRow(`SELECT value FROM platform_settings WHERE key='hosted_bootstrap_complete'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "1" {
		t.Fatalf("bootstrap marker = %q, want 1", marker)
	}
	var providerCount, settingCount int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE account_id=?`, database.LocalAccountID).Scan(&providerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM settings WHERE account_id=? AND key=?`, database.LocalAccountID, store.SettingFallbackTimeoutSeconds).Scan(&settingCount); err != nil {
		t.Fatal(err)
	}
	if providerCount != 1 || settingCount != 1 {
		t.Fatalf("migrated tenant data changed: providers=%d settings=%d", providerCount, settingCount)
	}
}

// TestLocalToHostedConversionPreservesOperatorAndPasskeys is the end-to-end
// regression for the broken local->hosted switch: a database that booted in
// local mode (materialising the local operator users row and owning
// LocalAccountID) must convert to hosted by reusing that SAME user, so passkeys
// bound to its id survive. Before the fix, hosted bootstrap only claimed an
// ownerless account and failed with ErrBootstrapCollision on the local
// operator's owner_user_id.
func TestLocalToHostedConversionPreservesOperatorAndPasskeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")

	// --- First boot: local mode, env credential, register a passkey. ---
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	localCfg := config.Config{DataDir: dir, ListenAddr: ":8080", PublicURL: "https://tiller.example.com", TillerUser: "operator", TillerUserPassword: "operator-password"}
	localApp, err := New(localCfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	operator, err := localApp.identity.LocalOperatorUser(ctx)
	if err != nil {
		t.Fatalf("local operator row missing: %v", err)
	}
	// Seed one tenant resource so conversion can prove tenant data survives.
	if err := store.New(db.SQL).For(database.LocalAccountID).SetSetting(ctx, store.SettingFallbackTimeoutSeconds, "12"); err != nil {
		t.Fatal(err)
	}
	localRouter := httptest.NewServer(localApp.Handler())
	jar, _ := cookiejar.New(nil)
	api := &testAPI{t: t, base: localRouter.URL, client: &http.Client{Jar: jar}, server: localApp}
	status, login, _ := api.request("POST", "/api/admin/session", map[string]any{"username": "operator", "password": "operator-password"})
	if status != 200 {
		t.Fatalf("local login: %d %v", status, login)
	}
	api.csrf, _ = login["csrf_token"].(string)
	// Register a passkey on the operator.
	auth := newHTTPTestAuthenticator(t)
	status, begin, _ := api.request("POST", "/api/auth/account/passkeys/register/begin", map[string]any{})
	if status != 200 {
		t.Fatalf("register begin = %d %v", status, begin)
	}
	body := auth.registrationBody(t, "tiller.example.com", beginChallenge(t, begin), "https://tiller.example.com")
	status, reg, _ := api.requestWithHeaders("POST", "/api/auth/account/passkeys/register/finish?name=Laptop", body, map[string]string{challengeHeader: begin["challenge_token"].(string)})
	if status != 200 {
		t.Fatalf("register finish = %d %v", status, reg)
	}
	localRouter.Close()
	db.Close()

	// --- Second boot: same database, hosted mode, migration credentials. ---
	db2, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	hostedApp, err := New(hostedBootstrapConfig("owner@example.com", "correct horse battery staple"), db2, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatalf("hosted startup on a local database failed: %v", err)
	}

	// The operator user id is preserved and its email converted to the hosted
	// address; the synthetic local email is gone.
	var convertedID, convertedEmail, owner string
	if err := db2.SQL.QueryRow(`SELECT u.id,u.email,a.owner_user_id FROM users u JOIN accounts a ON a.id=?`, database.LocalAccountID).Scan(&convertedID, &convertedEmail, &owner); err != nil {
		t.Fatal(err)
	}
	if convertedID != operator.ID {
		t.Fatalf("converted user id = %q, want preserved %q", convertedID, operator.ID)
	}
	if convertedEmail != "owner@example.com" || owner != operator.ID {
		t.Fatalf("converted identity = id=%q email=%q owner=%q", convertedID, convertedEmail, owner)
	}

	// Tenant data survives the conversion.
	var setting string
	if err := db2.SQL.QueryRow(`SELECT value FROM settings WHERE account_id=? AND key=?`, database.LocalAccountID, store.SettingFallbackTimeoutSeconds).Scan(&setting); err != nil {
		t.Fatalf("tenant setting lost in conversion: %v", err)
	}
	if setting != "12" {
		t.Fatalf("tenant setting = %q, want 12", setting)
	}

	hostedRouter := httptest.NewServer(hostedApp.Handler())
	t.Cleanup(hostedRouter.Close)
	jar2, _ := cookiejar.New(nil)
	hapi := &testAPI{t: t, base: hostedRouter.URL, client: &http.Client{Jar: jar2}, server: hostedApp}

	// Hosted password login with the migration credentials works.
	status, login, _ = hapi.request("POST", "/api/auth/login", map[string]any{"email": "owner@example.com", "password": "correct horse battery staple"})
	if status != 200 {
		t.Fatalf("hosted login after conversion: %d %v", status, login)
	}
	hapi.csrf, _ = login["csrf_token"].(string)

	// The pre-conversion passkey still signs in, under the same RP/origin. This
	// is the load-bearing proof that preserving users.id kept passkey bindings.
	status, pbegin, _ := hapi.request("POST", "/api/auth/passkey/begin", map[string]any{})
	if status != 200 {
		t.Fatalf("passkey begin after conversion: %d %v", status, pbegin)
	}
	assertion := auth.assertionBody(t, "tiller.example.com", beginChallenge(t, pbegin), "https://tiller.example.com", operator.ID)
	status, session, _ := hapi.requestWithHeaders("POST", "/api/auth/passkey/finish", assertion, map[string]string{challengeHeader: pbegin["challenge_token"].(string)})
	if status != 200 {
		t.Fatalf("passkey login after conversion: %d %v", status, session)
	}
	if session["authenticated"] != true {
		t.Fatalf("passkey login did not authenticate after conversion: %v", session)
	}

	// A repeat hosted restart is a no-op (marker guarded), not a collision.
	hostedRouter.Close()
	db2.Close()
	db3, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db3.Close() })
	if _, err := New(hostedBootstrapConfig("owner@example.com", "correct horse battery staple"), db3, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{})); err != nil {
		t.Fatalf("hosted restart after conversion: %v", err)
	}
}

func assertLocalDataUnchanged(t *testing.T, db *database.DB) {
	t.Helper()
	var providerCount, settingCount, ownerCount, userCount, markerCount int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE account_id=?`, database.LocalAccountID).Scan(&providerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM settings WHERE account_id=? AND key=?`, database.LocalAccountID, store.SettingFallbackTimeoutSeconds).Scan(&settingCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM accounts WHERE id=? AND owner_user_id IS NOT NULL`, database.LocalAccountID).Scan(&ownerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM users`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM platform_settings WHERE key='hosted_bootstrap_complete'`).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if providerCount != 1 || settingCount != 1 || ownerCount != 0 || userCount != 0 || markerCount != 0 {
		t.Fatalf("local state changed: providers=%d settings=%d owners=%d users=%d markers=%d", providerCount, settingCount, ownerCount, userCount, markerCount)
	}
}
