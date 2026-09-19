package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	SQL  *sql.DB
	Path string
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

func Open(ctx context.Context, path string) (*DB, error) {
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
	dsnURL := url.URL{Scheme: "file", Path: path}
	query := dsnURL.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	// Keep SQLite's scratch/temp files entirely in memory and never on a disk
	// temp path. Without this, a large catalogue UPSERT can trigger a temp-file
	// spill, and on constrained CI runners the temp path is intermittently
	// unresolvable -> SQLITE_IOERR_GETTEMPPATH (6410) at applyCatalogue commit,
	// which showed up in CI as a browser-suite flake ("Provider was saved, but
	// initial discovery failed"). This DB is small (a few hundred provider
	// models at most), so an in-memory temp store is a negligible cost and
	// removes the whole temp-path failure class. The on-disk DB and WAL are
	// unaffected.
	query.Add("_pragma", "temp_store(MEMORY)")
	dsnURL.RawQuery = query.Encode()
	dsn := dsnURL.String()
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
	d := &DB{SQL: db, Path: path}
	if err := d.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
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

func (d *DB) Close() error { return d.SQL.Close() }

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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "tiller-router-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".db"
	path := filepath.Join(dir, name)
	if !strings.HasPrefix(path, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", errors.New("invalid backup path")
	}
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := d.SQL.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return "", fmt.Errorf("sqlite backup: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func IsConstraint(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "constraint failed") || strings.Contains(s, "unique constraint") || strings.Contains(s, "foreign key constraint")
}
