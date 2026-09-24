package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ActivityFileName is the fixed filename of the separate Activity database
// under the data directory. It holds the high-churn, disposable Activity
// tables (request_logs and request_attempts), which stay out of the central
// database so a core backup never contains logs and a logging write cannot
// block the control plane. Both tables still carry account_id and every read
// and write is account-scoped.
const ActivityFileName = "activity.db"

// activitySchema is the Activity schema. It deliberately has no foreign key to
// client_keys (that table lives in the central database) and carries a
// denormalized client_name so Activity reads never cross databases. The
// request_attempts -> request_logs foreign key stays intact because both tables
// live in the same file.
const activitySchema = `
CREATE TABLE IF NOT EXISTS activity_schema_migrations (
	version TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS request_logs (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
	client_key_id TEXT NOT NULL,
	client_name TEXT NOT NULL DEFAULT '',
	requested_model TEXT NOT NULL,
	exposed_model TEXT,
	route_kind TEXT CHECK (route_kind IN ('real','virtual')),
	route_model_id TEXT,
	route_model TEXT,
	route_status TEXT NOT NULL DEFAULT 'legacy' CHECK (route_status IN ('legacy','routed','unresolved')),
	resolved_provider TEXT,
	resolved_model TEXT,
	protocol TEXT NOT NULL,
	streaming INTEGER NOT NULL CHECK (streaming IN (0,1)),
	http_status INTEGER NOT NULL,
	latency_ms INTEGER NOT NULL,
	input_tokens INTEGER,
	output_tokens INTEGER,
	cache_read_input_tokens INTEGER,
	cache_creation_input_tokens INTEGER,
	provider_request_id TEXT,
	client_request_id TEXT NOT NULL,
	error_text TEXT,
	error_message TEXT,
	request_body TEXT,
	request_body_truncated INTEGER NOT NULL DEFAULT 0 CHECK (request_body_truncated IN (0,1)),
	error_body TEXT,
	error_body_truncated INTEGER NOT NULL DEFAULT 0 CHECK (error_body_truncated IN (0,1)),
	attempt_count INTEGER NOT NULL DEFAULT 1,
	fallback_used INTEGER NOT NULL DEFAULT 0 CHECK (fallback_used IN (0,1)),
	fallback_reason TEXT,
	created_at TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS request_logs_client_created ON request_logs(client_key_id, created_at DESC);
CREATE INDEX IF NOT EXISTS request_logs_created ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS request_logs_route_created ON request_logs(route_kind, route_model_id, created_at);
CREATE INDEX IF NOT EXISTS request_logs_account_created ON request_logs(account_id, created_at DESC);

CREATE TABLE IF NOT EXISTS request_attempts (
	id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
	request_log_id TEXT NOT NULL REFERENCES request_logs(id) ON DELETE CASCADE,
	attempt_number INTEGER NOT NULL CHECK (attempt_number >= 1),
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	result TEXT NOT NULL CHECK (result IN ('success','failed','skipped')),
	http_status INTEGER,
	failure_class TEXT,
	error_message TEXT,
	error_body TEXT,
	error_body_truncated INTEGER NOT NULL DEFAULT 0 CHECK (error_body_truncated IN (0,1)),
	latency_ms INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE(request_log_id, attempt_number)
) STRICT;

CREATE INDEX IF NOT EXISTS request_attempts_request ON request_attempts(request_log_id, attempt_number);
CREATE INDEX IF NOT EXISTS request_attempts_account ON request_attempts(account_id, request_log_id);
`

// servicePragmas are the pragmas shared by the central database and the
// Activity database.
//
// synchronous=NORMAL under WAL removes the per-commit fsync: transactions are
// durable at the next checkpoint rather than on every commit. A power loss can
// lose the most recent commits but cannot corrupt the database. Activity is
// best-effort telemetry and the control plane is small, so this is the right
// trade for keeping the request path off the disk-flush critical path.
func servicePragmas() url.Values {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "temp_store(MEMORY)")
	return q
}

// sqliteDSN builds the file DSN for a SQLite database at path with the given
// pragmas.
func sqliteDSN(path string, pragmas url.Values) string {
	dsnURL := url.URL{Scheme: "file", Path: path}
	dsnURL.RawQuery = pragmas.Encode()
	return dsnURL.String()
}

