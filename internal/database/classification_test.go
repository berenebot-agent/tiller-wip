package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

// TestTenantTableClassification is the schema guard for the tenancy boundary.
//
// It fails when:
//   - the live schema contains a table that is not in TableClassification
//     (a new table must be deliberately classified); or
//   - a declared table is missing from the live schema (stale inventory); or
//   - a tenant-owned table does not have an account_id column.
func TestTenantTableClassification(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.SQL.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Every live table must be classified.
	for _, name := range actual {
		if _, ok := TableClassification[name]; !ok {
			t.Errorf("table %q is not classified in TableClassification; add it as ClassTenant (with account_id) or ClassPlatform", name)
		}
	}

	// Every declared table must exist (catches typos and stale entries).
	actualSet := make(map[string]bool, len(actual))
	for _, name := range actual {
		actualSet[name] = true
	}
	var declared []string
	for name := range TableClassification {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	for _, name := range declared {
		if !actualSet[name] {
			t.Errorf("TableClassification declares %q but it is not in the schema", name)
		}
	}

	// Every tenant-owned table must have an account_id column.
	for _, name := range MainTenantTables() {
		if !actualSet[name] {
			continue // already reported above
		}
		if !tableHasColumn(t, db, name, "account_id") {
			t.Errorf("tenant table %q is missing an account_id column", name)
		}
	}

	// Activity tables must live in the per-account Activity database, not the
	// central one: the central backup deliberately excludes Activity.
	for _, name := range ActivityTenantTables() {
		if actualSet[name] {
			t.Errorf("activity table %q must not exist in the central database", name)
		}
	}
	verifyActivitySchema(t, db)
}

// verifyActivitySchema opens a sample per-account Activity database and checks
// its table classification and account_id coverage, mirroring the central
// database guard for the split Activity schema.
func verifyActivitySchema(t *testing.T, db *DB) {
	t.Helper()
	path := filepath.Join(db.ActivityDir, LocalAccountID+".db")
	adb, err := OpenActivity(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()

	rows, err := adb.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range actual {
		if _, ok := ActivityTableClassification[name]; !ok {
			t.Errorf("activity table %q is not classified in ActivityTableClassification", name)
		}
	}
	for name := range ActivityTableClassification {
		if !actual[name] {
			t.Errorf("ActivityTableClassification declares %q but it is not in the activity schema", name)
		}
	}
	for _, name := range ActivityTenantTables() {
		if !actual[name] {
			continue
		}
		if !activityTableHasColumn(t, adb, name, "account_id") {
			t.Errorf("activity tenant table %q is missing an account_id column", name)
		}
	}
}

func activityTableHasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func tableHasColumn(t *testing.T, db *DB, table, column string) bool {
	t.Helper()
	rows, err := db.SQL.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}
