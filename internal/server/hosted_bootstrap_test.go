package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
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
		Mode:             config.ModeHosted,
		AdminUsername:    adminUsername,
		AdminPassword:    adminPassword,
		PlatformUsername: "platform-admin",
		PlatformPassword: "platform-secret",
		PublicURL:        "https://tiller.example.com",
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