// OpenActivity opens (creating if needed) the Activity database at path and
// ensures its schema is present. Unlike the central and audit databases,
// Activity is best-effort telemetry: Open treats a failure here as non-fatal
// so a missing or corrupt activity.db cannot prevent Tiller from starting or
// routing requests.
func OpenActivity(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, servicePragmas()))
	if err != nil {
		return nil, err
	}
	// A small pool lets Activity reads progress while one connection is
	// mid-write; SQLite serializes the writes.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, activitySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("activity schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO activity_schema_migrations(version,applied_at) VALUES('001',?)`, Now()); err != nil {
		db.Close()
		return nil, err
	}
	if err := restrictFileMode(path); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrateActivity performs the one-time move of the Activity tables out of the
// central database into the separate Activity database. It is idempotent: rows
// are copied with INSERT OR REPLACE and the central tables are dropped only
// after every row has been copied and verified, so an interrupted run resumes
// cleanly. A fresh or already-migrated database has no central request_logs
// table and is a no-op.
func (d *DB) migrateActivity(ctx context.Context) error {
	if d.Activity == nil {
		return nil
	}
	var n int
	if err := d.SQL.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='request_logs'`).Scan(&n); err != nil {
		return fmt.Errorf("check activity split: %w", err)
	}
	if n == 0 {
		return nil
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	quoted := strings.ReplaceAll(d.ActivityPath, "'", "''")
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE '`+quoted+`' AS act`); err != nil {
		return fmt.Errorf("attach activity db: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `DETACH DATABASE act`) }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO act.request_logs(id,account_id,client_key_id,client_name,requested_model,exposed_model,route_kind,route_model_id,route_model,route_status,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,error_message,request_body,request_body_truncated,error_body,error_body_truncated,attempt_count,fallback_used,fallback_reason,created_at)
		SELECT rl.id,rl.account_id,rl.client_key_id,coalesce(ck.name,''),rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.route_status,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.cache_creation_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.error_message,rl.request_body,rl.request_body_truncated,rl.error_body,rl.error_body_truncated,rl.attempt_count,rl.fallback_used,rl.fallback_reason,rl.created_at
		FROM main.request_logs rl LEFT JOIN main.client_keys ck ON ck.id=rl.client_key_id`); err != nil {
		return fmt.Errorf("copy request_logs: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO act.request_attempts(id,account_id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at)
		SELECT id,account_id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at
		FROM main.request_attempts`); err != nil {
		return fmt.Errorf("copy request_attempts: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	// Prove the copy before dropping the source: every central row must be
	// present in the Activity database.
	if err := verifyActivityCopy(ctx, conn); err != nil {
		return err
	}
	// request_attempts references request_logs, so drop the child first. The
	// two drops run in one transaction: an interrupted migration either leaves
	// both central tables intact (to resume) or neither. SQLite DDL is
	// transactional.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS request_attempts`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("drop central request_attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS request_logs`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("drop central request_logs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activity split: %w", err)
	}
	// The 035_activity_split.sql migration records the split version; the data
	// move itself is Go-side and idempotent via the table-existence check.
	return nil
}

// verifyActivityCopy fails if any row in the central request_logs or
// request_attempts table is missing from the Activity database. It is the gate
// that protects the source rows from being dropped after a partial copy.
func verifyActivityCopy(ctx context.Context, conn *sql.Conn) error {
	var missingLogs int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM main.request_logs rl WHERE NOT EXISTS (SELECT 1 FROM act.request_logs a WHERE a.id=rl.id)`).Scan(&missingLogs); err != nil {
		return fmt.Errorf("verify request_logs copy: %w", err)
	}
	if missingLogs != 0 {
		return fmt.Errorf("activity migration incomplete: %d request_logs rows missing", missingLogs)
	}
	var missingAttempts int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM main.request_attempts ra WHERE NOT EXISTS (SELECT 1 FROM act.request_attempts a WHERE a.id=ra.id)`).Scan(&missingAttempts); err != nil {
		return fmt.Errorf("verify request_attempts copy: %w", err)
	}
	if missingAttempts != 0 {
		return fmt.Errorf("activity migration incomplete: %d request_attempts rows missing", missingAttempts)
	}
	return nil
}
