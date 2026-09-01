package database

import (
	"context"
	"database/sql"
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

// TestBackfillMigration verifies migration 014 derives route_kind/route_model_id/
// route_model for legacy rows (route_kind NULL) where derivable, and leaves
// unmappable rows NULL.
func TestBackfillMigration(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := Now()
	for _, ns := range []struct{ name, kind, entity string }{
		{"prov", "real", "p1"},
		{"vg", "virtual", "g1"},
	} {
		if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES(?,?,?)`, ns.name, ns.kind, ns.entity); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('p1','prov','generic-openai','http://example.test/v1',1,'["chat"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO provider_models(id,provider_id,upstream_model_id,first_seen_at,last_seen_at,created_at,updated_at) VALUES('m1','p1','model-a',?,?,?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO virtual_provider_groups(id,name,created_at,updated_at) VALUES('g1','vg',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO virtual_models(id,virtual_group_id,name,target_provider_id,target_provider_model_id,created_at,updated_at) VALUES('vm1','g1','coding','p1','m1',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO client_keys(id,name,selector,secret_hash,secret_fingerprint,created_at,updated_at) VALUES('ck1','client','sel','h','f',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	insert := func(id, requested, rp, rm string) {
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, "ck1", requested, rp, rm, "chat", 0, 200, 1, "req-"+id, now); err != nil {
			t.Fatal(err)
		}
	}
	insert("row-virtual", "vg/coding", "prov", "model-a") // matches virtual canonical
	insert("row-real", "prov/model-a", "prov", "model-a") // matches real model
	insert("row-unmappable", "unknown/x", "other", "y")   // matches neither

	// Run the backfill migration body (already applied on empty tables at Open,
	// so re-run it now that legacy rows exist).
	body, err := migrations.ReadFile("migrations/014_backfill_route_attribution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(string(body)); err != nil {
		t.Fatalf("backfill migration failed: %v", err)
	}

	var kind, modelID, routeModel sql.NullString
	if err := db.SQL.QueryRow(`SELECT route_kind,route_model_id,route_model FROM request_logs WHERE id='row-virtual'`).Scan(&kind, &modelID, &routeModel); err != nil {
		t.Fatal(err)
	}
	if kind.String != "virtual" || modelID.String != "vm1" || routeModel.String != "vg/coding" {
		t.Fatalf("virtual backfill wrong: kind=%q id=%q model=%q", kind.String, modelID.String, routeModel.String)
	}
	if err := db.SQL.QueryRow(`SELECT route_kind,route_model_id,route_model FROM request_logs WHERE id='row-real'`).Scan(&kind, &modelID, &routeModel); err != nil {
		t.Fatal(err)
	}
	if kind.String != "real" || modelID.String != "m1" || routeModel.String != "prov/model-a" {
		t.Fatalf("real backfill wrong: kind=%q id=%q model=%q", kind.String, modelID.String, routeModel.String)
	}
	// Unmappable row stays NULL.
	if err := db.SQL.QueryRow(`SELECT route_kind,route_model_id,route_model FROM request_logs WHERE id='row-unmappable'`).Scan(&kind, &modelID, &routeModel); err != nil {
		t.Fatal(err)
	}
	if kind.Valid || modelID.Valid || routeModel.Valid {
		t.Fatalf("unmappable row should stay NULL: kind=%q id=%q model=%q", kind.String, modelID.String, routeModel.String)
	}
}

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
