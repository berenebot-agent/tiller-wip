package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
