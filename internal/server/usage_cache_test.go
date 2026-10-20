package server

import (
	"context"
	"fmt"
	"github.com/tiller-router/tiller-router/internal/database"
	"sync"
	"testing"
	"time"
)

// TestUsageAggregatesReusedWithinTTL verifies that the DB-derived aggregates
// are reused inside the TTL and that invalidateUsageAggregates forces a
// recompute, while live in-memory state is never cached.
func TestUsageAggregatesReusedWithinTTL(t *testing.T) {
	t.Parallel()
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = time.Minute
	now := time.Now().UTC()

	insert := func(id string, total int64) {
		in := total / 2
		out := total - in
		if _, err := activityDB(t, db).Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, clientID, "provider-a/model-a", "provider-a", "model-a", "chat", 0, 200, 1, in, out, "req-"+id, now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	insert("a", 100)
	snap, err := s.buildUsageSnapshot(context.Background(), database.LocalAccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.ClientKeys[clientID].H1; got != 100 {
		t.Fatalf("first snapshot 1h = %d, want 100", got)
	}

	// A second row within the TTL must not be visible: the aggregate is cached.
	insert("b", 200)
	snap, err = s.buildUsageSnapshot(context.Background(), database.LocalAccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.ClientKeys[clientID].H1; got != 100 {
		t.Fatalf("cached snapshot 1h = %d, want stale 100", got)
	}

	// Invalidation forces a recompute that sees both rows.
	s.invalidateUsageAggregates(database.LocalAccountID)
	snap, err = s.buildUsageSnapshot(context.Background(), database.LocalAccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.ClientKeys[clientID].H1; got != 300 {
		t.Fatalf("invalidated snapshot 1h = %d, want 300", got)
	}
}

// TestUsageSnapshotLiveStateNotCached verifies that in-memory last outcomes
// and cooldowns are read fresh on every snapshot even when the DB aggregates
// are served from cache.
func TestUsageSnapshotLiveStateNotCached(t *testing.T) {
	t.Parallel()
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = time.Minute

	if _, err := s.buildUsageSnapshot(context.Background(), database.LocalAccountID); err != nil {
		t.Fatal(err)
	}

	s.recordLastOutcome(&logRow{attempts: []requestAttempt{
		{providerModelID: "pm-live", provider: "p", model: "m", result: "failed", httpStatus: 500, failureClass: "http_500"},
	}})

	snap, err := s.buildUsageSnapshot(context.Background(), database.LocalAccountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.TargetLastOutcome["pm-live"]; !ok {
		t.Fatalf("live last outcome was not fresh despite cached aggregates: %v", snap.TargetLastOutcome)
	}
}

// TestUsageAggregatesConcurrentCoalesce exercises the cache under concurrent
// snapshot builds. The mutex must serialize the compute so there is no data
// race, and every caller must observe a consistent aggregate.
func TestUsageAggregatesConcurrentCoalesce(t *testing.T) {
	t.Parallel()
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = time.Minute
	if _, err := activityDB(t, db).Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"conc", clientID, "provider-a/model-a", "provider-a", "model-a", "chat", 0, 200, 1, 42, 8, "req-conc", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := s.buildUsageSnapshot(context.Background(), database.LocalAccountID)
			if err != nil {
				errs <- err
				return
			}
			if got := snap.ClientKeys[clientID].H1; got != 50 {
				errs <- fmt.Errorf("concurrent snapshot 1h = %d, want 50", got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestUsageCacheDisabledByDefaultInTests guards the test harness contract:
// production enables the TTL, tests must not, so a write-then-read sequence is
// never served stale.
func TestUsageCacheDisabledByDefaultInTests(t *testing.T) {
	t.Parallel()
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	if api.server.usageCacheTTL != 0 {
		t.Fatalf("test server usageCacheTTL = %v, want 0", api.server.usageCacheTTL)
	}
	if usageAggregateTTL <= 0 {
		t.Fatalf("production usageAggregateTTL = %v, want > 0", usageAggregateTTL)
	}
}

// TestUsageAggregateTTLBackoff pins the derivation of the aggregate reuse
// window. The window is the floor extended by the cost of the recompute, so
// the aggregate work keeps a bounded duty cycle however expensive the scan
// becomes, and an explicit override is never shortened.
func TestUsageAggregateTTLBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		floor time.Duration
		cost  time.Duration
		want  time.Duration
	}{
		{"a cheap recompute uses the floor", usageAggregateTTL, 30 * time.Millisecond, usageAggregateTTL},
		{"an expensive recompute backs off", usageAggregateTTL, 1600 * time.Millisecond, 32 * time.Second},
		{"a very expensive recompute is capped", usageAggregateTTL, 10 * time.Second, usageAggregateMaxTTL},
		{"an explicit override is never shortened", 5 * time.Minute, 10 * time.Second, 5 * time.Minute},
		{"a non-positive floor disables reuse", 0, time.Second, 0},
	}
	for _, tc := range cases {
		if got := usageAggregateTTLFor(tc.floor, tc.cost); got != tc.want {
			t.Errorf("%s: usageAggregateTTLFor(%v, %v) = %v, want %v", tc.name, tc.floor, tc.cost, got, tc.want)
		}
	}
}

// TestUsageAggregateUsesDerivedTTL proves the read path honours the derived
// window rather than the fixed floor: a stale row added just after a snapshot
// must stay invisible for as long as the derived window says, which is how a
// costly recompute stops being repeated often enough to dominate the process.
func TestUsageAggregateUsesDerivedTTL(t *testing.T) {
	t.Parallel()
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = usageAggregateTTL
	ctx := context.Background()
	now := time.Now().UTC()

	insert := func(id string, total int64) {
		in := total / 2
		out := total - in
		if _, err := activityDB(t, db).Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, clientID, "provider-a/model-a", "provider-a", "model-a", "chat", 0, 200, 1, in, out, "req-"+id, now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	insert("a", 100)
	if _, err := s.usageAggregates(ctx, database.LocalAccountID, now); err != nil {
		t.Fatal(err)
	}

	s.usageAggMu.Lock()
	entry := s.usageAgg[database.LocalAccountID]
	s.usageAggMu.Unlock()
	if entry == nil {
		t.Fatal("no aggregate cache entry for the local account")
	}
	entry.mu.Lock()
	if entry.ttl < usageAggregateTTL {
		entry.mu.Unlock()
		t.Fatalf("derived reuse window = %v, want at least the floor %v", entry.ttl, usageAggregateTTL)
	}
	// Stand in for an expensive recompute: the window the caller derives from
	// the measured cost is the one the read path must honour.
	entry.ttl = time.Hour
	entry.mu.Unlock()

	insert("b", 200)
	agg, err := s.usageAggregates(ctx, database.LocalAccountID, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.ClientKeys[clientID].H1; got != 100 {
		t.Fatalf("1h = %d, want stale 100: the derived reuse window must govern reuse", got)
	}
}
