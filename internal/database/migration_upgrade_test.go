package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"testing"

	_ "modernc.org/sqlite"
)

// openMigrationFixture creates a database that looks like an installation
// stopped immediately after checkpoint migrations have been applied. Keeping
// this fixture independent of Open is important: Open itself always runs the
// complete migration set.
func openMigrationFixture(t *testing.T, path string, checkpoint int) {
	t.Helper()
	entries, err := fsMigrationEntries()
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint < 0 || checkpoint > len(entries) {
		t.Fatalf("invalid migration checkpoint %d", checkpoint)
	}

	raw := openRawMigrationDB(t, path)
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries[:checkpoint] {
		tx, err := raw.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(entry.body); err != nil {
			tx.Rollback()
			t.Fatalf("apply fixture migration %s: %v", entry.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, entry.name, Now()); err != nil {
			tx.Rollback()
			t.Fatalf("record fixture migration %s: %v", entry.name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit fixture migration %s: %v", entry.name, err)
		}
	}
	if checkpoint == 0 {
		return
	}
	seedMigrationFixture(t, raw, checkpoint)
}

func openRawMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsnURL := url.URL{Scheme: "file", Path: path}
	query := dsnURL.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	dsnURL.RawQuery = query.Encode()
	raw, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type migrationEntry struct {
	name string
	body string
}

func fsMigrationEntries() ([]migrationEntry, error) {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]migrationEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, migrationEntry{name: entry.Name(), body: string(body)})
	}
	return result, nil
}

func seedMigrationFixture(t *testing.T, raw *sql.DB, checkpoint int) {
	t.Helper()
	now := Now()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO namespaces(name,kind,entity_id) VALUES('provider-a','real','p1')`, nil},
		{`INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES('p1','provider-a','generic-openai','http://example.test/v1',NULL,1,'["chat"]',?,?)`, []any{now, now}},
		{`INSERT INTO provider_models(id,provider_id,upstream_model_id,display_name,available,first_seen_at,last_seen_at,created_at,updated_at) VALUES('m1','p1','model-a','Fixture model',1,?,?,?,?)`, []any{now, now, now, now}},
		{`INSERT INTO namespaces(name,kind,entity_id) VALUES('virtual','virtual','g1')`, nil},
		{`INSERT INTO virtual_provider_groups(id,name,created_at,updated_at) VALUES('g1','virtual',?,?)`, []any{now, now}},
		{`INSERT INTO virtual_models(id,virtual_group_id,name,target_provider_id,target_provider_model_id,created_at,updated_at) VALUES('vm1','g1','coding','p1','m1',?,?)`, []any{now, now}},
		{`INSERT INTO client_keys(id,name,selector,secret_hash,secret_fingerprint,created_at,updated_at) VALUES('ck1','fixture-client','fixture-selector','fixture-hash','fixture-fingerprint',?,?)`, []any{now, now}},
		{`INSERT INTO client_model_permissions(client_key_id,model_kind,model_id,enabled,created_at,updated_at) VALUES('ck1','virtual','vm1',1,?,?)`, []any{now, now}},
	}
	for _, statement := range statements {
		if _, err := raw.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed checkpoint %d: %v", checkpoint, err)
		}
	}
	// request_logs exists after migration 003. This legacy-shaped row is
	// intentionally left without route attribution for migration 014 to fill.
	if checkpoint >= 3 {
		_, err := raw.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('log1','ck1','virtual/coding','provider-a','model-a','chat',0,200,12,'request-1',?)`, now)
		if err != nil {
			t.Fatalf("seed request log at checkpoint %d: %v", checkpoint, err)
		}
	}
	if checkpoint == 18 {
		if _, err := raw.Exec(`UPDATE request_logs SET error_text='upstream_error',error_message='PROVIDER-ERROR-SECRET-MARKER' WHERE id='log1'`); err != nil {
			t.Fatalf("seed legacy request error at checkpoint %d: %v", checkpoint, err)
		}
		if _, err := raw.Exec(`INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,error_message,latency_ms,created_at) VALUES('attempt1','log1',1,'provider-a','model-a','failed',502,'http_502','PROVIDER-ERROR-SECRET-MARKER',8,?)`, now); err != nil {
			t.Fatalf("seed legacy attempt error at checkpoint %d: %v", checkpoint, err)
		}
	}
}

