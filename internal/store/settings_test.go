package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

const otherAccount = "11111111-1111-1111-1111-111111111111"

func openStore(t *testing.T) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active','2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, otherAccount); err != nil {
		t.Fatal(err)
	}
	return db, store.New(db.SQL)
}

// TestSettingsAreAccountScoped proves a settings write under one account is
// invisible to another account.
func TestSettingsAreAccountScoped(t *testing.T) {
	_, st := openStore(t)
	ctx := context.Background()
	local := st.For(database.LocalAccountID)
	other := st.For(otherAccount)

	if err := local.SetSetting(ctx, store.SettingDefaultRetentionDays, "7"); err != nil {
		t.Fatal(err)
	}
	if err := other.SetSetting(ctx, store.SettingDefaultRetentionDays, "90"); err != nil {
		t.Fatal(err)
	}

	got, err := local.GetInt(ctx, store.SettingDefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("local account retention = %d, want 7", got)
	}
	gotOther, err := other.GetInt(ctx, store.SettingDefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	if gotOther != 90 {
		t.Fatalf("other account retention = %d, want 90", gotOther)
	}
}

// TestSettingsDefaultsArePerAccount proves defaults are computed per account
// and unaffected by another account's explicit values.
func TestSettingsDefaultsArePerAccount(t *testing.T) {
	_, st := openStore(t)
	ctx := context.Background()
	local := st.For(database.LocalAccountID)
	other := st.For(otherAccount)

	if err := local.SetSetting(ctx, store.SettingDefaultRetentionDays, "1"); err != nil {
		t.Fatal(err)
	}
	_, retention, err := other.GetLoggingDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retention != 30 {
		t.Fatalf("other account default retention = %d, want 30", retention)
	}
}

// TestRunTxCommitsAndRollsBack proves a transaction-bound scope writes for the
// scope's own account and that an error rolls the whole transaction back.
func TestRunTxCommitsAndRollsBack(t *testing.T) {
	_, st := openStore(t)
	ctx := context.Background()
	local := st.For(database.LocalAccountID)

	if err := local.RunTx(ctx, nil, func(tx *store.Scope) error {
		if tx.AccountID() != local.AccountID() {
			t.Fatalf("tx account = %q, want %q", tx.AccountID(), local.AccountID())
		}
		return tx.SetSetting(ctx, store.SettingDefaultRetentionDays, "5")
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := local.GetInt(ctx, store.SettingDefaultRetentionDays); err != nil || got != 5 {
		t.Fatalf("committed retention = %d (err %v), want 5", got, err)
	}

	wantErr := context.Canceled
	err := local.RunTx(ctx, nil, func(tx *store.Scope) error {
		if err := tx.SetSetting(ctx, store.SettingDefaultRetentionDays, "99"); err != nil {
			return err
		}
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("RunTx error = %v, want %v", err, wantErr)
	}
	if got, err := local.GetInt(ctx, store.SettingDefaultRetentionDays); err != nil || got != 5 {
		t.Fatalf("rolled-back retention = %d (err %v), want 5", got, err)
	}
}

// TestNotificationSettingsAreAccountScoped proves notification config does not
// leak across accounts.
func TestNotificationSettingsAreAccountScoped(t *testing.T) {
	_, st := openStore(t)
	ctx := context.Background()
	local := st.For(database.LocalAccountID)
	other := st.For(otherAccount)

	if err := local.SetSetting(ctx, store.SettingNotificationsEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if err := local.SetSetting(ctx, store.SettingNotificationsWebhookURL, "https://example.test/hook"); err != nil {
		t.Fatal(err)
	}
	cfg, err := other.GetNotificationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.WebhookURL != "" {
		t.Fatalf("other account saw local notification config: %+v", cfg)
	}
}
