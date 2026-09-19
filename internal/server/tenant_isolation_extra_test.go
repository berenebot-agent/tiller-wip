package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// TestLiveAccountIsolation proves a live delta emitted for one account reaches
// only that account's subscribers, even though all accounts share one hub.
func TestLiveAccountIsolation(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	h := api.server.liveHub
	// Keep the idle/debounce paths from pushing snapshots during the test so a
	// frame on the other account's channel can only mean a leak.
	h.mu.Lock()
	h.timings = liveTimings{debounce: time.Hour, idle: time.Hour, sessionCheck: time.Hour}
	h.mu.Unlock()

	local := database.LocalAccountID
	other := otherAccountID
	chLocal := h.subscribe(local)
	chOther := h.subscribe(other)
	defer h.unsubscribe(local, chLocal)
	defer h.unsubscribe(other, chOther)

	h.emitOutcome(local, map[string]lastOutcome{"pm-1": {IsSuccess: true, Degrading: true}})

	select {
	case msg := <-chLocal:
		if !strings.Contains(string(msg), "event: outcome") {
			t.Fatalf("local subscriber got unexpected frame: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local subscriber did not receive its own outcome")
	}
	select {
	case msg := <-chOther:
		t.Fatalf("other account received a leaked live frame: %s", msg)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestUsageCacheAccountIsolation proves the usage aggregate cache is keyed per
// account: one account's snapshot never includes another's traffic, and
// invalidating one account's cache leaves the other's intact.
func TestUsageCacheAccountIsolation(t *testing.T) {
	api, db, _, _ := loggingTestHarness(t, mockUpstream(t))
	s := api.server
	s.usageCacheTTL = time.Minute
	ctx := context.Background()

	insert := func(accountID, rowID, clientID string, tokens int64) {
		t.Helper()
		adb, err := database.OpenActivity(ctx, filepath.Join(db.ActivityDir, accountID+".db"))
		if err != nil {
			t.Fatal(err)
		}
		defer adb.Close()
		if _, err := adb.Exec(`INSERT INTO request_logs(id,account_id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			rowID, accountID, clientID, "provider-a/model-a", "provider-a", "model-a", "chat", 0, 200, 1, tokens/2, tokens-tokens/2, "req-"+rowID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	insert(database.LocalAccountID, "row-a", "ck-a", 100)
	insert(otherAccountID, "row-b", "ck-b", 60)

	snapA, err := s.buildUsageSnapshot(ctx, database.LocalAccountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapA.ClientKeys["ck-a"]; !ok {
		t.Fatalf("account A snapshot missing its own client: %v", snapA.ClientKeys)
	}
	if _, ok := snapA.ClientKeys["ck-b"]; ok {
		t.Fatalf("account A snapshot leaked account B traffic: %v", snapA.ClientKeys)
	}
	snapB, err := s.buildUsageSnapshot(ctx, otherAccountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapB.ClientKeys["ck-b"]; !ok {
		t.Fatalf("account B snapshot missing its own client: %v", snapB.ClientKeys)
	}
	if _, ok := snapB.ClientKeys["ck-a"]; ok {
		t.Fatalf("account B snapshot leaked account A traffic: %v", snapB.ClientKeys)
	}

	s.usageAggMu.Lock()
	_, hasA := s.usageAgg[database.LocalAccountID]
	_, hasB := s.usageAgg[otherAccountID]
	s.usageAggMu.Unlock()
	if !hasA || !hasB {
		t.Fatalf("expected per-account caches, got A=%v B=%v", hasA, hasB)
	}

	s.invalidateUsageAggregates(database.LocalAccountID)
	s.usageAggMu.Lock()
	_, hasA = s.usageAgg[database.LocalAccountID]
	_, hasB = s.usageAgg[otherAccountID]
	s.usageAggMu.Unlock()
	if hasA {
		t.Fatal("account A cache was not invalidated")
	}
	if !hasB {
		t.Fatal("invalidating account A wrongly dropped account B's cache")
	}
}