func TestMigrateFromEveryCheckpointPreservesData(t *testing.T) {
	entries, err := fsMigrationEntries()
	if err != nil {
		t.Fatal(err)
	}
	wantVersions := make([]string, 0, len(entries))
	for _, entry := range entries {
		wantVersions = append(wantVersions, entry.name)
	}

	for checkpoint := 0; checkpoint <= len(entries); checkpoint++ {
		t.Run(fmt.Sprintf("after_%s", func() string {
			if checkpoint == 0 {
				return "none"
			}
			return entries[checkpoint-1].name
		}()), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.db")
			openMigrationFixture(t, path, checkpoint)
			db, err := Open(context.Background(), path)
			if err != nil {
				t.Fatalf("upgrade from checkpoint %d: %v", checkpoint, err)
			}
			defer db.Close()
			if err := db.Ready(context.Background()); err != nil {
				t.Fatal(err)
			}

			rows, err := db.SQL.Query(`SELECT version FROM schema_migrations ORDER BY version`)
			if err != nil {
				t.Fatal(err)
			}
			var gotVersions []string
			for rows.Next() {
				var version string
				if err := rows.Scan(&version); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				gotVersions = append(gotVersions, version)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			if len(gotVersions) != len(wantVersions) {
				t.Fatalf("migration count = %d, want %d (%v)", len(gotVersions), len(wantVersions), gotVersions)
			}
			for i := range wantVersions {
				if gotVersions[i] != wantVersions[i] {
					t.Fatalf("migration %d = %q, want %q", i, gotVersions[i], wantVersions[i])
				}
			}

			if checkpoint == 0 {
				return
			}
			var providerName, model, virtualName, permission string
			if err := db.SQL.QueryRow(`SELECT p.name,m.upstream_model_id,v.name,cp.model_id FROM providers p JOIN provider_models m ON m.provider_id=p.id JOIN virtual_models v ON v.target_provider_id=p.id JOIN client_model_permissions cp ON cp.client_key_id='ck1' WHERE p.id='p1' AND m.id='m1' AND v.id='vm1'`).Scan(&providerName, &model, &virtualName, &permission); err != nil {
				t.Fatal(err)
			}
			if providerName != "provider-a" || model != "model-a" || virtualName != "coding" || permission != "vm1" {
				t.Fatalf("fixture data changed: provider=%q model=%q virtual=%q permission=%q", providerName, model, virtualName, permission)
			}
			if checkpoint >= 3 {
				var routeKind, routeModel, routeStatus sql.NullString
				query := `SELECT route_kind,route_model FROM request_logs WHERE id='log1'`
				if checkpoint >= 24 {
					query = `SELECT route_kind,route_model,route_status FROM request_logs WHERE id='log1'`
				}
				var err error
				if checkpoint >= 24 {
					err = db.SQL.QueryRow(query).Scan(&routeKind, &routeModel, &routeStatus)
				} else {
					err = db.SQL.QueryRow(query).Scan(&routeKind, &routeModel)
				}
				if err != nil {
					t.Fatal(err)
				}
				if checkpoint < 14 {
					if routeKind.String != "virtual" || routeModel.String != "virtual/coding" {
						t.Fatalf("legacy request attribution = %q/%q, want virtual/virtual/coding", routeKind.String, routeModel.String)
					}
				} else if routeKind.Valid || routeModel.Valid {
					t.Fatalf("post-backfill request should retain null attribution, got %q/%q", routeKind.String, routeModel.String)
				}
				if checkpoint >= 24 && routeStatus.String != "legacy" {
					t.Fatalf("legacy request route_status = %q, want legacy", routeStatus.String)
				}
				if checkpoint == 18 {
					var errorText string
					var errorMessage sql.NullString
					if err := db.SQL.QueryRow(`SELECT error_text,error_message FROM request_logs WHERE id='log1'`).Scan(&errorText, &errorMessage); err != nil {
						t.Fatal(err)
					}
					if errorText != "upstream_error" || errorMessage.Valid {
						t.Fatalf("request failure metadata changed: error_text=%q error_message=%q", errorText, errorMessage.String)
					}
					var failureClass string
					if err := db.SQL.QueryRow(`SELECT failure_class FROM request_attempts WHERE id='attempt1'`).Scan(&failureClass); err != nil {
						t.Fatal(err)
					}
					if failureClass != "http_502" {
						t.Fatalf("attempt failure class = %q, want http_502", failureClass)
					}
					var attemptMessage sql.NullString
					if err := db.SQL.QueryRow(`SELECT error_message FROM request_attempts WHERE id='attempt1'`).Scan(&attemptMessage); err != nil {
						t.Fatal(err)
					}
					if attemptMessage.Valid {
						t.Fatalf("attempt error_message survived migration: %q", attemptMessage.String)
					}
				}
			}
		})
	}
}

func TestMigration017UpgradesOpenCodeFreeProtocols(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	openMigrationFixture(t, path, 16)
	raw := openRawMigrationDB(t, path)
	now := Now()
	if _, err := raw.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('opencode-free','real','op-open')`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO providers(id,name,type,base_url,credential_secret,enabled,protocols,created_at,updated_at) VALUES('op-open','opencode-free','opencode-free','http://example.test/v1',NULL,1,'["chat"]',?,?)`, now, now); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	for i, modelID := range []string{"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free"} {
		if _, err := raw.Exec(`INSERT INTO provider_models(id,provider_id,upstream_model_id,display_name,native_protocol,available,first_seen_at,last_seen_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("op-model-%d", i), "op-open", modelID, modelID, "chat", 1, now, now, now, now); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var protocols string
	if err := db.SQL.QueryRow(`SELECT protocols FROM providers WHERE id='op-open'`).Scan(&protocols); err != nil {
		t.Fatal(err)
	}
	if protocols != `["chat","responses"]` {
		t.Fatalf("opencode-free protocols = %q, want [\"chat\",\"responses\"]", protocols)
	}
	rows, err := db.SQL.Query(`SELECT native_protocol FROM provider_models WHERE provider_id='op-open' ORDER BY upstream_model_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var protocol string
		if err := rows.Scan(&protocol); err != nil {
			t.Fatal(err)
		}
		if protocol != "responses" {
			t.Fatalf("Muse native protocol = %q, want responses", protocol)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFailureDoesNotAdvanceVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	// Make migration 002 fail deterministically by applying its schema change
	// without recording 002 in schema_migrations.
	openMigrationFixture(t, path, 1)
	raw := openRawMigrationDB(t, path)
	if _, err := raw.Exec(`ALTER TABLE provider_models ADD COLUMN context_length INTEGER`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if db, err := Open(context.Background(), path); err == nil {
		db.Close()
		t.Fatal("Open succeeded despite a failed migration")
	}

	raw = openRawMigrationDB(t, path)
	defer raw.Close()
	var applied int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version='002_context_length.sql'`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("failed migration was recorded as applied: %d", applied)
	}
	var total int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("migration count after failure = %d, want 1", total)
	}
}
