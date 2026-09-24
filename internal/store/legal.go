package store

import (
	"context"
	"database/sql"
	"errors"
)

// LegalDoc is the current published document for a slug.
type LegalDoc struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// LegalAcceptance is one recorded act of agreement.
type LegalAcceptance struct {
	ID               string `json:"id"`
	UserID           string `json:"user_id"`
	TermsUpdatedAt   string `json:"terms_updated_at,omitempty"`
	PrivacyUpdatedAt string `json:"privacy_updated_at,omitempty"`
	AcceptedAt       string `json:"accepted_at"`
	IP               string `json:"-"`
	UserAgent        string `json:"-"`
}

// ErrLegalDocNotFound is returned when a slug has no published document.
var ErrLegalDocNotFound = errors.New("store: legal document not found")

// GetLegalDoc returns the current document for a slug.
func (s *Store) GetLegalDoc(ctx context.Context, slug string) (LegalDoc, error) {
	var d LegalDoc
	var updatedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT slug,title,body,updated_at,updated_by FROM legal_documents WHERE slug=?`, slug).
		Scan(&d.Slug, &d.Title, &d.Body, &d.UpdatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return LegalDoc{}, ErrLegalDocNotFound
	}
	if updatedBy.Valid {
		d.UpdatedBy = updatedBy.String
	}
	return d, err
}

// ListLegalDocs returns every document ordered by slug (metadata only; used by
// the platform editor list).
func (s *Store) ListLegalDocs(ctx context.Context) ([]LegalDoc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug,title,body,updated_at,coalesce(updated_by,'') FROM legal_documents ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LegalDoc{}
	for rows.Next() {
		var d LegalDoc
		if err := rows.Scan(&d.Slug, &d.Title, &d.Body, &d.UpdatedAt, &d.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpsertLegalDoc writes the current document for a slug. It only replaces the
// body of rows already seeded by a migration (updated_by IS NULL) when seed is
// true; an operator-edited row is never overwritten by a deploy. updatedBy is
// the acting platform operator marker.
func (s *Store) UpsertLegalDoc(ctx context.Context, d LegalDoc, updatedBy string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO legal_documents(slug,title,body,updated_at,updated_by) VALUES(?,?,?,?,?)
ON CONFLICT(slug) DO UPDATE SET title=excluded.title,body=excluded.body,updated_at=excluded.updated_at,updated_by=excluded.updated_by`,
		d.Slug, d.Title, d.Body, now(), nullableStoreString(updatedBy))
	return err
}

// SeedLegalDoc inserts a document only when the slug does not exist, or when it
// still carries the migration placeholder (updated_by IS NULL). Operator edits
// are preserved.
func (s *Store) SeedLegalDoc(ctx context.Context, d LegalDoc) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO legal_documents(slug,title,body,updated_at,updated_by) VALUES(?,?,?,?,NULL)
ON CONFLICT(slug) DO UPDATE SET title=excluded.title,body=excluded.body,updated_at=excluded.updated_at WHERE legal_documents.updated_by IS NULL`,
		d.Slug, d.Title, d.Body, now())
	return err
}

// RecordLegalAcceptance stores a user's agreement to the current documents. It
// runs inside the signup transaction so an account is never created without its
// acceptance record.
func (s *Scope) RecordLegalAcceptance(ctx context.Context, id string, a LegalAcceptance) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO legal_acceptances(id,user_id,terms_updated_at,privacy_updated_at,accepted_at,ip,user_agent) VALUES(?,?,?,?,?,?,?)`,
		id, a.UserID, nullableStoreString(a.TermsUpdatedAt), nullableStoreString(a.PrivacyUpdatedAt), a.AcceptedAt, nullableStoreString(a.IP), nullableStoreString(a.UserAgent))
	return err
}

// LatestLegalAcceptance returns the most recent acceptance for a user, if any.
func (s *Store) LatestLegalAcceptance(ctx context.Context, userID string) (LegalAcceptance, error) {
	var a LegalAcceptance
	var terms, privacy sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,coalesce(terms_updated_at,''),coalesce(privacy_updated_at,''),accepted_at FROM legal_acceptances WHERE user_id=? ORDER BY accepted_at DESC LIMIT 1`, userID).
		Scan(&a.ID, &a.UserID, &terms, &privacy, &a.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LegalAcceptance{}, ErrLegalDocNotFound
	}
	a.TermsUpdatedAt = terms.String
	a.PrivacyUpdatedAt = privacy.String
	return a, err
}
