package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestActivitySchemaUpgradeAddsCostColumns proves the activity upgrade path:
// an existing v1 Activity database (created before the cost columns) gains the
// new columns on open, without losing rows, and the upgrade is idempotent.
func TestActivitySchemaUpgradeAddsCostColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), ActivityFileName)
	ctx := context.Background()

	// Create a v1-shaped Activity database without the cost columns.
	raw, err := sql.Open("sqlite", sqliteDSN(path, servicePragmas()))
	if err != nil {
		t.Fatal(err)
	}
	v1 := `CREATE TABLE activity_schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
CREATE TABLE request_logs (
	id TEXT PRIMARY KEY, account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
	client_key_id TEXT NOT NULL, client_name TEXT NOT NULL DEFAULT '', requested_model TEXT NOT NULL,
	exposed_model TEXT, route_kind TEXT, route_model_id TEXT, route_model TEXT,
	route_status TEXT NOT NULL DEFAULT 'legacy', resolved_provider TEXT, resolved_model TEXT,
	protocol TEXT NOT NULL, streaming INTEGER NOT NULL, http_status INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
	input_tokens INTEGER, output_tokens INTEGER, cache_read_input_tokens INTEGER, cache_creation_input_tokens INTEGER,
	provider_request_id TEXT, client_request_id TEXT NOT NULL, error_text TEXT, error_message TEXT,
	request_body TEXT, request_body_truncated INTEGER NOT NULL DEFAULT 0, error_body TEXT,
	error_body_truncated INTEGER NOT NULL DEFAULT 0, attempt_count INTEGER NOT NULL DEFAULT 1,
	fallback_used INTEGER NOT NULL DEFAULT 0, fallback_reason TEXT, created_at TEXT NOT NULL) STRICT;
INSERT INTO activity_schema_migrations(version,applied_at) VALUES('001','2026-01-01T00:00:00Z');
INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('old','00000000-0000-0000-0000-000000000001','ck','m','chat',0,200,1,'req','2026-01-01T00:00:00Z');`
	if _, err := raw.Exec(v1); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	// OpenActivity should upgrade the schema in place.
	adb, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()

	for _, col := range []string{"estimated_cost_micros", "provider_cost_micros", "input_tokens_estimated"} {
		exists, err := activityColumnExists(ctx, adb, "request_logs", col)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("column %s was not added by the upgrade", col)
		}
	}
	// The pre-existing row survived.
	var n int
	if err := adb.QueryRowContext(ctx, `SELECT count(*) FROM request_logs WHERE id='old'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pre-existing row count = %d, want 1", n)
	}
	// The upgrade version is recorded.
	var recorded int
	if err := adb.QueryRowContext(ctx, `SELECT count(*) FROM activity_schema_migrations WHERE version='002_cost_and_estimate'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("upgrade version rows = %d, want 1", recorded)
	}
	adb.Close()

	// Re-opening is a no-op (idempotent).
	adb2, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb2.Close()
}
