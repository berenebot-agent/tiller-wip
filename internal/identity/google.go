package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tiller-router/tiller-router/internal/id"
)

var (
	ErrGoogleIdentityTaken = errors.New("identity: Google identity is already linked")
	ErrLastIdentity        = errors.New("identity: cannot remove the last sign-in method")
)

// GoogleUserBySubject returns the Tiller user attached to Google's stable sub
// identifier. Email is deliberately never used as the lookup key.
func (s *Store) GoogleUserBySubject(ctx context.Context, subject string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.status,u.email_verified_at,a.id,a.status,u.password_auth_enabled FROM user_identities i JOIN users u ON u.id=i.user_id JOIN accounts a ON a.owner_user_id=u.id WHERE i.provider='google' AND i.subject=?`, subject).
		Scan(&u.ID, &u.Email, &u.Status, &u.VerifiedAt, &u.AccountID, &u.AccountStatus, &u.PasswordEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) GoogleIdentityCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM user_identities WHERE provider='google'`).Scan(&count)
	return count, err
}

// CreateGoogleSignup creates an active, email-verified account and its Google
// identity in one transaction. The caller has already validated Google's
// signed identity token and must supply the current legal acceptance.
func (s *Store) CreateGoogleSignup(ctx context.Context, email, subject string, acceptance SignupAcceptance) (User, error) {
	email = NormalizeEmail(email)
	if !ValidateEmail(email) || subject == "" || len(subject) > 255 {
		return User{}, ErrNotFound
	}
	userID, err := id.New()
	if err != nil {
		return User{}, err
	}
	accountID, err := id.New()
	if err != nil {
		return User{}, err
	}
	identityID, err := id.New()
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,email,password_hash,password_auth_enabled,status,email_verified_at,created_at,updated_at) VALUES(?,?, '',0,'active',?,?,?)`, userID, email, formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(id,plan,status,owner_user_id,created_at,updated_at) VALUES(?,'free','active',?,?,?)`, accountID, userID, formatTime(now), formatTime(now)); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_identities(id,user_id,provider,subject,email,created_at) VALUES(?,?,'google',?,?,?)`, identityID, userID, subject, email, formatTime(now)); err != nil {
		return User{}, err
	}
	if err := recordSignupAcceptance(ctx, tx, userID, acceptance, now); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: userID, Email: email, Status: "active", AccountID: accountID, AccountStatus: "active", VerifiedAt: sql.NullString{String: formatTime(now), Valid: true}, PasswordEnabled: false}, nil
}

// LinkGoogleIdentity attaches a verified Google subject to the authenticated
// Tiller user. Existing identities are never linked by matching email.
func (s *Store) LinkGoogleIdentity(ctx context.Context, userID, subject, email string) error {
	if subject == "" || len(subject) > 255 || !ValidateEmail(email) {
		return ErrNotFound
	}
	var existingUser string
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE provider='google' AND subject=?`, subject).Scan(&existingUser)
	if err == nil {
		if existingUser == userID {
			return nil
		}
		return ErrGoogleIdentityTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	identityID, err := id.New()
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO user_identities(id,user_id,provider,subject,email,created_at) SELECT ?,u.id,'google',?,?,? FROM users u JOIN accounts a ON a.owner_user_id=u.id AND a.status='active' WHERE u.id=? AND u.status='active' AND u.email_verified_at IS NOT NULL`, identityID, subject, NormalizeEmail(email), formatTime(time.Now().UTC()), userID)
	if err != nil {
		// The unique constraints close races where this subject or a Google
		// identity for the user was linked after the reads above.
		var count int
		if lookupErr := s.db.QueryRowContext(ctx, `SELECT count(*) FROM user_identities WHERE provider='google' AND subject=? AND user_id=?`, subject, userID).Scan(&count); lookupErr == nil && count == 1 {
			return nil
		}
		return ErrGoogleIdentityTaken
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if n != 1 {
		return ErrNotFound
	}
	return nil
}

// UnlinkGoogleIdentity removes Google only when the user has password sign-in
// available. This prevents deleting the only credential from a Google-first
// account.
func (s *Store) UnlinkGoogleIdentity(ctx context.Context, userID string) error {
	var passwordEnabled bool
	if err := s.db.QueryRowContext(ctx, `SELECT password_auth_enabled FROM users WHERE id=?`, userID).Scan(&passwordEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !passwordEnabled {
		return ErrLastIdentity
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM user_identities WHERE user_id=? AND provider='google'`, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
