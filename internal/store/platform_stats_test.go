package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

func TestPlatformUsageAggregatesOnlyAccountScope(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES('other-account','free','active','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id, accountID, created string
		input, output          int
	}{
		{"current-a", database.LocalAccountID, now.Add(-time.Hour).Format(time.RFC3339Nano), 10, 20},
		{"current-b", database.LocalAccountID, now.Add(-48 * time.Hour).Format(time.RFC3339Nano), 5, 7},
		{"other", "other-account", now.Add(-time.Minute).Format(time.RFC3339Nano), 100, 200},
	} {
		if _, err := db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,'model','chat',0,200,1,?,?,?,?)`, row.id, row.accountID, "key-"+row.id, row.input, row.output, "req-"+row.id, row.created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL.Exec(`INSERT INTO users(id,email,password_hash,status,created_at,updated_at) VALUES('hosted-owner','owner@platform-stats.test','test-hash','active','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE accounts SET owner_user_id='hosted-owner' WHERE id='other-account'`); err != nil {
		t.Fatal(err)
	}
	identityStore, err := identity.New(db.SQL, nil, nil, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	accountIDs, err := identityStore.PlatformAccountIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accountIDs) != 1 || accountIDs[0] != "other-account" {
		t.Fatalf("PlatformAccountIDs = %v, want only hosted account", accountIDs)
	}
	st := store.New(db.SQL, store.WithActivityDB(db.Activity))
	usage, err := st.For(database.LocalAccountID).PlatformUsage(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Requests24h != 1 || usage.Tokens24h != 30 || usage.Requests7d != 2 || usage.Tokens7d != 42 {
		t.Fatalf("local account usage = %+v", usage)
	}
	other, err := st.For("other-account").PlatformUsage(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if other.Requests24h != 1 || other.Tokens24h != 300 || other.Requests7d != 1 || other.Tokens7d != 300 {
		t.Fatalf("other account usage = %+v", other)
	}
}

func TestPlatformUsageReportsUnavailableActivityStore(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.New(db.SQL).For(database.LocalAccountID).PlatformUsage(context.Background(), time.Now()); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("PlatformUsage error = %v, want ErrActivityUnavailable", err)
	}
}

func TestPlatformUsagePropagatesActivityQueryError(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Activity.Exec(`DROP TABLE request_logs`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(db.SQL, store.WithActivityDB(db.Activity)).For(database.LocalAccountID).PlatformUsage(context.Background(), time.Now()); err == nil {
		t.Fatal("PlatformUsage succeeded after request_logs was dropped")
	}
}
