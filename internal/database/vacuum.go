package database

import (
	"context"
	"database/sql"
	"fmt"
)

// IncrementalVacuumPages bounds how many freelist pages a single maintenance
// pass returns to the filesystem. Reclaiming space in fixed-size chunks keeps
// the write lock to a bounded slice instead of the whole-file rewrite a
// `VACUUM` performs: a full rewrite of the 1.1 GB Activity database measured
// ~21 s on this deployment, while one 4096-page (16 MiB) chunk costs a fraction
// of a second even where fsync latency is ~300 ms.
const IncrementalVacuumPages = 4096

// autoVacuumIncremental is SQLite's PRAGMA auto_vacuum value for incremental
// mode: freed pages stay on the freelist until `PRAGMA incremental_vacuum` asks
// for them back, so reclamation is driven by the maintenance pass rather than
// by whichever COMMIT happens to free a page.
const autoVacuumIncremental = 2

// VacuumResult reports what one maintenance pass did, so the caller can log the
// work performed and not just how long it took.
type VacuumResult struct {
	// Converted is true when the database still ran with auto_vacuum off and
	// this pass performed the one-off full rewrite that switches it on. That
	// rewrite is the only database-sized lock this path can take, and it
	// happens at most once per database.
	Converted bool
	// FreedPages is how many freelist pages the pass returned to the
	// filesystem, capped by the per-pass budget.
	FreedPages int64
}

// VacuumCore reclaims free pages from the central database, returning the space
// deletes have freed to the filesystem.
//
// SQLite only shrinks a file by itself in incremental auto-vacuum mode, so the
// first pass over a database created before that mode existed converts it with
// a single full rewrite. Every pass after that frees a bounded number of pages,
// which is what makes the step safe to run against a live instance: the lock is
// sized by the budget, not by the file.
//
// maintenanceMu serializes it against a concurrent backup snapshot, which also
// rewrites the core file.
func (d *DB) VacuumCore(ctx context.Context) (VacuumResult, error) {
	d.maintenanceMu.Lock()
	defer d.maintenanceMu.Unlock()
	return vacuum(ctx, d.SQL, IncrementalVacuumPages)
}

// checkpointWAL folds the write-ahead log back into the main database and
// truncates it. Under WAL a rewrite must reconcile the WAL before it runs, so
// an unchecked WAL (which can grow larger than the database itself) turns
// compaction into a costly full-log replay. TRUNCATE also returns the WAL
// file to zero bytes so the space is reclaimed on disk. It is best-effort: a
// busy checkpoint (an active reader/writer) is not a vacuum failure, so the
// error is returned only for the caller to decide whether to surface it.
func checkpointWAL(ctx context.Context, db *sql.DB) error {
	// wal_checkpoint returns a row (busy, log, checkpointed) rather than being
	// a no-op; QueryContext consumes it. A non-WAL database simply reports
	// zeros.
	rows, err := db.QueryContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		return fmt.Errorf("sqlite wal checkpoint: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// VacuumActivity reclaims free pages from the Activity database. It is a no-op
// when Activity is unavailable, matching its best-effort telemetry posture (see
// OpenActivity). It shares maintenanceMu so a core backup and an Activity
// reclamation never rewrite files at the same time.
func (d *DB) VacuumActivity(ctx context.Context) (VacuumResult, error) {
	if d.Activity == nil {
		return VacuumResult{}, nil
	}
	d.maintenanceMu.Lock()
	defer d.maintenanceMu.Unlock()
	return vacuum(ctx, d.Activity, IncrementalVacuumPages)
}

// vacuum reclaims space from one database within a page budget.
//
// The WAL is checkpointed first because that is cheap, never holds the
// exclusive lock, and keeps the log from becoming the largest object in the
// directory. A database still in `auto_vacuum=NONE` is converted with a single
// full rewrite — the one unbounded step — and every later pass frees at most
// budgetPages pages, so the caller's write lock is bounded by the budget rather
// than by the file size.
func vacuum(ctx context.Context, db *sql.DB, budgetPages int) (VacuumResult, error) {
	var result VacuumResult
	_ = checkpointWAL(ctx, db)

	// Pin one connection: auto_vacuum is a connection-level pragma whose change
	// only takes effect once the file is rewritten, so the pragma and the
	// VACUUM that applies it must run together.
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, fmt.Errorf("sqlite vacuum connection: %w", err)
	}
	defer conn.Close()

	mode, err := autoVacuumMode(ctx, conn)
	if err != nil {
		return result, err
	}
	if mode != autoVacuumIncremental {
		if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
			return result, fmt.Errorf("sqlite auto_vacuum: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
			return result, fmt.Errorf("sqlite vacuum: %w", err)
		}
		result.Converted = true
		// The conversion rewrites the file through the WAL; fold it back so the
		// reclaimed space is real on disk.
		_ = checkpointWAL(ctx, db)
		return result, nil
	}

	before, err := freelistCount(ctx, conn)
	if err != nil {
		return result, err
	}
	if before == 0 {
		return result, nil
	}
	budget := int64(budgetPages)
	if budget <= 0 || budget > before {
		budget = before
	}
	// incremental_vacuum takes no bind parameters; the value is an integer this
	// package computes, never caller input.
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, budget))
	if err != nil {
		return result, fmt.Errorf("sqlite incremental_vacuum: %w", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("sqlite incremental_vacuum: %w", err)
	}
	rows.Close()

	after, err := freelistCount(ctx, conn)
	if err != nil {
		return result, err
	}
	result.FreedPages = before - after
	// Reclaimed pages move through the WAL, so truncate it once more; without
	// this the space is free in the main file but still occupied by the log.
	_ = checkpointWAL(ctx, db)
	return result, nil
}

// autoVacuumMode reports the database's PRAGMA auto_vacuum value.
func autoVacuumMode(ctx context.Context, conn *sql.Conn) (int, error) {
	var mode int
	if err := conn.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return 0, fmt.Errorf("sqlite auto_vacuum: %w", err)
	}
	return mode, nil
}

// freelistCount reports how many pages are on the database's freelist.
func freelistCount(ctx context.Context, conn *sql.Conn) (int64, error) {
	var free int64
	if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, fmt.Errorf("sqlite freelist_count: %w", err)
	}
	return free, nil
}
