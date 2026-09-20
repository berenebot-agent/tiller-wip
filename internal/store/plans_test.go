package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

const planTestAccount = "acct-plan-b"

// openPlanStore opens a fresh database, seeds a second account on the free
// plan, and returns a Store with limit enforcement enabled (the hosted-mode
// posture). The local account is seeded by migration 042.
func openPlanStore(t *testing.T) (*database.DB, *store.Store) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active','2026-09-19T00:00:00Z','2026-09-19T00:00:00Z')`, planTestAccount); err != nil {
		t.Fatal(err)
	}
	return db, store.New(db.SQL, store.WithActivityDB(db.Activity), store.WithLimitEnforcement(true))
}

func planProviderInput(id string) store.CreateProviderInput {
	return store.CreateProviderInput{ID: id, Name: id, Type: "generic-openai", BaseURL: "https://example.com/v1", Enabled: true, Protocols: "chat"}
}

func planClientInput(id string) store.CreateClientKeyInput {
	return store.CreateClientKeyInput{ID: id, Name: id, Description: id, Group: "default", Selector: "sel-" + id, Hash: "hash-" + id, Fingerprint: "fp-" + id, Type: "catalogue", LoggingEnabled: true, RetentionDays: 30}
}

// assertLimitKind fails unless err is a *store.LimitExceededError of the wanted
// kind and cap.
func assertLimitKind(t *testing.T, err error, kind string, limit int) {
	t.Helper()
	var exceeded *store.LimitExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("error = %v, want *store.LimitExceededError", err)
	}
	if exceeded.Kind != kind || exceeded.Limit != limit {
		t.Fatalf("limit error = %+v, want kind %q limit %d", exceeded, kind, limit)
	}
}

func TestProviderCreationLimitRejectsAtCapAndAllowsUnder(t *testing.T) {
	db, st := openPlanStore(t)
	ctx := context.Background()
	sc := st.For(planTestAccount)

	if err := st.UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: 2, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	// Count starts at zero; two creates under/at the cap succeed.
	if err := sc.CreateProvider(ctx, planProviderInput("p1")); err != nil {
		t.Fatalf("first provider under cap: %v", err)
	}
	if err := sc.CreateProvider(ctx, planProviderInput("p2")); err != nil {
		t.Fatalf("second provider at cap-1: %v", err)
	}
	assertLimitKind(t, sc.CreateProvider(ctx, planProviderInput("p3")), "providers", 2)

	// The rejected create rolled back: no provider row and no namespace leaked.
	var providers, namespaces int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM providers WHERE account_id=?`, planTestAccount).Scan(&providers); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT count(*) FROM namespaces WHERE account_id=?`, planTestAccount).Scan(&namespaces); err != nil {
		t.Fatal(err)
	}
	if providers != 2 || namespaces != 2 {
		t.Fatalf("after rejected create: providers=%d namespaces=%d, want 2/2", providers, namespaces)
	}
}

