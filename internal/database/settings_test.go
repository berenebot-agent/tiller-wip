package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSettingsAccessors(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// Seeded defaults from migration 004.
	enabled, retention, err := db.GetLoggingDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatalf("default logging should be enabled, got %v", enabled)
	}
	if retention != 30 {
		t.Fatalf("default retention should be 30, got %d", retention)
	}

	if err := db.SetSetting(ctx, SettingDefaultLoggingEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, SettingDefaultRetentionDays, "7"); err != nil {
		t.Fatal(err)
	}
	enabled, retention, err = db.GetLoggingDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatalf("logging should now be disabled, got %v", enabled)
	}
	if retention != 7 {
		t.Fatalf("retention should now be 7, got %d", retention)
	}

	if v, err := db.GetBool(ctx, SettingDefaultLoggingEnabled); err != nil || v {
		t.Fatalf("GetBool: %v %v", v, err)
	}
	if v, err := db.GetInt(ctx, SettingDefaultRetentionDays); err != nil || v != 7 {
		t.Fatalf("GetInt: %v %v", v, err)
	}
}

func TestFallbackTimeout(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// Seeded default from migration 012.
	if v, err := db.GetFallbackTimeout(ctx); err != nil || v != 60 {
		t.Fatalf("default fallback timeout = %d, %v; want 60, nil", v, err)
	}

	if err := db.SetSetting(ctx, SettingFallbackTimeoutSeconds, "120"); err != nil {
		t.Fatal(err)
	}
	if v, err := db.GetFallbackTimeout(ctx); err != nil || v != 120 {
		t.Fatalf("fallback timeout after set = %d, %v; want 120, nil", v, err)
	}
}

func TestNotificationCooldownDefault(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	ns, err := db.GetNotificationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ns.CooldownSeconds != 60 {
		t.Fatalf("default cooldown = %d, want 60", ns.CooldownSeconds)
	}

	if err := db.SetSetting(ctx, SettingNotificationsCooldownSeconds, "0"); err != nil {
		t.Fatal(err)
	}
	ns, err = db.GetNotificationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ns.CooldownSeconds != 0 {
		t.Fatalf("cooldown after set = %d, want 0", ns.CooldownSeconds)
	}
}
