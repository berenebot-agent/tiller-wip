package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrManagementTokenNotFound is returned when a management-token operation
// targets a row that does not exist in the scope's account. It intentionally
// means "not visible to this account" for the caller.
var ErrManagementTokenNotFound = errors.New("store: management token not found")

// EnvironmentManagementTokenID is reserved for the local environment-managed credential.
const EnvironmentManagementTokenID = "boot-admin"

// ManagementTokenRow is one management-token row. The secret hash is never
// included; only the display fingerprint.
type ManagementTokenRow struct {
	ID          string
	Name        string
	Description string
	Fingerprint string
	Scopes      string
	Enabled     bool
	ExpiresAt   sql.NullString
	LastUsedAt  sql.NullString
	CreatedAt   string
	RotatedAt   sql.NullString
	UpdatedAt   string
}

// ManagementTokenAuthRow is the principal lookup row used by token
// authentication.
type ManagementTokenAuthRow struct {
	ID            string
	AccountID     string
	AccountStatus string
	Name          string
	Enabled       bool
	SecretHash    string
	Scopes        string
	ExpiresAt     sql.NullString
}

// ManagementTokenBySelector looks up a management token by its public
// selector. It is the pre-account authentication path (the token determines
// the account), so it is deliberately platform-level rather than
// account-scoped.
func (s *Store) ManagementTokenBySelector(ctx context.Context, selector string) (ManagementTokenAuthRow, error) {
	var v ManagementTokenAuthRow
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT t.id,t.account_id,a.status,t.name,t.enabled,t.secret_hash,t.scopes,t.expires_at FROM api_tokens t JOIN accounts a ON a.id=t.account_id WHERE t.selector=?`, selector).
		Scan(&v.ID, &v.AccountID, &v.AccountStatus, &v.Name, &enabled, &v.SecretHash, &v.Scopes, &v.ExpiresAt)
	if err != nil {
		return ManagementTokenAuthRow{}, err
	}
	v.Enabled = enabled != 0
	return v, nil
}

// UpdateManagementTokenSecretHash lazily migrates a management token's stored
// hash after a successful verify. The compare-and-swap keeps it safe against
// concurrent rotation.
func (s *Store) UpdateManagementTokenSecretHash(ctx context.Context, id, oldHash, newHash, updatedAt string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET secret_hash=?, updated_at=? WHERE id=? AND secret_hash=?`, newHash, updatedAt, id, oldHash)
	return err
}

// TouchManagementTokenLastUsed records successful token use. It is
// best-effort observability, never an authorization input.
func (s *Store) TouchManagementTokenLastUsed(ctx context.Context, id, at string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, at, id)
	return err
}

func (s *Scope) scanManagementTokenRow(row interface{ Scan(...any) error }) (ManagementTokenRow, error) {
	var v ManagementTokenRow
	var enabled int
	if err := row.Scan(&v.ID, &v.Name, &v.Description, &v.Fingerprint, &v.Scopes, &enabled, &v.ExpiresAt, &v.LastUsedAt, &v.CreatedAt, &v.RotatedAt, &v.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagementTokenRow{}, ErrManagementTokenNotFound
		}
		return ManagementTokenRow{}, err
	}
	v.Enabled = enabled != 0
	return v, nil
}

