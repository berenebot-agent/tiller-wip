package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestRunClientCreatesCatalogueFixture(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	old := outputDest
	outputDest = &buf
	defer func() { outputDest = old }()

	args := []string{"--db", dbPath, "--name", "browser-catalogue-fixture", "--type", "catalogue"}
	if err := runClient(args); err != nil {
		t.Fatal(err)
	}

	var out fixtureOutput
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("parse output: %v (got %q)", err, buf.String())
	}
	if out.ID != "browser-browser-catalogue-fixture" {
		t.Errorf("id = %q", out.ID)
	}
	if out.Name != "browser-catalogue-fixture" {
		t.Errorf("name = %q", out.Name)
	}
	if out.Type != "catalogue" {
		t.Errorf("type = %q", out.Type)
	}
	if out.Secret != "sk-tr-"+pinnedSelector+"."+pinnedSecret {
		t.Errorf("secret = %q", out.Secret)
	}
	if out.Fingerprint != pinnedFingerprint {
		t.Errorf("fingerprint = %q", out.Fingerprint)
	}

	// Re-running with the same name must be idempotent (no unique-constraint error).
	buf.Reset()
	if err := runClient(args); err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	var out2 fixtureOutput
	if err := json.Unmarshal(buf.Bytes(), &out2); err != nil {
		t.Fatalf("parse re-run output: %v", err)
	}
	if out2.ID != out.ID {
		t.Errorf("id changed on re-run: %q vs %q", out2.ID, out.ID)
	}
}

func TestRunClientCreatesSingleFixture(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := database.Now()
	// Seed a provider + model so the single binding has a real target.
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('fixtureprov','real','fixtureprov')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,created_at,updated_at) VALUES('fp','fixtureprov','generic-openai','http://127.0.0.1:1/v1',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO provider_models(id,provider_id,upstream_model_id,first_seen_at,last_seen_at,created_at,updated_at) VALUES('fm','fp','mock-model',?,?,?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	old := outputDest
	outputDest = &buf
	defer func() { outputDest = old }()

	args := []string{"--db", dbPath, "--name", "browser-single-fixture", "--type", "single", "--target-kind", "real", "--target-id", "fm"}
	if err := runClient(args); err != nil {
		t.Fatal(err)
	}
	var out fixtureOutput
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("parse output: %v (got %q)", err, buf.String())
	}
	if out.Type != "single" {
		t.Errorf("type = %q", out.Type)
	}

	// Verify the binding row exists.
	reopen, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	var bindingCount int
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM client_single_bindings WHERE client_key_id=?`, out.ID).Scan(&bindingCount); err != nil {
		t.Fatal(err)
	}
	if bindingCount != 1 {
		t.Fatalf("single binding count = %d, want 1", bindingCount)
	}
}

func TestRunClientValidatesArgs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	if _, err := database.Open(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}

	// Missing --name.
	if err := runClient([]string{"--db", dbPath}); err == nil {
		t.Fatal("expected error for missing --name")
	}
	// Invalid --type.
	if err := runClient([]string{"--db", dbPath, "--name", "x", "--type", "bogus"}); err == nil {
		t.Fatal("expected error for invalid --type")
	}
	// Single without --target-id.
	if err := runClient([]string{"--db", dbPath, "--name", "x", "--type", "single"}); err == nil {
		t.Fatal("expected error for single without --target-id")
	}
}

func TestRunClientPinnedHashIsProductionArgon2id(t *testing.T) {
	// The pinned hash must be a genuine Argon2id PHC string with production
	// parameters, so the real router authenticates it through its normal
	// verifier. If this ever drifts, the fixture silently breaks.
	if !strings.HasPrefix(pinnedHash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("pinned hash does not use production argon2id params: %q", pinnedHash)
	}
}
