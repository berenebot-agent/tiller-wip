package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	SQL  *sql.DB
	Path string
	// Activity is the separate Activity database (request_logs and
	// request_attempts). It is best-effort: a missing or unopenable Activity
	// database leaves it nil so Tiller still starts and routes. ActivityPath is
	// its file, a sibling of the central database under the data directory.
	Activity     *sql.DB
	ActivityPath string
	// maintenanceMu serializes whole-file operations on the databases: a
	// backup snapshot (VACUUM INTO) and an in-place compaction (VACUUM) both
	// rewrite the core file and must not overlap. Row reads and writes are
	// never serialized here; SQLite's own locking governs those.
	maintenanceMu sync.Mutex
}

// OpenOption configures Open.
type OpenOption func(*openConfig)

type openConfig struct {
	backupDir string
}

// WithBackupDir sets where a one-time pre-major-migration rollback snapshot is
// written. It defaults to <data dir>/backups. It is independent of the
// scheduled-backup setting, so it follows the operator's configured backup
// location when one is supplied.
func WithBackupDir(dir string) OpenOption {
	return func(c *openConfig) { c.backupDir = dir }
}

// LocalAccountID is the fixed, well-known identifier of the single implicit
// account that owns every row in a self-hosted/local installation. Hosted
// accounts use random id.New() UUIDs instead. It is a constant (rather than a
// generated value) so pure-SQL migrations can seed the account and backfill
// account_id columns without a Go bootstrap step.
const LocalAccountID = "00000000-0000-0000-0000-000000000001"

// ErrDataDirUnwritable is wrapped and returned by Open when the data directory
// is not owned by (and therefore not writable to) the runtime user — typically
// a fresh rootful-Docker bind mount created on the host as root. Callers can
// detect it to surface a precise first-run remediation (host-side chown)
// instead of a cryptic chmod EPERM.
var ErrDataDirUnwritable = errors.New("data directory is not writable by the runtime user")

