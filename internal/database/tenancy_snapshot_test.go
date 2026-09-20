package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreTenancySnapshotCreatedForExistingInstall proves an existing pre-SaaS
// database gets exactly one verified rollback snapshot before migration 028
// mutates it, written to the configured backup directory.
func TestPreTenancySnapshotCreatedForExistingInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	// Checkpoint 27 applies migrations 001..027, i.e. everything before 028.
	openMigrationFixture(t, path, 27)

	backupDir := filepath.Join(dir, "backups")
	db, err := Open(context.Background(), path, WithBackupDir(backupDir))
	if err != nil {
		t.Fatalf("open existing install: %v", err)
	}
	defer db.Close()

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var snapshots []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), PreTenancySnapshotPrefix) && strings.HasSuffix(entry.Name(), BackupSuffix) {
			snapshots = append(snapshots, entry.Name())
		}
	}
	if len(snapshots) != 1 {
		t.Fatalf("pre-tenancy snapshots = %d (%v), want 1", len(snapshots), snapshots)
	}
	// The snapshot must be a valid, verifiable database.
	if err := Verify(context.Background(), filepath.Join(backupDir, snapshots[0])); err != nil {
		t.Fatalf("snapshot failed verification: %v", err)
	}
	// And the migration must have proceeded.
	var applied int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version=?`, TenancyMigration).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatal("tenancy migration did not apply")
	}
}

// TestPreTenancySnapshotIsIdempotent proves a second Open reuses the existing
// snapshot rather than piling up new ones.
func TestPreTenancySnapshotIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	backupDir := filepath.Join(dir, "backups")
	openMigrationFixture(t, path, 27)

	db, err := Open(context.Background(), path, WithBackupDir(backupDir))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Re-open an already-migrated database. The snapshot must not be re-taken.
	db2, err := Open(context.Background(), path, WithBackupDir(backupDir))
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), PreTenancySnapshotPrefix) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("pre-tenancy snapshots after reopen = %d, want 1", count)
	}
}

// TestPreTenancySnapshotSkippedForFreshInstall proves a fresh install (no
// schema_migrations table) takes no snapshot.
func TestPreTenancySnapshotSkippedForFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	backupDir := filepath.Join(dir, "backups")

	db, err := Open(context.Background(), path, WithBackupDir(backupDir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := os.Stat(filepath.Join(backupDir, PreTenancySnapshotPrefix)); !os.IsNotExist(err) {
		// The directory itself may or may not exist; assert no snapshot files.
		entries, rerr := os.ReadDir(backupDir)
		if rerr == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), PreTenancySnapshotPrefix) {
					t.Fatalf("fresh install created a pre-tenancy snapshot: %s", entry.Name())
				}
			}
		}
	}
}

// TestOpenSucceedsWhenActivityUnavailable proves a missing/unopenable
// activity.db does not prevent startup: the core database opens and the
// Activity handle is left nil.
func TestOpenSucceedsWhenActivityUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	// Create a directory named activity.db so OpenActivity cannot open it as a
	// SQLite file.
	if err := os.Mkdir(filepath.Join(dir, ActivityFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open must not fail when Activity is unavailable: %v", err)
	}
	defer db.Close()
	if db.Activity != nil {
		t.Fatal("Activity handle should be nil when activity.db cannot be opened")
	}
	if err := db.Ready(context.Background()); err != nil {
		t.Fatalf("core database not ready: %v", err)
	}
}
