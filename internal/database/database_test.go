package database

import (
	"context"
	"path/filepath"
	"testing"
)

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
