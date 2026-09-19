package server

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

func newLogWriterServer(t *testing.T) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app := newTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	return app, db
}

func writerRow(id string) logWrite {
	return logWrite{accountID: database.LocalAccountID, row: store.RequestLogInsert{
		ID:              id,
		ClientKeyID:     "ck-1",
		ClientName:      "client",
		RequestedModel:  "provider-a/model-a",
		Protocol:        "chat",
		HTTPStatus:      200,
		ClientRequestID: id,
		CreatedAt:       database.Now(),
	}}
}

// TestLogWriterBatchesAndFlushes verifies queued Activity rows are written in
// the background without the caller waiting.
func TestLogWriterBatchesAndFlushes(t *testing.T) {
	app, db := newLogWriterServer(t)
	w := newLogWriter(app.scopeFor, app.logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.start(ctx)
	for i := 0; i < 3; i++ {
		w.enqueue(writerRow(fmt.Sprintf("w-%d", i)))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := activityDB(t, db).QueryRow(`SELECT count(*) FROM request_logs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer did not flush 3 rows, got %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	w.wait()
}

// TestLogWriterFlushesOnShutdown verifies rows queued right before shutdown are
// drained and committed.
func TestLogWriterFlushesOnShutdown(t *testing.T) {
	app, db := newLogWriterServer(t)
	w := newLogWriter(app.scopeFor, app.logger)
	ctx, cancel := context.WithCancel(context.Background())
	w.start(ctx)
	w.enqueue(writerRow("shutdown-row"))
	cancel()
	w.wait()
	var n int
	if err := activityDB(t, db).QueryRow(`SELECT count(*) FROM request_logs WHERE id='shutdown-row'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("shutdown flush lost row: count=%d", n)
	}
}

// TestLogWriterDropsWhenQueueFull verifies a saturated queue drops rather than
// blocks the caller.
func TestLogWriterDropsWhenQueueFull(t *testing.T) {
	w := newLogWriter(func(string) *store.Scope { return nil }, nil)
	for i := 0; i < logWriteQueueSize; i++ {
		w.enqueue(writerRow(fmt.Sprintf("q-%d", i)))
	}
	w.enqueue(writerRow("overflow"))
	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}
