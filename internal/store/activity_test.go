package store_test

import (
	"context"
	"errors"
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

// TestActivityReadsReturnUnavailableWhenHandleMissing proves Activity reads
// return the explicit sentinel (not an empty slice and not a nil error) when
// the Activity handle is absent, so callers can surface an unavailable state.
func TestActivityCleanupRecoversAfterUnavailable(t *testing.T) {
	db, _ := openActivityStore(t)
	ctx := context.Background()
	_, err := db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('cleanup-local',?,'key','model','chat',0,200,1,'cleanup-local',?)`, database.LocalAccountID, database.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES('cleanup-other',?,'key','model','chat',0,200,1,'cleanup-other',?)`, otherAccount, database.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Activity.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable := store.New(db.SQL)
	if err := unavailable.DeleteAccountActivity(database.LocalAccountID); err != nil {
		t.Fatal(err)
	}
	activity, err := database.OpenActivity(ctx, db.ActivityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer activity.Close()
	available := store.New(db.SQL, store.WithActivityDB(activity))
	if err := available.ReconcileActivityCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var local, other int
	if err := activity.QueryRow(`SELECT count(*) FROM request_logs WHERE account_id=?`, database.LocalAccountID).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if err := activity.QueryRow(`SELECT count(*) FROM request_logs WHERE account_id=?`, otherAccount).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if local != 0 || other != 1 {
		t.Fatalf("cleanup counts local=%d other=%d", local, other)
	}
}

func TestActivityReadsReturnUnavailableWhenHandleMissing(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New(db.SQL) // no WithActivityDB: core-only store
	sc := st.For(database.LocalAccountID)
	ctx := context.Background()

	if _, err := sc.ListGlobalActivity(ctx, "", 10, 0); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("ListGlobalActivity err = %v, want ErrActivityUnavailable", err)
	}
	if _, err := sc.ListRequestAttempts(ctx, "x"); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("ListRequestAttempts err = %v, want ErrActivityUnavailable", err)
	}
	if err := sc.ClearClientActivity(ctx, "x"); !errors.Is(err, store.ErrActivityUnavailable) {
		t.Fatalf("ClearClientActivity err = %v, want ErrActivityUnavailable", err)
	}
	if sc.ActivityAvailable() {
		t.Fatal("ActivityAvailable() = true without an Activity handle")
	}
}

func TestDeleteClientKeyRetryWithPendingCleanup(t *testing.T) {
	db, st := openActivityStore(t)
	ctx := context.Background()
	if _, err := db.SQL.Exec(`INSERT INTO client_keys(id,account_id,name,description,selector,secret_hash,secret_fingerprint,enabled,created_at,updated_at,logging_enabled,retention_days,key_type,key_group) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"retry-key", database.LocalAccountID, "Retry", "", "retry-selector", "hash", "finger", 1, database.Now(), database.Now(), 1, 30, "catalogue", "default"); err != nil {
		t.Fatal(err)
	}

	scope := st.For(database.LocalAccountID)
	found, err := scope.DeleteClientKey(ctx, "retry-key")
	if err != nil || !found {
		t.Fatalf("first delete found=%v err=%v", found, err)
	}
	found, err = scope.DeleteClientKey(ctx, "retry-key")
	if err != nil || !found {
		t.Fatalf("retry delete found=%v err=%v", found, err)
	}
	var pending int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM activity_cleanup WHERE account_id=? AND client_key_id=?`, database.LocalAccountID, "retry-key").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending cleanup count=%d, want 1", pending)
	}
}

// TestUsageCostAndTokenTypeAggregates proves the new cost and token-type
// aggregates: per-type sums are exact, and the provider-reported cost is
// preferred over the estimate per row.
func TestUsageCostAndTokenTypeAggregates(t *testing.T) {
	_, st := openActivityStore(t)
	ctx := context.Background()
	sc := st.For(database.LocalAccountID)
	now := database.Now()
	in, out := int64(100), int64(50)
	est, prov := int64(2_000_000), int64(5_000_000)
	cr := int64(10)
	insert := func(id string, input, output *int64, estCost, provCost *int64, estimated bool, providerType string) {
		t.Helper()
		var pt *string
		if providerType != "" {
			pt = &providerType
		}
		pm := "model-a"
		if err := sc.InsertRequestLog(ctx, store.RequestLogInsert{
			ID: id, ClientKeyID: "ck", ClientName: "ck", RequestedModel: "model-a",
			Protocol: "chat", HTTPStatus: 200, LatencyMs: 1, ClientRequestID: id, CreatedAt: now,
			ResolvedProvider: pt, ResolvedModel: &pm,
			InputTokens: input, OutputTokens: output, CacheReadInputTokens: &cr,
			EstimatedCostMicros: estCost, ProviderCostMicros: provCost, InputTokensEstimated: estimated,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("r1", &in, &out, &est, nil, false, "openai")
	insert("r2", &in, &out, &est, &prov, true, "openai")

	c1 := now
	types, err := sc.TokenTypesByClient(ctx, c1, c1, c1)
	if err != nil {
		t.Fatal(err)
	}
	got := types["ck"]
	if got.Input.D7 != 200 || got.Output.D7 != 100 || got.CacheRead.D7 != 20 {
		t.Fatalf("token types = %+v", got)
	}

	costs, err := sc.CostByClient(ctx, c1, c1, c1)
	if err != nil {
		t.Fatal(err)
	}
	// r1 uses the estimate (2e6), r2 prefers the provider cost (5e6).
	if costs["ck"].D7 == nil || *costs["ck"].D7 != 7_000_000 {
		t.Fatalf("cost total = %v, want 7000000", costs["ck"].D7)
	}

	estimated, err := sc.EstimatedClients(ctx, c1)
	if err != nil {
		t.Fatal(err)
	}
	if !estimated["ck"] {
		t.Fatal("client with an estimated row should be flagged")
	}
}

func ids(rows []store.ActivityRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
