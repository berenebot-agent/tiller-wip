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
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("sqlite vacuum: %w", err)
	}
	return nil
}
