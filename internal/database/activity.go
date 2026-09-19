package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ActivityDirName is the subdirectory (under the data dir) that holds one
// SQLite file per account for the high-churn, disposable Activity tables
// (request_logs and request_attempts). The control-plane tables stay in the
// central database file; Activity is split out so the central backup never
// contains logs and so a single account's logging cannot block another's.
const ActivityDirName = "activity"

// accountIDPattern matches the UUID shape produced by internal/id and the
// fixed LocalAccountID. Account identifiers are used as SQLite filenames, so
// they are validated before any path join — never trusted from request input.
var accountIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateAccountID reports whether id is a safe, well-formed account
// identifier. It is the gate between an account id and a filesystem path.
func ValidateAccountID(id string) error {
	if !accountIDPattern.MatchString(id) {
		return fmt.Errorf("invalid account id %q", id)
	}
	return nil
}

// activitySchema is the per-account Activity schema. It deliberately has no
// foreign key to client_keys (that table lives in the central database) and
// carries a denormalized client_name so Activity reads never cross files. The
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

// servicePragmas are the pragmas shared by the central database and every
// per-account Activity database.
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

// OpenActivity opens (creating if needed) the per-account Activity database at
// path and ensures its schema is present. Activity files are opened lazily by
// the store's handle manager, never on the request path's critical section.
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
	// One connection per Activity file: writes are serialized by SQLite, and a
	// single idle connection bounds file descriptors across many accounts.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
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
// central database into per-account files. It is idempotent: rows are copied
// with INSERT OR REPLACE and the central tables are dropped only after every
// account has been copied, so an interrupted run resumes cleanly.
func (d *DB) migrateActivity(ctx context.Context) error {
	var n int
	if err := d.SQL.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='request_logs'`).Scan(&n); err != nil {
		return fmt.Errorf("check activity split: %w", err)
	}
	if n == 0 {
		return nil
	}
	rows, err := d.SQL.QueryContext(ctx, `SELECT DISTINCT account_id FROM request_logs`)
	if err != nil {
		return fmt.Errorf("list activity accounts: %w", err)
	}
	var accounts []string
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, account)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, account := range accounts {
		if err := d.copyAccountActivity(ctx, account); err != nil {
			return err
		}
	}
	// request_attempts references request_logs, so drop the child first.
	if _, err := d.SQL.ExecContext(ctx, `DROP TABLE IF EXISTS request_attempts`); err != nil {
		return fmt.Errorf("drop central request_attempts: %w", err)
	}
	if _, err := d.SQL.ExecContext(ctx, `DROP TABLE IF EXISTS request_logs`); err != nil {
		return fmt.Errorf("drop central request_logs: %w", err)
	}
	// The 035_activity_split.sql migration records the split version; the
	// data move itself is Go-side and idempotent via the table-existence check.
	return nil
}

// copyAccountActivity moves one account's request logs and attempts from the
// central database into its Activity file. The Activity schema is created
// first; the copy runs with foreign-key enforcement off on a dedicated
// connection and is re-verified with foreign_key_check.
func (d *DB) copyAccountActivity(ctx context.Context, accountID string) error {
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	path := filepath.Join(d.ActivityDir, accountID+".db")
	adb, err := OpenActivity(ctx, path)
	if err != nil {
		return err
	}
	adb.Close()

	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE '`+quoted+`' AS act`); err != nil {
		return fmt.Errorf("attach activity db: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `DETACH DATABASE act`) }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO act.request_logs(id,account_id,client_key_id,client_name,requested_model,exposed_model,route_kind,route_model_id,route_model,route_status,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,error_message,request_body,request_body_truncated,error_body,error_body_truncated,attempt_count,fallback_used,fallback_reason,created_at)
		SELECT rl.id,rl.account_id,rl.client_key_id,coalesce(ck.name,''),rl.requested_model,rl.exposed_model,rl.route_kind,rl.route_model_id,rl.route_model,rl.route_status,rl.resolved_provider,rl.resolved_model,rl.protocol,rl.streaming,rl.http_status,rl.latency_ms,rl.input_tokens,rl.output_tokens,rl.cache_read_input_tokens,rl.cache_creation_input_tokens,rl.provider_request_id,rl.client_request_id,rl.error_text,rl.error_message,rl.request_body,rl.request_body_truncated,rl.error_body,rl.error_body_truncated,rl.attempt_count,rl.fallback_used,rl.fallback_reason,rl.created_at
		FROM main.request_logs rl LEFT JOIN main.client_keys ck ON ck.id=rl.client_key_id WHERE rl.account_id=?`, accountID); err != nil {
		return fmt.Errorf("copy request_logs: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO act.request_attempts(id,account_id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at)
		SELECT id,account_id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,error_body,error_body_truncated,latency_ms,created_at
		FROM main.request_attempts WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("copy request_attempts: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	checkRows, err := conn.QueryContext(ctx, `PRAGMA act.foreign_key_check`)
	if err != nil {
		return fmt.Errorf("activity foreign_key_check: %w", err)
	}
	violations := 0
	for checkRows.Next() {
		violations++
	}
	checkRows.Close()
	if err := checkRows.Err(); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("activity foreign_key_check: %d violations for account %s", violations, accountID)
	}
	return restrictFileMode(path)
}
