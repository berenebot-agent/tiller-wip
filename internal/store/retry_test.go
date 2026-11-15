package store

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// TestRunTxRetriesBusySnapshot forces the exact failure the retry exists for: a
// transaction reads, another connection commits, and the first transaction's
// write then fails immediately with SQLITE_BUSY_SNAPSHOT (which busy_timeout
// does not retry). The whole transaction must be retried and commit.
func TestRunTxRetriesBusySnapshot(t *testing.T) {
	t.Parallel()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.SQL.Exec(`CREATE TABLE tx_probe (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sc := New(db.SQL).For(database.LocalAccountID)

	readDone := make(chan struct{})
	writeDone := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		<-readDone
		_, werr := db.SQL.ExecContext(ctx, `INSERT INTO tx_probe(v) VALUES('w')`)
		writerErr <- werr
		close(writeDone)
	}()

	var attempts atomic.Int32
	err = sc.RunTx(ctx, nil, func(tx *Scope) error {
		n := attempts.Add(1)
		var count int
		if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM tx_probe`).Scan(&count); err != nil {
			return err
		}
		if n == 1 {
			// Let the other connection commit after this read, then attempt the
			// write against the now-stale snapshot.
			close(readDone)
			select {
			case <-writeDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_, err := tx.q.ExecContext(ctx, `INSERT INTO tx_probe(v) VALUES('x')`)
		return err
	})
	if err != nil {
		t.Fatalf("RunTx after a busy snapshot: %v", err)
	}
	if werr := <-writerErr; werr != nil {
		t.Fatalf("concurrent writer: %v", werr)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("transaction attempts = %d, want at least 2 (the busy snapshot was not retried)", got)
	}
	var rows int
	if err := db.SQL.QueryRow(`SELECT count(*) FROM tx_probe`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want 2 (both the concurrent writer and the retried transaction)", rows)
	}
}