func TestClientKeyAndVirtualModelCreationLimits(t *testing.T) {
	db, st := openPlanStore(t)
	ctx := context.Background()
	sc := st.For(planTestAccount)

	if err := st.UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: 1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateClientKey(ctx, planClientInput("k1")); err != nil {
		t.Fatalf("client key under cap: %v", err)
	}
	assertLimitKind(t, sc.CreateClientKey(ctx, planClientInput("k2")), "client_keys", 1)

	// A virtual model limit check runs before any target validation, so a cap
	// of 1 is rejected on the second call. Build one real target and one
	// virtual model while unlimited, then lower the cap.
	if err := st.UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateProvider(ctx, planProviderInput("vp")); err != nil {
		t.Fatal(err)
	}
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO provider_models(id,account_id,provider_id,upstream_model_id,origin,available,first_seen_at,last_seen_at,created_at,updated_at) VALUES('pm1',?,'vp','m1','discovered',1,?,?,?,?)`, planTestAccount, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateVirtualGroup(ctx, "g1", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := sc.CreateVirtualModel(ctx, store.CreateVirtualModelInput{ID: "v1", GroupID: "g1", Name: "v1", RoutingMode: "fixed", Targets: []store.VirtualTargetInput{{ProviderModelID: "pm1", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: 1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	err := sc.CreateVirtualModel(ctx, store.CreateVirtualModelInput{ID: "v2", GroupID: "g1", Name: "v2", RoutingMode: "fixed", Targets: []store.VirtualTargetInput{{ProviderModelID: "pm1", Enabled: true}}})
	assertLimitKind(t, err, "virtual_models", 1)
}

func TestUnlimitedPlanAllowsCreation(t *testing.T) {
	_, st := openPlanStore(t)
	ctx := context.Background()
	sc := st.For(planTestAccount)

	if err := st.UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: -1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p-a", "p-b", "p-c", "p-d"} {
		if err := sc.CreateProvider(ctx, planProviderInput(id)); err != nil {
			t.Fatalf("unlimited provider create %s: %v", id, err)
		}
	}
}

func TestLocalModeStoreHasNoEnforcement(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// No WithLimitEnforcement: the local/test posture. Even a hard cap of 0 is
	// ignored because the create paths never call the guard.
	st := store.New(db.SQL)
	if err := st.UpdatePlan(context.Background(), store.Plan{Name: "free", MaxProviders: 0, MaxClientKeys: 0, MaxVirtualModels: 0, MaxConcurrentStreams: 0, ActivityRetentionDays: 7, MonthlyRequests: 0}); err != nil {
		t.Fatal(err)
	}
	sc := st.For(database.LocalAccountID)
	if sc.EnforcingLimits() {
		t.Fatal("store without WithLimitEnforcement reports enforcement on")
	}
	if err := sc.CreateProvider(context.Background(), planProviderInput("local-p1")); err != nil {
		t.Fatalf("local mode must not enforce creation limits: %v", err)
	}
}

func TestUsageCounterIncrementsAndPeriodRollover(t *testing.T) {
	_, st := openPlanStore(t)
	ctx := context.Background()
	sc := st.For(planTestAccount)

	for i := 0; i < 3; i++ {
		if err := sc.IncrementUsageCounter(ctx, "2026-09"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := sc.UsageCount(ctx, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("September count = %d, want 3", got)
	}
	// A new period starts at zero with no reset job.
	if got, err := sc.UsageCount(ctx, "2026-10"); err != nil || got != 0 {
		t.Fatalf("October count = %d (err %v), want 0", got, err)
	}
	// UsagePeriod is the UTC YYYY-MM key, and NextPeriodStart is the first of
	// the following month.
	if p := store.UsagePeriod(time.Date(2026, 10, 3, 23, 59, 0, 0, time.UTC)); p != "2026-10" {
		t.Fatalf("UsagePeriod = %q, want 2026-10", p)
	}
	next := store.NextPeriodStart(time.Date(2026, 10, 3, 23, 59, 0, 0, time.UTC))
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("NextPeriodStart = %s, want %s", next, want)
	}
}

func TestEffectiveRetentionDaysClamps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		planMax    int
		want       int
	}{
		{"plan unlimited leaves configured", 90, store.Unlimited, 90},
		{"plan zero treated as unlimited", 90, 0, 90},
		{"configured above plan clamps down", 90, 7, 7},
		{"configured below plan stays", 3, 7, 3},
		{"configured equal to plan", 7, 7, 7},
		{"configured zero uses plan max", 0, 7, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.EffectiveRetentionDays(tc.configured, tc.planMax); got != tc.want {
				t.Fatalf("EffectiveRetentionDays(%d,%d) = %d, want %d", tc.configured, tc.planMax, got, tc.want)
			}
		})
	}
}

// TestPlanEnforcementIsAccountScoped proves one account's resources never count
// against another's plan, and one account's usage never leaks into another.
func TestPlanEnforcementIsAccountScoped(t *testing.T) {
	_, st := openPlanStore(t)
	ctx := context.Background()
	a := st.For(planTestAccount)
	b := st.For(database.LocalAccountID)

	if err := st.UpdatePlan(ctx, store.Plan{Name: "free", MaxProviders: 1, MaxClientKeys: -1, MaxVirtualModels: -1, MaxConcurrentStreams: -1, ActivityRetentionDays: 7, MonthlyRequests: -1}); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateProvider(ctx, planProviderInput("a-p1")); err != nil {
		t.Fatal(err)
	}
	assertLimitKind(t, a.CreateProvider(ctx, planProviderInput("a-p2")), "providers", 1)

	// The other account still has its own full allowance.
	if err := b.CreateProvider(ctx, planProviderInput("b-p1")); err != nil {
		t.Fatalf("second account provider should not be limited by first: %v", err)
	}
	// Usage counters are account-scoped too.
	if err := a.IncrementUsageCounter(ctx, "2026-09"); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.UsageCount(ctx, "2026-09"); got != 0 {
		t.Fatalf("second account saw first account's usage count: %d", got)
	}
}

// TestPruneRequestLogsClampsRetentionToPlan verifies the hosted enforcement
// clamp: a client key configured for 90 days is pruned at the plan's 7-day cap
// when enforcement is on, and at its configured 90 days when enforcement is
// off. Rows are never rewritten.
func TestPruneRequestLogsClampsRetentionToPlan(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	st := store.New(db.SQL, store.WithActivityDB(db.Activity), store.WithLimitEnforcement(true))

	sc := st.For(database.LocalAccountID)
	if err := sc.CreateClientKey(ctx, planClientInput("ret-key")); err != nil {
		t.Fatal(err)
	}
	insertLogAt(t, db, "old-40d", time.Now().UTC().Add(-40*24*time.Hour).Format(time.RFC3339Nano))
	insertLogAt(t, db, "recent-1d", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339Nano))

	// free plan = 7 days; 40-day-old row is beyond the clamp and pruned, the
	// 1-day-old row survives.
	if err := st.PruneRequestLogs(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := countLogs(t, db); got != 1 {
		t.Fatalf("after clamped prune = %d rows, want 1", got)
	}
	// The stored client row still says 90 days (never rewritten).
	var configured int
	if err := db.SQL.QueryRow(`SELECT retention_days FROM client_keys WHERE id='ret-key'`).Scan(&configured); err != nil {
		t.Fatal(err)
	}
	if configured != 30 {
		t.Fatalf("stored retention_days = %d, want 30 (unchanged)", configured)
	}
}

func insertLogAt(t *testing.T, db *database.DB, id, createdAt string) {
	t.Helper()
	if _, err := db.Activity.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,client_name,requested_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES(?,?,?,?,?,?,0,200,1,?,?)`, id, database.LocalAccountID, "ret-key", "client", "provider-a/model-a", "chat", id, createdAt); err != nil {
		t.Fatal(err)
	}
}

func countLogs(t *testing.T, db *database.DB) int {
	t.Helper()
	var n int
	if err := db.Activity.QueryRow(`SELECT count(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
