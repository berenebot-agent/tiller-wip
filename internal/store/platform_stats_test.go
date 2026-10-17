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
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, account := range []struct{ id, status string }{
		{"other-account", "active"},
		{"deleting-account", "deleting"},
		{"ownerless-account", "active"},
	} {
		if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free',?,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`, account.id, account.status); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id, accountID, created string
		input, output          int
	}{
		{"current-a", database.LocalAccountID, now.Add(-time.Hour).Format(time.RFC3339Nano), 10, 20},
		{"current-b", database.LocalAccountID, now.Add(-48 * time.Hour).Format(time.RFC3339Nano), 5, 7},
		{"other", "other-account", now.Add(-time.Minute).Format(time.RFC3339Nano), 100, 200},
		// Residual activity from an account mid-deletion and from a local-mode
		// account must not reach the platform totals.
		{"mid-delete", "deleting-account", now.Add(-time.Minute).Format(time.RFC3339Nano), 1000, 2000},
		{"ownerless", "ownerless-account", now.Add(-time.Minute).Format(time.RFC3339Nano), 1000, 2000},
	} {
		if _, err := db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,'model','chat',0,200,1,?,?,?,?)`, row.id, row.accountID, "key-"+row.id, row.input, row.output, "req-"+row.id, row.created); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL.Exec(`INSERT INTO users(id,email,password_hash,status,created_at,updated_at) VALUES('hosted-owner','owner@platform-stats.test','test-hash','active','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z'),('deleting-owner','deleting@platform-stats.test','test-hash','active','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// owner_user_id is unique per account, so each hosted account gets its own
	// owner; the ownerless account is left without one on purpose.
	if _, err := db.SQL.Exec(`UPDATE accounts SET owner_user_id='hosted-owner' WHERE id='other-account'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`UPDATE accounts SET owner_user_id='deleting-owner' WHERE id='deleting-account'`); err != nil {
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
	usages, err := st.PlatformUsageForAccounts(context.Background(), []string{database.LocalAccountID, "other-account"}, now)
	if err != nil {
		t.Fatal(err)
	}
	local := usages[database.LocalAccountID]
	if local.Requests1h != 1 || local.Tokens1h != 30 || local.Requests24h != 1 || local.Tokens24h != 30 || local.Requests7d != 2 || local.Tokens7d != 42 {
		t.Fatalf("local account usage = %+v", local)
	}
	other := usages["other-account"]
	if other.Requests1h != 1 || other.Tokens1h != 300 || other.Requests24h != 1 || other.Tokens24h != 300 || other.Requests7d != 1 || other.Tokens7d != 300 {
		t.Fatalf("other account usage = %+v", other)
	}
	totals, err := st.PlatformUsageTotals(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	// Only the eligible hosted account contributes; the local account and the
	// mid-deletion account are excluded even though both have rows in the window.
	if totals.Requests7d != 1 || totals.Tokens7d != 300 || totals.Requests24h != 1 || totals.Tokens24h != 300 {
		t.Fatalf("platform totals = %+v", totals)
	}
}

func TestPlatformUsageReportsUnavailableActivityStore(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New(db.SQL)
	if _, err := st.PlatformUsageTotals(context.Background(), time.Now()); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("PlatformUsageTotals error = %v, want ErrActivityUnavailable", err)
	}
	if _, err := st.PlatformUsageForAccounts(context.Background(), []string{database.LocalAccountID}, time.Now()); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("PlatformUsageForAccounts error = %v, want ErrActivityUnavailable", err)
	}
}

func TestPlatformUsagePropagatesActivityQueryError(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Activity.Exec(`DROP TABLE request_logs`); err != nil {
		t.Fatal(err)
	}
	st := store.New(db.SQL, store.WithActivityDB(db.Activity))
	if _, err := st.PlatformUsageTotals(context.Background(), time.Now()); err == nil {
		t.Fatal("PlatformUsageTotals succeeded after request_logs was dropped")
	}
	if _, err := st.PlatformUsageForAccounts(context.Background(), []string{database.LocalAccountID}, time.Now()); err == nil {
		t.Fatal("PlatformUsageForAccounts succeeded after request_logs was dropped")
	}
}

// TestPlatformUsageTotalsPropagatesMissingAccountsTable guards the eligibility
// lookup added to exclude non-hosted and mid-deletion accounts: losing the
// accounts table must fail loud rather than silently widen the totals back to
// every account.
func TestPlatformUsageTotalsPropagatesMissingAccountsTable(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Rename rather than drop: other tables reference accounts, so a drop trips
	// a foreign-key constraint while the query is what is under test.
	if _, err := db.SQL.Exec(`ALTER TABLE accounts RENAME TO accounts_hidden`); err != nil {
		t.Fatal(err)
	}
	st := store.New(db.SQL, store.WithActivityDB(db.Activity))
	if _, err := st.PlatformUsageTotals(context.Background(), time.Now()); err == nil {
		t.Fatal("PlatformUsageTotals succeeded without the accounts table")
	}
}
