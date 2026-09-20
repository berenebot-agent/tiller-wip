package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenRestrictsFileAndDirPermissions(t *testing.T) {
	dir := t.TempDir()
	// Simulate a host bind-mount that pre-exists with loose perms.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "router.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The data dir is tightened to 0700.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data dir mode = %o, want 700", perm)
	}
	// The DB file is tightened to 0600.
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db file mode = %o, want 600", perm)
	}
}

func TestFreshHostedOpenPersistsBootstrapState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	db, err := Open(context.Background(), path, WithHostedMode(true))
	if err != nil {
		t.Fatal(err)
	}
	if !db.FreshInstall {
		db.Close()
		t.Fatal("fresh hosted database was not classified as fresh")
	}
	var marker string
	if err := db.SQL.QueryRow(`SELECT value FROM platform_settings WHERE key='hosted_bootstrap_complete'`).Scan(&marker); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if marker != "1" {
		db.Close()
		t.Fatalf("fresh hosted bootstrap marker = %q, want 1", marker)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.FreshInstall {
		t.Fatal("reopened hosted database was classified as fresh")
	}
	if err := reopened.SQL.QueryRow(`SELECT value FROM platform_settings WHERE key='hosted_bootstrap_complete'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "1" {
		t.Fatalf("reopened hosted bootstrap marker = %q, want 1", marker)
	}
}

func TestMigrationsAndSharedNamespace(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('virtual','real','p1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('p1','virtual','generic-openai','http://example.test/v1',1,'["chat"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('virtual','virtual','g1')`); !IsConstraint(err) {
		t.Fatalf("shared namespace collision was not rejected: %v", err)
	}
}

func TestBackupIsConsistentAndReadable(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := db.Backup(context.Background(), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	restored, err := Open(context.Background(), backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Migration 014 (backfill of legacy route attribution) is exercised end to end
// by TestMigrateFromEveryCheckpointPreservesData, which seeds a legacy-shaped
// request row and upgrades from each historical checkpoint. The standalone
// test that re-ran the 014 body against the central request_logs table is gone
// because Activity now lives in per-account files.

// TestBackupExcludesActivityAndRestoresCore proves a snapshot is a usable
// restore point for the control plane (including audit history) and
// deliberately carries no Activity: the central request_logs table is gone
// from the snapshot and the separate activity.db is not part of it.
func TestBackupExcludesActivityAndRestoresCore(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(context.Background(), filepath.Join(dir, "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('backup-prov','real','bp1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('bp1','backup-prov','generic-openai','http://example.test/v1',1,'["chat"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Activity.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('act-1','ck','m','chat',0,200,1,'r1',?)`, now); err != nil {
		t.Fatal(err)
	}
	// Audit history is durable control-plane state and must be captured by the
	// core snapshot.
	if _, err := db.SQL.Exec(`INSERT INTO account_audit_events(id,account_id,event,actor_type,outcome,metadata,created_at) VALUES('ae1',?,'user.login','user','success','{}',?)`, LocalAccountID, now); err != nil {
		t.Fatal(err)
	}

	backup, err := db.Backup(context.Background(), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := Verify(context.Background(), backup); err != nil {
		t.Fatalf("backup failed verification: %v", err)
	}
	restored, err := Open(context.Background(), backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var name string
	if err := restored.SQL.QueryRow(`SELECT name FROM providers WHERE id='bp1'`).Scan(&name); err != nil {
		t.Fatalf("backup lost control-plane row: %v", err)
	}
	if name != "backup-prov" {
		t.Fatalf("provider name = %q", name)
	}
	var tables int
	if err := restored.SQL.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('request_logs','request_attempts')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("central backup must not contain Activity tables, found %d", tables)
	}
	var auditRows int
	if err := restored.SQL.QueryRow(`SELECT count(*) FROM account_audit_events`).Scan(&auditRows); err != nil {
		t.Fatalf("backup lost audit table: %v", err)
	}
	if auditRows != 1 {
		t.Fatalf("backup audit rows = %d, want 1", auditRows)
	}
}

func TestPruneBackupsRemovesOnlyOldSnapshots(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for _, name := range []string{"tiller-router-old.db", "tiller-router-new.db", "keepme.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "tiller-router-old.db"), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneBackups(dir, 7*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "tiller-router-old.db")); !os.IsNotExist(err) {
		t.Fatalf("old snapshot was not pruned: %v", err)
	}
	for _, name := range []string{"tiller-router-new.db", "keepme.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s should be kept: %v", name, err)
		}
	}
}

func TestMapDataDirChmodErr(t *testing.T) {
	// A chmod whose EPERM means the caller doesn't own the dir (the fresh
	// root-owned bind-mount case) must surface the ErrDataDirUnwritable sentinel
	// so main can print a first-run remediation instead of "operation not
	// permitted".
	synthetic := &os.PathError{Op: "chmod", Path: "/data", Err: syscall.EPERM}
	mapped := mapDataDirChmodErr("/data", synthetic)
	if !errors.Is(mapped, ErrDataDirUnwritable) {
		t.Fatalf("EPERM chmod should wrap ErrDataDirUnwritable, got: %v", mapped)
	}
	if !errors.Is(mapped, synthetic) {
		t.Fatalf("wrapped error should preserve the underlying chmod error, got: %v", mapped)
	}
	// Non-EPERM chmod errors pass through untouched.
	other := &os.PathError{Op: "chmod", Path: "/data", Err: syscall.ENOSPC}
	if got := mapDataDirChmodErr("/data", other); got != other {
		t.Fatalf("non-EPERM chmod error should pass through unchanged, got: %v", got)
	}
}