func Open(ctx context.Context, path string, opts ...OpenOption) (*DB, error) {
	var cfg openConfig
	cfg.backupDir = filepath.Join(filepath.Dir(path), "backups")
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// The data dir may pre-exist (e.g. a host bind-mount) with looser perms;
	// MkdirAll won't tighten an existing dir, so chmod it explicitly. Chmod can
	// only be performed by the directory's owner, so a dir owned by someone
	// else — a fresh rootful-Docker bind mount created as root — fails with
	// EPERM. Surface that specific case with a helpful sentinel rather than the
	// bare "operation not permitted".
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, mapDataDirChmodErr(filepath.Dir(path), err)
	}
	// Keep SQLite's scratch/temp files entirely in memory and never on a disk
	// temp path. Without this, a large catalogue UPSERT can trigger a temp-file
	// spill, and on constrained CI runners the temp path is intermittently
	// unresolvable -> SQLITE_IOERR_GETTEMPPATH (6410) at applyCatalogue commit,
	// which showed up in CI as a browser-suite flake ("Provider was saved, but
	// initial discovery failed"). This DB is small (a few hundred provider
	// models at most), so an in-memory temp store is a negligible cost and
	// removes the whole temp-path failure class. The on-disk DB and WAL are
	// unaffected. See servicePragmas for the shared pragma set.
	dsn := sqliteDSN(path, servicePragmas())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Restrict the on-disk DB and any existing WAL/SHM sidecars to the owning
	// user. SQLite creates -wal/-shm with the same mode as the main DB file, so
	// tightening the DB file also governs future sidecar files.
	if err := restrictFileMode(path); err != nil {
		db.Close()
		return nil, err
	}
	d := &DB{SQL: db, Path: path, ActivityPath: filepath.Join(filepath.Dir(path), ActivityFileName)}
	// Take a one-time, verified rollback snapshot before the account-tenancy
	// migration (028) mutates an existing pre-SaaS database. It is a no-op for
	// fresh installs and for databases that have already crossed 028.
	if err := d.snapshotBeforeTenancy(ctx, cfg.backupDir); err != nil {
		db.Close()
		return nil, err
	}
	if err := d.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Activity is best-effort telemetry: if it cannot be opened, leave the
	// handle nil (reads return an explicit unavailable error, writes no-op)
	// rather than refusing to start. The one-time move from the central tables
	// also runs here so a released install upgrades transparently.
	if activity, aerr := OpenActivity(ctx, d.ActivityPath); aerr == nil {
		d.Activity = activity
	}
	if err := d.migrateActivity(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// TenancyMigration is the migration that starts the account-tenancy work. An
// existing database without it is a pre-SaaS install.
const TenancyMigration = "028_accounts.sql"

// PreTenancySnapshotPrefix names the one-time rollback snapshot taken before the
// account-tenancy migration. It deliberately differs from BackupPrefix so the
// ordinary short-retention prune (PruneBackups) never removes it.
const PreTenancySnapshotPrefix = "pre-saas-migration-"

// snapshotBeforeTenancy creates a verified rollback snapshot before migration
// 028 mutates an existing pre-SaaS database. It uses migration state, not an
// application version string, to decide whether the snapshot is required:
//
//   - a fresh install has no schema_migrations table, so no snapshot is taken;
//   - a database that already applied 028 has nothing to roll back;
//   - an existing install with 028 still pending gets exactly one snapshot.
//
// Reuse is bound to this installation: the snapshot filename embeds the
// database's durable installation id, so a leftover snapshot taken for a
// different database that happens to share the backup directory is never
// treated as this installation's rollback point. Migration aborts if the
// snapshot cannot be created or verified.
func (d *DB) snapshotBeforeTenancy(ctx context.Context, backupDir string) error {
	var hasMigrations int
	if err := d.SQL.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&hasMigrations); err != nil {
		return fmt.Errorf("check migration state: %w", err)
	}
	if hasMigrations == 0 {
		return nil
	}
	var applied int
	if err := d.SQL.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, TenancyMigration).Scan(&applied); err != nil {
		return fmt.Errorf("check tenancy migration state: %w", err)
	}
	if applied != 0 {
		return nil
	}
	installID, err := d.installationID(ctx)
	if err != nil {
		return fmt.Errorf("installation id: %w", err)
	}
	prefix := fmt.Sprintf("%s%08x-", PreTenancySnapshotPrefix, uint32(installID))
	// Idempotent: reuse this installation's snapshot if one was already taken.
	if existing, err := findSnapshot(backupDir, prefix); err != nil {
		return err
	} else if existing != "" {
		if err := Verify(ctx, existing); err != nil {
			return fmt.Errorf("pre-migration snapshot failed verification (%s): %w", existing, err)
		}
		return nil
	}
	path, err := BackupNamed(ctx, d.SQL, backupDir, prefix)
	if err != nil {
		return fmt.Errorf("pre-migration snapshot failed: %w", err)
	}
	if err := Verify(ctx, path); err != nil {
		return fmt.Errorf("pre-migration snapshot failed verification (%s): %w", path, err)
	}
	return nil
}

// installationID returns this database's durable install identifier, creating
// one on first use. It is stored in the SQLite header's reserved application_id
// field, so it needs no extra table or companion file and travels with the
// database (including inside a VACUUM INTO snapshot) across directory moves.
//
// application_id is unused by Tiller otherwise; a non-zero value is set once,
// before migration 028, and never changed.
func (d *DB) installationID(ctx context.Context) (int64, error) {
	var id int64
	if err := d.SQL.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&id); err != nil {
		return 0, err
	}
	if id != 0 {
		return id, nil
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	id = int64(binary.BigEndian.Uint32(b[:]) & 0x7fffffff)
	if id == 0 {
		id = 1
	}
	// PRAGMA does not accept bound parameters; the value is a locally generated
	// integer, never request input.
	if _, err := d.SQL.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", id)); err != nil {
		return 0, err
	}
	return id, nil
}

// findSnapshot returns the newest file in dir named with prefix and the backup
// suffix, or "" when none exists.
func findSnapshot(dir, prefix string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	newest := ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, BackupSuffix) {
			continue
		}
		if newest == "" || name > newest {
			newest = name
		}
	}
	if newest == "" {
		return "", nil
	}
	return filepath.Join(dir, newest), nil
}

