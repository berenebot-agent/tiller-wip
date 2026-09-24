package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

func TestPruneAuditEventsUsesCutoffForBothTables(t *testing.T) {
	db, st := openAuditStore(t)
	ctx := context.Background()
	current := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	old := current.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	atCutoff := current.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	newer := current.Add(-23 * time.Hour).Format(time.RFC3339Nano)
	if err := st.SetAuditRetentionDays(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	insertAccountAudit(t, db, "account-old", "deleted-account", old)
	insertAccountAudit(t, db, "account-cutoff", "deleted-account", atCutoff)
	insertAccountAudit(t, db, "account-new", "active-account", newer)
	insertPlatformAudit(t, db, "platform-old", old)
	insertPlatformAudit(t, db, "platform-cutoff", atCutoff)
	insertPlatformAudit(t, db, "platform-new", newer)

	if err := st.PruneAuditEvents(ctx, current); err != nil {
		t.Fatal(err)
	}

	assertAuditCount(t, db, "account_audit_events", 2)
	assertAuditCount(t, db, "platform_audit_events", 2)
	assertAuditRow(t, db, "account_audit_events", "account-cutoff")
	assertAuditRow(t, db, "account_audit_events", "account-new")
	assertAuditRow(t, db, "platform_audit_events", "platform-cutoff")
	assertAuditRow(t, db, "platform_audit_events", "platform-new")
}

func TestPruneAuditEventsRetentionSettingChanges(t *testing.T) {
	db, st := openAuditStore(t)
	ctx := context.Background()
	current := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	insertAccountAudit(t, db, "old", "account", current.Add(-3*24*time.Hour).Format(time.RFC3339Nano))
	insertPlatformAudit(t, db, "recent", current.Add(-2*24*time.Hour).Format(time.RFC3339Nano))

	if err := st.SetAuditRetentionDays(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneAuditEvents(ctx, current); err != nil {
		t.Fatal(err)
	}
	assertAuditCount(t, db, "account_audit_events", 1)
	assertAuditCount(t, db, "platform_audit_events", 1)

	if err := st.SetAuditRetentionDays(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneAuditEvents(ctx, current); err != nil {
		t.Fatal(err)
	}
	assertAuditCount(t, db, "account_audit_events", 0)
	assertAuditCount(t, db, "platform_audit_events", 0)
}

func TestPruneAuditEventsDoesNotRequireActivity(t *testing.T) {
	db, st := openAuditStore(t)
	ctx := context.Background()
	current := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	insertAccountAudit(t, db, "deleted", "account-no-longer-exists", current.Add(-2*24*time.Hour).Format(time.RFC3339Nano))
	insertPlatformAudit(t, db, "platform-new", current.Add(-time.Hour).Format(time.RFC3339Nano))

	if err := st.SetAuditRetentionDays(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneAuditEvents(ctx, current); err != nil {
		t.Fatal(err)
	}
	assertAuditCount(t, db, "account_audit_events", 0)
	assertAuditCount(t, db, "platform_audit_events", 1)
}

func openAuditStore(t *testing.T) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, store.New(db.SQL)
}

func insertAccountAudit(t *testing.T, db *database.DB, id, accountID, createdAt string) {
	t.Helper()
	if _, err := db.SQL.Exec(`INSERT INTO account_audit_events(id,account_id,event,actor_type,outcome,metadata,created_at) VALUES(?,?, 'test', 'test', 'success', '{}', ?)`, id, accountID, createdAt); err != nil {
		t.Fatal(err)
	}
}

func insertPlatformAudit(t *testing.T, db *database.DB, id, createdAt string) {
	t.Helper()
	if _, err := db.SQL.Exec(`INSERT INTO platform_audit_events(id,event,actor_type,outcome,metadata,created_at) VALUES(?, 'test', 'platform', 'success', '{}', ?)`, id, createdAt); err != nil {
		t.Fatal(err)
	}
}

func assertAuditCount(t *testing.T, db *database.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func assertAuditRow(t *testing.T, db *database.DB, table, id string) {
	t.Helper()
	var got string
	if err := db.SQL.QueryRow(`SELECT id FROM `+table+` WHERE id=?`, id).Scan(&got); err != nil {
		t.Fatalf("%s row %q missing: %v", table, id, err)
	}
}