// ListManagementTokens returns the account's management tokens ordered by
// name.
func (s *Scope) ListManagementTokens(ctx context.Context) ([]ManagementTokenRow, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id,name,description,secret_fingerprint,scopes,enabled,expires_at,last_used_at,created_at,rotated_at,updated_at FROM api_tokens WHERE account_id=? ORDER BY name`, s.accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagementTokenRow{}
	for rows.Next() {
		v, err := s.scanManagementTokenRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetManagementToken returns one management token from the scope's account.
func (s *Scope) GetManagementToken(ctx context.Context, id string) (ManagementTokenRow, error) {
	return s.scanManagementTokenRow(s.q.QueryRowContext(ctx, `SELECT id,name,description,secret_fingerprint,scopes,enabled,expires_at,last_used_at,created_at,rotated_at,updated_at FROM api_tokens WHERE id=? AND account_id=?`, id, s.accountID))
}

// CreateManagementTokenInput carries the generated key material and metadata
// for a new management token.
type CreateManagementTokenInput struct {
	ID          string
	Name        string
	Description string
	Selector    string
	Hash        string
	Fingerprint string
	Scopes      string
	ExpiresAt   sql.NullString
}

// CreateManagementToken persists a management token for the scope's account.
func (s *Scope) CreateManagementToken(ctx context.Context, in CreateManagementTokenInput) error {
	now := now()
	_, err := s.q.ExecContext(ctx, `INSERT INTO api_tokens(id,account_id,name,description,selector,secret_hash,secret_fingerprint,scopes,enabled,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?,?)`,
		in.ID, s.accountID, in.Name, in.Description, in.Selector, in.Hash, in.Fingerprint, in.Scopes, in.ExpiresAt, now, now)
	return err
}

// UpdateManagementTokenInput is the merged, fully-validated field set for an
// update. The handler reads the current row and merges the request before
// calling, so the write always carries every editable field.
type UpdateManagementTokenInput struct {
	ID          string
	Name        string
	Description string
	Scopes      string
	Enabled     bool
	ExpiresAt   sql.NullString
}

// UpdateManagementToken writes the editable fields for a token in the scope's
// account.
func (s *Scope) UpdateManagementToken(ctx context.Context, in UpdateManagementTokenInput) error {
	res, err := s.q.ExecContext(ctx, `UPDATE api_tokens SET name=?,description=?,scopes=?,enabled=?,expires_at=?,updated_at=? WHERE id=? AND account_id=?`,
		in.Name, in.Description, in.Scopes, boolInt(in.Enabled), in.ExpiresAt, now(), in.ID, s.accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrManagementTokenNotFound
	}
	return nil
}

// RotateManagementToken replaces a token's selector and secret hash. It
// returns false when no row in the scope's account matched.
func (s *Scope) RotateManagementToken(ctx context.Context, id, selector, hash, fingerprint string) (bool, error) {
	now := now()
	res, err := s.q.ExecContext(ctx, `UPDATE api_tokens SET selector=?,secret_hash=?,secret_fingerprint=?,rotated_at=?,updated_at=? WHERE id=? AND account_id=?`, selector, hash, fingerprint, now, now, id, s.accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteManagementToken removes a token from the scope's account. It returns
// false when no row matched.
func (s *Scope) DeleteManagementToken(ctx context.Context, id string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM api_tokens WHERE id=? AND account_id=?`, id, s.accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeAllManagementTokens removes every browser-managed token in the scope's account and
// returns how many were removed.
func (s *Scope) RevokeAllManagementTokens(ctx context.Context) (int64, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM api_tokens WHERE account_id=? AND id<>?`, s.accountID, EnvironmentManagementTokenID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpsertManagementToken inserts or replaces a named management token in one
// account. It is used only by the environment boot seed, where re-seeding on a
// changed value must be idempotent: the fixed ID is the identity. Each startup
// restores the configured credential and its full scopes together.
func (s *Scope) UpsertManagementToken(ctx context.Context, in CreateManagementTokenInput) error {
	now := now()
	_, err := s.q.ExecContext(ctx, `INSERT INTO api_tokens(id,account_id,name,description,selector,secret_hash,secret_fingerprint,scopes,enabled,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,description=excluded.description,selector=excluded.selector,secret_hash=excluded.secret_hash,secret_fingerprint=excluded.secret_fingerprint,scopes=excluded.scopes,enabled=1,expires_at=excluded.expires_at,updated_at=excluded.updated_at`,
		in.ID, s.accountID, in.Name, in.Description, in.Selector, in.Hash, in.Fingerprint, in.Scopes, in.ExpiresAt, now, now)
	return err
}
