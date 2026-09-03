package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestRunActivitySeedsDeterministicRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO client_keys(id,name,selector,secret_hash,secret_fingerprint,created_at,updated_at) VALUES('ck-fixture','fixture','sel','h','f',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	args := []string{"--db", dbPath, "--client", "ck-fixture", "--rows", "20", "--fail-every", "10", "--fallback-every", "4"}
	if err := runActivity(args); err != nil {
		t.Fatal(err)
	}

	reopen, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()

	var logs int
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM request_logs WHERE client_key_id='ck-fixture'`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 20 {
		t.Fatalf("request_logs count = %d, want 20", logs)
	}
	// Every log has >=1 attempt; fallback logs have exactly 2.
	var attempts int
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM request_attempts WHERE request_log_id LIKE 'activity-%'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	// 20 logs, 5 fallbacks (indices 3,7,11,15,19 -> 5) with 2 attempts, rest 1.
	if want := 20 + 5; attempts != want {
		t.Fatalf("request_attempts count = %d, want %d", attempts, want)
	}

	// Deterministic ids and all-or-nothing per logical request.
	var ids int
	if err := reopen.SQL.QueryRow(`SELECT count(DISTINCT id) FROM request_logs WHERE client_key_id='ck-fixture'`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != 20 {
		t.Fatalf("distinct request_logs ids = %d, want 20", ids)
	}
	var orphaned int
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM request_attempts a LEFT JOIN request_logs l ON l.id=a.request_log_id WHERE l.id IS NULL`).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if orphaned != 0 {
		t.Fatalf("orphaned attempt rows = %d", orphaned)
	}

	// A fallback row: http 200, fallback_used=1, resolved target + 2 attempts.
	var fb struct {
		status     int
		fallback   int
		resolvedM  *string
		attemptCnt int
	}
	if err := reopen.SQL.QueryRow(`SELECT http_status, fallback_used, resolved_model, attempt_count FROM request_logs WHERE id='activity-ck-fixtu-004'`).Scan(&fb.status, &fb.fallback, &fb.resolvedM, &fb.attemptCnt); err != nil {
		t.Fatal(err)
	}
	if fb.status != 200 || fb.fallback != 1 || fb.resolvedM == nil || *fb.resolvedM != "mock-model-b" || fb.attemptCnt != 2 {
		t.Fatalf("fallback row wrong: %+v", fb)
	}
	var fbAttempts int
	if err := reopen.SQL.QueryRow(`SELECT count(*) FROM request_attempts WHERE request_log_id='activity-ck-fixtu-004'`).Scan(&fbAttempts); err != nil {
		t.Fatal(err)
	}
	if fbAttempts != 2 {
		t.Fatalf("fallback attempts = %d, want 2", fbAttempts)
	}

	// A failure row (every 10th -> seq 010): 500, error_text set, no resolved target.
	var fl status
	if err := reopen.SQL.QueryRow(`SELECT http_status, error_text, resolved_provider, fallback_used FROM request_logs WHERE id='activity-ck-fixtu-010'`).Scan(&fl.status, &fl.errorText, &fl.resolvedProvider, &fl.fallbackUsed); err != nil {
		t.Fatal(err)
	}
	if fl.status != 500 || fl.errorText == nil || *fl.errorText != "upstream_error" || fl.resolvedProvider != nil || fl.fallbackUsed != 0 {
		t.Fatalf("failure row wrong: %+v", fl)
	}
}

type status struct {
	status           int
	errorText        *string
	resolvedProvider *string
	fallbackUsed     int
}

func TestRunActivityValidatesClient(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "router.db")
	if _, err := database.Open(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}
	err := runActivity([]string{"--db", dbPath, "--client", "missing", "--rows", "1"})
	if err == nil {
		t.Fatal("expected an error for a nonexistent client")
	}
}
