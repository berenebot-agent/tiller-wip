package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestActivitySchemaUpgradeAddsCostColumns proves the activity upgrade path:
// an existing v1 Activity database (created before the cost columns) gains the
// new columns on open, without losing rows, and the upgrade is idempotent.
func TestActivitySchemaUpgradeAddsCostColumns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ActivityFileName)
	ctx := context.Background()

	// Create a v1-shaped Activity database without the cost columns. The
	// pre-existing row carries both an input and an output count so the
	// provenance backfill can be asserted for each direction.
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
INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES('old','00000000-0000-0000-0000-000000000001','ck','m','chat',0,200,1,10,4,'req','2026-01-01T00:00:00Z');
INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,input_tokens,client_request_id,created_at) VALUES('nooutput','00000000-0000-0000-0000-000000000001','ck','m','chat',0,200,1,7,'req-noout','2026-01-01T00:00:00Z');`
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

	for _, col := range []string{"estimated_cost_micros", "provider_cost_micros", "input_tokens_source", "output_tokens_source"} {
		exists, err := activityColumnExists(ctx, adb, "request_logs", col)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("column %s was not added by the upgrade", col)
		}
	}
	// The retired input-only boolean is dropped by the provenance step.
	if exists, err := activityColumnExists(ctx, adb, "request_logs", "input_tokens_estimated"); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("input_tokens_estimated should have been dropped by 006_drop_input_tokens_estimated")
	}
	// The pre-existing rows survived.
	var n int
	if err := adb.QueryRowContext(ctx, `SELECT count(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pre-existing row count = %d, want 2", n)
	}
	// The provenance backfill labels both directions: the row with an input and
	// an output count is provider/provider; the row with only an input count
	// leaves output_tokens_source NULL.
	var inSrc, outSrc sql.NullString
	if err := adb.QueryRowContext(ctx, `SELECT input_tokens_source, output_tokens_source FROM request_logs WHERE id='old'`).Scan(&inSrc, &outSrc); err != nil {
		t.Fatal(err)
	}
	if !inSrc.Valid || inSrc.String != "provider" {
		t.Fatalf("backfilled input_tokens_source = %v, want provider", inSrc)
	}
	if !outSrc.Valid || outSrc.String != "provider" {
		t.Fatalf("backfilled output_tokens_source = %v, want provider", outSrc)
	}
	var inNoOut, outNoOut sql.NullString
	if err := adb.QueryRowContext(ctx, `SELECT input_tokens_source, output_tokens_source FROM request_logs WHERE id='nooutput'`).Scan(&inNoOut, &outNoOut); err != nil {
		t.Fatal(err)
	}
	if !inNoOut.Valid || inNoOut.String != "provider" {
		t.Fatalf("row without output: input_tokens_source = %v, want provider", inNoOut)
	}
	if outNoOut.Valid {
		t.Fatalf("row without output: output_tokens_source = %q, want NULL", outNoOut.String)
	}
	// The upgrade versions are recorded.
	for _, version := range []string{"002_cost_and_estimate", "004_token_provenance_columns", "005_token_provenance_backfill", "006_drop_input_tokens_estimated"} {
		var recorded int
		if err := adb.QueryRowContext(ctx, `SELECT count(*) FROM activity_schema_migrations WHERE version=?`, version).Scan(&recorded); err != nil {
			t.Fatal(err)
		}
		if recorded != 1 {
			t.Fatalf("upgrade version %s rows = %d, want 1", version, recorded)
		}
	}
	adb.Close()

	// Re-opening is a no-op (idempotent).
	adb2, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb2.Close()
}

