package database

import (
	"context"
	"database/sql"
	"fmt"
)

// VacuumCore compacts the central database in place, returning pages freed by
// deletes to the filesystem. SQLite never shrinks a database file on its own
// (auto_vacuum is off), so without a VACUUM the core file keeps its high-water
// mark after the Activity split and routine session/audit cleanup.
//
// VACUUM cannot run inside a transaction; it rewrites the file into a temporary
// copy, so it needs free disk roughly equal to the database size and takes an
// exclusive lock on the database. maintenanceMu serializes it against a
// concurrent backup snapshot, which also rewrites the core file.
func (d *DB) VacuumCore(ctx context.Context) error {
	d.maintenanceMu.Lock()
	defer d.maintenanceMu.Unlock()
	return vacuum(ctx, d.SQL)
}

// checkpointWAL folds the write-ahead log back into the main database and
// truncates it. Under WAL a VACUUM must reconcile the WAL before rewriting the
// file, so an unchecked WAL (which can grow larger than the database itself)
// turns compaction into a costly full-log replay. TRUNCATE also returns the WAL
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

// VacuumActivity compacts the Activity database. It is a no-op when Activity is
// unavailable, matching its best-effort telemetry posture (see OpenActivity).
// It shares maintenanceMu so a core backup and an Activity compaction never
// rewrite files at the same time.
func (d *DB) VacuumActivity(ctx context.Context) error {
	if d.Activity == nil {
		return nil
	}
	d.maintenanceMu.Lock()
	defer d.maintenanceMu.Unlock()
	return vacuum(ctx, d.Activity)
}

func vacuum(ctx context.Context, db *sql.DB) error {
	// Fold and truncate the WAL before compaction so the VACUUM rewrite does
	// not have to reconcile a large write-ahead log (which can grow past the
	// database itself). Checkpoint failures are ignored: the VACUUM is still
	// correct against whatever WAL state exists, just slower.
	_ = checkpointWAL(ctx, db)
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("sqlite vacuum: %w", err)
	}
	// VACUUM rewrites the database through the WAL, leaving the log non-empty
	// again. Checkpoint once more so the compaction actually reclaims the WAL
	// space on disk rather than just moving it back into a new log.
	_ = checkpointWAL(ctx, db)
	return nil
}
