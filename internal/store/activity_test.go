package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

// openActivityStore opens a database, seeds a second account, and returns a
// Store wired to the shared Activity database.
func openActivityStore(t *testing.T) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if db.Activity == nil {
		t.Fatal("database has no Activity handle")
	}
	if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active','2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, otherAccount); err != nil {
		t.Fatal(err)
	}
	return db, store.New(db.SQL, store.WithActivityDB(db.Activity))
}

// TestActivityIsAccountScoped proves every Activity read path binds the
// scope's account_id: a row written under one account is never visible to
// another, across list, attempts, export, and clear.
func TestActivityIsAccountScoped(t *testing.T) {
	_, st := openActivityStore(t)
	ctx := context.Background()
	local := st.For(database.LocalAccountID)
	other := st.For(otherAccount)

	insert := func(sc *store.Scope, id, clientID string) {
		t.Helper()
		err := sc.InsertRequestLog(ctx, store.RequestLogInsert{
			ID:              id,
			ClientKeyID:     clientID,
			ClientName:      clientID,
			RequestedModel:  "provider-a/model-a",
			Protocol:        "chat",
			HTTPStatus:      200,
			LatencyMs:       1,
			ClientRequestID: id,
			CreatedAt:       database.Now(),
			Attempts: []store.RequestAttemptInsert{{
				Provider:  "provider-a",
				Model:     "model-a",
				Result:    "success",
				LatencyMs: 1,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(local, "row-local", "ck-local")
	insert(other, "row-other", "ck-other")

	globalLocal, err := local.ListGlobalActivity(ctx, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(globalLocal) != 1 || globalLocal[0].ID != "row-local" {
		t.Fatalf("local account sees %d rows (%v), want only row-local", len(globalLocal), ids(globalLocal))
	}
	globalOther, err := other.ListGlobalActivity(ctx, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(globalOther) != 1 || globalOther[0].ID != "row-other" {
		t.Fatalf("other account sees %d rows (%v), want only row-other", len(globalOther), ids(globalOther))
	}

	// Attempts are account-scoped too: the other account cannot read the local
	// account's attempt rows even with the known request id.
	attempts, err := other.ListRequestAttempts(ctx, "row-local")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("other account read %d attempts for the local account's request", len(attempts))
	}

	// Clearing one account's client activity leaves the other account intact.
	if err := local.ClearClientActivity(ctx, "ck-local"); err != nil {
		t.Fatal(err)
	}
	globalLocal, err = local.ListGlobalActivity(ctx, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(globalLocal) != 0 {
		t.Fatalf("local account still sees %d rows after clear", len(globalLocal))
	}
	globalOther, err = other.ListGlobalActivity(ctx, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(globalOther) != 1 {
		t.Fatalf("other account rows changed after the local account clear: %d", len(globalOther))
	}
}

func ids(rows []store.ActivityRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
