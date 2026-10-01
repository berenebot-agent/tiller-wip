package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVacuumCoreReclaimsFreePages(t *testing.T) {
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
	if err := db.VacuumCore(ctx); err != nil {
		t.Fatal(err)
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

	if err := db.VacuumCore(ctx); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("wal after vacuum: %v", err)
	} else if fi.Size() != 0 {
		t.Fatalf("wal after vacuum = %d bytes, want 0 (truncated)", fi.Size())
	}
}

func TestVacuumActivityAndNilHandle(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if db.Activity == nil {
		t.Fatal("activity handle not opened")
	}
	if err := db.VacuumActivity(ctx); err != nil {
		t.Fatal(err)
	}
	// A DB with no Activity handle is a no-op, not an error.
	if err := (&DB{SQL: db.SQL}).VacuumActivity(ctx); err != nil {
		t.Fatalf("nil activity vacuum = %v, want nil", err)
	}
}
