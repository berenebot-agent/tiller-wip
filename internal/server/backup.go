package server

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

// startBackupScheduler writes core and audit-database snapshots on a fixed
// interval, verifies each snapshot, and prunes snapshots past the retention
// window.
//
// Activity lives in separate per-account files and is deliberately excluded;
// audit.db is included because it contains security history (see
// docs/backup_restore_runbook.md for RPO/RTO and off-host copy guidance).
func (s *Server) startBackupScheduler(ctx context.Context) {
	interval := s.config.BackupInterval
	if interval <= 0 {
		return
	}
	dir := s.config.BackupDir
	if dir == "" {
		dir = filepath.Join(s.config.DataDir, "backups")
	}
	run := func() {
		path, err := s.db.Backup(ctx, dir)
		if err != nil {
			s.warnBackup("scheduled backup failed", err)
			return
		}
		if err := database.Verify(ctx, path); err != nil {
			s.warnBackup("scheduled backup verification failed", err)
			return
		}
		if s.db.Audit != nil {
			auditPath, auditErr := database.BackupNamed(ctx, s.db.Audit, dir, "tiller-audit-")
			if auditErr != nil {
				s.warnBackup("scheduled audit backup failed", auditErr)
				return
			}
			if auditErr := database.Verify(ctx, auditPath); auditErr != nil {
				s.warnBackup("scheduled audit backup verification failed", auditErr)
				return
			}
		}
		if _, err := database.PruneBackups(dir, s.config.BackupRetention, time.Now()); err != nil {
			s.warnBackup("scheduled backup prune failed", err)
		}
		if _, err := database.PrunePrefixedBackups(dir, s.config.BackupRetention, time.Now(), "tiller-audit-"); err != nil {
			s.warnBackup("scheduled audit backup prune failed", err)
		}
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

// warnBackup logs a backup failure by error type only; error strings from the
// database layer never contain credential material, but the class is enough to
// diagnose without risking it.
func (s *Server) warnBackup(msg string, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(msg, "error_class", fmt.Sprintf("%T", err))
}