// mapDataDirChmodErr rewrites a chmod failure on the data directory so the
// common fresh-deployment case — a bind-mounted dir the runtime user doesn't
// own, which chmods with EPERM ("operation not permitted") — surfaces a helpful
// sentinel (ErrDataDirUnwritable) instead of the bare "operation not permitted".
// Any other chmod error passes through unchanged.
func mapDataDirChmodErr(dir string, err error) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("%w %q (chmod: %w)", ErrDataDirUnwritable, dir, err)
	}
	return err
}

// restrictFileMode chmods the given SQLite file and any existing -wal/-shm
// sidecars to 0600 so provider credentials and other sensitive state are not
// world-readable on the host bind-mount.
func restrictFileMode(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			if err := os.Chmod(p, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *DB) Close() error {
	if d.Activity != nil {
		if err := d.Activity.Close(); err != nil {
			_ = d.SQL.Close()
			return err
		}
	}
	return d.SQL.Close()
}

func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.SQL.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("initialize migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	// SQLite cannot alter constraints in place, so migrations that make
	// uniqueness account-local rebuild their tables (create-new, copy, drop,
	// rename). Foreign-key enforcement must be off for the duration or the
	// drop/rename sequence fails; integrity is re-verified explicitly with
	// foreign_key_check before the connection is returned to the pool.
	// A dedicated *sql.Conn is used so the PRAGMA and the migrations run on
	// the same connection (PRAGMA foreign_keys is a no-op inside a
	// transaction and is per-connection).
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
	}()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var exists int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, entry.Name()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, entry.Name(), Now())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}
	}
	if err := checkForeignKeys(ctx, conn); err != nil {
		return err
	}
	return nil
}

// checkForeignKeys runs PRAGMA foreign_key_check and fails if any row violates
// a foreign-key constraint. Migrations run with enforcement disabled, so this
// is the proof that the rebuilds left referential integrity intact.
func checkForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		var rowid int64
		var parent string
		var fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("foreign_key_check scan: %w", err)
		}
		return fmt.Errorf("foreign_key_check: table %s row %d violates constraint %d referencing %s", table, rowid, fkid, parent)
	}
	return rows.Err()
}

func (d *DB) Ready(ctx context.Context) error {
	var one int
	return d.SQL.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

func (d *DB) Backup(ctx context.Context, dir string) (string, error) {
	// Serialize against an in-place VACUUM: both rewrite the core file.
	d.maintenanceMu.Lock()
	defer d.maintenanceMu.Unlock()
	return BackupNamed(ctx, d.SQL, dir, "tiller-router-")
}

// BackupNamed creates a consistent SQLite snapshot with a caller-supplied
// filename prefix. It is used for the core and separate audit databases.
func BackupNamed(ctx context.Context, source *sql.DB, dir, prefix string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := prefix + time.Now().UTC().Format("20060102T150405.000000000Z") + ".db"
	path := filepath.Join(dir, name)
	if !strings.HasPrefix(path, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", errors.New("invalid backup path")
	}
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := source.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return "", fmt.Errorf("sqlite backup: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// BackupPrefix and BackupSuffix identify scheduled/core backup snapshots.
const (
	BackupPrefix = "tiller-router-"
	BackupSuffix = ".db"
)

// Verify opens a SQLite database file read-only and checks its integrity and
// foreign keys. It is used to prove a snapshot is a usable restore point
// before the snapshot is trusted.
func Verify(ctx context.Context, path string) error {
	q := url.Values{}
	q.Add("mode", "ro")
	db, err := sql.Open("sqlite", sqliteDSN(path, q))
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity_check: %s", integrity)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		return errors.New("foreign_key_check: snapshot has a foreign-key violation")
	}
	return rows.Err()
}

// PruneBackups removes backup snapshots older than retention. It only touches
// files matching the backup naming convention, so unrelated files in the
// backup directory are left alone.
func PruneBackups(dir string, retention time.Duration, now time.Time) (int, error) {
	return PrunePrefixedBackups(dir, retention, now, BackupPrefix)
}

// PrunePrefixedBackups removes snapshots matching prefix older than retention.
func PrunePrefixedBackups(dir string, retention time.Duration, now time.Time, prefix string) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := now.Add(-retention)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, BackupSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func IsConstraint(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "constraint failed") || strings.Contains(s, "unique constraint") || strings.Contains(s, "foreign key constraint")
}
