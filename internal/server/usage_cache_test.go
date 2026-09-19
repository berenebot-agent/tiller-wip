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
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = time.Minute
	now := time.Now().UTC()

	insert := func(id string, total int64) {
		in := total / 2
		out := total - in
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
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
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = time.Minute
	if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
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
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	if api.server.usageCacheTTL != 0 {
		t.Fatalf("test server usageCacheTTL = %v, want 0", api.server.usageCacheTTL)
	}
	if usageAggregateTTL <= 0 {
		t.Fatalf("production usageAggregateTTL = %v, want > 0", usageAggregateTTL)
	}
}
