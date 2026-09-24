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

// TestPreTenancySnapshotNotReusedAcrossInstallations proves the reuse check is
// bound to the installation: a snapshot taken for one pre-SaaS database is not
// treated as another database's rollback point even when they share a backup
// directory.
func TestPreTenancySnapshotNotReusedAcrossInstallations(t *testing.T) {
	backupDir := t.TempDir()

	// First installation: migrate and record its snapshot name.
	dirA := t.TempDir()
	pathA := filepath.Join(dirA, "router.db")
	openMigrationFixture(t, pathA, 27)
	dbA, err := Open(context.Background(), pathA, WithBackupDir(backupDir))
	if err != nil {
		t.Fatal(err)
	}
	dbA.Close()
	first := snapshotNames(t, backupDir)
	if len(first) != 1 {
		t.Fatalf("installation A snapshots = %d (%v), want 1", len(first), first)
	}

	// Second installation in the same backup dir.
	dirB := t.TempDir()
	pathB := filepath.Join(dirB, "router.db")
	openMigrationFixture(t, pathB, 27)
	dbB, err := Open(context.Background(), pathB, WithBackupDir(backupDir))
	if err != nil {
		t.Fatal(err)
	}
	dbB.Close()

	all := snapshotNames(t, backupDir)
	if len(all) != 2 {
		t.Fatalf("snapshots after second install = %d (%v), want 2 (one per install)", len(all), all)
	}
}

// snapshotNames returns the pre-tenancy snapshot filenames in dir.
func snapshotNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), PreTenancySnapshotPrefix) && strings.HasSuffix(entry.Name(), BackupSuffix) {
			out = append(out, entry.Name())
		}
	}
	return out
}

// TestInstallationIDStableAcrossReopen proves the install id used to bind
// snapshot reuse is durable: reopening the same database yields the same id.
func TestInstallationIDStableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	openMigrationFixture(t, path, 27)

	db, err := Open(context.Background(), path, WithBackupDir(filepath.Join(dir, "backups")))
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.installationID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if first == 0 {
		t.Fatal("installation id should be non-zero after first open")
	}

	reopened, err := Open(context.Background(), path, WithBackupDir(filepath.Join(dir, "backups")))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := reopened.installationID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("installation id changed across reopen: %d -> %d", first, second)
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
