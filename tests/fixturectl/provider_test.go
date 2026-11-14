package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestRunProviderSeedsProviderAndModels(t *testing.T) {
	t.Parallel()
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
	args := []string{"--db", dbPath, "--name", "browser-prov", "--models", "mock-model,mock-alt"}
	if err := runProvider(args, &buf); err != nil {
		t.Fatal(err)
	}

	var out providerOutput
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("parse output: %v (got %q)", err, buf.String())
	}
	if out.ID != "browser-browser-prov" {
		t.Errorf("id = %q", out.ID)
	}
	if len(out.ModelPKs) != 2 {
		t.Fatalf("model pks = %v, want 2", out.ModelPKs)
	}

	reopen, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()

	var enabled int
	var lastRefresh *string
	if err := reopen.SQL.QueryRow(
		`SELECT enabled, last_refresh_at FROM providers WHERE id=?`, out.ID,
	).Scan(&enabled, &lastRefresh); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Errorf("enabled = %d, want 1", enabled)
	}
	if lastRefresh == nil {
		t.Error("last_refresh_at is NULL; the console would treat this as awaiting a first refresh")
	}

	var models int
	if err := reopen.SQL.QueryRow(
		`SELECT count(*) FROM provider_models WHERE provider_id=? AND available=1 AND origin='discovered'`, out.ID,
	).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if models != 2 {
		t.Fatalf("available models = %d, want 2", models)
	}

	var ns int
	if err := reopen.SQL.QueryRow(
		`SELECT count(*) FROM namespaces WHERE name='browser-prov' AND kind='real' AND entity_id=?`, out.ID,
	).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if ns != 1 {
		t.Fatalf("namespace rows = %d, want 1", ns)
	}
}

func TestRunProviderIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	args := []string{"--db", dbPath, "--name", "browser-idem"}
	for i := 0; i < 3; i++ {
		var buf bytes.Buffer
		if err := runProvider(args, &buf); err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}

	reopen, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()

	var providers, models, namespaces int
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM providers WHERE name='browser-idem'`).Scan(&providers); err != nil {
		t.Fatal(err)
	}
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM provider_models WHERE provider_id='browser-browser-idem'`).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM namespaces WHERE name='browser-idem'`).Scan(&namespaces); err != nil {
		t.Fatal(err)
	}
	if providers != 1 || models != 1 || namespaces != 1 {
		t.Fatalf("after 3 runs: providers=%d models=%d namespaces=%d, want 1/1/1", providers, models, namespaces)
	}
}

func TestRunProviderValidatesArgs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	if _, err := database.Open(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}

	if err := runProvider([]string{"--db", dbPath}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for missing --name")
	}
	if err := runProvider([]string{"--db", dbPath, "--name", "x", "--models", " , "}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for empty --models")
	}
	if err := runProvider([]string{"--db", dbPath, "--name", "x", "--origin", "bogus"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for invalid --origin")
	}
}