// TestActivityRetiredColumnStaysDropped is the regression for two review
// findings about the retired input_tokens_estimated column:
//
//  1. It must not return on a later boot. The additive 002_cost_and_estimate
//     step re-adds the column whenever it is absent, so an earlier design that
//     used the drop's own version row as the guard recreated it on the second
//     startup. It is now gated so it is never re-added once
//     006_drop_input_tokens_estimated has completed.
//  2. Reaching steady state must not rewrite the table on every boot. SQLite's
//     DROP COLUMN rewrites the whole table, so the column probe plus a re-add
//     would cost a full table copy per startup. The test asserts the schema is
//     unchanged across subsequent opens (the recorded migration set and the
//     stored CREATE TABLE text for request_logs), which fails if any schema
//     mutation runs on a steady-state boot.
func TestActivityRetiredColumnStaysDropped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ActivityFileName)
	ctx := context.Background()

	// Seed a v1-shaped database that still has the retired column, so the drop
	// path runs on the first open.
	raw, err := sql.Open("sqlite", sqliteDSN(path, servicePragmas()))
	if err != nil {
		t.Fatal(err)
	}
	seed := `CREATE TABLE activity_schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
CREATE TABLE request_logs (
	id TEXT PRIMARY KEY, account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
	client_key_id TEXT NOT NULL, client_name TEXT NOT NULL DEFAULT '', requested_model TEXT NOT NULL,
	exposed_model TEXT, route_kind TEXT, route_model_id TEXT, route_model TEXT,
	route_status TEXT NOT NULL DEFAULT 'legacy', resolved_provider TEXT, resolved_model TEXT,
	protocol TEXT NOT NULL, streaming INTEGER NOT NULL, http_status INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
	input_tokens INTEGER, output_tokens INTEGER, cache_read_input_tokens INTEGER, cache_creation_input_tokens INTEGER,
	estimated_cost_micros INTEGER, provider_cost_micros INTEGER, input_tokens_estimated INTEGER NOT NULL DEFAULT 0,
	provider_request_id TEXT, client_request_id TEXT NOT NULL, error_text TEXT, error_message TEXT,
	request_body TEXT, request_body_truncated INTEGER NOT NULL DEFAULT 0, error_body TEXT,
	error_body_truncated INTEGER NOT NULL DEFAULT 0, attempt_count INTEGER NOT NULL DEFAULT 1,
	fallback_used INTEGER NOT NULL DEFAULT 0, fallback_reason TEXT, created_at TEXT NOT NULL) STRICT;
INSERT INTO activity_schema_migrations(version,applied_at) VALUES('001','2026-01-01T00:00:00Z');`
	if _, err := raw.Exec(seed); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	// snapshot captures everything that must be identical on a steady-state
	// boot: the recorded migration set and the database's persisted schema
	// version. SQLite's PRAGMA schema_version is stored in the database header
	// and incremented by any DDL — including a DROP COLUMN's table rebuild and an
	// identical ADD/DROP round-trip — so it is the direct signal that no schema
	// mutation ran, and it survives across connections. Comparing sqlite_master
	// text alone would miss a re-add/drop that reproduces the same CREATE TABLE
	// text.
	type snapshot struct {
		versions      string
		schemaVersion int
	}
	read := func(adb *sql.DB, open int) snapshot {
		t.Helper()
		rows, err := adb.QueryContext(ctx, `SELECT version FROM activity_schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatalf("open %d: versions: %v", open, err)
		}
		var versions []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				t.Fatalf("open %d: scan version: %v", open, err)
			}
			versions = append(versions, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("open %d: versions: %v", open, err)
		}
		var schemaVersion int
		if err := adb.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
			t.Fatalf("open %d: schema_version: %v", open, err)
		}
		return snapshot{versions: strings.Join(versions, ","), schemaVersion: schemaVersion}
	}

	var first snapshot
	for i := 0; i < 3; i++ {
		adb, err := OpenActivity(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		exists, err := activityColumnExists(ctx, adb, "request_logs", "input_tokens_estimated")
		if err != nil {
			adb.Close()
			t.Fatalf("open %d: probe: %v", i, err)
		}
		if exists {
			adb.Close()
			t.Fatalf("open %d: retired column input_tokens_estimated reappeared", i)
		}
		got := read(adb, i)
		adb.Close()
		if i == 0 {
			first = got
			continue
		}
		if got.versions != first.versions {
			t.Fatalf("open %d: migration set changed: %q -> %q", i, first.versions, got.versions)
		}
		if got.schemaVersion != first.schemaVersion {
			t.Fatalf("open %d: schema_version changed (%d -> %d): a DDL mutation ran on a steady-state boot", i, first.schemaVersion, got.schemaVersion)
		}
	}
}

// TestActivityDataUpgradeReclassifiesLegacyRouteStatus proves the one-time data
// upgrade rewrites historical route_status='legacy' rows to 'unresolved', keeps
// already-routed rows untouched, records its version, and is idempotent.
func TestActivityDataUpgradeReclassifiesLegacyRouteStatus(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ActivityFileName)
	ctx := context.Background()

	// Create a current-schema Activity database with a legacy and a routed row.
	raw, err := sql.Open("sqlite", sqliteDSN(path, servicePragmas()))
	if err != nil {
		t.Fatal(err)
	}
	seed := `
CREATE TABLE activity_schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
CREATE TABLE request_logs (
	id TEXT PRIMARY KEY, account_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
	client_key_id TEXT NOT NULL, client_name TEXT NOT NULL DEFAULT '', requested_model TEXT NOT NULL,
	exposed_model TEXT, route_kind TEXT, route_model_id TEXT, route_model TEXT,
	route_status TEXT NOT NULL DEFAULT 'legacy', resolved_provider TEXT, resolved_model TEXT,
	protocol TEXT NOT NULL, streaming INTEGER NOT NULL, http_status INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
	input_tokens INTEGER, output_tokens INTEGER, cache_read_input_tokens INTEGER, cache_creation_input_tokens INTEGER,
	estimated_cost_micros INTEGER, provider_cost_micros INTEGER, input_tokens_estimated INTEGER NOT NULL DEFAULT 0,
	provider_request_id TEXT, client_request_id TEXT NOT NULL, error_text TEXT, error_message TEXT,
	request_body TEXT, request_body_truncated INTEGER NOT NULL DEFAULT 0, error_body TEXT,
	error_body_truncated INTEGER NOT NULL DEFAULT 0, attempt_count INTEGER NOT NULL DEFAULT 1,
	fallback_used INTEGER NOT NULL DEFAULT 0, fallback_reason TEXT, created_at TEXT NOT NULL) STRICT;
INSERT INTO activity_schema_migrations(version,applied_at) VALUES('001','2026-01-01T00:00:00Z');
INSERT INTO request_logs(id,account_id,client_key_id,requested_model,route_kind,route_status,protocol,streaming,http_status,latency_ms,client_request_id,created_at)
	VALUES('legacy','00000000-0000-0000-0000-000000000001','ck','m',NULL,'legacy','chat',0,200,1,'req-l','2026-01-01T00:00:00Z');
INSERT INTO request_logs(id,account_id,client_key_id,requested_model,route_kind,route_status,protocol,streaming,http_status,latency_ms,client_request_id,created_at)
	VALUES('routed','00000000-0000-0000-0000-000000000001','ck','m','virtual','routed','chat',0,200,1,'req-r','2026-01-01T00:00:00Z');`
	if _, err := raw.Exec(seed); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	adb, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()

	var legacyStatus, routedStatus string
	if err := adb.QueryRowContext(ctx, `SELECT route_status FROM request_logs WHERE id='legacy'`).Scan(&legacyStatus); err != nil {
		t.Fatal(err)
	}
	if err := adb.QueryRowContext(ctx, `SELECT route_status FROM request_logs WHERE id='routed'`).Scan(&routedStatus); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != "unresolved" {
		t.Fatalf("legacy row status = %q, want unresolved", legacyStatus)
	}
	if routedStatus != "routed" {
		t.Fatalf("routed row status = %q, want routed (untouched)", routedStatus)
	}
	var recorded int
	if err := adb.QueryRowContext(ctx, `SELECT count(*) FROM activity_schema_migrations WHERE version='003_legacy_route_unresolved'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("upgrade version rows = %d, want 1", recorded)
	}
	adb.Close()

	// Re-opening is a no-op: no legacy rows remain and the version is recorded.
	adb2, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb2.Close()
	var remaining int
	if err := adb2.QueryRowContext(ctx, `SELECT count(*) FROM request_logs WHERE route_status='legacy'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("legacy rows remaining after rerun = %d, want 0", remaining)
	}
}

// TestActivityDataUpgradePreservesAmbiguousInput proves that cache metrics do
// not establish upstream accounting provenance. All historical counts survive.
func TestActivityDataUpgradePreservesAmbiguousInput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ActivityFileName)
	ctx := context.Background()
	adb, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Pretend these rows were written by a build that stored the upstream
	// counts verbatim: drop the marker so the one-time rewrite runs over them.
	if _, err := adb.ExecContext(ctx, `DELETE FROM activity_schema_migrations WHERE version='007_cache_inclusive_input'`); err != nil {
		adb.Close()
		t.Fatal(err)
	}
	insert := func(id string, input, read, creation any) {
		t.Helper()
		if _, err := adb.ExecContext(ctx, `INSERT INTO request_logs(id,account_id,client_key_id,client_name,requested_model,protocol,streaming,http_status,latency_ms,input_tokens,cache_read_input_tokens,cache_creation_input_tokens,client_request_id,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, LocalAccountID, "ck", "ck", "m", "messages", 0, 200, 1, input, read, creation, id, Now()); err != nil {
			adb.Close()
			t.Fatal(err)
		}
	}
	insert("anthropic", int64(10), int64(90), int64(20))     // uncached 10 + read 90 + write 20
	insert("anthropic-read", int64(10), int64(90), int64(0)) // zero creation is not provenance
	insert("inclusive-creation", int64(120), int64(90), int64(20))
	insert("inclusive-zero", int64(100), int64(90), int64(0))
	insert("openai", int64(100), int64(90), nil)    // already inclusive
	insert("ambiguous", int64(100), int64(40), nil) // 140 (Anthropic) or 100 (OpenAI): do not guess
	insert("no-input", nil, int64(40), int64(10))   // nothing to normalise
	adb.Close()

	upgraded, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]sql.NullInt64{
		"anthropic":          {Int64: 10, Valid: true},
		"anthropic-read":     {Int64: 10, Valid: true},
		"inclusive-creation": {Int64: 120, Valid: true},
		"inclusive-zero":     {Int64: 100, Valid: true},
		"openai":             {Int64: 100, Valid: true},
		"ambiguous":          {Int64: 100, Valid: true},
		"no-input":           {},
	}
	for id, expected := range want {
		var got sql.NullInt64
		if err := upgraded.QueryRowContext(ctx, `SELECT input_tokens FROM request_logs WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != expected {
			t.Fatalf("%s: input_tokens = %+v, want %+v", id, got, expected)
		}
	}
	var recorded int
	if err := upgraded.QueryRowContext(ctx, `SELECT count(*) FROM activity_schema_migrations WHERE version='007_cache_inclusive_input'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("upgrade version rows = %d, want 1", recorded)
	}
	upgraded.Close()

	// Re-opening must preserve the historical counts too.
	reopened, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var input int64
	if err := reopened.QueryRowContext(ctx, `SELECT input_tokens FROM request_logs WHERE id='anthropic'`).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if input != 10 {
		t.Fatalf("input_tokens after reopen = %d, want unchanged 10", input)
	}
}
