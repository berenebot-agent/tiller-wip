package store

import (
	"context"
	"errors"
	"math/rand"
	"time"

	sqlite "modernc.org/sqlite"
)

// Transaction contention handling.
//
// SQLite runs transactions in deferred mode by default: BEGIN takes no lock,
// and the transaction upgrades to a writer on its first write. If another
// connection committed between this transaction's first read and that write,
// the upgrade fails immediately with SQLITE_BUSY_SNAPSHOT. The busy handler
// (busy_timeout) is deliberately not invoked for that error -- the snapshot is
// already stale, so waiting cannot help. Retrying the whole transaction is the
// only recovery, and it is safe here because tenant transaction callbacks are
// database-only by rule (no network or streaming I/O inside RunTx).
const (
	txMaxAttempts = 5
	txBaseBackoff = 4 * time.Millisecond
	txMaxBackoff  = 100 * time.Millisecond
)

// isBusyError reports whether err is SQLite contention that a fresh transaction
// can resolve: SQLITE_BUSY (5), SQLITE_LOCKED (6) and their extended codes such
// as SQLITE_BUSY_SNAPSHOT (517).
func isBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6:
		return true
	default:
		return false
	}
}

// retryBusy runs one transaction attempt repeatedly while it fails with a
// retryable contention error. Any other error, and a cancelled context, returns
// immediately.
func retryBusy(ctx context.Context, attempt func() error) error {
	var last error
	for n := 0; n < txMaxAttempts; n++ {
		if n > 0 {
			delay := txBaseBackoff << (n - 1)
			if delay > txMaxBackoff {
				delay = txMaxBackoff
			}
			delay += time.Duration(rand.Int63n(int64(delay)/2 + 1))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		err := attempt()
		if err == nil {
			return nil
		}
		if !isBusyError(err) {
			return err
		}
		last = err
	}
	return last
}
