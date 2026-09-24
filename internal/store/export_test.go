package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

// insertAccountActivity seeds one request_logs row with the given status for the
// account so the export/onboarding queries can be exercised directly.
func insertAccountActivity(t *testing.T, db *database.DB, accountID, id string, status int) {
	t.Helper()
	_, err := db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,client_name,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, accountID, "ck-"+id, "client", "provider-a/model-a", "chat", 0, status, 1, "req-"+id, "2026-01-01T00:00:01Z")
	if err != nil {
		t.Fatal(err)
	}
}

// TestFirstRoutedRequestAtIgnoresNon2xxAndScopesAccount proves onboarding state
// derives from a 2xx request only and never leaks across accounts.
func TestFirstRoutedRequestAtIgnoresNon2xxAndScopesAccount(t *testing.T) {
	db, st := openActivityStore(t)
	ctx := context.Background()
	local := st.For(database.LocalAccountID)
	other := st.For(otherAccount)

	// No rows yet.
	if at, err := local.FirstRoutedRequestAt(ctx); err != nil || at != "" {
		t.Fatalf("empty account first_request_at = %q err=%v, want empty", at, err)
	}
	// A failing row under the local account must not count.
	insertAccountActivity(t, db, database.LocalAccountID, "row-failed", 503)
	if at, err := local.FirstRoutedRequestAt(ctx); err != nil || at != "" {
		t.Fatalf("non-2xx satisfied onboarding: %q err=%v", at, err)
	}
	// A 2xx row under another account must not count for the local account.
	insertAccountActivity(t, db, otherAccount, "row-other", 200)
	if at, err := local.FirstRoutedRequestAt(ctx); err != nil || at != "" {
		t.Fatalf("other account's success leaked into local scope: %q err=%v", at, err)
	}
	// The local account's own 2xx row completes onboarding.
	insertAccountActivity(t, db, database.LocalAccountID, "row-ok", 204)
	at, err := local.FirstRoutedRequestAt(ctx)
	if err != nil || at != "2026-01-01T00:00:01Z" {
		t.Fatalf("FirstRoutedRequestAt = %q err=%v, want the 2xx row timestamp", at, err)
	}
	if count, err := local.CountAccountActivityRows(ctx); err != nil || count != 2 {
		t.Fatalf("CountAccountActivityRows local = %d err=%v, want 2", count, err)
	}
	if count, err := other.CountAccountActivityRows(ctx); err != nil || count != 1 {
		t.Fatalf("CountAccountActivityRows other = %d err=%v, want 1", count, err)
	}
}

// TestBuildAccountExportConfigOmitsSecrets proves config.json never carries a
// provider credential, an API-key selector/hash/fingerprint, or a secret
// setting, while still including the non-secret metadata.
func TestBuildAccountExportConfigOmitsSecrets(t *testing.T) {
	db, st := openActivityStore(t)
	ctx := context.Background()
	sc := st.For(database.LocalAccountID)

	if err := sc.CreateProvider(ctx, store.CreateProviderInput{ID: "prov-1", Name: "provider-a", Type: "generic-openai", BaseURL: "https://api.example.com/v1", Credential: "credential-marker", Enabled: true, Protocols: "chat"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateClientKey(ctx, store.CreateClientKeyInput{ID: "ck-1", Name: "export client", Description: "d", Group: "default", Selector: "selector-marker", Hash: "hash-marker", Fingerprint: "fingerprint-marker", Type: "catalogue", LoggingEnabled: true, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := sc.SetSetting(ctx, store.SettingNotificationsAuthHeader, "auth-header-marker"); err != nil {
		t.Fatal(err)
	}
	if err := sc.SetSetting(ctx, store.SettingDefaultRetentionDays, "7"); err != nil {
		t.Fatal(err)
	}

	cfg, err := sc.BuildAccountExportConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "provider-a" {
		t.Fatalf("export providers = %+v", cfg.Providers)
	}
	if len(cfg.ClientKeys) != 1 || cfg.ClientKeys[0].Name != "export client" {
		t.Fatalf("export client keys = %+v", cfg.ClientKeys)
	}
	if cfg.Settings[store.SettingDefaultRetentionDays] != "7" {
		t.Fatalf("non-secret setting missing: %+v", cfg.Settings)
	}
	for _, key := range []string{store.SettingNotificationsAuthHeader} {
		if _, ok := cfg.Settings[key]; ok {
			t.Fatalf("secret setting %q leaked into export", key)
		}
	}
	_ = db
}

// TestExportAccountActivityUnavailable proves the export Activity stream
// returns the explicit sentinel when the Activity handle is absent, so the
// handler can reject with 503 before emitting bytes.
func TestExportAccountActivityUnavailable(t *testing.T) {
	db, err := database.Open(context.Background(), t.TempDir()+"/router.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New(db.SQL) // no WithActivityDB
	sc := st.For(database.LocalAccountID)
	if err := sc.ExportAccountActivity(context.Background(), func(store.ActivityRow) error { return nil }); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("ExportAccountActivity err = %v, want ErrActivityUnavailable", err)
	}
}
