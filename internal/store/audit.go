package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/tiller-router/tiller-router/internal/id"
)

// AuditEvent is the bounded, non-content representation persisted in the
// core database's audit tables. Audit is logically separate even though it now
// lives in router.db: account reads are account-scoped and platform events are
// never exposed through a customer API.
type AuditEvent struct {
	Event      string
	ActorType  string
	ActorID    string
	TargetType string
	TargetID   string
	Outcome    string
	Metadata   map[string]string
}

func auditMetadata(metadata map[string]string) (string, error) {
	if len(metadata) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	if len(b) > 8192 {
		return "", errors.New("store: audit metadata exceeds 8 KiB")
	}
	return string(b), nil
}

func (s *Scope) RecordAccountAudit(ctx context.Context, event AuditEvent) error {
	if event.Outcome == "" {
		event.Outcome = "success"
	}
	metadata, err := auditMetadata(event.Metadata)
	if err != nil {
		return err
	}
	eventID, err := id.New()
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO account_audit_events(id,account_id,event,actor_type,actor_id,target_type,target_id,outcome,metadata,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, eventID, s.accountID, event.Event, event.ActorType, nullableAuditID(event.ActorID), nullableAuditID(event.TargetType), nullableAuditID(event.TargetID), event.Outcome, metadata, now())
	return err
}

func (s *Store) RecordPlatformAudit(ctx context.Context, event AuditEvent) error {
	if event.Outcome == "" {
		event.Outcome = "success"
	}
	metadata, err := auditMetadata(event.Metadata)
	if err != nil {
		return err
	}
	eventID, err := id.New()
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO platform_audit_events(id,event,actor_type,actor_id,target_type,target_id,outcome,metadata,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, eventID, event.Event, event.ActorType, nullableAuditID(event.ActorID), nullableAuditID(event.TargetType), nullableAuditID(event.TargetID), event.Outcome, metadata, now())
	return err
}

type AuditRow struct {
	ID         string `json:"id"`
	AccountID  string `json:"account_id,omitempty"`
	Event      string `json:"event"`
	ActorType  string `json:"actor_type"`
	ActorID    string `json:"actor_id,omitempty"`
	TargetType string `json:"target_type,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	Outcome    string `json:"outcome"`
	Metadata   string `json:"metadata"`
	CreatedAt  string `json:"created_at"`
}

func (s *Scope) ListAccountAudit(ctx context.Context, limit, offset int) ([]AuditRow, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id,account_id,event,actor_type,coalesce(actor_id,''),coalesce(target_type,''),coalesce(target_id,''),outcome,metadata,created_at FROM account_audit_events WHERE account_id=? ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, s.accountID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var row AuditRow
		if err := rows.Scan(&row.ID, &row.AccountID, &row.Event, &row.ActorType, &row.ActorID, &row.TargetType, &row.TargetID, &row.Outcome, &row.Metadata, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ListPlatformAudit(ctx context.Context, limit, offset int) ([]AuditRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,event,actor_type,coalesce(actor_id,''),coalesce(target_type,''),coalesce(target_id,''),outcome,metadata,created_at FROM platform_audit_events ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var row AuditRow
		if err := rows.Scan(&row.ID, &row.Event, &row.ActorType, &row.ActorID, &row.TargetType, &row.TargetID, &row.Outcome, &row.Metadata, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SetAuditRetentionDays stores retention in the audit_meta table rather than in
// the account settings namespace, so it is independent of tenant settings.
func (s *Store) SetAuditRetentionDays(ctx context.Context, days int) error {
	if days < 1 {
		return errors.New("store: invalid audit retention")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_meta(key,value,updated_at) VALUES('audit_retention_days',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, strconv.Itoa(days), now())
	return err
}

func (s *Store) AuditRetentionDays(ctx context.Context) (int, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM audit_meta WHERE key='audit_retention_days'`).Scan(&raw); err != nil {
		return 0, err
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 {
		return 0, errors.New("store: invalid audit retention")
	}
	return days, nil
}

func (s *Store) PruneAuditEvents(ctx context.Context, current time.Time) error {
	days, err := s.AuditRetentionDays(ctx)
	if err != nil {
		return err
	}
	cutoff := current.UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_audit_events WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM platform_audit_events WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func nullableAuditID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
