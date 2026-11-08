package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVacuumCoreReclaimsFreePages(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.SQL.Exec(`CREATE TABLE scratch(id INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("x", 512)
	for i := 0; i < 1000; i++ {
		if _, err := db.SQL.Exec(`INSERT INTO scratch(blob) VALUES(?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	var free int
	if err := db.SQL.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Fatal("expected free pages before vacuum")
	}
	result, err := db.VacuumCore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Converted {
		t.Fatal("a database created by this package is already incremental; no conversion should be reported")
	}
	if result.FreedPages == 0 {
		t.Fatal("pass freed nothing from a non-empty freelist")
	}
	if err := db.SQL.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free != 0 {
		t.Fatalf("freelist after vacuum = %d, want 0", free)
	}
	var tables int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='scratch'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Fatal("vacuum dropped schema")
	}
}

func TestVacuumCoreTruncatesWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.SQL.Exec(`CREATE TABLE scratch(id INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("x", 4096)
	for i := 0; i < 2000; i++ {
		if _, err := db.SQL.Exec(`INSERT INTO scratch(blob) VALUES(?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("wal before vacuum: %v", err)
	} else if fi.Size() == 0 {
		t.Fatal("expected a non-empty WAL before vacuum")
	}

	if _, err := db.VacuumCore(ctx); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("wal after vacuum: %v", err)
	} else if fi.Size() != 0 {
		t.Fatalf("wal after vacuum = %d bytes, want 0 (truncated)", fi.Size())
	}
}

func TestVacuumActivityAndNilHandle(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if db.Activity == nil {
		t.Fatal("activity handle not opened")
	}
	if _, err := db.VacuumActivity(ctx); err != nil {
		t.Fatal(err)
	}
	// A DB with no Activity handle is a no-op, not an error.
	if _, err := (&DB{SQL: db.SQL}).VacuumActivity(ctx); err != nil {
		t.Fatalf("nil activity vacuum = %v, want nil", err)
	}
}

// TestVacuumRespectsPageBudget is the property the maintenance pass depends on:
// once a database is in incremental mode, one pass frees no more than its page
// budget, so the write lock is bounded by the budget instead of the file size.
func TestVacuumRespectsPageBudget(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// Convert first, so the budget path is what runs.
	if _, err := db.VacuumCore(ctx); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("y", 512)
	for _, table := range []string{"filler"} {
		if _, err := db.SQL.Exec(`CREATE TABLE ` + table + `(id INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2000; i++ {
			if _, err := db.SQL.Exec(`INSERT INTO `+table+`(blob) VALUES(?)`, blob); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.SQL.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.SQL.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := freelistCount(ctx, conn)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("expected a freelist to reclaim")
	}

	const budget = 32
	result, err := vacuum(ctx, db.SQL, budget)
	if err != nil {
		t.Fatal(err)
	}
	if result.Converted {
		t.Fatal("budget pass re-converted an already-incremental database")
	}
	if result.FreedPages == 0 {
		t.Fatal("budget pass freed nothing from a non-empty freelist")
	}
	if result.FreedPages > budget {
		t.Fatalf("freed %d pages, exceeding the %d-page budget", result.FreedPages, budget)
	}
	conn, err = db.SQL.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := freelistCount(ctx, conn)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if after != before-result.FreedPages {
		t.Fatalf("freelist went %d -> %d but the pass reported %d pages freed", before, after, result.FreedPages)
	}
	// Repeated passes keep draining the same freelist.
	second, err := vacuum(ctx, db.SQL, budget)
	if err != nil {
		t.Fatal(err)
	}
	if second.FreedPages == 0 || second.FreedPages > budget {
		t.Fatalf("second budget pass freed %d pages, want 1..%d", second.FreedPages, budget)
	}
}

func TestVacuumWithoutFreePagesIsANoOp(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A database created by this package is born in incremental mode, so the
	// pass has no conversion to perform.
	result, err := db.VacuumCore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Converted {
		t.Fatal("a fresh database was already incremental; no conversion should be reported")
	}
	var free int64
	if err := db.SQL.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free != 0 {
		t.Fatalf("pass left %d free pages behind", free)
	}
}

// TestVacuumConvertsPreIncrementalDatabase covers the one-off migration the
// maintenance pass performs on a database created before incremental
// auto-vacuum: the file is rewritten once, switched to incremental mode, and
// every later pass takes the bounded path instead.
func TestVacuumConvertsPreIncrementalDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "router.db")
	// Build a legacy database: no auto_vacuum pragma at connect time, so the
	// file header stays at auto_vacuum=NONE exactly like the live instance.
	legacy, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE scratch(id INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
		t.Fatal(err)
	}
	blob := strings.Repeat("x", 512)
	for i := 0; i < 1000; i++ {
		if _, err := legacy.Exec(`INSERT INTO scratch(blob) VALUES(?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var mode int
	if err := db.SQL.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode == autoVacuumIncremental {
		t.Fatalf("legacy fixture is already incremental (%d); the conversion path would go untested", mode)
	}
	result, err := db.VacuumCore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Converted {
		t.Fatal("first pass over a pre-incremental database must report the conversion")
	}
	if err := db.SQL.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != autoVacuumIncremental {
		t.Fatalf("auto_vacuum after conversion = %d, want %d", mode, autoVacuumIncremental)
	}
	var free int64
	if err := db.SQL.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free != 0 {
		t.Fatalf("freelist after conversion = %d, want 0", free)
	}
	again, err := db.VacuumCore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Converted {
		t.Fatal("second pass reported a conversion that already happened")
	}
}
