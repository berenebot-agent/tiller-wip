package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestActivityAttemptIndexCoversTargetHealth proves the request_attempts
// account/created_at index exists and is what the Activity target-health
// aggregate uses.
//
// The aggregate filters attempts on account_id and a created_at window. Without
// the index SQLite walks every attempt row the account has ever written and
// filters created_at in the WHERE clause, which makes it the single most
// expensive query in the usage snapshot. The index is created by the schema
// block, which runs on every open, so an existing Activity database picks it
// up on the next boot.
func TestActivityAttemptIndexCoversTargetHealth(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ActivityFileName)
	ctx := context.Background()

	adb, err := OpenActivity(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()

	var name string
	err = adb.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='request_attempts' AND name='request_attempts_account_created'`).Scan(&name)
	if err != nil {
		t.Fatalf("request_attempts_account_created index missing: %v", err)
	}

	rows, err := adb.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT provider,model FROM request_attempts WHERE account_id=? AND created_at>=?`, "acct", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := []string{}
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "request_attempts_account_created") {
		t.Fatalf("target-health attempt scan does not use the index: %s", joined)
	}
	if strings.Contains(joined, "SCAN request_attempts") {
		t.Fatalf("target-health attempt scan is a full table scan: %s", joined)
	}
}
