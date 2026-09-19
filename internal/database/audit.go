package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// AuditFileName is the fixed filename of the central audit database under the
// data directory. Unlike Activity, audit events are security-relevant and live
// in a single central file shared by every account, so a suspended or deleted
// account cannot take its security history with it. The core control-plane
// database deliberately does not hold audit rows; see AGENTS.md and
// docs/hosted_status.md.
const AuditFileName = "audit.db"

// auditSchema is the central audit schema. account_audit_events is
// account-scoped (ClassTenant); platform_audit_events is platform-global and is
// never exposed through a customer API. Metadata is a bounded JSON object and
// must never contain credentials, tokens, prompts, responses, or email bodies.
const auditSchema = `
CREATE TABLE IF NOT EXISTS audit_schema_migrations (
  version TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS account_audit_events (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  event TEXT NOT NULL,
  actor_type TEXT NOT NULL,
  actor_id TEXT,
  target_type TEXT,
  target_id TEXT,
  outcome TEXT NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure')),
  metadata TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS account_audit_events_account_created ON account_audit_events(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS account_audit_events_created ON account_audit_events(created_at);

CREATE TABLE IF NOT EXISTS platform_audit_events (
  id TEXT PRIMARY KEY,
  event TEXT NOT NULL,
  actor_type TEXT NOT NULL DEFAULT 'platform',
  actor_id TEXT,
  target_type TEXT,
  target_id TEXT,
  outcome TEXT NOT NULL DEFAULT 'success' CHECK (outcome IN ('success','failure')),
  metadata TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS platform_audit_events_created ON platform_audit_events(created_at);

CREATE TABLE IF NOT EXISTS audit_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
) STRICT;

INSERT OR IGNORE INTO audit_meta(key,value,updated_at) VALUES('audit_retention_days','365',strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`

// OpenAudit opens (creating if needed) the central audit database at path and
// ensures its schema is present. It uses the same pragmas, ownership, and 0600
// hardening as the core and Activity databases.
func OpenAudit(ctx context.Context, path string) (*sql.DB, error) {
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
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, auditSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO audit_schema_migrations(version,applied_at) VALUES('001',?)`, Now()); err != nil {
		db.Close()
		return nil, err
	}
	if err := restrictFileMode(path); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
