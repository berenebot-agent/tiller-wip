package server

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// startMaintenanceScheduler runs the periodic maintenance pass: a core-database
// snapshot (verified, then pruned by retention), followed by in-place
// compaction of both databases. It runs once at startup and then on a fixed
// interval. Zero disables the whole pass.
//
// Backup and vacuum are sequential by construction: they share one goroutine,
// and the DB-level maintenance mutex serializes them against an ad-hoc admin
// export. A backup failure is logged but never skips the vacuum.
//
// The core database includes durable control-plane state and the security audit
// tables, so a single snapshot restores both. Activity lives in the separate
// activity.db and is deliberately excluded from snapshots (see
// docs/backup_restore_runbook.md for RPO/RTO and off-host copy guidance), but
// it is compacted in place here.
func (s *Server) startMaintenanceScheduler(ctx context.Context) {
	interval := s.config.BackupInterval
	if interval <= 0 {
		return
	}
	dir := s.config.BackupDir
	if dir == "" {
		dir = filepath.Join(s.config.DataDir, "backups")
	}
	run := func() {
		if err := s.storeHandle().PruneAuditEvents(ctx, time.Now()); err != nil {
			s.warnBackup("scheduled audit prune failed", err)
		}
		path, err := s.db.Backup(ctx, dir)
		if err != nil {
			s.warnBackup("scheduled backup failed", err)
		} else if err := database.Verify(ctx, path); err != nil {
			s.warnBackup("scheduled backup verification failed", err)
		} else if _, err := database.PruneBackups(dir, s.config.BackupRetention, time.Now()); err != nil {
			s.warnBackup("scheduled backup prune failed", err)
		}
		// Compact after the snapshot so the pages freed by deletion are
		// reclaimed in place.
		s.vacuumOne(ctx, "core", s.db.VacuumCore)
		s.vacuumOne(ctx, "activity", s.db.VacuumActivity)
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// vacuumOne runs one database's compaction, logging start and completion at
// Info and failures at Warn by error class only (never the raw error, which
// could embed a path or SQL text). A failed vacuum is non-fatal: the database
// stays usable, it is just not compacted this pass.
func (s *Server) vacuumOne(ctx context.Context, db string, fn func(context.Context) error) {
	if s.logger == nil {
		return
	}
	s.logger.Info("database vacuum started", "db", db)
	start := time.Now()
	if err := fn(ctx); err != nil {
		s.logger.Warn("database vacuum failed", "db", db, "error_class", fmt.Sprintf("%T", err))
		return
	}
	s.logger.Info("database vacuum completed", "db", db, "duration_ms", time.Since(start).Milliseconds())
}

// warnBackup logs a backup failure by error type only; error strings from the
// database layer never contain credential material, but the class is enough to
// diagnose without risking it.
func (s *Server) warnBackup(msg string, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(msg, "error_class", fmt.Sprintf("%T", err))
}
